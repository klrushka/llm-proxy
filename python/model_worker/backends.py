"""Model backends for the PII NER worker.

Each backend loads a ready-made Hugging Face model exactly once and returns
only safe entity values (label, start, end, confidence, model). Source text is
never carried on the returned values.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol


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
            model="redmadrobot-rnd/rubert-base-pii-ner",
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


# Canonical GLiNER label set from the model card. Not invented; passed as input.
GLINER_LABELS = [
    "ru_pii_person",
    "ru_pii_location",
    "ru_pii_date",
    "ru_pii_phone",
    "ru_pii_email",
    "ru_pii",
]


class GLiNERBackend:
    """Boundary-architecture backend for vladlinv/ru-pii-ner-gliner2.5."""

    name = "gliner"

    def __init__(self) -> None:
        from gliner2 import AutoExtractor  # lazy import: heavy deps

        self._model = AutoExtractor.from_pretrained("vladlinv/ru-pii-ner-gliner2.5")

    def infer(self, text: str) -> list[Entity]:
        result = self._model.extract_entities(
            text,
            GLINER_LABELS,
            threshold=0.5,
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