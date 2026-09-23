#!/usr/bin/env sh
# verify-source-zip.sh <zip-file> [--require <path>]...
#
# Verifies that a source-only ZIP is safe and complete for this repository.
#
# Checks:
#   1. Name/path checks on the archive listing: no absolute paths, no backslash
#      paths, no empty names, no parent traversal, no duplicate entries.
#   2. Forbidden-path rejection: .git, virtualenvs, dependency/vendor dirs,
#      build outputs, binaries, caches, coverage, datasets/corpora, nested
#      archives, media, OS junk, and likely secret material (real .env files,
#      private keys, certificates, credentials, token files) at any path depth.
#      Non-secret templates such as .env.example are allowed at any depth.
#      Local development-control directories (.opencode, .agents, .codex) are
#      rejected.
#   3. Compact extracted-content check: after extraction, `file` rejects
#      binaries/executables (including extensionless ones), nested archives,
#      and media by content; secret markers are detected by content.
#   4. Required anchor presence: go.mod, Dockerfile, docker-compose.yml,
#      cmd/pii-service source, Python worker source, and active OpenSpec files.
#
# Fails closed on missing tools or any violation. Exits 0 on pass.
set -eu

ZIP="${1:-}"
if [ -z "$ZIP" ]; then
  echo "usage: $0 <zip-file> [--require <path>]..." >&2
  exit 2
fi
if [ ! -f "$ZIP" ]; then
  echo "error: not a file: $ZIP" >&2
  exit 2
fi
shift

REQUIRE=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --require)
      [ "$#" -ge 2 ] || { echo "error: --require needs a path" >&2; exit 2; }
      REQUIRE="$REQUIRE $2"
      shift 2
      ;;
    *)
      echo "error: unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

for tool in unzip file mktemp grep sort uniq tr find; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "error: missing required tool: $tool" >&2
    exit 1
  fi
done

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

LISTING_FILE="$(mktemp)"
EXTRACT_DIR="$(mktemp -d)"
trap 'rm -rf "$EXTRACT_DIR"; rm -f "$LISTING_FILE"' EXIT HUP INT TERM

if ! unzip -Z1 "$ZIP" > "$LISTING_FILE" 2>/dev/null; then
  fail "cannot read zip listing: $ZIP"
fi
if [ ! -s "$LISTING_FILE" ]; then
  fail "zip has no entries"
fi

# duplicate entries
DUPS="$(sort "$LISTING_FILE" | uniq -d)"
if [ -n "$DUPS" ]; then
  fail "duplicate entries: $(printf '%s' "$DUPS" | tr '\n' ' ')"
fi

# forbidden path patterns. Returns 0 (allowed) or 1 (forbidden).
check_forbidden_path() {
  p="$1"
  case "$p" in
    ./*) p="${p#./}" ;;
  esac
  case "$p" in
    .git|.git/*|*/.git|*/.git/*) return 1 ;;
    .opencode|.opencode/*|*/.opencode|*/.opencode/*) return 1 ;;
    .agents|.agents/*|*/.agents|*/.agents/*) return 1 ;;
    .codex|.codex/*|*/.codex|*/.codex/*) return 1 ;;
    .venv|.venv/*|venv|venv/*|virtualenv|virtualenv/*|virtualenvs|virtualenvs/*) return 1 ;;
    __pycache__|__pycache__/*|*/__pycache__|*/__pycache__/*) return 1 ;;
    *.pyc|*.pyo|*.py[co]) return 1 ;;
    vendor|vendor/*|*/vendor|*/vendor/*) return 1 ;;
    node_modules|node_modules/*|*/node_modules|*/node_modules/*) return 1 ;;
    site-packages|site-packages/*|*/site-packages|*/site-packages/*) return 1 ;;
    .cache|.cache/*|*/.cache|*/.cache/*) return 1 ;;
    .pytest_cache|.pytest_cache/*|*/.pytest_cache|*/.pytest_cache/*) return 1 ;;
    .mypy_cache|.mypy_cache/*|*/.mypy_cache|*/.mypy_cache/*) return 1 ;;
    .ruff_cache|.ruff_cache/*|*/.ruff_cache|*/.ruff_cache/*) return 1 ;;
    CACHEDIR.TAG) return 1 ;;
    dist|dist/*|*/dist|*/dist/*) return 1 ;;
    build|build/*|*/build|*/build/*) return 1 ;;
    bin|bin/*|*/bin|*/bin/*) return 1 ;;
    out|out/*|*/out|*/out/*) return 1 ;;
    target|target/*|*/target|*/target/*) return 1 ;;
    *.o|*.a|*.so|*.dylib|*.dll|*.exe|*.obj|*.lib) return 1 ;;
    .coverage|coverage|coverage/*|*/coverage|*/coverage/*|htmlcov|htmlcov/*|*/htmlcov|*/htmlcov/*) return 1 ;;
    *.lcov|*.gcov|*.gcda|*.gcno) return 1 ;;
    data|data/*|*/data|*/data/*) return 1 ;;
    datasets|datasets/*|*/datasets|*/datasets/*) return 1 ;;
    corpus|corpus/*|*/corpus|*/corpus/*) return 1 ;;
    corpora|corpora/*|*/corpora|*/corpora/*) return 1 ;;
    models|models/*|*/models|*/models/*) return 1 ;;
    weights|weights/*|*/weights|*/weights/*) return 1 ;;
    checkpoints|checkpoints/*|*/checkpoints|*/checkpoints/*) return 1 ;;
    *.jsonl|*.parquet|*.csv|*.onnx|*.pt|*.pth|*.safetensors|*.h5|*.hdf5|*.npy|*.npz|*.arrow) return 1 ;;
    *.zip|*.tar|*.tar.gz|*.tgz|*.gz|*.bz2|*.xz|*.7z|*.rar|*.jar|*.war|*.whl|*.egg|*.tar.bz2|*.tar.xz) return 1 ;;
    *.png|*.jpg|*.jpeg|*.gif|*.bmp|*.ico|*.svg|*.webp|*.tif|*.tiff) return 1 ;;
    *.mp3|*.mp4|*.wav|*.ogg|*.webm|*.mov|*.avi|*.flac|*.m4a|*.aac) return 1 ;;
    *.pdf|*.woff|*.woff2|*.ttf|*.otf|*.eot|*.eps) return 1 ;;
    .DS_Store|Thumbs.db|desktop.ini) return 1 ;;
    *.swp|*.swo|*~) return 1 ;;
    .env|.env.*|*/.env|*/.env.*)
      case "$p" in
        .env.example|.env.sample|.env.template|.env.example.*|.env.sample.*|.env.template.*|*/.env.example|*/.env.sample|*/.env.template|*/.env.example.*|*/.env.sample.*|*/.env.template.*) return 0 ;;
        *) return 1 ;;
      esac
      ;;
    *.pem|*.key|*.p12|*.pfx|*.jks|*.keystore|*.crt|*.cer|*.der|*.p8|*.ppk) return 1 ;;
    id_rsa|id_rsa.pub|id_ed25519|id_ed25519.pub|id_dsa|id_ecdsa|*/id_rsa|*/id_rsa.pub|*/id_ed25519|*/id_ed25519.pub|*/id_dsa|*/id_ecdsa) return 1 ;;
    *.token|*.secret|*.credentials|credentials|credentials/*|*/credentials|*/credentials/*) return 1 ;;
    .aws|.aws/*|*/.aws|*/.aws/*|.ssh|.ssh/*|*/.ssh|*/.ssh/*) return 1 ;;
    .npmrc|.pypirc|.netrc|.git-credentials) return 1 ;;
  esac
  return 0
}

# name/path checks
violation=""
while IFS= read -r entry; do
  if [ -z "$entry" ]; then
    violation="empty entry name"
    break
  fi
  case "$entry" in
    /*) violation="absolute path: $entry"; break ;;
    *\\*) violation="backslash path: $entry"; break ;;
  esac
  case "/$entry" in
    */../*|*/..) violation="parent traversal: $entry"; break ;;
  esac
  if ! check_forbidden_path "$entry"; then
    violation="forbidden path: $entry"
    break
  fi
