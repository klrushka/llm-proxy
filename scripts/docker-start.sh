#!/usr/bin/env sh
# Minimal task-12.5 startup helper for the PII service + model worker.
#
# Generates a 32-byte base64 vault key if PII_VAULT_KEY is not already set,
# validates the compose file, and brings the stack up. The key is never
# committed or baked into an image; it lives only in the environment.
set -eu

if [ -z "${PII_VAULT_KEY:-}" ]; then
  PII_VAULT_KEY="$(openssl rand -base64 32)"
  export PII_VAULT_KEY
  echo "Generated a fresh PII_VAULT_KEY (32-byte base64)."
fi

echo "Validating docker compose config..."
docker compose config >/dev/null

echo "Building and starting the stack..."
docker compose up -d --build

echo "Stack is up. Go service: http://localhost:8080"
echo "Health: curl -s http://localhost:8080/health/ready"
echo "Stop with: docker compose down"