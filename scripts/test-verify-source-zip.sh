#!/usr/bin/env sh
# test-verify-source-zip.sh
#
# Automated tests for scripts/verify-source-zip.sh. Builds synthetic ZIPs and
# asserts the verifier's pass/fail behavior. Fails closed: missing tools or an
# inability to construct a malicious fixture fails the suite (no SKIP path).
#
# Content-detection fixtures (media, nested archive, native binary) are stored
# under neutral .bin / extensionless names so rejection proves the content
# check, not a filename rule. Binary/media bytes are generated with POSIX octal
# printf escapes. The traversal fixture is a minimal ZIP byte fixture whose
# central-directory listing contains the exact malicious name; the verifier
# must reject the unsafe name before extraction.
set -eu

VERIFY="$(dirname "$0")/verify-source-zip.sh"

for tool in zip unzip mktemp grep; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "FAIL: missing required tool: $tool" >&2
    exit 1
  fi
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

pass=0
fail=0

# --- build a valid base source tree ---
SRC="$TMP/src"
mkdir -p "$SRC/cmd/pii-service" "$SRC/python/model_worker" "$SRC/openspec/changes/test-change"
printf 'module example.com/pii\n\ngo 1.23\n' > "$SRC/go.mod"
printf 'FROM scratch\n' > "$SRC/Dockerfile"
printf 'services: {}\n' > "$SRC/docker-compose.yml"
printf 'package main\n' > "$SRC/cmd/pii-service/main.go"
printf 'def main():\n    pass\n' > "$SRC/python/model_worker/model_worker.py"
printf '# task\n' > "$SRC/openspec/changes/test-change/tasks.md"

make_zip() { # make_zip <dir> <out.zip>
  (cd "$1" && zip -qr "$2" .)
}

# run the verifier and assert the expected exit status
expect() { # expect <expected-exit> <zip> <label>
  expected="$1"
  z="$2"
  label="$3"
  if sh "$VERIFY" "$z" >/dev/null 2>&1; then
    got=0
  else
    got=1
  fi
  if [ "$got" -eq "$expected" ]; then
    echo "PASS: $label"
    pass=$((pass + 1))
  else
    echo "FAIL: $label (expected exit $expected, got $got)"
    fail=$((fail + 1))
  fi
}

# assert a fixture entry exists in the zip listing; failure is fatal
assert_entry() { # assert_entry <zip> <entry> <label>
  z="$1"
  entry="$2"
  label="$3"
  if ! unzip -Z1 "$z" | grep -qx "$entry"; then
    echo "FAIL: could not construct $label fixture (missing entry: $entry)" >&2
    exit 1
  fi
}

# 1. valid archive passes
make_zip "$SRC" "$TMP/valid.zip"
expect 0 "$TMP/valid.zip" "valid archive passes"

# 2. forbidden path (.env) rejected
cp -r "$SRC" "$TMP/forbidden"
printf 'SECRET=1\n' > "$TMP/forbidden/.env"
make_zip "$TMP/forbidden" "$TMP/forbidden.zip"
assert_entry "$TMP/forbidden.zip" ".env" "forbidden .env"
expect 1 "$TMP/forbidden.zip" "forbidden .env rejected"

# 3. nested secret filename (nested/.env) rejected
cp -r "$SRC" "$TMP/nestedenv"
mkdir -p "$TMP/nestedenv/nested"
printf 'SECRET=1\n' > "$TMP/nestedenv/nested/.env"
make_zip "$TMP/nestedenv" "$TMP/nestedenv.zip"
assert_entry "$TMP/nestedenv.zip" "nested/.env" "nested .env"
expect 1 "$TMP/nestedenv.zip" "nested .env rejected"

# 4. nested credential/key name rejected
cp -r "$SRC" "$TMP/nestedkey"
mkdir -p "$TMP/nestedkey/nested"
printf 'x\n' > "$TMP/nestedkey/nested/id_rsa"
make_zip "$TMP/nestedkey" "$TMP/nestedkey.zip"
assert_entry "$TMP/nestedkey.zip" "nested/id_rsa" "nested id_rsa"
expect 1 "$TMP/nestedkey.zip" "nested id_rsa rejected"

# 5. .env.example template allowed at any depth
cp -r "$SRC" "$TMP/envtmpl"
mkdir -p "$TMP/envtmpl/nested"
printf 'EXAMPLE=1\n' > "$TMP/envtmpl/nested/.env.example"
make_zip "$TMP/envtmpl" "$TMP/envtmpl.zip"
assert_entry "$TMP/envtmpl.zip" "nested/.env.example" ".env.example template"
expect 0 "$TMP/envtmpl.zip" ".env.example template allowed"

