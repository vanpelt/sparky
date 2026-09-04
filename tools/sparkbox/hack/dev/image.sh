#!/usr/bin/env bash
# Build the CKS node image from the working tree and get it into the Apple
# container machine, where `sparkbox devpod` can run it.
#
# THE DIRECTION IS NOT NEGOTIABLE, AND IT IS THE COUNTERINTUITIVE ONE:
#
#   Mac / OrbStack  --build--> 127.0.0.1:5001  <--pull--  Apple machine
#                              (hack/dev/registry.sh, running on the Mac;
#                               the machine reaches it as 192.168.64.1:5001)
#
# The obvious arrangement — run the registry inside the machine and
# `docker push 192.168.64.2:5000/...` from the Mac — TIMES OUT. Measured this
# session, and the reason is worth writing down because the symptom is so
# misleading: `docker` on a Mac is a thin client for a daemon that lives in
# OrbStack's OWN Linux VM, and that VM has no route to Apple's 192.168.64.0/24
# vmnet. macOS `curl http://192.168.64.2:5000/v2/` succeeds — from the Mac's
# network stack — which makes it look like a registry or TLS problem when it is
# a routing one. Pushing to 127.0.0.1 keeps the traffic inside OrbStack's own
# port forward, and the machine pulling outward reaches the Mac's vmnet gateway
# address fine.
#
# Two more things that follow from the direction:
#   - 127.0.0.1:5001 needs no `insecure-registries` on the Mac (Docker trusts
#     127.0.0.0/8 for plain-HTTP registries by default), but 192.168.64.1:5001
#     DOES inside the machine. `hack/dev/machine.sh ensure` writes it.
#   - the image is tagged 127.0.0.1:5001/... for the push and pulled back as
#     192.168.64.1:5001/... — same blobs, two names, because a registry
#     reference is an address and the two sides do not share one.
#
# Measured on an M4 Max: build 57s cold, push 3s, pull 1.9s.
#
# The build context is tools/ (the Containerfile COPYs both sparkbox/ and
# sluice/), and it is the LIVE working tree — uncommitted edits are included by
# design, which is the entire point, and also why a build kicked off while
# someone else is mid-edit bakes in a half-written file.
#
# Usage:
#   hack/dev/image.sh all      registry up -> build -> push -> pull  (the loop)
#   hack/dev/image.sh build    build on the Mac only
#   hack/dev/image.sh push     push an already-built image to the local registry
#   hack/dev/image.sh pull     have the machine pull it
#
# Env:
#   SPARKBOX_DEV_IMAGE          repo:tag              (default sparkbox-cks:dev)
#   SPARKBOX_DEV_IMAGE_VERSION  -X main.version       (default dev-mac)
#   SPARKBOX_DEV_PLATFORM       build platform        (default linux/arm64)
#   SPARKBOX_DEV_MACHINE        machine name          (default sparkbox)
#   SPARKBOX_DEV_HOST_IP        the Mac, from inside  (default 192.168.64.1)
#   SPARKBOX_DEV_REGISTRY_PORT  registry port         (default 5001)
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
readonly script_dir
readonly module_dir=$(cd -- "$script_dir/../.." && pwd)
readonly context_dir=$(cd -- "$module_dir/.." && pwd) # tools/ — holds sparkbox/ and sluice/
readonly containerfile="$module_dir/deploy/kubernetes/Containerfile"

readonly image="${SPARKBOX_DEV_IMAGE:-sparkbox-cks:dev}"
readonly version="${SPARKBOX_DEV_IMAGE_VERSION:-dev-mac}"
readonly platform="${SPARKBOX_DEV_PLATFORM:-linux/arm64}"
readonly machine="${SPARKBOX_DEV_MACHINE:-sparkbox}"
readonly host_ip="${SPARKBOX_DEV_HOST_IP:-192.168.64.1}"
readonly reg_port="${SPARKBOX_DEV_REGISTRY_PORT:-5001}"

readonly push_ref="127.0.0.1:$reg_port/$image"  # what the Mac's daemon writes
readonly pull_ref="$host_ip:$reg_port/$image"   # the same blobs, named from inside

die() {
  echo "image.sh: $*" >&2
  exit 1
}

need_docker() {
  command -v docker > /dev/null 2>&1 ||
    die "no \`docker\` on PATH — this needs the Mac's Docker (OrbStack or Docker Desktop)"
  docker info > /dev/null 2>&1 ||
    die "the Mac's Docker daemon is not answering
     fix: start OrbStack (or Docker Desktop) and retry"
}

registry_up() {
  "$script_dir/registry.sh" up
}

stage_start=0
stage() {
  stage_start=$SECONDS
  echo "== $* =="
}
stage_done() {
  echo "   ${1} took $((SECONDS - stage_start))s"
}

cmd_build() {
  need_docker
  [ -f "$containerfile" ] || die "no Containerfile at $containerfile"
  [ -d "$context_dir/sluice" ] ||
    die "$context_dir has no sluice/ — the Containerfile builds both modules from tools/"
  stage "build $push_ref ($platform, version=$version)"
  docker build \
    --platform "$platform" \
    -f "$containerfile" \
    -t "$push_ref" \
    --build-arg "SPARKBOX_VERSION=$version" \
    "$context_dir" ||
    die "docker build failed; nothing was pushed"
  stage_done build
  docker image inspect "$push_ref" \
    --format '   {{.Id}}  {{.Architecture}}/{{.Os}}  {{.Size}} bytes' 2>/dev/null || true
}

cmd_push() {
  need_docker
  docker image inspect "$push_ref" > /dev/null 2>&1 ||
    die "$push_ref is not built on this Mac
     fix: hack/dev/image.sh build"
  registry_up
  stage "push $push_ref"
  docker push "$push_ref" ||
    die "docker push failed
     If this TIMED OUT against an address other than 127.0.0.1, read this file's
     header: OrbStack's daemon cannot route to Apple's vmnet, so the registry has
     to live on the Mac and the machine has to pull."
  stage_done push
}

cmd_pull() {
  command -v container > /dev/null 2>&1 ||
    die "no \`container\` on PATH — Apple's container CLI is not installed"
  # `machine.sh ensure` is what guarantees the insecure-registries entry and a
  # data-root on the reflink volume; without it the pull either refuses plain
  # HTTP or lands the layers on the wrong filesystem.
  "$script_dir/machine.sh" ensure
  stage "pull $pull_ref (inside machine '$machine')"
  # -i is MANDATORY: without it stdin is silently discarded and the command
  # exits 0 having pulled nothing. See hack/dev/machine.sh's header.
  printf 'set -e\ndocker pull %s\ndocker image inspect %s --format "{{.Id}} {{.Architecture}}/{{.Os}} {{.Size}} bytes"\n' \
    "$pull_ref" "$pull_ref" |
    container machine run -i --root --name "$machine" -- bash -s ||
    die "the machine could not pull $pull_ref
     check: hack/dev/registry.sh status   (is anything serving it?)
            hack/dev/machine.sh status    (can the machine reach it?)"
  stage_done pull
}

cmd_all() {
  local started=$SECONDS
  registry_up
  cmd_build
  cmd_push
  cmd_pull
  echo "== done: $pull_ref is in machine '$machine' after $((SECONDS - started))s =="
}

usage() {
  echo "usage: hack/dev/image.sh all|build|push|pull" >&2
}

case "${1:-}" in
  all) cmd_all ;;
  build) cmd_build ;;
  push) cmd_push ;;
  pull) cmd_pull ;;
  *)
    usage
    exit 2
    ;;
esac
