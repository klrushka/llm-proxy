# Developer commands
#
# gitleaks: reproducible Gitleaks secret-scan gate.
#   - Scans the full git history (all commits).
#   - Scans the current working-tree files.
#   - Redacted output; any finding fails the command (exit code 1).
#   - No baseline: a clean repo must pass with zero findings.
#   - Pinned tool version: GITLEAKS_VERSION.
#
# semgrep: reproducible Semgrep CE SAST gate.
#   - Local CE ruleset requires no cloud login or token; uvx fetches the pinned
#     package when needed.
#   - Uses the checked-in local ruleset .semgrep.yml (never --config auto).
#   - --metrics=off disables telemetry.
#   - --severity ERROR + --error: only ERROR findings and scanner errors fail.
#   - Scans only repository Go/Python source: cmd, internal, python.
#   - Pinned tool version: SEMGREP_VERSION.
#
# source-zip: build and verify a source-only ZIP from Git-tracked source at
#   HEAD (see scripts/build-source-zip.sh). The archive is written to the
#   ignored dist/ directory and is never committed.
# verify-source-zip: verify a supplied source-only ZIP:
#   make verify-source-zip ZIP=path/to.zip
# test-source-zip: run focused automated tests for the verifier.

GITLEAKS_VERSION := v8.21.2
SEMGREP_VERSION := 1.177.0

.PHONY: gitleaks semgrep source-zip verify-source-zip test-source-zip

gitleaks:
	go run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) git --redact --exit-code 1
	go run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) dir --redact --exit-code 1

semgrep:
	uvx --from semgrep==$(SEMGREP_VERSION) semgrep scan --config .semgrep.yml --metrics=off --severity ERROR --error cmd internal python

source-zip:
	sh scripts/build-source-zip.sh

verify-source-zip:
	sh scripts/verify-source-zip.sh $(ZIP)

test-source-zip:
	sh scripts/test-verify-source-zip.sh