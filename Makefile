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

GITLEAKS_VERSION := v8.21.2
SEMGREP_VERSION := 1.177.0

.PHONY: gitleaks semgrep

gitleaks:
	go run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) git --redact --exit-code 1
	go run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) dir --redact --exit-code 1

semgrep:
	uvx --from semgrep==$(SEMGREP_VERSION) semgrep scan --config .semgrep.yml --metrics=off --severity ERROR --error cmd internal python