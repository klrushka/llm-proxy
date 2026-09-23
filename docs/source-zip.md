# Source-only ZIP build and verification (task 13.1)

This document describes the source-only ZIP build and verification flow. It is
scoped to task 13.1.

## Commands

Build and verify a source-only ZIP from Git-tracked source at `HEAD`:

```sh
make source-zip
```

The archive is written to the ignored `dist/` directory
(`dist/llm-proxy-source-<short-sha>.zip`) and is never added to Git.

Verify a supplied ZIP (verify-only):

```sh
make verify-source-zip ZIP=path/to.zip
```

Run focused automated tests for the verifier:

```sh
make test-source-zip
```

## How it works

- `scripts/build-source-zip.sh [treeish]` builds the archive with `git archive`
  from the selected Git tree (default `HEAD`), never a recursive
  working-directory copy. `export-ignore` exclusions from `.gitattributes`
  (`.opencode`, `.agents`, `.codex`, `dist`) are applied automatically.
- When a treeish is supplied, the build script independently verifies that
  every archived file exists in that selected Git tree, allowing intentional
  `export-ignore` exclusions (the archive may contain fewer files than the
  tree, but never files absent from it). Only non-directory ZIP entries are
  compared against Git-tree file entries. Comparison data lives in a private
  `mktemp` directory removed exactly via a trap.
- `scripts/verify-source-zip.sh <zip>` performs:
  1. name/path checks (no absolute paths, backslash paths, empty names, parent
     traversal, or duplicate entries);
  2. forbidden-path rejection (`.git`, virtualenvs, dependency/vendor dirs,
     build outputs, binaries, caches, coverage, datasets/corpora, nested
     archives, media, OS junk, and likely secret material such as real `.env`
     files, private keys, certificates, credentials, and token files at any
     path depth; non-secret templates such as `.env.example` are allowed at any
     depth);
  3. a compact extracted-content check using `file` so an extensionless
     executable/binary, nested archive, or media file cannot pass only because
     its filename looks harmless; secret markers are detected by content;
  4. required anchor presence: `go.mod`, `Dockerfile`, `docker-compose.yml`,
     `cmd/pii-service` source, Python worker source, and the active OpenSpec
     files.
- The verifier fails closed on missing tools or ambiguous input, and uses exact
  `mktemp` cleanup via a trap.

## Tests

`scripts/test-verify-source-zip.sh` builds synthetic ZIPs and asserts the
verifier's pass/fail behavior. It fails closed: missing tools or an inability
to construct a malicious fixture fails the suite (no SKIP path). Content
detection fixtures (media, nested archive, native binary) are stored under
neutral `.bin` / extensionless names so rejection proves the content check, not
a filename rule. Binary/media bytes are generated with POSIX octal `printf`
escapes. The traversal fixture is a minimal ZIP byte fixture whose
central-directory listing contains the exact malicious name; the verifier must
reject the unsafe name before extraction. Duplicate-entry rejection is
implemented in the verifier but is not covered by an automated fixture, because
reliable construction of a duplicate-name ZIP is not feasible with the standard
`zip` tool (it refuses repeated names).

## Evidence boundary

- The archive is built from the Git tree at the selected treeish, so it contains
  only Git-tracked source (plus intentional `export-ignore` exclusions). It is
  not a snapshot of the working directory.
- Tracked synthetic fixtures under `testdata/` are allowed because they are
  required to test the source; real dataset/corpus/model/weights/checkpoint
  directories and data/model-like payloads are rejected.
- The archive is verified for unsafe paths, duplicates, forbidden artifacts,
  likely secrets, media/nested archives, and extensionless native binaries
  before it is accepted.
- The generated archive lives in the ignored `dist/` directory and is never
  committed.