"""Model backends for the PII NER worker.

Each backend loads a ready-made Hugging Face model exactly once and returns
only safe entity values (label, start, end, confidence, model). Source text is
never carried on the returned values.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol

# Canonical primary model for token counting. The token-count contract is bound
# to this exact model id; it is used both when building the pipeline and when
# reporting the model in the /count_tokens response.
RUBERT_MODEL_ID = "redmadrobot-rnd/rubert-base-pii-ner"

# Upper bound for a realistic effective tokenizer capacity. A tokenizer whose
# model_max_length is unbounded (e.g. 10**30) or otherwise implausible must fail
# closed rather than produce a plan with an unbounded window.
MAX_CAPACITY = 100000


@dataclass(frozen=True)
class Entity:
    """A detected entity span. Immutable; never carries source text."""

    label: str
    start: int
    end: int
    confidence: float
    model: str


class Backend(Protocol):
    """Narrow inference surface shared by all model backends."""

    name: str

    def infer(self, text: str) -> list[Entity]:
        """Return detected entities for text. Must not retain text."""


class RuBERTBackend:
    """Token-classification backend for redmadrobot-rnd/rubert-base-pii-ner."""

    name = "rubert"

    def __init__(self) -> None:
        from transformers import pipeline  # lazy import: heavy deps

        self._pipe = pipeline(
            "token-classification",
            model=RUBERT_MODEL_ID,
            aggregation_strategy="simple",
        )

    def infer(self, text: str) -> list[Entity]:
        results = self._pipe(text)
        entities: list[Entity] = []
        for item in results:
            entities.append(
                Entity(
                    label=item["entity_group"],
                    start=int(item["start"]),
                    end=int(item["end"]),
                    confidence=float(item["score"]),
                    model=self.name,
                )
            )
        return entities

    def _tokenize(self, text: str) -> dict:
        """Tokenize with the already-loaded RuBERT tokenizer.

        Reuses the tokenizer owned by the loaded pipeline; no second model or
        tokenizer instance is created. ``add_special_tokens=False`` and no
        truncation so the result reflects the raw input length. Offset mapping
        is requested so code-point boundaries can be recovered.
        """
        tokenizer = self._pipe.tokenizer
        return tokenizer(
            text,
            add_special_tokens=False,
            truncation=False,
            return_offsets_mapping=True,
        )

    def count_tokens(self, text: str) -> int:
        """Count tokens with the already-loaded RuBERT tokenizer.

        Reuses the tokenizer owned by the loaded pipeline; no second model or
        tokenizer instance is created. ``add_special_tokens=False`` and no
        truncation so the count reflects the raw input length. The count is
        bound to the model tokenizer, never to characters or words.
        """
        return len(self._tokenize(text)["input_ids"])

    def _effective_capacity(self) -> int:
        """Real effective per-window token capacity, fail closed.

        Capacity is the real ``model_max_length`` minus the special tokens the
        tokenizer would add for a single sequence. It is never hardcoded. An
        unbounded, non-positive, or otherwise unrealistic capacity raises so the
        caller can fail closed instead of emitting a bogus plan.
        """
        tokenizer = self._pipe.tokenizer
        model_max = tokenizer.model_max_length
        special = tokenizer.num_special_tokens_to_add(pair=False)
        capacity = model_max - special
        if isinstance(capacity, bool) or not isinstance(capacity, int):
            raise ValueError("unrealistic tokenizer capacity")
        if capacity <= 0 or capacity > MAX_CAPACITY:
            raise ValueError("unrealistic tokenizer capacity")
        return capacity

    def plan_windows(self, text: str, overlap_tokens: int) -> tuple[int, list[dict]]:
        """Build a safe tokenizer-derived window plan for long text.

        The full source text is tokenized exactly once (no truncation); that
        single result supplies both the exact ``total_count`` and the global
        ordered code-point boundaries. Windows are built from those boundaries
        without re-tokenizing the whole suffix per window. Every emitted
        substring is re-tokenized and verified to fit within the effective
        capacity; on a contextual difference the boundary is safely shrunk with
        guaranteed progress. The last window absorbs any trailing untokenized
        tail so the plan ends exactly at ``len(text)``.

        Returns ``(total_count, windows)``. ``total_count`` is the exact token
        count of the full source text, never the number of windows nor the sum
        of overlapping window counts. The plan carries only code-point ranges
        and per-window token counts; no text, token ids, token strings, or
        mappings. Non-empty text that tokenizes to zero tokens yields an empty
        window list with ``total_count == 0``.
        """
        capacity = self._effective_capacity()
        if isinstance(overlap_tokens, bool) or not isinstance(overlap_tokens, int):
            raise ValueError("overlap_tokens must be an integer")
        if overlap_tokens < 0 or overlap_tokens >= capacity:
            raise ValueError("overlap_tokens out of range")

        text_len = len(text)
        if text_len == 0:
            return 0, []

        enc = self._tokenize(text)
        offsets = enc["offset_mapping"]
        total_count = len(enc["input_ids"])
        if total_count == 0:
            return 0, []

        windows: list[dict] = []
        start = 0
        tok_idx = 0
        while start < text_len:
            end, token_count, next_start, tok_idx = self._window_step(
                text, start, capacity, overlap_tokens, offsets, total_count, text_len, tok_idx
            )
            if token_count == 0:
                # Only the trailing untokenized tail remains; absorb it into the
                # previous window so no zero-token window is emitted. The
                # extended window must stay tokenful and within capacity.
                if not windows:
                    raise ValueError("window exceeds capacity")
                prev = windows[-1]
                prev["end"] = text_len
                prev["token_count"] = self.count_tokens(text[prev["start"]:text_len])
                if prev["token_count"] > capacity or prev["token_count"] == 0:
                    raise ValueError("window exceeds capacity")
                break
            windows.append({"start": start, "end": end, "token_count": token_count})
            if end >= text_len:
                break
            start = next_start
        return total_count, windows

    def _window_step(
        self,
        text: str,
        start: int,
        capacity: int,
        overlap_tokens: int,
        offsets: list[tuple[int, int]],
        total_count: int,
        text_len: int,
        tok_idx: int,
    ) -> tuple[int, int, int, int]:
        """Compute one window's end, token count, next start, and token cursor."""
        while tok_idx < total_count and offsets[tok_idx][1] <= start:
            tok_idx += 1
        if tok_idx >= total_count:
            # All global tokens are exhausted; only the trailing tail remains.
            # It is only admissible if re-tokenizing it stays within capacity.
            end = text_len
            token_count = self.count_tokens(text[start:end])
            if token_count > capacity:
                raise ValueError("window exceeds capacity")
            return end, token_count, end, tok_idx

        last = min(tok_idx + capacity, total_count) - 1
        end = offsets[last][1]
        if last == total_count - 1:
            end = text_len

        enc2 = self._tokenize(text[start:end])
        ids2 = enc2["input_ids"]
        offsets2 = enc2["offset_mapping"]
        token_count = len(ids2)

        if token_count > capacity:
            # The initial boundary exceeds capacity under re-tokenization. Find
            # the largest ordered token-boundary candidate that fits via bounded
            # binary search (O(log capacity) re-tokenizations), never a
            # one-code-point walk. If even the smallest candidate exceeds
            # capacity, fail closed rather than emit an oversized window. The
            # chosen boundary is never re-expanded to a previously rejected
            # oversized ``text_len``; any trailing tail is handled separately.
            if self.count_tokens(text[start:offsets[tok_idx][1]]) > capacity:
                raise ValueError("window exceeds capacity")
            lo, hi = tok_idx, last
            best = tok_idx
            while lo <= hi:
                mid = (lo + hi) // 2
                if self.count_tokens(text[start:offsets[mid][1]]) <= capacity:
                    best = mid
                    lo = mid + 1
                else:
                    hi = mid - 1
            end = offsets[best][1]
            enc2 = self._tokenize(text[start:end])
            ids2 = enc2["input_ids"]
            offsets2 = enc2["offset_mapping"]
            token_count = len(ids2)
            if token_count > capacity:
                raise ValueError("window exceeds capacity")

        if overlap_tokens > 0 and end < text_len:
            m = len(ids2)
            if m == 0:
                next_start = end
            else:
                overlap_begin = max(0, m - overlap_tokens)
                next_start = start + offsets2[overlap_begin][0]
        else:
            next_start = end

        if next_start <= start:
            next_start = start + 1
        return end, token_count, next_start, tok_idx


