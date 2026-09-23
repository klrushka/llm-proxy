# Deployment / runbook (task 13.2)

Deployment/runbook for the current prototype on a generic Linux host with
Docker Engine and Docker Compose v2, deployed from the verified source-only ZIP
produced by task 13.1.

## Evidence boundary

- **Task 13.2 (this document):** contains a bounded local validation path (see
  below). It does not claim a remote deployment.
- **Task 13.3 (not this document):** an actual deployed URL and a live smoke
  against that URL. Nothing here claims a deployed URL, a domain, a registry, a
  CI system, or a live smoke result.

## Current runtime status (do not overclaim)

- `POST /v1/runtime/chat` runs the full product flow (mask -> downstream LLM ->
  demask) when the complete LLM configuration group is set (`PII_LLM_URL` and
  `PII_LLM_MODEL`). When the group is absent the route fails closed with `503`
  so `POST /process` can run alone.
- The benchmark adapter `POST /process` (mask/restore) and the extended
  `/v1/pii/*` API are the live, testable surface. `POST /process` never calls
  the LLM.
- The vault is the **in-memory demo adapter**: token mappings do not survive a
  container restart. The only persistent state is the `hf-cache` Docker volume
  holding the downloaded Hugging Face models.

## Prerequisites

- Docker Engine and the Compose v2 plugin (`docker compose version`).
- Operator has Docker Compose access and `sudo` rights for `/srv/llm-proxy`.
- `openssl` (vault key), `curl` (health checks), `jq` (round-trip demo only).
- Outbound internet from the host **and** the `model-worker` container on first
  run in `full` mode to download the two pinned Hugging Face models
  (`redmadrobot-rnd/rubert-base-pii-ner`, `vladlinv/ru-pii-ner-gliner2.5`). In
  `fast` mode no model download happens.

### Ports and resources

| Port | Service | Published to host |
| --- | --- | --- |
| `8080` | Go `pii-service` | Yes (`8080:8080`) |
| `8000` | Python `model-worker` | No (internal bridge only) |

`8080` must be free on the host. `8000` is reachable only from the Go container
at `http://model-worker:8000`. Disk: a few GB for the models plus images.
Memory: a few GB in `full` mode; far less in `fast` mode.

## Layout

Immutable release directories plus a shared, protected env file. Releases are
never extracted over each other.

```
/srv/llm-proxy/
  env                    # shared host-local env file, mode 600, never committed
  releases/
    <short-sha>/         # one immutable extracted source ZIP per release
    <short-sha>/docker-compose.yml
```

Every compose command pins the project name (`-p llm-proxy`) and the shared env
file (`--env-file /srv/llm-proxy/env`) so the named `hf-cache` volume is reused
across release directories and rollback.

## Configuration and secret handling

All runtime configuration is read from the environment. The vault key is the
only required secret and is **never committed**; it lives only in the shared
host-local env file.

| Variable | Default | Required | Notes |
| --- | --- | --- | --- |
| `PII_VAULT_KEY` | — | **yes** | base64 of exactly 32 bytes. Compose fails with a clear message if unset. |
| `PII_MODEL_MODE` | `full` | no | `full` (worker + models) or `fast` (Go rules only, no worker). |
| `PII_VAULT_TTL` | `15m` | no | TTL of token mappings in the vault. |
| `PII_MODEL_CLIENT_TIMEOUT` | `30s` | no | Timeout for worker calls. |
| `PII_API_LISTEN_ADDRESS` | `127.0.0.1:8080` | no | Overridden to `0.0.0.0:8080` by the compose file. |
| `PII_MODEL_WORKER_URL` | `http://127.0.0.1:8000` | no | Overridden to `http://model-worker:8000` by the compose file. |
| `PII_LLM_URL` | — | no | URL of a chat-completions-compatible downstream LLM. Optional as a group with `PII_LLM_MODEL`. |
| `PII_LLM_MODEL` | — | no | Model identifier sent in the outbound JSON. Optional as a group with `PII_LLM_URL`. |
| `PII_LLM_API_KEY` | — | no | Sent as `Authorization: Bearer` when non-empty; no header when empty. |
| `PII_LLM_TIMEOUT` | `60s` | no | Timeout for downstream LLM calls. |

