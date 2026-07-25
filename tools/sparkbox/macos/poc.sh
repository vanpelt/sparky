#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SPARKBOX_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
OUT_DIR="${SCRIPT_DIR}/out"

MACHINE_NAME="sparkbox-poc"
GATEWAY_IMAGE="local/sparkbox-gateway:macos-poc"
GATEWAY_CONTAINERFILE="${SCRIPT_DIR}/Containerfile.gateway"
KERNEL_BUILD_SCRIPT="${SCRIPT_DIR}/kernel/build.sh"
KERNEL_PATH="${OUT_DIR}/vmlinux-kvm"
SMOKE_SCRIPT="${SCRIPT_DIR}/smoke.sh"

MIN_CONTAINER_VERSION="1.1.0"
MIN_MACOS_VERSION="15.0"
SPARKBOX_RELEASE="${SPARKBOX_RELEASE:-v0.4.0}"
MACHINE_CPUS="${SPARKBOX_CPUS:-8}"
MACHINE_MEMORY="${SPARKBOX_MEMORY:-24G}"
BUILD_CPUS="${SPARKBOX_BUILD_CPUS:-8}"
BUILD_MEMORY="${SPARKBOX_GATEWAY_BUILD_MEMORY:-8G}"
DATA_VOLUME_GB="${SPARKBOX_DATA_VOLUME_GB:-40}"
PROXY_DOMAIN="${SPARKBOX_PROXY_DOMAIN:-sparkbox.test}"
OPERATOR_HANDLE="${SPARKBOX_OPERATOR_HANDLE:-operator}"
OPERATOR_KEY_FILE="${SPARKBOX_OPERATOR_KEY_FILE:-${HOME}/.ssh/id_ed25519.pub}"
FLEET_GATEWAY="${SPARKBOX_FLEET_GATEWAY:-}"
NODE_NAME="${SPARKBOX_NODE_NAME:-sparkbox-poc}"
SERVICE_WAIT_SECONDS="${SPARKBOX_SERVICE_WAIT_SECONDS:-90}"

GO_IMAGE="docker.io/library/golang:1.25.0-bookworm@sha256:81dc45d05a7444ead8c92a389621fafabc8e40f8fd1a19d7e5df14e61e98bc1a"
UBUNTU_IMAGE="docker.io/library/ubuntu:24.04@sha256:4fbb8e6a8395de5a7550b33509421a2bafbc0aab6c06ba2cef9ebffbc7092d90"

DOCTOR_FAILURES=0

usage() {
  cat <<'EOF'
usage: ./macos/poc.sh <command>

Commands:
  doctor         Check the macOS host without changing it
  build          Build the pinned KVM kernel and gateway OCI image
  create         Create/reuse sparkbox-poc and run the full L1 preflight
  provision      Run the pinned Sparkbox setup inside sparkbox-poc
  status         Report outer-machine and Sparkbox gateway state
  stop           Stop sparkbox-poc without deleting its state
  start          Start sparkbox-poc and wait for Sparkbox readiness
  smoke          Run the L2 SSH/network/metadata/proxy smoke test
  destroy        Tear down by cost tier; always needs --yes (see below)

Teardown (./macos/poc.sh destroy [targets] --yes):
  --machine      delete sparkbox-poc and its state       (default; seconds to redo)
  --image        delete the local gateway OCI image      (minutes: a container build)
  --kernel       delete the compiled outer kernel in macos/out
                                                         (very expensive: a full
                                                          in-container Linux build)
  --all          all three, plus the rest of macos/out (inputs + results evidence)

  With no target, destroy --yes deletes only the machine. The kernel is pinned
  and byte-reproducible, so rebuilding it produces the identical file — deleting
  it buys nothing and costs an hour.

Environment overrides:
  SPARKBOX_CPUS                  outer machine CPUs (default 8)
  SPARKBOX_MEMORY                outer machine memory (default 24G)
  SPARKBOX_BUILD_CPUS            image/kernel build CPUs (default 8)
  SPARKBOX_KERNEL_BUILD_MEMORY   kernel build memory (default 16G)
  SPARKBOX_GATEWAY_BUILD_MEMORY  gateway image build memory (default 8G)
  SPARKBOX_DATA_VOLUME_GB        required free space (default 40)
  SPARKBOX_RELEASE               pinned setup artifact release (default v0.4.0)
  SPARKBOX_PROXY_DOMAIN          gateway proxy domain (default sparkbox.test)
  SPARKBOX_OPERATOR_HANDLE       operator account handle (default operator)
  SPARKBOX_OPERATOR_KEY_FILE     public key to seed (default ~/.ssh/id_ed25519.pub)
  SPARKBOX_OPERATOR_PRIVATE_KEY_FILE
                                 matching private key for smoke (default public key path without .pub)
  SPARKBOX_FLEET_GATEWAY          gateway host:port; provision as a fleet node when set
  SPARKBOX_NODE_NAME              fleet node name (default sparkbox-poc)
  SPARKBOX_SERVICE_WAIT_SECONDS  start readiness timeout (default 90)
EOF
}

