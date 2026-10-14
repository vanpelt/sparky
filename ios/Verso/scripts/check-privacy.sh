#!/usr/bin/env bash
# Fails if any figure from the gitignored local seed appears in a tracked-or-trackable file under ios/.
# The forbidden list is derived at runtime from Seed/grant.local.json, so it is never committed.
set -euo pipefail
cd "$(dirname "$0")/.."
SEED="Seed/grant.local.json"
[ -f "$SEED" ] || { echo "no local seed; nothing to check"; exit 0; }

git check-ignore -q "$SEED" || { echo "FAIL: $SEED is NOT gitignored"; exit 1; }

# Share counts, salary and bonus: plain and comma-grouped forms (skip tiny values that would match everywhere).
NEEDLES=$(python3 - "$SEED" <<'PY'
import json, sys
g = json.load(open(sys.argv[1]))["grant"]
out = set()
for k in ("revestShares", "rsuShares", "rolloverShares", "salaryAnnual", "bonusAnnual"):
    v = g.get(k)
    if isinstance(v, (int, float)) and v >= 1000:
        v = int(v); out.update({str(v), f"{v:,}", f"{v:_}"})
print("\n".join(sorted(out)))
PY
)

# Files git would commit: tracked + untracked-but-not-ignored.
FILES=$( (git ls-files -- . ; git ls-files --others --exclude-standard -- .) | sort -u)
fail=0
while IFS= read -r n; do
  [ -z "$n" ] && continue
  hits=$(printf '%s\n' "$FILES" | xargs -I{} grep -lF -- "$n" {} 2>/dev/null || true)
  if [ -n "$hits" ]; then echo "FAIL: a private figure appears in:"; echo "$hits"; fail=1; fi
done <<< "$NEEDLES"
[ $fail -eq 0 ] && echo "ok: no private figures in $(printf '%s\n' "$FILES" | wc -l | tr -d ' ') committable files"
exit $fail
