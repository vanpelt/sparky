#!/usr/bin/env bash
# Run hack/smoke-setup.sh's `full` phase inside a disposable systemd container.
#
# This is what CI calls, and it is runnable by hand on any machine with docker
# (including a Mac — Docker Desktop's Linux VM is a fine host for it):
#
#   cd tools/sparkbox
#   go build -o /tmp/sparkbox -ldflags "-X main.version=v0-smoke" ./cmd/sparkbox
#   SPARKBOX_BIN=/tmp/sparkbox ./hack/smoke-container.sh
#
# On a Mac, GOOS/GOARCH must target linux/<container arch>; the script checks the
# binary is a Linux ELF and says so rather than failing inside the container with
# "exec format error".
#
# The container is privileged with the host's cgroup namespace so systemd can be
# PID 1. Nothing outside the container is modified, and the container is removed
# on exit — pass KEEP=1 to leave it running for a post-mortem.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SPARKBOX_BIN=${SPARKBOX_BIN:?set SPARKBOX_BIN to a linux sparkbox binary to test}
RELEASE=${RELEASE:-v0-smoke}
SETTLE=${SETTLE:-12}
IMAGE=${IMAGE:-sparkbox-smoke:local}
NAME=${NAME:-sparkbox-smoke-$$}
KEEP=${KEEP:-0}

[ -x "$SPARKBOX_BIN" ] || { echo "SPARKBOX_BIN=$SPARKBOX_BIN is not executable" >&2; exit 2; }
if ! head -c 4 "$SPARKBOX_BIN" | grep -q ELF; then
  echo "SPARKBOX_BIN=$SPARKBOX_BIN is not a Linux ELF — build with GOOS=linux GOARCH=<container arch>" >&2
  exit 2
fi

# Stage the binary + the smoke script into one directory so the container gets a
# single read-only mount and no view of the repo.
STAGE=$(mktemp -d)
cleanup() {
  if [ "$KEEP" = 1 ]; then
    echo "KEEP=1 — container $NAME left running; \`docker exec -it $NAME bash\`, then \`docker rm -f $NAME\`"
  else
    docker rm -f "$NAME" >/dev/null 2>&1 || true
  fi
  rm -rf "$STAGE"
}
trap cleanup EXIT
install -m 0755 "$SPARKBOX_BIN" "$STAGE/sparkbox"
install -m 0755 "$HERE/smoke-setup.sh" "$STAGE/smoke-setup.sh"

echo "== building $IMAGE =="
docker build -q -f "$HERE/Dockerfile.smoke" -t "$IMAGE" "$HERE" >/dev/null

echo "== booting systemd container $NAME =="
docker run -d --name "$NAME" \
  --privileged --cgroupns=host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  -v "$STAGE":/smoke:ro \
  --tmpfs /run --tmpfs /run/lock \
  "$IMAGE" >/dev/null

# Wait for PID 1 to finish booting. `running` is the happy path; `degraded`
# also means "systemd is up, some unit failed" and is normal for a stripped
# container image — either is a usable host, anything else after the timeout is
# a hard failure, never a skip.
for _ in $(seq 1 60); do
  state=$(docker exec "$NAME" systemctl is-system-running 2>/dev/null || true)
  case "$state" in running | degraded) break ;; esac
  sleep 1
done
case "${state:-}" in
  running | degraded) echo "systemd is up (is-system-running=$state)" ;;
  *)
    echo "FAIL: systemd never came up inside the container (state='${state:-none}')" >&2
    echo "      the provision smoke cannot prove anything without a real PID 1." >&2
    docker logs "$NAME" 2>&1 | tail -40 >&2
    exit 1
    ;;
esac

echo "== running the full smoke phase =="
set +e
docker exec \
  -e SPARKBOX_BIN=/smoke/sparkbox \
  -e RELEASE="$RELEASE" \
  -e SETTLE="$SETTLE" \
  "$NAME" /smoke/smoke-setup.sh full
rc=$?
set -e

if [ $rc -ne 0 ]; then
  echo "== post-mortem: container journal ==" >&2
  docker exec "$NAME" journalctl -n 120 --no-pager 2>&1 | tail -120 >&2 || true
fi
exit $rc