pass() {
  printf '  [PASS] %-22s %s\n' "$1" "$2"
}

warn() {
  printf '  [WARN] %-22s %s\n' "$1" "$2"
}

fail() {
  printf '  [FAIL] %-22s %s\n' "$1" "$2"
  DOCTOR_FAILURES=$((DOCTOR_FAILURES + 1))
}

die() {
  echo "error: $*" >&2
  exit 1
}

version_ge() {
  awk -v have="$1" -v need="$2" 'BEGIN {
    split(have, h, ".");
    split(need, n, ".");
    for (i = 1; i <= 3; i++) {
      hv = h[i] + 0;
      nv = n[i] + 0;
      if (hv > nv) exit 0;
      if (hv < nv) exit 1;
    }
    exit 0;
  }'
}

machine_exists() {
  local machines
  machines="$(container machine list --quiet)" || return 2
  grep -Fqx "${MACHINE_NAME}" <<<"${machines}"
}

machine_inspect() {
  container machine inspect "${MACHINE_NAME}"
}

machine_is_owned() {
  local inspect_json="$1"
  jq -e --arg image "${GATEWAY_IMAGE}" \
    '.[0].id == "sparkbox-poc"
      and .[0].image.reference == $image
      and .[0].homeMount == "none"' \
    >/dev/null <<<"${inspect_json}"
}

require_owned_machine() {
  local inspect_json machine_status

  # The status must be read in an else branch: after `fi`, `$?` is the `if`
  # statement's own status (always 0), which would swallow the "does not
  # exist" case behind the misleading list-failure message.
  if machine_exists; then
    inspect_json="$(machine_inspect)"
    machine_is_owned "${inspect_json}" \
      || die "${MACHINE_NAME} exists but is not owned by this PoC"
    printf '%s\n' "${inspect_json}"
    return
  else
    machine_status="$?"
  fi

  [[ "${machine_status}" -eq 1 ]] || die "could not list container machines"
  die "${MACHINE_NAME} does not exist; run ./macos/poc.sh create"
}

require_container_runtime() {
  command -v container >/dev/null || die "Apple Container CLI is not installed"
  command -v jq >/dev/null || die "jq is required"
  container system status >/dev/null 2>&1 \
    || die "container service is not running; run: container system start"
}

