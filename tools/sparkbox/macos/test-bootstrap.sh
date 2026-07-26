#!/bin/bash
# test-bootstrap.sh — exercise macos/sparkbox-bootstrap.sh without a machine.
#
# The bootstrap script is the thing standing between a nested gateway and an
# unverified binary running as root, and none of the macOS PoC can be run in CI
# (it needs Apple Silicon, the `container` CLI and nested virtualization). So its
# decisions — which URL, which checksum, what to do when they disagree — are
# tested here instead, against a fake release served by a stub `curl` on PATH.
#
# What this does NOT prove: TLS behaviour (that is curl's job, and the real
# script passes --proto '=https' --tlsv1.2), and that `container machine run`
# carries poc.sh's chunked RAW-BYTE `dd` append faithfully — 64 KiB pieces
# concatenated inside the machine, deliberately not base64 (see the comment on
# push_binary_to_machine). Only a real machine settles the latter.
#
# usage: ./macos/test-bootstrap.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BOOTSTRAP="${SCRIPT_DIR}/sparkbox-bootstrap.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/sparkbox-bootstrap-test.XXXXXX")"
trap 'rm -rf -- "${WORK}"' EXIT

FAILURES=0
CASES=0

ok() {
  CASES=$((CASES + 1))
  printf '  [PASS] %s\n' "$1"
}

bad() {
  CASES=$((CASES + 1))
  FAILURES=$((FAILURES + 1))
  printf '  [FAIL] %s\n' "$1"
}

case "$(uname -m)" in
  arm64 | aarch64) ARCH=arm64 ;;
  *) ARCH=amd64 ;;
esac

# --- the fake release ------------------------------------------------------
# A "sparkbox binary" that answers `version` the way the real one does:
#   sparkbox <version> (<goos>/<goarch>)
make_fake_sparkbox() {
  local dest="$1" version="$2"
  cat > "${dest}" <<EOF
#!/bin/bash
[[ "\${1:-}" == "version" ]] || exit 2
echo "sparkbox ${version} (linux/${ARCH})"
EOF
  chmod 0755 "${dest}"
}

# make_release lays out one release under ${WORK}/serve/<tag>/ the way GitHub's
# /releases/download/<tag>/<asset> namespace does, so the stub curl can map a URL
# straight onto a file and a wrong URL is a miss rather than a silent success.
make_release() {
  local tag="$1" version="$2" declared_sha="${3:-}"
  local dir="${WORK}/serve/${tag}"
  mkdir -p "${dir}"
  make_fake_sparkbox "${dir}/sparkbox-linux-${ARCH}" "${version}"
  local real_sha
  real_sha="$(shasum -a 256 "${dir}/sparkbox-linux-${ARCH}" | awk '{print $1}')"
  [[ -n "${declared_sha}" ]] || declared_sha="${real_sha}"
  cat > "${dir}/manifest-${ARCH}.env" <<EOF
RELEASE=${tag}
ARCH=${ARCH}
PLATFORM=linux
FIRECRACKER_VERSION=v1.16.1
SPARKBOX_ASSET=sparkbox-linux-${ARCH}
SHA256_VMLINUX=$(printf 'a%.0s' $(seq 64))
SHA256_FIRECRACKER=$(printf 'b%.0s' $(seq 64))
SHA256_SPARKBOX=${declared_sha}
ROOTFS_NAME=universal
ROOTFS_ASSET=universal-${ARCH}.ext4.zst
SHA256_ROOTFS=$(printf 'c%.0s' $(seq 64))
ROOTFS_LOGIN_USER=sparky
GATEWAY_PUBKEY="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI fake gateway key"
EOF
}

# --- the stub toolchain ----------------------------------------------------
# A PATH shim, not a patch to the script: the script under test is byte-for-byte
# the one baked into the image.
mkdir -p "${WORK}/bin"
cat > "${WORK}/bin/curl" <<'EOF'
#!/bin/bash
# Stub curl: understands only the flags sparkbox-bootstrap passes, resolves
# https://base/{latest,}/download/... against ${FAKE_SERVE}, and records every
# URL it was asked for in ${FAKE_URLLOG}.
url=""
out=""
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --output) out="$2"; shift 2 ;;
    --fail|--silent|--show-error|--location) shift ;;
    --proto|--tlsv1.2|--retry|--max-time) [[ "$1" == "--tlsv1.2" ]] && shift || shift 2 ;;
    http*|https*) url="$1"; shift ;;
    *) shift ;;
  esac
