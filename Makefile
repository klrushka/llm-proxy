# Developer commands
#
# gitleaks: reproducible Gitleaks secret-scan gate.
#   - Scans the full git history (all commits).
#   - Scans the current working-tree files.
#   - Redacted output; any finding fails the command (exit code 1).
#   - No baseline: a clean repo must pass with zero findings.
#   - Pinned tool version: GITLEAKS_VERSION.

GITLEAKS_VERSION := v8.21.2

.PHONY: gitleaks

gitleaks:
	go run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) git --redact --exit-code 1
	go run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) dir --redact --exit-code 1