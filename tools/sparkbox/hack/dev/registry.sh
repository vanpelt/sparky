#!/usr/bin/env bash
# Own the local container registry the dev loop pushes CKS images through.
#
# WHY THIS EXISTS. The registry used to be a `docker run` somebody typed once.
# It was created with no restart policy, and it died on its own (exit 2, about
# seven minutes in) — taking the build/push/pull loop with it and leaving a
# `docker push` that timed out with no obvious cause. Unmanaged state that the
# whole loop depends on is state that will be wrong at the worst moment, so the
# invocation lives here, with `--restart unless-stopped`, and everything else
# in hack/dev calls `registry.sh up` before it needs the registry.
#
# WHY A REGISTRY AT ALL, AND WHY ON THE MAC. The Apple container machine has its
# own Docker daemon; the Mac has OrbStack's. There is no shared image store and
# no `container machine cp`, so an image built on the Mac reaches the machine
# only over the network. The direction is fixed, and it is not the obvious one:
#
#   Mac (OrbStack) --push--> 127.0.0.1:5001 (this registry) <--pull-- machine
#                                    ^ same container, reached from the machine
#                                      as 192.168.64.1:5001
#
# The reverse — run the registry in the machine and push to 192.168.64.2:5000 —
# TIMES OUT, measured. OrbStack's Docker daemon runs inside its own Linux VM,
# which has no route to Apple's 192.168.64.0/24 vmnet; macOS `curl` reaching
# that address proves nothing about the daemon that does the pushing. See
# hack/dev/image.sh, which encodes the working direction.
#
# 127.0.0.1:5001 needs no `insecure-registries` entry on the Mac side — Docker
# trusts 127.0.0.0/8 as a registry over plain HTTP by default. 192.168.64.1:5001
# does need one inside the machine; hack/dev/machine.sh writes it.
#
# Usage:
#   hack/dev/registry.sh up            create/start it; idempotent, safe to spam
#   hack/dev/registry.sh status        state, restart policy, contents, size
#   hack/dev/registry.sh gc [--dry-run]  reclaim blobs no tag references any more
#   hack/dev/registry.sh down [--purge]  remove the container (--purge: + volume)
#
# Env:
#   SPARKBOX_DEV_REGISTRY_NAME    container name   (default sparkbox-reg-mac)
#   SPARKBOX_DEV_REGISTRY_VOLUME  docker volume    (default sparkbox-reg-data)
#   SPARKBOX_DEV_REGISTRY_PORT    host port        (default 5001)
#   SPARKBOX_DEV_REGISTRY_IMAGE   registry image   (default registry:2)
set -euo pipefail

readonly reg_name="${SPARKBOX_DEV_REGISTRY_NAME:-sparkbox-reg-mac}"
readonly reg_volume="${SPARKBOX_DEV_REGISTRY_VOLUME:-sparkbox-reg-data}"
readonly reg_port="${SPARKBOX_DEV_REGISTRY_PORT:-5001}"
readonly reg_image="${SPARKBOX_DEV_REGISTRY_IMAGE:-registry:2}"

# Inside registry:2 (verified with `docker inspect`: entrypoint /entrypoint.sh,
# cmd /etc/docker/registry/config.yml). The 3.x images moved this to
# /etc/distribution/config.yml, so it is read from the container's own Cmd when
# that is available rather than hard-coded blindly.
readonly reg_config_fallback=/etc/docker/registry/config.yml

# The Mac-side URL. The machine uses 192.168.64.1:<same port>; see the header.
readonly reg_url="http://127.0.0.1:$reg_port"

die() {
  echo "registry.sh: $*" >&2
  exit 1
}

need_docker() {
  command -v docker > /dev/null 2>&1 ||
    die "no \`docker\` on PATH — this needs the Mac's Docker (OrbStack or Docker Desktop)"
  docker info > /dev/null 2>&1 ||
    die "the Mac's Docker daemon is not answering
     fix: start OrbStack (or Docker Desktop) and retry"
}

# One of: missing, running, exited, created, paused, restarting, dead.
#
# The result is assigned, not piped through `|| echo missing`: on a container it
# does not know, `docker inspect` writes a bare newline to stdout AND exits
# non-zero, so the naive form yields "\nmissing" and every `= missing` test
# quietly stops matching. Measured — it printed a two-line status header before
# this was fixed.
reg_state() {
  local state
  state=$(docker inspect -f '{{.State.Status}}' "$reg_name" 2>/dev/null) || state=missing
  printf '%s' "${state:-missing}"
}

reg_field() {
  local value
  value=$(docker inspect -f "$1" "$reg_name" 2>/dev/null) || value=""
  printf '%s' "$value"
}