done
printf '%s\n' "${url}" >> "${FAKE_URLLOG}"
# https://example.invalid/releases/download/<tag>/<asset>
# https://example.invalid/releases/latest/download/<asset>
path="${url#*://}"
path="${path#*/}"
case "${path}" in
  */latest/download/*) src="${FAKE_SERVE}/${FAKE_LATEST}/${path##*/}" ;;
  */download/*)
    rest="${path#*/download/}"
    src="${FAKE_SERVE}/${rest}"
    ;;
  *) exit 22 ;;
esac
[[ -f "${src}" ]] || exit 22
cp "${src}" "${out}"
EOF
cat > "${WORK}/bin/sha256sum" <<'EOF'
#!/bin/bash
# macOS ships shasum, not sha256sum; the guest is Linux and has the latter.
shasum -a 256 "$@"
EOF
chmod 0755 "${WORK}/bin/curl" "${WORK}/bin/sha256sum"

# run_bootstrap invokes the real script with the stub toolchain first on PATH and
# a scratch install root, and prints its combined output.
run_bootstrap() {
  local root="$1"
  shift
  PATH="${WORK}/bin:${PATH}" \
  FAKE_SERVE="${WORK}/serve" \
  FAKE_LATEST="${FAKE_LATEST:-}" \
  FAKE_URLLOG="${WORK}/urls.log" \
  SPARKBOX_BOOTSTRAP_DIR="${root}/var" \
  SPARKBOX_BOOTSTRAP_MARKER="${root}/marker" \
    bash "${BOOTSTRAP}" "$@" 2>&1
}

result_value() {
  sed -n "s/^$2=//p" "$1/var/release.env" | tail -1
}

echo "sparkbox-bootstrap tests (arch ${ARCH})"

# 1. The happy path: a good release installs, verifies and records itself.
make_release v0.4.0 v0.4.0
root="${WORK}/case-release"; mkdir -p "${root}"
: > "${root}/marker"
out="$(run_bootstrap "${root}" release v0.4.0 https://example.invalid/releases)"
if [[ "$?" -eq 0 ]] && [[ -x "${root}/var/sparkbox" ]]; then
  ok "release install"
else
  bad "release install: ${out}"
fi
[[ "$(result_value "${root}" SOURCE)" == "release" ]] \
  && [[ "$(result_value "${root}" RELEASE)" == "v0.4.0" ]] \
  && [[ "$(result_value "${root}" VERSION)" == "v0.4.0" ]] \
  && ok "release provenance recorded" \
  || bad "release provenance recorded: $(cat "${root}/var/release.env" 2>&1)"
# A previous source build must not survive a release install, or the machine
# keeps claiming to be a dev box (or, worse, stops claiming it when it is one).
[[ ! -e "${root}/marker" ]] \
  && ok "source-build marker cleared by a release install" \
  || bad "source-build marker cleared by a release install"
grep -q '/download/v0.4.0/sparkbox-linux-' "${WORK}/urls.log" \
  && ok "asset URL is /download/<tag>/sparkbox-linux-<arch>" \
  || bad "asset URL: $(cat "${WORK}/urls.log")"

# 2. `latest` resolves through the manifest's RELEASE, and the BINARY is then
#    fetched from that concrete tag — never from /latest/ a second time.
: > "${WORK}/urls.log"
FAKE_LATEST=v0.4.0
root="${WORK}/case-latest"; mkdir -p "${root}"
out="$(run_bootstrap "${root}" release latest https://example.invalid/releases)"
if [[ "$(result_value "${root}" RELEASE)" == "v0.4.0" ]] \
  && grep -q '/latest/download/manifest-' "${WORK}/urls.log" \
  && grep -q '/download/v0.4.0/sparkbox-linux-' "${WORK}/urls.log" \
  && ! grep -q '/latest/download/sparkbox-linux-' "${WORK}/urls.log"; then
  ok "latest resolves to a concrete tag before fetching the binary"
else
  bad "latest resolution: ${out}
$(cat "${WORK}/urls.log")"
fi
FAKE_LATEST=""

# 3. A manifest whose SHA256_SPARKBOX does not describe the asset must refuse.
#    This is the case that matters: it is the difference between verifying a
#    download and pretending to.
make_release v9.9.9 v9.9.9 "$(printf 'd%.0s' $(seq 64))"
root="${WORK}/case-badsha"; mkdir -p "${root}"
out="$(run_bootstrap "${root}" release v9.9.9 https://example.invalid/releases)"
status="$?"
if [[ "${status}" -ne 0 ]] && grep -q 'sha256 mismatch' <<<"${out}" \
  && [[ ! -e "${root}/var/sparkbox" ]]; then
  ok "checksum mismatch refuses and installs nothing"
