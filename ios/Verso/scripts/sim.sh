#!/usr/bin/env bash
# Simulator helper. Ad-hoc signs (CODE_SIGN_IDENTITY=-) so the App Group entitlement is embedded and the app and
# widget extension really share the App Group; CODE_SIGNING_ALLOWED=NO would silently strip it.
#
#   scripts/sim.sh build | test [xcodebuild args…] | run [launch args…] | shot <name> [launch args…]
#   SIM=<udid> overrides the simulator (default: the first booted iPhone; never `booted`, which can pick an iPad).
#   IPAD=1 builds for iPhone and iPad (TARGETED_DEVICE_FAMILY=1,2, on this command line only: shipping stays
#     iPhone-only) and prefers a booted iPad, the 13-inch first since every iPhone Duo window fits it unrotated.
#     Pair it with `-versoFrame WxH` to stand in for a Duo window (see README, "Testing on iPhone Duo").
#   VERSO_SHOT_DIR=<dir> also writes DuoLayoutUITests' screenshots there as PNGs (`test`), and is where `shot`
#     saves (default ../.build/shots).
set -euo pipefail
cd "$(dirname "$0")/.."
IPAD="${IPAD:-}"
SIM="${SIM:-$(xcrun simctl list devices booted -j | IPAD="$IPAD" python3 -c '
import json, os, sys
d = [x for v in json.load(sys.stdin)["devices"].values() for x in v if x["state"] == "Booted"]
want = "iPad" if os.environ.get("IPAD") else "iPhone"
d.sort(key=lambda x: (want not in x["name"], "13-inch" not in x["name"]))
print(d[0]["udid"] if d else "")')}"
[ -n "$SIM" ] || { echo "no booted simulator; boot one or set SIM=<udid>"; exit 1; }
DD="${DERIVED_DATA:-../.build/dd}"
ID="$(sed -n 's/^ *VERSO_ID: *//p' project.yml)"   # single source of truth for every identifier
SIGN=(CODE_SIGN_STYLE=Manual CODE_SIGN_IDENTITY=- DEVELOPMENT_TEAM=)
[ -n "$IPAD" ] && SIGN+=(TARGETED_DEVICE_FAMILY=1,2)
xb() { xcodebuild -project Verso.xcodeproj -scheme Verso -destination "id=$SIM" -derivedDataPath "$DD" "${SIGN[@]}" "$@"; }
APP="$DD/Build/Products/Debug-iphonesimulator/Verso.app"
launch() {
  xcrun simctl install "$SIM" "$APP"
  xcrun simctl terminate "$SIM" "$ID" 2>/dev/null || true
  xcrun simctl launch "$SIM" "$ID" ${@+"$@"}
}

cmd="${1:-build}"; shift || true
case "$cmd" in
  build) xb build | grep -E " error: |\*\* BUILD" ;;
  test)
    # xcodebuild hands TEST_RUNNER_-prefixed variables to the test process with the prefix stripped.
    [ -n "${VERSO_SHOT_DIR:-}" ] && { mkdir -p "$VERSO_SHOT_DIR"; export TEST_RUNNER_VERSO_SHOT_DIR="$(cd "$VERSO_SHOT_DIR" && pwd)"; }
    xb test ${@+"$@"} | grep -E "✘|Test run with|TEST (SUCC|FAIL)|error:"
    ;;
  run)
    xb build | grep -E " error: |\*\* BUILD"
    launch ${@+"$@"}
    echo "group container: $(xcrun simctl get_app_container "$SIM" "$ID" "group.$ID")"
    ;;
  shot)
    # Relaunches the last build with the given arguments, lets it settle, and saves a screenshot.
    # e.g. IPAD=1 scripts/sim.sh shot duo-open -versoSample YES -versoFrame 951x669
    name="${1:?usage: $0 shot <name> [launch args…]}"; shift
    [ -d "$APP" ] || xb build | grep -E " error: |\*\* BUILD"
    out="${VERSO_SHOT_DIR:-../.build/shots}"; mkdir -p "$out"
    launch ${@+"$@"} >/dev/null
    sleep "${SHOT_WAIT:-4}"
    xcrun simctl io "$SIM" screenshot "$out/$name.png"
    ;;
  *) echo "usage: $0 build | test [xcodebuild args…] | run [launch args…] | shot <name> [launch args…]"; exit 2 ;;
esac