reg_config_path() {
  local cmd
  cmd=$(reg_field '{{index .Config.Cmd 0}}')
  case "$cmd" in
    /*) printf '%s' "$cmd" ;;
    *) printf '%s' "$reg_config_fallback" ;;
  esac
}

# The registry answers /v2/ with 200 once it is serving. Anything else (000 for
# a refused connection included) means not ready.
reg_probe() {
  local code
  code=$(curl -s -o /dev/null -m 3 -w '%{http_code}' "$reg_url/v2/" 2>/dev/null) || true
  printf '%s' "${code:-000}"
}

wait_ready() {
  local deadline=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(reg_probe)" = 200 ] && return 0
    sleep 0.2
  done
  return 1
}

# KiB of the volume's contents, measured inside the container so the number is
# the registry's own view of its storage rather than the host's opinion of a
# volume that may live inside a VM.
reg_size_kb() {
  if [ "$(reg_state)" = running ]; then
    docker exec "$reg_name" du -sk /var/lib/registry 2>/dev/null | awk '{print $1}'
    return
  fi
  # --entrypoint is required: registry:2's entrypoint takes a config path as its
  # argument, so a bare `du` would be read as one and the container would exit.
  docker run --rm --entrypoint du -v "$reg_volume:/v" "$reg_image" -sk /v 2>/dev/null |
    awk '{print $1}'
}

human_kb() {
  awk -v kb="${1:-0}" 'BEGIN {
    if (kb >= 1048576) printf "%.1f GiB", kb / 1048576
    else if (kb >= 1024) printf "%.1f MiB", kb / 1024
    else printf "%d KiB", kb
  }'
}

create_registry() {
  # The exact invocation the loop was verified against. --restart unless-stopped
  # is the whole reason this function exists rather than a note in a README.
  docker run -d \
    --name "$reg_name" \
    --restart unless-stopped \
    -p "$reg_port:5000" \
    -v "$reg_volume:/var/lib/registry" \
    -e REGISTRY_STORAGE_DELETE_ENABLED=true \
    "$reg_image" > /dev/null
}

cmd_up() {
  need_docker
  local state
  state=$(reg_state)
  case "$state" in
    running)
      echo "already running  $reg_name  ($reg_url, HTTP $(reg_probe) on /v2/)"
      check_shape
      return 0
      ;;
    missing)
      echo "creating $reg_name from $reg_image"
      create_registry
      ;;
    *)
      echo "starting $reg_name (was: $state)"
      docker start "$reg_name" > /dev/null
      ;;
  esac
  wait_ready ||
    die "$reg_name did not answer $reg_url/v2/ within 30s
     look:  docker logs $reg_name
     reset: hack/dev/registry.sh down && hack/dev/registry.sh up"
  echo "registry up      $reg_name  ($reg_url, HTTP 200 on /v2/)"
  check_shape
}

# An existing container created by hand, or by an older version of this script,
# can be running and still be the wrong thing. Say so instead of letting a push
# land somewhere surprising — the fix is cheap because the data is in a volume
# that `down` never touches.
check_shape() {
  local policy port vol
  policy=$(reg_field '{{.HostConfig.RestartPolicy.Name}}')
  port=$(docker port "$reg_name" 5000/tcp 2>/dev/null | head -n 1)
  vol=$(reg_field '{{range .Mounts}}{{if eq .Destination "/var/lib/registry"}}{{.Name}}{{end}}{{end}}')
  [ "$policy" = "unless-stopped" ] ||
    echo "  note: restart policy is '${policy:-none}', not 'unless-stopped' — it will not
        come back after a Docker restart. Fix: hack/dev/registry.sh down && up" >&2
  case "$port" in
    *":$reg_port") : ;;
    *) echo "  note: port 5000/tcp is published as '${port:-nothing}', not :$reg_port" >&2 ;;
  esac
  [ "$vol" = "$reg_volume" ] ||
    echo "  note: /var/lib/registry is backed by '${vol:-a bind or anonymous volume}', not
        the named volume $reg_volume" >&2
}

# Repositories and their tags, straight off the v2 API. python3 is used only to
# read the JSON; if it is absent the raw bodies are printed rather than guessed
# at with sed.
list_contents() {
  local catalog repo tags
  catalog=$(curl -s -m 5 "$reg_url/v2/_catalog" 2>/dev/null) || catalog=""
  if [ -z "$catalog" ]; then
    echo "  contents  (catalog did not answer)"
    return
  fi
  if ! command -v python3 > /dev/null 2>&1; then
    echo "  contents  $catalog   (no python3 to expand tags)"
    return
  fi
  local repos
  repos=$(printf '%s' "$catalog" |
    python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin).get("repositories") or []))')
  if [ -z "$repos" ]; then
    echo "  contents  (empty — nothing has been pushed)"
    return
  fi
  echo "  contents"
  while IFS= read -r repo; do
    [ -n "$repo" ] || continue
    tags=$(curl -s -m 5 "$reg_url/v2/$repo/tags/list" 2>/dev/null |
      python3 -c 'import json,sys
try: d = json.load(sys.stdin)
except Exception: d = {}
print(" ".join(d.get("tags") or ["<untagged>"]))' 2>/dev/null) || tags="?"
    printf '    %-24s %s\n' "$repo" "$tags"
  done <<< "$repos"
}

cmd_status() {
  need_docker
  local state
  state=$(reg_state)
  if [ "$state" != running ]; then
    echo "registry  $state  ($reg_name)"
    echo "  fix:    hack/dev/registry.sh up"
    [ "$state" = missing ] ||
      echo "  look:   docker logs $reg_name"
    # The volume outlives the container, so report what is banked even when
    # nothing is serving it.
    echo "  volume  $reg_volume  $(human_kb "$(reg_size_kb)")"
    return 1
  fi
  cat << EOF
registry  running  ($reg_name, $reg_image)
  since     $(reg_field '{{.State.StartedAt}}')
  restart   $(reg_field '{{.HostConfig.RestartPolicy.Name}}')
  mac url   $reg_url            HTTP $(reg_probe) on /v2/
  machine   http://192.168.64.1:$reg_port   (needs insecure-registries; machine.sh ensure)
  volume    $reg_volume  $(human_kb "$(reg_size_kb)")
EOF
  list_contents
  check_shape
}

# REGISTRY_STORAGE_DELETE_ENABLED=true only PERMITS deletes. It reclaims nothing
# by itself: pushing :dev a second time leaves the first manifest in place,
# untagged but still referencing every layer, so the volume only grows. The
# garbage collector is a separate, offline-ish pass — this subcommand — and
# `--delete-untagged` is what actually unlinks those orphaned manifests. Without
# it a repeatedly-rebuilt :dev tag reclaims almost nothing.
#
# The upstream advice is to run gc with the registry in read-only mode, because
# a blob being uploaded concurrently can be collected out from under the push.
# This registry has exactly one writer — the developer running image.sh — so the
# window is "do not gc while a push is in flight" rather than a reason for a
# maintenance mode. Do not copy this shortcut into anything shared.
cmd_gc() {
  need_docker
  [ "$(reg_state)" = running ] ||
    die "$reg_name is not running
     fix: hack/dev/registry.sh up"
  local config before after freed dry=""
  case "${1:-}" in
    --dry-run) dry=--dry-run ;;
    "") : ;;
    *) die "unknown flag ${1}; usage: gc [--dry-run]" ;;
  esac
  config=$(reg_config_path)
  before=$(reg_size_kb)
  echo "garbage-collecting $reg_name ($config, --delete-untagged $dry)"
  docker exec "$reg_name" /bin/registry garbage-collect --delete-untagged $dry "$config" ||
    die "garbage-collect failed; nothing was reclaimed and the registry is untouched"
  if [ -n "$dry" ]; then
    echo "dry run: nothing was deleted; volume still $(human_kb "$before")"
    return 0
  fi
  after=$(reg_size_kb)
  freed=$((before - after))
  printf 'before %s   after %s   reclaimed %s\n' \
    "$(human_kb "$before")" "$(human_kb "$after")" "$(human_kb "$freed")"
  # The collector unlinks blobs but leaves the now-empty directory tree, and the
  # running process keeps its in-memory blob descriptor cache. A restart costs
  # ~1s and makes the next pull consistent with what is actually on disk.
  docker restart "$reg_name" > /dev/null
  wait_ready || die "$reg_name did not come back after gc; check: docker logs $reg_name"
  echo "restarted; $reg_url/v2/ answers $(reg_probe)"
}

cmd_down() {
  need_docker
  local purge=0
  case "${1:-}" in
    --purge) purge=1 ;;
    "") : ;;
    *) die "unknown flag ${1}; usage: down [--purge]" ;;
  esac
  if [ "$(reg_state)" = missing ]; then
    echo "not present ($reg_name)"
  else
    docker rm -f "$reg_name" > /dev/null
    echo "removed container $reg_name"
  fi
  # The volume is deliberately NOT removed by default. It holds every pushed
  # image; losing it means a full rebuild and re-push (57s + 3s, measured) for
  # every tag. Removing a container is a 1s mistake to undo, removing the volume
  # is not.
  if [ "$purge" = 1 ]; then
    docker volume rm "$reg_volume" > /dev/null 2>&1 &&
      echo "purged volume $reg_volume" ||
      echo "volume $reg_volume was already gone"
  else
    echo "kept volume $reg_volume ($(human_kb "$(reg_size_kb)")) — pass --purge to delete it too"
  fi
}

usage() {
  echo "usage: hack/dev/registry.sh up|status|gc [--dry-run]|down [--purge]" >&2
}

case "${1:-}" in
  up) cmd_up ;;
  status) cmd_status ;;
  gc)
    shift
    cmd_gc "$@"
    ;;
  down)
    shift
    cmd_down "$@"
    ;;
  *)
    usage
    exit 2
    ;;
esac