# 6. path traversal rejected (minimal ZIP byte fixture)
# A single stored entry "../evil" (7-byte name, empty data). The central
# directory is valid and its CRC (0) matches the empty data, so `unzip -Z1`
# lists the entry; the verifier must reject the unsafe name before extraction.
# Each ZIP field is written with its own printf call so the byte layout is
# explicit and correct.
{
  # local file header
  printf 'PK\003\004'                    # signature
  printf '\024\000'                      # version needed (20)
  printf '\000\000'                      # flags
  printf '\000\000'                      # method (0 = stored)
  printf '\000\000'                      # mod time
  printf '\000\000'                      # mod date
  printf '\000\000\000\000'              # crc32 (0, valid for empty data)
  printf '\000\000\000\000'              # compressed size (0)
  printf '\000\000\000\000'              # uncompressed size (0)
  printf '\007\000'                      # filename length (7)
  printf '\000\000'                      # extra length (0)
  printf '../evil'                       # filename
  # central directory header
  printf 'PK\001\002'                    # signature
  printf '\024\000'                      # version made by (20)
  printf '\024\000'                      # version needed (20)
  printf '\000\000'                      # flags
  printf '\000\000'                      # method (0 = stored)
  printf '\000\000'                      # mod time
  printf '\000\000'                      # mod date
  printf '\000\000\000\000'              # crc32 (0)
  printf '\000\000\000\000'              # compressed size (0)
  printf '\000\000\000\000'              # uncompressed size (0)
  printf '\007\000'                      # filename length (7)
  printf '\000\000'                      # extra length (0)
  printf '\000\000'                      # comment length (0)
  printf '\000\000'                      # disk number start (0)
  printf '\000\000'                      # internal attrs (0)
  printf '\000\000\000\000'              # external attrs (0)
  printf '\000\000\000\000'              # local header offset (0)
  printf '../evil'                       # filename
  # end of central directory
  printf 'PK\005\006'                    # signature
  printf '\000\000'                      # disk number (0)
  printf '\000\000'                      # disk with central dir (0)
  printf '\001\000'                      # entries on this disk (1)
  printf '\001\000'                      # total entries (1)
  printf '\065\000\000\000'              # central dir size (53)
  printf '\045\000\000\000'              # central dir offset (37)
  printf '\000\000'                      # comment length (0)
} > "$TMP/trav.zip"
assert_entry "$TMP/trav.zip" "../evil" "path traversal"
expect 1 "$TMP/trav.zip" "path traversal rejected"

# 7. nested archive rejected (neutral .bin name, real zip content)
cp -r "$SRC" "$TMP/nested"
mkdir -p "$TMP/nested/inner"
printf 'x\n' > "$TMP/nested/inner/evil.txt"
(cd "$TMP/nested/inner" && zip -q "$TMP/nested/data.bin" evil.txt)
make_zip "$TMP/nested" "$TMP/nested.zip"
assert_entry "$TMP/nested.zip" "data.bin" "nested archive"
expect 1 "$TMP/nested.zip" "nested archive rejected"

# 8. secret content rejected (neutral .bin name, private-key content)
# The marker is assembled at runtime from separate fragments so the checked-in
# source does not contain a private-key signature; the generated fixture still
# holds exactly a generic BEGIN/END PRIVATE KEY marker.
cp -r "$SRC" "$TMP/secret"
BEGIN="$(printf '%s' '-----BEGIN ')"
END="$(printf '%s' '-----END ')"
KEY="$(printf '%s' 'PRIVATE KEY-----')"
printf '%s\n%s\n%s\n' "$BEGIN$KEY" 'AAAA' "$END$KEY" > "$TMP/secret/data.bin"
make_zip "$TMP/secret" "$TMP/secret.zip"
assert_entry "$TMP/secret.zip" "data.bin" "secret content"
expect 1 "$TMP/secret.zip" "secret content rejected"

# 9. media rejected (neutral .bin name, PNG content via octal escapes)
cp -r "$SRC" "$TMP/media"
printf '\211PNG\015\012\032\012\000\000\000\015IHDR\000\000\000\001\000\000\000\001\010\006\000\000\000\037\025\304\211' > "$TMP/media/data.bin"
make_zip "$TMP/media" "$TMP/media.zip"
assert_entry "$TMP/media.zip" "data.bin" "media content"
expect 1 "$TMP/media.zip" "media content rejected"

# 10. extensionless native binary rejected (ELF content via octal escapes)
cp -r "$SRC" "$TMP/bin"
printf '\177ELF\002\001\001' > "$TMP/bin/runner"
make_zip "$TMP/bin" "$TMP/bin.zip"
assert_entry "$TMP/bin.zip" "runner" "extensionless binary"
expect 1 "$TMP/bin.zip" "extensionless binary rejected"

echo
echo "results: $pass passed, $fail failed"
[ "$fail" -eq 0 ]