else
  bad "checksum mismatch (status ${status}): ${out}"
fi

# 4. A release whose manifest has no SHA256_SPARKBOX at all must refuse rather
#    than fall back to trusting the download.
mkdir -p "${WORK}/serve/v0.0.9"
make_fake_sparkbox "${WORK}/serve/v0.0.9/sparkbox-linux-${ARCH}" v0.0.9
printf 'RELEASE=v0.0.9\nARCH=%s\n' "${ARCH}" > "${WORK}/serve/v0.0.9/manifest-${ARCH}.env"
root="${WORK}/case-nosha"; mkdir -p "${root}"
out="$(run_bootstrap "${root}" release v0.0.9 https://example.invalid/releases)"
status="$?"
if [[ "${status}" -ne 0 ]] && grep -q 'SHA256_SPARKBOX' <<<"${out}"; then
  ok "a manifest without SHA256_SPARKBOX refuses"
else
  bad "missing SHA256_SPARKBOX (status ${status}): ${out}"
fi

# 5. THE B3 CASE. A release whose binary reports a different version than the tag
#    it was published under is exactly the skew that let a v0.3.0 build provision
#    v0.4.0 artifacts. It must be a refusal, not a note.
make_release v1.2.3 v0.3.0-5-g18bfe3b
root="${WORK}/case-skew"; mkdir -p "${root}"
out="$(run_bootstrap "${root}" release v1.2.3 https://example.invalid/releases)"
status="$?"
if [[ "${status}" -ne 0 ]] && grep -q "reports version 'v0.3.0-5-g18bfe3b'" <<<"${out}"; then
  ok "release/binary version skew refuses"
else
  bad "version skew (status ${status}): ${out}"
fi

# 6. A cached binary is re-verified, not trusted for existing. A truncated or
#    swapped file in the staging directory must be replaced by a fresh download.
root="${WORK}/case-cache"; mkdir -p "${root}/var"
printf 'not a sparkbox\n' > "${root}/var/sparkbox"
: > "${WORK}/urls.log"
out="$(run_bootstrap "${root}" release v0.4.0 https://example.invalid/releases)"
if [[ "$?" -eq 0 ]] && grep -q 'sparkbox-linux-' "${WORK}/urls.log" \
  && "${root}/var/sparkbox" version >/dev/null 2>&1; then
  ok "a corrupt cached binary is re-downloaded"
else
  bad "cache re-verification: ${out}"
fi
: > "${WORK}/urls.log"
out="$(run_bootstrap "${root}" release v0.4.0 https://example.invalid/releases)"
if [[ "$?" -eq 0 ]] && ! grep -q 'sparkbox-linux-' "${WORK}/urls.log"; then
  ok "a verified cached binary is reused without a download"
else
  bad "cache reuse: ${out}
$(cat "${WORK}/urls.log")"
fi

# 7. Local mode: the escape hatch installs, but only against the host's sha256,
#    and it must leave the machine visibly marked as a source build.
root="${WORK}/case-local"; mkdir -p "${root}"
make_fake_sparkbox "${WORK}/local-sparkbox" source-v0.4.0-3-gdeadbee
sha="$(shasum -a 256 "${WORK}/local-sparkbox" | awk '{print $1}')"
out="$(run_bootstrap "${root}" local "${sha}" source-label - < "${WORK}/local-sparkbox")"
if [[ "$?" -eq 0 ]] && [[ -f "${root}/marker" ]] \
  && [[ "$(result_value "${root}" SOURCE)" == "local" ]] \
  && grep -q 'SOURCE BUILD' <<<"${out}"; then
  ok "local mode installs, announces and marks the machine"
else
  bad "local mode: ${out}"
fi

# 8. Local mode with the wrong checksum — a mangled or truncated push — must
#    refuse. This is what makes the chunked transport safe: poc.sh appends the
#    binary in 64 KiB pieces because a single stdin stream deadlocks above
#    128 KiB, and a lost piece has to be a refusal rather than a subtly wrong
#    binary running as root.
root="${WORK}/case-local-bad"; mkdir -p "${root}"
out="$(run_bootstrap "${root}" local "$(printf 'e%.0s' $(seq 64))" source-label - \
  < "${WORK}/local-sparkbox")"