run_doctor() {
  DOCTOR_FAILURES=0
  echo "sparkbox macOS nested gateway doctor"
  echo

  local os_name
  os_name="$(uname -s 2>/dev/null || true)"
  if [[ "${os_name}" == "Darwin" ]]; then
    pass "operating system" "macOS"
  else
    fail "operating system" "${os_name:-unknown}; macOS is required"
  fi

  local arch
  arch="$(uname -m 2>/dev/null || true)"
  if [[ "${arch}" == "arm64" ]]; then
    pass "architecture" "Apple Silicon (arm64)"
  else
    fail "architecture" "${arch:-unknown}; Apple Silicon is required"
  fi

  if command -v sw_vers >/dev/null; then
    local macos_version
    macos_version="$(sw_vers -productVersion)"
    if version_ge "${macos_version}" "${MIN_MACOS_VERSION}"; then
      pass "macOS version" "${macos_version} (>= ${MIN_MACOS_VERSION})"
    else
      fail "macOS version" "${macos_version}; nested virtualization requires >= ${MIN_MACOS_VERSION}"
    fi
  else
    fail "macOS version" "sw_vers not found"
  fi

  if command -v sysctl >/dev/null; then
    local chip generation
    chip="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || true)"
    generation="$(sed -E 's/^Apple M([0-9]+).*/\1/' <<<"${chip}")"
    if [[ "${generation}" =~ ^[0-9]+$ ]] && [[ "${generation}" -ge 3 ]]; then
      pass "nested virtualization" "${chip}"
    else
      fail "nested virtualization" "${chip:-unknown chip}; Apple M3 or newer is required"
    fi
  else
    fail "nested virtualization" "sysctl not found"
  fi

  local tool
  for tool in container curl jq shasum; do
    if command -v "${tool}" >/dev/null; then
      pass "host tool: ${tool}" "$(command -v "${tool}")"
    else
      fail "host tool: ${tool}" "not found"
    fi
  done

  if command -v container >/dev/null; then
    local container_output container_version
    container_output="$(container --version 2>/dev/null || true)"
    container_version="$(sed -E 's/.*version ([0-9]+\.[0-9]+\.[0-9]+).*/\1/' <<<"${container_output}")"
    if [[ "${container_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] \
      && version_ge "${container_version}" "${MIN_CONTAINER_VERSION}"; then
      pass "Apple Container CLI" "${container_version} (>= ${MIN_CONTAINER_VERSION})"
    else
      fail "Apple Container CLI" "${container_output:-unavailable}; need >= ${MIN_CONTAINER_VERSION}"
    fi
  fi

  local service_running=0
  if command -v container >/dev/null; then
    if container system status >/dev/null 2>&1; then
      service_running=1
      pass "container service" "running"
    else
      fail "container service" "not running; run: container system start"
    fi
  fi

  local available_kb required_kb
  if [[ ! "${DATA_VOLUME_GB}" =~ ^[0-9]+$ ]] || [[ "${DATA_VOLUME_GB}" -lt 1 ]]; then
    fail "data volume size" "${DATA_VOLUME_GB}; SPARKBOX_DATA_VOLUME_GB must be a positive integer"
  else
    available_kb="$(df -Pk "${SCRIPT_DIR}" | awk 'NR == 2 {print $4}')"
    required_kb=$((DATA_VOLUME_GB * 1024 * 1024))
    if [[ "${available_kb:-0}" -ge "${required_kb}" ]]; then
      pass "available disk" "$((available_kb / 1024 / 1024)) GiB (need ${DATA_VOLUME_GB} GiB)"
    else
      fail "available disk" "$((available_kb / 1024 / 1024)) GiB; need ${DATA_VOLUME_GB} GiB"
    fi
  fi

  if [[ "${service_running}" -eq 1 ]]; then
    if machine_exists; then
      local inspect_json
      inspect_json="$(machine_inspect)"
      if machine_is_owned "${inspect_json}"; then
        pass "machine name" "${MACHINE_NAME} is an existing Sparkbox PoC with home-mount=none"
      else
        fail "machine name" "${MACHINE_NAME} exists but is not the expected home-mount=none Sparkbox image"
      fi
    else
      local machine_status="$?"
      if [[ "${machine_status}" -eq 1 ]]; then
        pass "machine name" "${MACHINE_NAME} is available"
      else
        fail "machine name" "could not list container machines"
      fi
    fi
  else
    warn "machine name" "ownership check skipped until the container service is running"
  fi

  echo
  if [[ "${DOCTOR_FAILURES}" -gt 0 ]]; then
    echo "${DOCTOR_FAILURES} check(s) failed"
    return 1
  fi
  echo "all host checks passed"
}

write_build_manifest() {
  local commit source_version
  commit="$(git -C "${SPARKBOX_DIR}" rev-parse HEAD)"
  source_version="$(git -C "${SPARKBOX_DIR}" describe --tags --always --dirty)"

  {
    printf 'apple_container_min_version=%s\n' "${MIN_CONTAINER_VERSION}"
    printf 'gateway_image=%s\n' "${GATEWAY_IMAGE}"
    printf 'go_image=%s\n' "${GO_IMAGE}"
    printf 'ubuntu_image=%s\n' "${UBUNTU_IMAGE}"
    printf 'sparkbox_commit=%s\n' "${commit}"
    printf 'sparkbox_source_version=%s\n' "${source_version}"
    printf 'sparkbox_release=%s\n' "${SPARKBOX_RELEASE}"
    printf 'machine_cpus=%s\n' "${MACHINE_CPUS}"
    printf 'machine_memory=%s\n' "${MACHINE_MEMORY}"
    printf 'data_volume_gb=%s\n' "${DATA_VOLUME_GB}"
  } > "${OUT_DIR}/inputs.txt"
}

build_all() {
  run_doctor || die "host doctor failed"
  mkdir -p "${OUT_DIR}"

  "${KERNEL_BUILD_SCRIPT}"

  local source_version
  source_version="$(git -C "${SPARKBOX_DIR}" describe --tags --always --dirty)"
  echo "building ${GATEWAY_IMAGE} for linux/arm64"
  container build \
    --arch arm64 \
    --cpus "${BUILD_CPUS}" \
    --memory "${BUILD_MEMORY}" \
    --file "${GATEWAY_CONTAINERFILE}" \
    --tag "${GATEWAY_IMAGE}" \
    --build-arg "GO_IMAGE=${GO_IMAGE}" \
    --build-arg "UBUNTU_IMAGE=${UBUNTU_IMAGE}" \
    --build-arg "SPARKBOX_VERSION=${source_version}" \
    "${SPARKBOX_DIR}"

  container image inspect "${GATEWAY_IMAGE}" >/dev/null
  write_build_manifest
  echo
  echo "build complete"
  echo "  kernel: ${KERNEL_PATH}"
  echo "  image:  ${GATEWAY_IMAGE}"
  echo "  inputs: ${OUT_DIR}/inputs.txt"
}

write_host_evidence() {
  local destination="$1"
  {
    sw_vers
    uname -a
    sysctl -n machdep.cpu.brand_string
    container --version
    df -h "${SCRIPT_DIR}"
  } > "${destination}"
}

run_gateway_preflight() {
  local destination="$1"
  local attempt

  for attempt in 1 2 3; do
    if container machine run --name "${MACHINE_NAME}" --root \
      /usr/local/sbin/sparkbox-poc-verify \
      2>&1 | tee "${destination}"; then
      return 0
    fi

    if [[ "${attempt}" -lt 3 ]]; then
      echo "gateway preflight attempt ${attempt} failed; retrying machine connection" >&2
      sleep 2
    fi
  done

  return 1
}

write_provision_manifest() {
  local destination="$1"
  local operator_key="$2"

  {
    printf 'machine_name=%s\n' "${MACHINE_NAME}"
    printf 'gateway_image=%s\n' "${GATEWAY_IMAGE}"
    printf 'sparkbox_release=%s\n' "${SPARKBOX_RELEASE}"
    printf 'proxy_domain=%s\n' "${PROXY_DOMAIN}"
    printf 'data_volume_gb=%s\n' "${DATA_VOLUME_GB}"
    printf 'swap_gb=0\n'
    if [[ -n "${FLEET_GATEWAY}" ]]; then
      printf 'role=fleet-node\n'
      printf 'fleet_gateway=%s\n' "${FLEET_GATEWAY}"
      printf 'node_name=%s\n' "${NODE_NAME}"
    else
      printf 'role=standalone-gateway\n'
      printf 'operator_handle=%s\n' "${OPERATOR_HANDLE}"
      printf 'operator_key_file=%s\n' "${OPERATOR_KEY_FILE}"
      printf 'operator_key_sha256=%s\n' \
        "$(printf '%s' "${operator_key}" | shasum -a 256 | awk '{print $1}')"
    fi
  } > "${destination}"
}

run_machine_script() {
  container machine run --name "${MACHINE_NAME}" --root --interactive \
    /bin/bash -s
}

capture_gateway_storage() {
  run_machine_script <<'EOF'
set -euo pipefail
echo "== df -h =="
df -h
echo
echo "== df -T =="
df -T
echo
echo "== lsblk =="
lsblk
EOF
}

capture_gateway_health() {
  if [[ -n "${FLEET_GATEWAY}" ]]; then
    container machine run --name "${MACHINE_NAME}" --root \
      /usr/local/bin/sparkbox doctor --gateway "${FLEET_GATEWAY}"
  else
    # Bash 3.2 (the macOS system shell) treats "${empty_array[@]}" as an
    # unbound variable under set -u. Keep the no-argument path explicit.
    container machine run --name "${MACHINE_NAME}" --root \
      /usr/local/bin/sparkbox doctor
  fi
  echo
  run_machine_script <<'EOF'
set -euo pipefail
echo "== services =="
systemctl is-active sparkbox-net.service sparkbox.service
systemctl is-enabled sparkbox-net.service sparkbox.service
echo
echo "== installed artifacts =="
/usr/local/bin/firecracker --version
uname -r
findmnt -T /srv/sparkbox/data
xfs_info /srv/sparkbox/data
stat -c "%n %s bytes" /srv/sparkbox/vmlinux /srv/sparkbox/data/images/*.ext4
EOF
}

# describe_role renders a GATEWAY_FLAG value as the role an operator thinks in.
# An empty flag is not "no role"; it is the standalone gateway, which is the
# thing the message has to say out loud or the refusal reads as a parse error.
describe_role() {
  if [[ -z "$1" ]]; then
    printf 'a standalone gateway'
  else
    printf 'a fleet node (%s)' "$1"
  fi
}

# machine_sandbox_count prints how many sandboxes this machine still carries.
#
# Conservative by construction, because the only safe error here is
# over-counting: it sums persisted VM directories, persisted manager records
# and live firecracker processes, and it looks for the first two under EVERY
# state directory below /srv/sparkbox rather than the one this script expects.
# A layout it does not recognise therefore reads as occupied, not as empty, and
# a machine it cannot inspect at all makes the caller refuse (a non-numeric or
# empty answer is not zero).
machine_sandbox_count() {
  run_machine_script <<'EOF'
set -uo pipefail
count=0
# One directory per sandbox the firecracker driver has ever materialised.
while IFS= read -r dir; do
  n="$(find "${dir}" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l)"
  count=$((count + n))
done < <(find /srv/sparkbox -maxdepth 4 -type d -name fc-vms 2>/dev/null)
# Manager records outlive a stopped VM's directory, and an archived sandbox has
# a record and no directory at all. grep -c counts matching LINES, which for a
# single-line JSON document is 0 or 1 — enough, since the caller only asks
# whether the total is zero.
while IFS= read -r file; do
  n="$(grep -c '"name"' "${file}" 2>/dev/null || true)"
  count=$((count + ${n:-0}))
done < <(find /srv/sparkbox -maxdepth 4 -type f -name sandboxes.json 2>/dev/null)
# Anything live right now, in case its state lives somewhere unexpected.
for comm in /proc/[0-9]*/comm; do
  [[ -r "${comm}" ]] || continue
  read -r name < "${comm}" || true
  [[ "${name:-}" == firecracker ]] && count=$((count + 1))
done
printf '%s\n' "${count}"
EOF
}

# switch_machine_role changes a provisioned machine between standalone gateway
# and fleet node in place, or refuses and says why.
#
# The change itself is one line in sparkbox.env, so destroying the machine to
# make it is pure waste — and under the old teardown it also took the compiled
# outer kernel with it. What makes the switch unsafe is not the flag but the
# state underneath: a gateway's users DB, routes, invite codes and fleet
# secrets are meaningless on a node, and the sandboxes on it belong to accounts
# that would stop existing. With no sandboxes there is nothing to strand, so the
# switch proceeds; with any, or with no way to prove there are none, it refuses.
switch_machine_role() {
  local from="$1" to="$2" count=""

  count="$(machine_sandbox_count 2>/dev/null | tr -d '[:space:]' || true)"
  [[ "${count}" =~ ^[0-9]+$ ]] \
    || die "cannot switch ${MACHINE_NAME} from $(describe_role "${from}") to $(describe_role "${to}"): could not count the sandboxes on it, so it cannot be shown to be empty. Start the machine and retry, or destroy it (./macos/poc.sh destroy --machine --yes) and provision again"
  if [[ "${count}" -ne 0 ]]; then
    die "refusing to switch ${MACHINE_NAME} from $(describe_role "${from}") to $(describe_role "${to}") while it carries ${count} sandbox(es): a gateway's users DB, routes and fleet secrets are meaningless on a node, and those sandboxes belong to accounts that would stop existing. Destroy the sandboxes first, or the machine (./macos/poc.sh destroy --machine --yes), which keeps the image and the kernel"
  fi

  echo "switching ${MACHINE_NAME} from $(describe_role "${from}") to $(describe_role "${to}") (no sandboxes on it)"
  # setup writes sparkbox.env only when there is none, so the file is moved
  # aside rather than edited: setup then renders the whole thing from the new
  # flags. The operator's own lines (TLS_FLAGS, the console password) go with
  # it, which is why it is a backup and not a delete.
  run_machine_script <<'EOF'
set -euo pipefail
env_file=/srv/sparkbox/sparkbox.env
if [[ -f "${env_file}" ]]; then
  backup="${env_file}.bak-$(date -u +%Y%m%dT%H%M%SZ)"
  mv "${env_file}" "${backup}"
  echo "  moved ${env_file} to ${backup}"
fi
EOF
  echo "  gateway state (fleet keys, certificates, sqlite) is left untouched"
}

provision_machine() {
  run_doctor || die "host doctor failed"
  require_owned_machine >/dev/null
  local operator_key=""
  if [[ -n "${FLEET_GATEWAY}" ]]; then
    [[ "${FLEET_GATEWAY}" =~ ^[^[:space:]]+:[0-9]+$ ]] \
      || die "SPARKBOX_FLEET_GATEWAY must be host:port"
    [[ "${NODE_NAME}" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] \
      || die "invalid SPARKBOX_NODE_NAME: ${NODE_NAME}"
  else
    [[ -f "${OPERATOR_KEY_FILE}" ]] \
      || die "operator public key not found: ${OPERATOR_KEY_FILE}"
    [[ "${OPERATOR_HANDLE}" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] \
      || die "invalid SPARKBOX_OPERATOR_HANDLE: ${OPERATOR_HANDLE}"
    operator_key="$(<"${OPERATOR_KEY_FILE}")"
    [[ "${operator_key}" == ssh-* ]] \
      || die "${OPERATOR_KEY_FILE} does not contain a supported SSH public key"
  fi

  # setup deliberately preserves an existing sparkbox.env so operator TLS and
  # console edits survive idempotent reruns. That also means setup alone cannot
  # change GATEWAY_FLAG: it would report success while leaving the old role
  # live. So the role change is handled here, before setup runs.
  local existing_env existing_gateway_flag requested_gateway_flag=""
  existing_env="$(
    container machine run --name "${MACHINE_NAME}" --root \
      /bin/cat /srv/sparkbox/sparkbox.env 2>/dev/null || true
  )"
  if [[ -n "${FLEET_GATEWAY}" ]]; then
    requested_gateway_flag="--gateway ${FLEET_GATEWAY} --node-name ${NODE_NAME}"
  fi
  if [[ -n "${existing_env}" ]]; then
    existing_gateway_flag="$(
      sed -n 's/^GATEWAY_FLAG=//p' <<<"${existing_env}" | tail -1
    )"
    if [[ "${existing_gateway_flag}" != "${requested_gateway_flag}" ]]; then
      switch_machine_role "${existing_gateway_flag}" "${requested_gateway_flag}"
    fi
  fi

  local timestamp result_dir
  timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
  result_dir="${OUT_DIR}/results/${timestamp}"
  mkdir -p "${result_dir}"
  write_host_evidence "${result_dir}/host.txt"
  machine_inspect > "${result_dir}/machine-inspect-before.json"
  capture_gateway_storage > "${result_dir}/gateway-storage-before.txt"
  write_provision_manifest "${result_dir}/provision-manifest.txt" "${operator_key}"
  cp "${OUT_DIR}/inputs.txt" "${result_dir}/build-inputs.txt"
  cp "${OUT_DIR}/kernel-manifest.txt" "${result_dir}/kernel-manifest.txt"

  local remote_key_path=""
  local setup_args=(
    /usr/local/bin/sparkbox setup
    --release "${SPARKBOX_RELEASE}"
    --proxy-domain "${PROXY_DOMAIN}"
    --data-volume-gb "${DATA_VOLUME_GB}"
    --swap-gb 0
  )
  if [[ -n "${FLEET_GATEWAY}" ]]; then
    setup_args+=(--gateway "${FLEET_GATEWAY}" --node-name "${NODE_NAME}")
  else
    # `container machine run` does not preserve spaces inside a single
    # argument, so transport the public key over stdin and give setup an
    # in-VM path.
    remote_key_path="/run/sparkbox-poc-operator.pub"
    printf '%s\n' "${operator_key}" \
      | container machine run --name "${MACHINE_NAME}" --root --interactive \
        /usr/bin/tee "${remote_key_path}" \
      >/dev/null
    container machine run --name "${MACHINE_NAME}" --root \
      /bin/chmod 0600 "${remote_key_path}"
    setup_args+=(
      --operator-handle "${OPERATOR_HANDLE}"
      --operator-key "${remote_key_path}"
    )
  fi

  echo "provisioning ${MACHINE_NAME} with Sparkbox ${SPARKBOX_RELEASE}"
  local setup_status=0
  container machine run --name "${MACHINE_NAME}" --root \
    "${setup_args[@]}" \
    2>&1 | tee "${result_dir}/sparkbox-setup.txt" \
    || setup_status="$?"
  if [[ -n "${remote_key_path}" ]]; then
    container machine run --name "${MACHINE_NAME}" --root \
      /bin/rm -f "${remote_key_path}"
  fi
  [[ "${setup_status}" -eq 0 ]] || return "${setup_status}"

  echo
  echo "verifying provisioned gateway"
  capture_gateway_health 2>&1 | tee "${result_dir}/sparkbox-doctor.txt"
  capture_gateway_storage > "${result_dir}/gateway-storage-after.txt"
  machine_inspect > "${result_dir}/machine-inspect-after.json"
  container machine run --name "${MACHINE_NAME}" --root \
    journalctl --no-pager -u sparkbox-net.service -u sparkbox.service \
    > "${result_dir}/sparkbox-journal.txt"

  echo
  if [[ -n "${FLEET_GATEWAY}" ]]; then
    echo "Sparkbox fleet node is provisioned"
    echo "  node:     ${NODE_NAME}"
    echo "  gateway:  ${FLEET_GATEWAY}"
  else
    echo "Sparkbox gateway is provisioned"
  fi
  echo "  machine:  ${MACHINE_NAME}"
  echo "  evidence: ${result_dir}"
}

status_machine() {
  require_container_runtime

  local inspect_json machine_state
  inspect_json="$(require_owned_machine)"
  machine_state="$(jq -r '.[0].status' <<<"${inspect_json}")"

  jq -r '.[0] |
    "machine:  \(.id)\n" +
    "state:    \(.status)\n" +
    "address:  \(.ipAddress // "-")\n" +
    "cpus:     \(.cpus)\n" +
    "memory:   \(.memory / 1073741824 | floor) GiB\n" +
    "disk:     \(.diskSize) bytes\n" +
    "image:    \(.image.reference)\n" +
    "home:     \(.homeMount)"' <<<"${inspect_json}"

  # Exit non-zero when the machine is not up. There is nothing further to
  # report, and callers (smoke.sh's readiness guard) rely on the status to tell
  # "ready" from "exists but stopped" — returning 0 here made that guard dead.
  if [[ "${machine_state}" != "running" ]]; then
    echo "${MACHINE_NAME} is ${machine_state}; run ./macos/poc.sh start" >&2
    return 1
  fi

  echo
  run_machine_script <<'EOF'
set -euo pipefail
printf "kernel:   %s\n" "$(uname -r)"
if [[ -c /dev/kvm && -w /dev/kvm ]]; then
  echo "kvm:      ready"
else
  echo "kvm:      unavailable"
fi
printf "data:     "
if mountpoint -q /srv/sparkbox/data; then
  findmnt -n -o FSTYPE,SOURCE,TARGET -T /srv/sparkbox/data
else
  echo "not mounted"
fi
role="standalone-gateway"
if grep -q '^GATEWAY_FLAG=--gateway' /srv/sparkbox/sparkbox.env 2>/dev/null; then
  role="fleet-node"
fi
printf "role:     %s\n" "${role}"
printf "services: "
service_states="$(systemctl is-active sparkbox-net.service sparkbox.service 2>/dev/null || true)"
printf '%s\n' "${service_states}" | paste -sd " " -
persisted=0
if [[ -d /srv/sparkbox/data/state/fc-vms ]]; then
  persisted="$(find /srv/sparkbox/data/state/fc-vms -mindepth 1 -maxdepth 1 -type d | wc -l)"
fi
running=0
for comm in /proc/[0-9]*/comm; do
  [[ -r "${comm}" ]] || continue
  read -r name < "${comm}" || true
  [[ "${name:-}" == firecracker ]] && running=$((running + 1))
done
printf "sandboxes: %s persisted, %s running\n" "${persisted}" "${running}"
EOF
}

stop_machine() {
  require_container_runtime
  local inspect_json machine_state
  inspect_json="$(require_owned_machine)"
  machine_state="$(jq -r '.[0].status' <<<"${inspect_json}")"
  if [[ "${machine_state}" == "stopped" ]]; then
    echo "${MACHINE_NAME} is already stopped"
    return
  fi

  echo "stopping ${MACHINE_NAME}"
  container machine stop "${MACHINE_NAME}"
  echo "${MACHINE_NAME} stopped"
}

start_machine() {
  require_container_runtime
  [[ "${SERVICE_WAIT_SECONDS}" =~ ^[0-9]+$ ]] \
    || die "SPARKBOX_SERVICE_WAIT_SECONDS must be an integer"

  local inspect_json machine_state
  inspect_json="$(require_owned_machine)"
  machine_state="$(jq -r '.[0].status' <<<"${inspect_json}")"
  if [[ "${machine_state}" == "running" ]]; then
    echo "${MACHINE_NAME} is already running"
  else
    echo "starting ${MACHINE_NAME}"
    container machine run --name "${MACHINE_NAME}" --root /bin/true
  fi

  local waited=0
  while [[ "${waited}" -lt "${SERVICE_WAIT_SECONDS}" ]]; do
    if container machine run --name "${MACHINE_NAME}" --root \
      systemctl is-active --quiet sparkbox.service; then
      echo "sparkbox.service is ready"
      status_machine
      return
    fi
    sleep 2
    waited=$((waited + 2))
  done

  container machine run --name "${MACHINE_NAME}" --root \
    journalctl --no-pager -n 100 -u sparkbox.service || true
  die "sparkbox.service was not ready after ${SERVICE_WAIT_SECONDS}s"
}

create_machine() {
  run_doctor || die "host doctor failed"
  [[ -s "${KERNEL_PATH}" ]] || die "missing ${KERNEL_PATH}; run ./macos/poc.sh build"
  [[ -s "${OUT_DIR}/inputs.txt" ]] || die "missing build inputs; run ./macos/poc.sh build"
  container image inspect "${GATEWAY_IMAGE}" >/dev/null \
    || die "missing ${GATEWAY_IMAGE}; run ./macos/poc.sh build"

  if machine_exists; then
    local existing_inspect
    existing_inspect="$(machine_inspect)"
    machine_is_owned "${existing_inspect}" \
      || die "${MACHINE_NAME} exists but is not owned by this PoC"
    echo "reusing existing ${MACHINE_NAME}"
  else
    local machine_status="$?"
    [[ "${machine_status}" -eq 1 ]] || die "could not list container machines"
    echo "creating ${MACHINE_NAME}"
    container machine create \
      --virtualization \
      --kernel "${KERNEL_PATH}" \
      --cpus "${MACHINE_CPUS}" \
      --memory "${MACHINE_MEMORY}" \
      --home-mount none \
      --name "${MACHINE_NAME}" \
      "${GATEWAY_IMAGE}"
  fi

  local timestamp result_dir
  timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
  result_dir="${OUT_DIR}/results/${timestamp}"
  mkdir -p "${result_dir}"
  write_host_evidence "${result_dir}/host.txt"
  machine_inspect | tee "${result_dir}/machine-inspect.json"

  echo
  echo "verifying nested gateway"
  run_gateway_preflight "${result_dir}/gateway-preflight.txt"

  echo
  echo "persistent nested gateway is ready"
  echo "  machine:  ${MACHINE_NAME}"
  echo "  evidence: ${result_dir}"
}

# The kernel build products under macos/out, listed apart from the rest of the
# directory (inputs.txt, results/) because they are the expensive tier.
KERNEL_ARTIFACTS=(vmlinux-kvm kernel.config kernel-manifest.txt downloads)

# destroy_all tears down by cost tier. The tiers are separate because their
# costs differ by orders of magnitude: the machine is seconds to recreate, the
# gateway image is a multi-minute container build, and the outer kernel is an
# 8-CPU in-container Linux compile.
#
# The kernel is also the one artifact where deleting it buys nothing at all:
# kernel/build.sh pins the source tarball and Apple's config by SHA-256 and
# already reuses a verified vmlinux-kvm, so a rebuild reproduces the same bytes
# an hour later. `--yes` used to mean all three tiers, which is how a role
# change — one line in sparkbox.env — ended up costing a kernel build. It now
# means the cheap tier unless a target says otherwise.
destroy_all() {
  local confirmed=0 targeted=0
  local want_machine=0 want_image=0 want_kernel=0 want_rest=0
  local arg
  for arg in "$@"; do
    case "${arg}" in
      --yes) confirmed=1 ;;
      --machine) want_machine=1; targeted=1 ;;
      --image) want_image=1; targeted=1 ;;
      --kernel) want_kernel=1; targeted=1 ;;
      --all)
        want_machine=1
        want_image=1
        want_kernel=1
        want_rest=1
        targeted=1
        ;;
      *) die "unknown destroy option ${arg}; usage: ./macos/poc.sh destroy [--machine] [--image] [--kernel] [--all] --yes" ;;
    esac
  done
  [[ "${targeted}" -eq 1 ]] || want_machine=1
  [[ "${confirmed}" -eq 1 ]] \
    || die "destroy is destructive; rerun with --yes. Targets: --machine (the default) --image --kernel --all"

  if [[ "${want_machine}" -eq 1 ]] || [[ "${want_image}" -eq 1 ]]; then
    require_container_runtime
  fi

  if [[ "${want_machine}" -eq 1 ]]; then
    if machine_exists; then
      local inspect_json
      inspect_json="$(machine_inspect)"
      machine_is_owned "${inspect_json}" \
        || die "refusing to delete unexpected machine ${MACHINE_NAME}"
      container machine delete "${MACHINE_NAME}"
      echo "deleted machine ${MACHINE_NAME}"
    else
      local machine_status="$?"
      [[ "${machine_status}" -eq 1 ]] || die "could not list container machines"
      echo "machine ${MACHINE_NAME} does not exist"
    fi
  fi

  if [[ "${want_image}" -eq 1 ]]; then
    if container image inspect "${GATEWAY_IMAGE}" >/dev/null 2>&1; then
      container image delete --force "${GATEWAY_IMAGE}"
      echo "deleted local image ${GATEWAY_IMAGE}"
    else
      echo "local image ${GATEWAY_IMAGE} does not exist"
    fi
  fi

  if [[ "${want_kernel}" -eq 1 ]] && [[ "${OUT_DIR}" == "${SCRIPT_DIR}/out" ]]; then
    local artifact removed=0
    for artifact in "${KERNEL_ARTIFACTS[@]}"; do
      if [[ -e "${OUT_DIR}/${artifact}" ]]; then
        rm -rf -- "${OUT_DIR:?}/${artifact}"
        removed=1
      fi
    done
    if [[ "${removed}" -eq 1 ]]; then
      echo "deleted the compiled outer kernel in ${OUT_DIR} (rebuild: ./macos/poc.sh build)"
    else
      echo "no compiled outer kernel in ${OUT_DIR}"
    fi
  fi

  # Everything else under macos/out: inputs.txt and the results/ evidence
  # bundles that create/provision wrote. Only --all reaches these, because they
  # are the record of what was run and nothing regenerates them.
  if [[ "${want_rest}" -eq 1 ]] && [[ -d "${OUT_DIR}" ]] && [[ "${OUT_DIR}" == "${SCRIPT_DIR}/out" ]]; then
    rm -rf -- "${OUT_DIR}"
    echo "deleted generated output ${OUT_DIR} (including the results/ evidence bundles)"
  fi

  if [[ "${want_kernel}" -eq 0 ]] && [[ -s "${KERNEL_PATH}" ]]; then
    echo "kept the outer kernel ${KERNEL_PATH} (delete it with --kernel)"
  fi
}

