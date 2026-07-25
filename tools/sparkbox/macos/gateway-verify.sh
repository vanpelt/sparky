#!/bin/bash
set -euo pipefail

release_endpoint="${SPARKBOX_RELEASE_ENDPOINT:-https://github.com/vanpelt/sparky/releases}"
expected_kernel="6.14.9-sparkbox-poc"
scratch="$(mktemp -d /var/tmp/sparkbox-poc-verify.XXXXXX)"
loop_device=""
tap_created=0

cleanup() {
  set +e
  if mountpoint -q "${scratch}/mnt"; then
    umount "${scratch}/mnt"
  fi
  if [[ -n "${loop_device}" ]]; then
    losetup -d "${loop_device}"
  fi
  if [[ "${tap_created}" -eq 1 ]]; then
    ip link delete sbtap99
  fi
  rm -rf -- "${scratch}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

pass() {
  echo "PASS: $*"
}

[[ -f /etc/sparkbox-poc ]] || fail "gateway image marker is missing"
pass "gateway image marker"

kernel_release="$(uname -r)"
[[ "${kernel_release}" == "${expected_kernel}" ]] \
  || fail "kernel is ${kernel_release}; expected ${expected_kernel}"
pass "custom kernel ${kernel_release}"

[[ -c /dev/kvm ]] || fail "/dev/kvm is not a character device"
[[ -w /dev/kvm ]] || fail "/dev/kvm is not writable by root"
pass "/dev/kvm present and writable"

[[ -c /dev/net/tun ]] || fail "/dev/net/tun is not a character device"
pass "/dev/net/tun present"

kvm_log="$(dmesg | grep -i kvm || true)"
grep -Eiq 'Hyp.*initialized successfully' <<<"${kvm_log}" \
  || fail "dmesg does not report successful KVM Hyp initialization"
pass "KVM initialized in dmesg"

if findmnt -rn -t virtiofs -o TARGET | grep -Eq '^/Users(/|$)'; then
  fail "the macOS home directory is mounted in the gateway"
fi
pass "macOS home mount absent"

if ip link show dev sbtap99 >/dev/null 2>&1; then
  fail "temporary TAP name sbtap99 is already in use"
fi
ip tuntap add dev sbtap99 mode tap
tap_created=1
ip link show dev sbtap99 >/dev/null
pass "temporary sbtap99 TAP device"

mkdir -p "${scratch}/mnt"
truncate -s 512M "${scratch}/xfs.img"
loop_device="$(losetup --find --show "${scratch}/xfs.img")"
mkfs.xfs -q -m reflink=1 "${loop_device}"
mount "${loop_device}" "${scratch}/mnt"
xfs_info "${scratch}/mnt" | grep -q 'reflink=1' \
  || fail "temporary XFS filesystem does not have reflink enabled"
touch "${scratch}/mnt/write-test"
pass "reflink-enabled XFS loop mount"

curl --fail --silent --show-error --location \
  --max-time 20 --output /dev/null "${release_endpoint}"
pass "outbound HTTPS to ${release_endpoint}"

echo
echo "gateway preflight passed"
