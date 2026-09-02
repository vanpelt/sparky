#!/bin/bash
# Rebuild the published lesson sheet and push it. Safe to run on a schedule:
# it does nothing if the sheet hasn't changed, and it never touches other work
# in the tree — only tools/klozar/index.html is staged.
set -euo pipefail

cd "$(dirname "$0")"
REPO=$(git rev-parse --show-toplevel)

uv run klozar.py site

# index.html changes every week (it bakes the newest sheet); weeks/ only grows.
PATHS="tools/klozar/index.html tools/klozar/weeks tools/klozar/weeks.json"
if git -C "$REPO" diff --quiet -- $PATHS && \
   [ -z "$(git -C "$REPO" ls-files --others --exclude-standard -- $PATHS)" ]; then
  echo "No change this week."
  exit 0
fi

BRANCH=$(git -C "$REPO" rev-parse --abbrev-ref HEAD)
if [ "$BRANCH" != "main" ]; then
  echo "On branch $BRANCH, not main — sheet rebuilt but not pushed."
  exit 0
fi

git -C "$REPO" add $PATHS
git -C "$REPO" commit -q -m "chore(klozar): Serbian sheet for week ending $(date +%Y-%m-%d)"
git -C "$REPO" push -q
echo "Pushed. Live in a minute at https://vanpelt.github.io/sparky/tools/klozar/"