command_name="${1:-}"
case "${command_name}" in
  doctor)
    [[ "$#" -eq 1 ]] || die "doctor takes no arguments"
    run_doctor
    ;;
  build)
    [[ "$#" -eq 1 ]] || die "build takes no arguments"
    build_all
    ;;
  create)
    [[ "$#" -eq 1 ]] || die "create takes no arguments"
    create_machine
    ;;
  provision)
    [[ "$#" -eq 1 ]] || die "provision takes no arguments"
    provision_machine
    ;;
  status)
    [[ "$#" -eq 1 ]] || die "status takes no arguments"
    status_machine
    ;;
  stop)
    [[ "$#" -eq 1 ]] || die "stop takes no arguments"
    stop_machine
    ;;
  start)
    [[ "$#" -eq 1 ]] || die "start takes no arguments"
    start_machine
    ;;
  smoke)
    [[ "$#" -eq 1 ]] || die "smoke takes no arguments"
    if container machine run --name "${MACHINE_NAME}" --root \
      /usr/bin/grep -q '^GATEWAY_FLAG=--gateway' /srv/sparkbox/sparkbox.env; then
      die "smoke exercises a standalone gateway; this machine is configured as a fleet node"
    fi
    "${SMOKE_SCRIPT}"
    ;;
  destroy)
    shift
    destroy_all "$@"
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
