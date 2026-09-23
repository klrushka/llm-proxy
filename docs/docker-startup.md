# Docker startup (task 12.5)

This document is scoped to task 12.5: minimal production-ready Docker packaging
for the Go service and the Python model worker. The full README is task 12.6.

## Prerequisites

- Docker with the Compose plugin (`docker compose version`).
- A 32-byte vault key, base64-encoded. The service refuses to start without it.

## Generate the vault key

The vault key must decode to exactly 32 bytes. Generate one with:

```sh
export PII_VAULT_KEY="$(openssl rand -base64 32)"
```

The key is read from the environment only; it is never committed to the
repository or baked into an image. `docker compose config` fails with a clear
message if `PII_VAULT_KEY` is not set, so the compose file can be validated
without committing a real secret (set any 32-byte base64 value for validation).

## Build and start

```sh
export PII_VAULT_KEY="$(openssl rand -base64 32)"
docker compose up -d --build
```

- The Go service listens on `0.0.0.0:8080` and is published to the host at
  `http://localhost:8080`.
- Prometheus metrics are served on the internal port `9464`, which is only
  exposed on the private network and never published to the host.
- The Python worker binds `0.0.0.0:8000` on the private bridge network only; it
  is not published to the host. The Go service reaches it at
  `http://model-worker:8000`. The network is not `internal: true` because the
  worker needs outbound internet access on first run to download the models.

## Health verification

```sh
# Go service liveness and readiness
curl -s http://localhost:8080/health/live
curl -s http://localhost:8080/health/ready

# Metrics (internal port, read from inside the container)
docker compose exec pii-service wget -q -O - http://127.0.0.1:9464/metrics

# Worker health (from inside the Go container, or via docker compose exec)
docker compose exec pii-service wget -q -O - http://model-worker:8000/health
```

A healthy `/health/ready` returns `{"status":"ready"}`. The `/v1/runtime/chat`
route is registered but returns `503` because no real LLM client is configured
yet; this is intentional and fail-closed.

## Fast mode

Fast mode skips the model worker entirely and uses only the Go rules/validators.
It makes no HTTP calls to the worker and needs no model download. On a clean
stack, start only the Go service with `--no-deps` so `model-worker` is not
started at all:

```sh
PII_MODEL_MODE=fast docker compose up -d --build --no-deps pii-service
```

`--no-deps` is required: without it Compose starts `model-worker` too, which
would download the models on first run even though fast mode never consults it.
If a worker is already running from a previous full-mode `up`, stop it
separately before relying on fast mode:

```sh
docker compose stop model-worker
```

The normal full-mode topology (both services, worker on the private bridge
network) is unchanged; fast mode only changes which containers are started and
which the Go client calls.

## First-run model download and cache

On first run in `full` mode the worker downloads the two pinned Hugging Face
models (`redmadrobot-rnd/rubert-base-pii-ner` and
`vladlinv/ru-pii-ner-gliner2.5`) into `HF_HOME=/home/pii/.cache/huggingface`.
This can take several minutes and requires network access from the worker
container. The cache is persisted in the `hf-cache` Docker volume, so
subsequent restarts reuse the downloaded models.

## Stop

```sh
docker compose down
```

`docker compose down -v` also removes the model cache volume (forces a
re-download on the next `up`).

## Validation commands

```sh
# Compose file is valid (set any 32-byte base64 value first)
export PII_VAULT_KEY="$(openssl rand -base64 32)"
docker compose config

# Go build and tests
go build ./...
go test ./...

# Python worker tests (from the repo root)
python -m unittest discover -s python/model_worker -p 'test_*.py'
```