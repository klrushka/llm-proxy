#!/usr/bin/env sh
# build-source-zip.sh [treeish]
#
# Builds a source-only ZIP from Git-tracked source at TREEISH (default HEAD)
# and verifies it with scripts/verify-source-zip.sh.
#
# The archive is produced with `git archive` from the selected Git tree (never
# a recursive working-directory copy), so every archived file is exactly a
# Git-tracked file at that treeish, minus intentional export-ignore exclusions
# (see .gitattributes). The archive is written to the ignored dist/ directory
# and is never added to Git.
#
# When a treeish is supplied, verification ensures every archived file exists
# in that selected Git tree while allowing intentional export-ignore
# exclusions. Comparison data is kept in a private mktemp directory and removed
# exactly via a trap.
set -eu

TREEISH="${1:-HEAD}"

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "error: not a git repository" >&2
  exit 1
}
cd "$REPO_ROOT"

for tool in git unzip mktemp comm sort grep; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "error: missing required tool: $tool" >&2
    exit 1
  fi
done

if ! git rev-parse --verify "$TREEISH^{commit}" >/dev/null 2>&1; then
  echo "error: invalid treeish: $TREEISH" >&2
  exit 1
fi

SHORT="$(git rev-parse --short "$TREEISH")"
OUT_DIR="$REPO_ROOT/dist"
OUT_ZIP="$OUT_DIR/llm-proxy-source-$SHORT.zip"
mkdir -p "$OUT_DIR"

# Private scratch for comparison data; removed exactly via trap.
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT HUP INT TERM

# Build from the Git tree at TREEISH (not the working directory). export-ignore
# exclusions from .gitattributes are applied automatically by git archive.
git archive --format=zip --output="$OUT_ZIP" "$TREEISH"

# Ensure every archived file exists in the selected Git tree, allowing
# intentional export-ignore exclusions. git archive guarantees this by
# construction; this is an explicit, independent check. The archive may contain
# fewer files than the tree (export-ignore), but never files absent from it.
# Only non-directory ZIP entries are compared against Git-tree file entries.
git ls-tree -r --name-only "$TREEISH" | sort > "$SCRATCH/tree-files"
unzip -Z1 "$OUT_ZIP" | grep -v '/$' | sort > "$SCRATCH/zip-files"
# comm -13: lines only in the second file (archived files not in the tree).
if comm -13 "$SCRATCH/tree-files" "$SCRATCH/zip-files" | grep -q .; then
  echo "error: archive contains files not present in the Git tree at $TREEISH" >&2
  exit 1
fi

# Active OpenSpec change anchors (the change under openspec/changes, excluding
# the archive/ directory).
ACTIVE_CHANGE=""
for d in openspec/changes/*/; do
  case "$d" in
    openspec/changes/archive/) continue ;;
    *) ACTIVE_CHANGE="${d%/}"; break ;;
  esac
done

REQUIRE=""
if [ -n "$ACTIVE_CHANGE" ]; then
  REQUIRE="--require $ACTIVE_CHANGE/tasks.md --require $ACTIVE_CHANGE/design.md --require $ACTIVE_CHANGE/proposal.md --require $ACTIVE_CHANGE/specs/"
fi

echo "Verifying $OUT_ZIP ..."
# shellcheck disable=SC2086
sh scripts/verify-source-zip.sh "$OUT_ZIP" $REQUIRE
echo "OK: $OUT_ZIP"