# Canonical GLiNER label set from the model card. Not invented; passed as input.
# The generic "ru_pii" label is deliberately excluded: it has no safe canonical
# type, so it is never requested.
GLINER_LABELS = [
    "ru_pii_person",
    "ru_pii_location",
    "ru_pii_date",
    "ru_pii_phone",
    "ru_pii_email",
]


class GLiNERBackend:
    """Boundary-architecture backend for vladlinv/ru-pii-ner-gliner2.5."""

    name = "gliner"

    def __init__(self) -> None:
        from gliner2 import AutoExtractor  # lazy import: heavy deps

        self._model = AutoExtractor.from_pretrained("vladlinv/ru-pii-ner-gliner2.5")

    def infer(self, text: str) -> list[Entity]:
        result = self._model.extract_entities_long(
            text,
            GLINER_LABELS,
            threshold=0.5,
            chunk_size=384,
            chunk_overlap=64,
            include_spans=True,
            include_confidence=True,
        )
        entities: list[Entity] = []
        for label, entries in result["entities"].items():
            for entry in entries:
                entities.append(
                    Entity(
                        label=label,
                        start=int(entry["start"]),
                        end=int(entry["end"]),
                        confidence=float(entry["confidence"]),
                        model=self.name,
                    )
                )
        return entities