The LLM group is optional as a whole: `PII_LLM_URL` and `PII_LLM_MODEL` must be
set together. When both are absent, `POST /v1/runtime/chat` fails closed with
`503` and `POST /process` runs alone. Any partial configuration is a startup
validation error.

Create the shared env file once. The key is generated at runtime, written
directly to the file (never printed to the terminal), and all temporary
variables holding the key are unset afterwards. The file is created/truncated
with mode 0600 and owned by the current deployment user's numeric uid/gid
*before* any secret is written, so it never has broader default permissions:

```sh
sudo mkdir -p /srv/llm-proxy/releases
sudo install -o "$(id -u)" -g "$(id -g)" -m 600 /dev/null /srv/llm-proxy/env
KEY="$(openssl rand -base64 32)"
printf 'PII_VAULT_KEY=%s\nPII_MODEL_MODE=full\n' "$KEY" | sudo tee /srv/llm-proxy/env >/dev/null
unset KEY
sudo chmod 600 /srv/llm-proxy/env
```

## Transfer, extract, verify

```sh
# 1. Verify the transferred archive checksum against the expected SHA-256
#    (before any extraction)
printf '%s  %s\n' '<expected-sha256>' /path/to/llm-proxy-source-<short-sha>.zip \
  | sha256sum -c -

# 2. Extract into a new immutable release directory
sudo mkdir -p /srv/llm-proxy/releases/<short-sha>
sudo unzip /path/to/llm-proxy-source-<short-sha>.zip -d /srv/llm-proxy/releases/<short-sha>

# 3. Run the verifier included in the extracted release against the original
#    archive, before any Docker build or run
sh /srv/llm-proxy/releases/<short-sha>/scripts/verify-source-zip.sh /path/to/llm-proxy-source-<short-sha>.zip
```

## Preflight

Validate the compose file without printing resolved values. Plain
`docker compose config` expands environment variables and can print the vault
key, so it must **not** be used with the real secret:

```sh
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml config --quiet
```

`config --quiet` fails with a clear message if `PII_VAULT_KEY` is unset, and
prints nothing on success.

## Build and start

Full mode (Go service + Python worker, models downloaded on first run):

```sh
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml up -d --build
```

Fast mode (Go service only, no worker, no model download). `--no-deps` is
required so `model-worker` is not started:

```sh
PII_MODEL_MODE=fast docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml up -d --build --no-deps pii-service
```

## Health and readiness

`curl -fsS` fails on non-2xx, so a non-ready service aborts the check:

```sh
curl -fsS http://localhost:8080/health/live    # {"status":"ok"}
curl -fsS http://localhost:8080/health/ready   # {"status":"ready"}
curl -fsS http://localhost:8080/metrics

# Worker health (full mode only), verified in-container with BusyBox wget
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml \
  exec pii-service wget -q -O - http://model-worker:8000/health
```

A healthy `/health/ready` returns `{"status":"ready"}`; the worker `/health`
returns `{"status":"ok","models":[...]}`.

## Logs and status

```sh
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml ps
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml logs -f
```

## Restart

```sh
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml restart
```

The vault is in-memory, so a restart clears token mappings; the `hf-cache`
volume is preserved.

## Upgrade

Repeat the Transfer, extract, verify procedure for the new ZIP (checksum
against its expected SHA-256, extraction into a fresh immutable release
directory, and the included verifier validating the original archive), then
point Compose at the new release:

```sh
# Checksum, extract, and verify as in "Transfer, extract, verify" above,
# using <new-short-sha> and its expected SHA-256.

docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<new-short-sha>/docker-compose.yml up -d --build
```

The `hf-cache` volume is reused, so models are not re-downloaded.

## Rollback

Point Compose at the previous immutable release directory and recreate from it.
No source overwrite, no broad cleanup, no `down -v`, no prune:

```sh
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<previous-short-sha>/docker-compose.yml up -d --build
```