status="$?"
if [[ "${status}" -ne 0 ]] && grep -q 'sha256 mismatch' <<<"${out}" \
  && [[ ! -e "${root}/var/sparkbox" ]]; then
  ok "local mode refuses a stream that does not match its checksum"
else
  bad "local mode checksum (status ${status}): ${out}"
fi

# 8b. The path form is what poc.sh actually uses (the chunks are assembled in the
#     machine first). It must consume the staged file, not leave a second copy of
#     a private binary lying around.
root="${WORK}/case-local-path"; mkdir -p "${root}/var"
cp "${WORK}/local-sparkbox" "${root}/var/incoming"
out="$(run_bootstrap "${root}" local "${sha}" source-label "${root}/var/incoming")"
if [[ "$?" -eq 0 ]] && [[ -x "${root}/var/sparkbox" ]] \
  && [[ ! -e "${root}/var/incoming" ]]; then
  ok "local mode installs from a staged path and consumes it"
else
  bad "local mode staged path: ${out}"
fi

# 8c. A staged path that is not there at all must fail loudly rather than install
#     whatever happens to be in the staging directory already.
root="${WORK}/case-local-missing"; mkdir -p "${root}"
out="$(run_bootstrap "${root}" local "${sha}" source-label "${root}/var/nope")"
status="$?"
if [[ "${status}" -ne 0 ]] && grep -q 'no staged binary' <<<"${out}"; then
  ok "local mode fails when the staged binary is missing"
else
  bad "local mode missing path (status ${status}): ${out}"
fi

# 9. SPARKBOX_ASSET is data: a release that renames the binary asset must be
#    followed, not second-guessed with a constructed name (the same reason
#    ROOTFS_ASSET is a manifest key).
mkdir -p "${WORK}/serve/v2.0.0"
make_fake_sparkbox "${WORK}/serve/v2.0.0/sparkbox-linux-renamed-${ARCH}" v2.0.0
renamed_sha="$(shasum -a 256 "${WORK}/serve/v2.0.0/sparkbox-linux-renamed-${ARCH}" | awk '{print $1}')"
cat > "${WORK}/serve/v2.0.0/manifest-${ARCH}.env" <<EOF
RELEASE=v2.0.0
ARCH=${ARCH}
PLATFORM=linux
SPARKBOX_ASSET=sparkbox-linux-renamed-${ARCH}
SHA256_SPARKBOX=${renamed_sha}
EOF
root="${WORK}/case-renamed"; mkdir -p "${root}"
: > "${WORK}/urls.log"
out="$(run_bootstrap "${root}" release v2.0.0 https://example.invalid/releases)"
if [[ "$?" -eq 0 ]] && grep -q 'sparkbox-linux-renamed-' "${WORK}/urls.log"; then
  ok "SPARKBOX_ASSET in the manifest names the download"
else
  bad "SPARKBOX_ASSET honoured: ${out}
$(cat "${WORK}/urls.log")"
fi

# 10. A manifest for another platform must be refused rather than installed.
#     manifest-<arch>.env means linux by convention; if a release ever changes
#     that, this machine must stop rather than run a darwin binary as init's
#     neighbour.
mkdir -p "${WORK}/serve/v3.0.0"
make_fake_sparkbox "${WORK}/serve/v3.0.0/sparkbox-linux-${ARCH}" v3.0.0
cat > "${WORK}/serve/v3.0.0/manifest-${ARCH}.env" <<EOF
RELEASE=v3.0.0
ARCH=${ARCH}
PLATFORM=darwin
SHA256_SPARKBOX=$(shasum -a 256 "${WORK}/serve/v3.0.0/sparkbox-linux-${ARCH}" | awk '{print $1}')
EOF
root="${WORK}/case-platform"; mkdir -p "${root}"
out="$(run_bootstrap "${root}" release v3.0.0 https://example.invalid/releases)"
status="$?"
if [[ "${status}" -ne 0 ]] && grep -q 'PLATFORM=darwin' <<<"${out}"; then
  ok "a non-linux manifest refuses"
else
  bad "platform check (status ${status}): ${out}"
fi

# 11. An unknown subcommand is a usage error, not a no-op success.
root="${WORK}/case-usage"; mkdir -p "${root}"
out="$(run_bootstrap "${root}" bogus)"
[[ "$?" -ne 0 ]] && ok "unknown mode fails" || bad "unknown mode fails: ${out}"

echo
if [[ "${FAILURES}" -gt 0 ]]; then
  echo "${FAILURES}/${CASES} bootstrap checks failed"
  exit 1
fi
echo "all ${CASES} bootstrap checks passed"
