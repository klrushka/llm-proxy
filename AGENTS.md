# Project Guidance

This repository is managed with OpenSpec. During proposal work, create planning artifacts only; do not implement service code until the proposal is reviewed and explicitly approved.

## Stack

- Main service: Go 1.23+.
- HTTP API: Go `net/http` unless an approved future task introduces a framework.
- Python: only a model worker for ready-made Hugging Face NER models.
- Models: `redmadrobot-rnd/rubert-base-pii-ner` and `vladlinv/ru-pii-ner-gliner2.5`.
- Fine-tuning is forbidden.

## Architecture Rules

- Go owns orchestration, rules, validators, model-result merging, ownership decisions, tokenization, vault, audit logging, config, and tests.
- Python worker returns only spans, labels, confidence, and model source.
- Python worker must not tokenize, store mappings, access encryption keys, or decide whether an entity is personal data.
- Tokenization must be reversible by `scope_id`; detokenization must not rerun NER.
- Persistent vault storage must contain ciphertext only.

## Safety Rules

- Never log raw text, found PII values, restored text, token mappings, CVV/PIN values, ciphertext, keys, authorization headers, or request/response bodies on errors.
- Tests and fixtures must use synthetic data only.
- Ambiguous entities must not become personal data automatically; return review metadata instead.

## Workflow

- Use OpenSpec for requirements and changes.
- One git commit must correspond to exactly one task from `tasks.md`.
- Keep changes small and task-scoped.