Rollback is explicit and non-destructive: it retains the named `hf-cache`
volume (models are not re-downloaded). Recreating the Go service loses the
in-memory vault token mappings, so in-flight conversations must not be resumed
across rollback/redeploy. Do **not** use `docker compose down -v` or
`docker system prune`; those destroy the model cache and force a re-download.

## Safe stop

```sh
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml down
```

`down` keeps named volumes by default. Only use `-v` if you intentionally want
to delete the model cache (forces a re-download on next `up`).

## Bounded local validation path

This path exercises the same commands against the local Docker daemon. It is
bounded to the local host and does not claim a deployed URL or a live smoke;
those remain task 13.3.

```sh
# 1. Preflight (no secret printed)
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml config --quiet

# 2. Build and start (fast mode avoids the model download for a quick check)
PII_MODEL_MODE=fast docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml up -d --build --no-deps pii-service

# 3. Health and readiness (fail on non-2xx)
curl -fsS http://localhost:8080/health/live
curl -fsS http://localhost:8080/health/ready
curl -fsS http://localhost:8080/metrics

# 4. Benchmark adapter round trip (synthetic data only)
MASK=$(curl -fsS -X POST http://localhost:8080/process \
  -H 'Content-Type: application/json' \
  -d '{"payload":"Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com","payload_id":"runbook-demo"}' \
  | jq -r .result)
echo "mask: $MASK"
curl -fsS -X POST http://localhost:8080/process \
  -H 'Content-Type: application/json' \
  -d "{\"payload\":\"$MASK\",\"payload_id\":\"runbook-demo\"}" \
  | jq -r .result

# 4b. Runtime smoke (synthetic data only). Run this when the env file
#     (/srv/llm-proxy/env) contains the complete LLM group (PII_LLM_URL and
#     PII_LLM_MODEL). Those values live in the env file and are not exported
#     into this shell, so no conditional is used here. If the group is absent,
#     the route fails closed with 503 and this command returns that status.
curl -fsS -X POST http://localhost:8080/v1/runtime/chat \
  -H 'Content-Type: application/json' \
  -d '{"text":"Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com","scope_id":"runbook-runtime"}' \
  | jq -r .result

# 5. Logs and status
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml ps
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml logs --tail=50 pii-service

# 6. Safe stop (preserves the hf-cache volume)
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml down
```

## Live smoke (task 13.3a client)

`cmd/pii-smoke` is a reusable, standard-library-only live-smoke client. It
checks `/health/live`, `/health/ready` and the full `POST /v1/runtime/chat`
product flow (mask -> LLM -> demask) using only synthetic Russian PII, and
verifies that the final response restored the synthetic values. It exits
non-zero on any non-2xx, malformed/trailing JSON, oversized body, wrong
contract or missing restored synthetic value, and never prints request/response
bodies, credentials or sensitive data. Successful output is short and
CI-friendly.

The client is implemented and covered by focused `httptest` tests (task 13.3a).
Running it against a real deployed URL is task 13.3b and is not claimed here.

### HTTP

```sh
go run ./cmd/pii-smoke -base-url http://<host>:8080
```

### HTTPS with a self-signed certificate

`-allow-self-signed` is an explicit opt-in that is applicable only to an
`https` base URL; it is rejected for `http`. Use it only when the deployed
service presents a self-signed certificate:

```sh
go run ./cmd/pii-smoke -base-url https://<host> -allow-self-signed
```

### HTTPS with a trusted certificate

```sh
go run ./cmd/pii-smoke -base-url https://<host>
```

Optional `-timeout` bounds the whole run and every HTTP request (default `30s`).

## Validation commands (task 13.2)

```sh
# Compose file is valid (no secret printed)
docker compose -p llm-proxy --env-file /srv/llm-proxy/env \
  -f /srv/llm-proxy/releases/<short-sha>/docker-compose.yml config --quiet

# Go build and tests
go build ./...
go test ./...

# Python worker tests (from the repo root)
python -m unittest discover -s python/model_worker -p 'test_*.py'

# Source-only ZIP build and verify (task 13.1 dependency)
make source-zip
make verify-source-zip ZIP=dist/llm-proxy-source-<short-sha>.zip
```