done < "$LISTING_FILE"
if [ -n "$violation" ]; then
  fail "$violation"
fi

# extraction
if ! unzip -q "$ZIP" -d "$EXTRACT_DIR" 2>/dev/null; then
  fail "extraction failed: $ZIP"
fi

# reject symlinks (zip-slip defense)
if find "$EXTRACT_DIR" -type l | grep -q .; then
  fail "archive contains symlinks"
fi

# compact extracted-content check
find "$EXTRACT_DIR" -type f > "$EXTRACT_DIR/.filelist"
# Secret-marker ERE assembled at runtime from separate fragments so this
# checked-in source does not contain a complete marker expression that would
# match itself during archive scanning. Detection still covers generic and
# typed (RSA/EC/OPENSSH/DSA) private keys plus certificates.
B="$(printf '%s' 'BEGIN')"
K="$(printf '%s' 'PRIVATE')"
Y="$(printf '%s' 'KEY')"
C="$(printf '%s' 'CERTIFICATE')"
SECRET_ERE="$B (RSA |EC |OPENSSH |DSA )?$K $Y|$B $C"
violation=""
while IFS= read -r f; do
  rel="${f#"$EXTRACT_DIR"/}"
  ft="$(file -b "$f")"
  case "$ft" in
    *ELF*|*Mach-O*|*PE32*|*MS-DOS*)
      violation="binary/executable content: $rel ($ft)"; break ;;
    *Zip*archive*|*gzip*compressed*|*tar*archive*|*bzip2*compressed*|*XZ*compressed*|*7-zip*archive*|*RAR*archive*)
      violation="nested archive content: $rel ($ft)"; break ;;
    *PNG*image*|*JPEG*image*|*GIF*image*|*BMP*image*|*ICO*image*|*SVG*|*Web/P*image*|*TIFF*image*|*PDF*document*|*audio*|*video*|*font*)
      violation="media content: $rel ($ft)"; break ;;
  esac
  if [ -z "$violation" ] && grep -qE "$SECRET_ERE" "$f" 2>/dev/null; then
    violation="secret content: $rel"
    break
  fi
done < "$EXTRACT_DIR/.filelist"
if [ -n "$violation" ]; then
  fail "$violation"
fi

# required anchors
check_anchor() {
  a="$1"
  case "$a" in
    */)
      if ! grep -q "^${a%/}/" "$LISTING_FILE"; then
        fail "missing required anchor: $a"
      fi
      ;;
    *)
      if ! grep -qx "$a" "$LISTING_FILE"; then
        fail "missing required anchor: $a"
      fi
      ;;
  esac
}

check_anchor "go.mod"
check_anchor "Dockerfile"
check_anchor "docker-compose.yml"
check_anchor "cmd/pii-service/"
check_anchor "python/model_worker/"
if ! grep -qE '^openspec/changes/[^/]+/tasks\.md$' "$LISTING_FILE"; then
  fail "missing required anchor: active OpenSpec tasks.md"
fi
for r in $REQUIRE; do
  check_anchor "$r"
done

echo "OK: $ZIP"