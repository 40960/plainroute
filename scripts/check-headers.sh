#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Check that every file in the repository carries the licence identifier. Two files are
# excluded because they cannot carry one: LICENSE is the licence text itself, and go.sum is
# machine generated with one hash per line, so a comment in it is not valid content.
#
# The identifier must be in the header, within the first three lines. A file that mentions the
# string in prose is not a file that carries it, which is why this checks the header rather
# than grepping the whole file.
#
# Usage: ./scripts/check-headers.sh
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

MISSING=0
for f in $(git ls-files); do
  if [ "$f" = "LICENSE" ] || [ "$f" = "go.sum" ]; then
    continue
  fi
  if ! head -3 "$f" | grep -q 'SPDX-License-Identifier: AGPL-3.0-only'; then
    printf '  %s\n' "$f"
    MISSING=1
  fi
done

if [ "$MISSING" = 1 ]; then
  printf 'files above are missing the licence identifier\n' >&2
  exit 1
fi

printf 'every tracked file carries the licence identifier\n'
exit 0
