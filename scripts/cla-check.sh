#!/usr/bin/env bash
# Lists commit authors in <base>..<head> that are not in .github/CLA-SIGNERS.
# Exit 0 when everyone has signed, 1 otherwise. No network, no gh.
#   scripts/cla-check.sh origin/main HEAD
set -euo pipefail

base="${1:?usage: cla-check.sh <base-ref> [head-ref]}"
head="${2:-HEAD}"
root="$(git rev-parse --show-toplevel)"
signers="${CLA_SIGNERS_FILE:-$root/.github/CLA-SIGNERS}"

if [ ! -f "$signers" ]; then
  echo "cla-check: $signers not found" >&2
  exit 2
fi

missing=0
while IFS= read -r author; do
  [ -z "$author" ] && continue
  if ! grep -vE '^\s*(#|$)' "$signers" | grep -Fxq -- "$author"; then
    echo "NOT SIGNED: $author"
    missing=1
  fi
done < <(git log --format='%an <%ae>' "$base..$head" | sort -u)

if [ "$missing" -ne 0 ]; then
  echo "cla-check: unsigned contributors in $base..$head" >&2
  exit 1
fi
echo "cla-check: all authors in $base..$head have signed"
