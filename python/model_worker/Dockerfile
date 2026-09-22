# syntax=docker/dockerfile:1

# Python model worker for the PII NER service. It loads the two pinned
# Hugging Face models once at startup and serves the narrow /health, /infer
# and /count_tokens surface. It runs as a non-root user and never receives
# encryption keys, token mappings or privacy decisions.
FROM python:3.11-slim

# Non-root user. The worker downloads models on first run into HF_HOME, so the
# cache directory must be writable by this user.
RUN groupadd --gid 10001 pii && useradd --uid 10001 --gid pii --create-home --home-dir /home/pii pii

WORKDIR /app

# Install pinned runtime dependencies first so the layer is cached across
# source changes. requirements.txt is pinned to exact versions and includes a
# git dependency (gliner2), so git is required at build time.
RUN apt-get update && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/*
COPY python/model_worker/requirements.txt ./requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

# Copy the worker source only. No tests, schemas, virtualenvs or datasets.
COPY python/model_worker/backends.py ./backends.py
COPY python/model_worker/model_worker.py ./model_worker.py

ENV HF_HOME=/home/pii/.cache/huggingface \
    PYTHONUNBUFFERED=1

# Pre-create the model cache directory owned by the non-root user so a mounted
# volume at this path is writable by the worker.
RUN mkdir -p /home/pii/.cache/huggingface && chown -R pii:pii /home/pii/.cache

USER pii

EXPOSE 8000

# python is present in the base image; the healthcheck probes the worker's
# /health endpoint without sending or logging request/response bodies. The
# start-period is generous because the first run downloads the models.
HEALTHCHECK --interval=30s --timeout=5s --start-period=300s --retries=3 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/health', timeout=5).status==200 else 1)"

# Bind 0.0.0.0 so the Go service can reach the worker over the internal Docker
# network. Outside Docker the worker keeps its safe loopback default.
CMD ["python", "model_worker.py", "--host", "0.0.0.0", "--port", "8000"]