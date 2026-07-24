#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${SCRIPT_DIR}/out"

MACHINE_NAME="sparkbox-poc"
SANDBOX_NAME="${SPARKBOX_SMOKE_SANDBOX:-macpoc}"
OPERATOR_KEY_FILE="${SPARKBOX_OPERATOR_KEY_FILE:-${HOME}/.ssh/id_ed25519.pub}"
OPERATOR_PRIVATE_KEY_FILE="${SPARKBOX_OPERATOR_PRIVATE_KEY_FILE:-${OPERATOR_KEY_FILE%.pub}}"
PROXY_DOMAIN="${SPARKBOX_PROXY_DOMAIN:-sparkbox.test}"
GATEWAY_PORT="${SPARKBOX_GATEWAY_PORT:-2222}"
GUEST_WAIT_SECONDS="${SPARKBOX_GUEST_WAIT_SECONDS:-150}"
DEFAULT_HTTP_PORT=8000
ANY_HTTP_PORT=8123
CROSS_SLOT_ADDRESS="172.30.255.1/32"

route_public=0
http_servers_started=0
cross_slot_added=0
gateway_ip=""
result_dir=""

die() {
  echo "error: $*" >&2
  exit 1
}

pass() {
  echo "PASS: $*"
}

guest_ssh() {
  ssh "${ssh_options[@]}" "${SANDBOX_NAME}@${gateway_ip}" "$@"
}

control_ssh() {
  ssh "${ssh_options[@]}" "ctl@${gateway_ip}" "$@"
}

cleanup() {
  set +e
  if [[ "${cross_slot_added}" -eq 1 ]]; then
    container machine run --name "${MACHINE_NAME}" --root \
      /usr/sbin/ip address delete "${CROSS_SLOT_ADDRESS}" dev lo \
      >/dev/null 2>&1
  fi
  if [[ "${http_servers_started}" -eq 1 && -n "${gateway_ip}" ]]; then
    guest_ssh '
      for pidfile in /tmp/sparkbox-http-8000.pid /tmp/sparkbox-http-8123.pid; do
        if [ -s "${pidfile}" ]; then
          kill "$(cat "${pidfile}")" 2>/dev/null || true
          rm -f "${pidfile}"
        fi
      done
    ' >/dev/null 2>&1
  fi
  if [[ "${route_public}" -eq 1 && -n "${gateway_ip}" ]]; then
    control_ssh share "${SANDBOX_NAME}" private >/dev/null 2>&1
  fi
}
trap cleanup EXIT

for tool in container curl jq ssh; do
  command -v "${tool}" >/dev/null || die "required host tool not found: ${tool}"
done
[[ -f "${OPERATOR_KEY_FILE}" ]] \
  || die "operator public key not found: ${OPERATOR_KEY_FILE}"
[[ -f "${OPERATOR_PRIVATE_KEY_FILE}" ]] \
  || die "operator private key not found: ${OPERATOR_PRIVATE_KEY_FILE}"
[[ "${SANDBOX_NAME}" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] \
  || die "invalid SPARKBOX_SMOKE_SANDBOX: ${SANDBOX_NAME}"
[[ "${GUEST_WAIT_SECONDS}" =~ ^[0-9]+$ ]] \
  || die "SPARKBOX_GUEST_WAIT_SECONDS must be an integer"

"${SCRIPT_DIR}/poc.sh" status >/dev/null \
  || die "${MACHINE_NAME} is not ready; run ./macos/poc.sh start"

inspect_json="$(container machine inspect "${MACHINE_NAME}")"
gateway_ip="$(jq -r '.[0] | select(.status == "running") | .ipAddress // empty' <<<"${inspect_json}")"
[[ -n "${gateway_ip}" ]] || die "${MACHINE_NAME} has no running vmnet address"

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
result_dir="${OUT_DIR}/results/${timestamp}"
mkdir -p "${result_dir}"
printf '%s\n' "${inspect_json}" > "${result_dir}/machine-inspect.json"
{
  sw_vers
  uname -a
  container --version
  ssh -V
} > "${result_dir}/host.txt" 2>&1

known_hosts="${result_dir}/known_hosts"
touch "${known_hosts}"
ssh_options=(
  -i "${OPERATOR_PRIVATE_KEY_FILE}"
  -o BatchMode=yes
  -o ConnectTimeout=20
  -o LogLevel=ERROR
  -o StrictHostKeyChecking=accept-new
  -o "UserKnownHostsFile=${known_hosts}"
  -p "${GATEWAY_PORT}"
)

control_list="$(control_ssh list | tr -d '\r')"
if awk -v name="${SANDBOX_NAME}" '$1 == name { found=1 } END { exit !found }' \
  <<<"${control_list}"; then
  echo "reusing existing sandbox ${SANDBOX_NAME}" \
    | tee "${result_dir}/l2-create.txt"
else
  echo "creating sandbox ${SANDBOX_NAME}"
  # The gateway's first attach has a short fixed readiness window. Nested KVM
  # cold boots can outlast it, so creation success is decided by the bounded
  # reconnect loop below, not the first session's exit code.
  printf '%s\n' exit \
    | ssh -tt "${ssh_options[@]}" "new+${SANDBOX_NAME}@${gateway_ip}" \
    > "${result_dir}/l2-create.txt" 2>&1 \
    || true
fi

deadline=$((SECONDS + GUEST_WAIT_SECONDS))
guest_arch=""
while [[ "${SECONDS}" -lt "${deadline}" ]]; do
  if guest_arch="$(guest_ssh uname -m 2>/dev/null)" \
    && [[ "${guest_arch//$'\r'/}" == "aarch64" ]]; then
    break
  fi
  sleep 5
done
[[ "${guest_arch//$'\r'/}" == "aarch64" ]] \
  || die "${SANDBOX_NAME} did not become SSH-ready within ${GUEST_WAIT_SECONDS}s"
pass "L2 guest is SSH-ready and reports aarch64"

guest_ssh '
  set -eu
  uname -a
  printf "hostname=%s\n" "$(hostname)"
  printf "boot_id=%s\n" "$(cat /proc/sys/kernel/random/boot_id)"
  ip addr show eth0
  ip route
' | tee "${result_dir}/l2-boot.txt"

sentinel="sparkbox-macos-persist-${timestamp}"
guest_ssh "printf '%s\n' '${sentinel}' > \"\$HOME/.sparkbox-macos-persist\""
reconnected="$(guest_ssh 'cat "$HOME/.sparkbox-macos-persist"' | tr -d '\r')"
[[ "${reconnected}" == "${sentinel}" ]] || die "guest reconnect sentinel did not persist"
printf 'sentinel=%s\n' "${reconnected}" | tee "${result_dir}/l2-reconnect.txt"
pass "guest rootfs data survives reconnect"

guest_ssh '
  set -eu
  getent ahostsv4 github.com | head -1
  curl -fsS --max-time 20 -o /dev/null https://github.com/vanpelt/sparky/releases
  gateway="$(ip route | awk "/default/{print \$3; exit}")"
  curl -fsS --max-time 10 "http://${gateway}:8967/identity"
  test -s /var/run/secrets/hivemind/token
  awk -F. "NF == 3 { ok=1 } END { exit !ok }" /var/run/secrets/hivemind/token
  echo "token-file=valid-jwt"
' | tee "${result_dir}/gateway-network.txt"
pass "guest DNS, outbound HTTPS, metadata identity, and token file"

container machine run --name "${MACHINE_NAME}" --root \
  /usr/sbin/ip address delete "${CROSS_SLOT_ADDRESS}" dev lo \
  >/dev/null 2>&1 || true
container machine run --name "${MACHINE_NAME}" --root \
  /usr/sbin/ip address add "${CROSS_SLOT_ADDRESS}" dev lo
cross_slot_added=1
cross_slot_status="$(guest_ssh \
  'curl -sS --max-time 10 -o /tmp/sparkbox-cross-slot -w "%{http_code}" http://172.30.255.1:8967/identity')"
[[ "${cross_slot_status//$'\r'/}" == "403" ]] \
  || die "cross-slot metadata request returned ${cross_slot_status}, want 403"
container machine run --name "${MACHINE_NAME}" --root \
  /usr/sbin/ip address delete "${CROSS_SLOT_ADDRESS}" dev lo
cross_slot_added=0
printf 'cross_slot_http_status=%s\n' "${cross_slot_status//$'\r'/}" \
  >> "${result_dir}/gateway-network.txt"
pass "cross-slot metadata request is refused"

control_ssh share "${SANDBOX_NAME}" public \
  | tee "${result_dir}/route-visibility.txt"
route_public=1
guest_ssh "
  set -eu
  printf '%s\n' sparkbox-http-route-ok > \"\$HOME/sparkbox-http.txt\"
  nohup python3 -m http.server ${DEFAULT_HTTP_PORT} --bind 0.0.0.0 --directory \"\$HOME\" \
    >/tmp/sparkbox-http-${DEFAULT_HTTP_PORT}.log 2>&1 </dev/null &
  echo \$! > /tmp/sparkbox-http-${DEFAULT_HTTP_PORT}.pid
  nohup python3 -m http.server ${ANY_HTTP_PORT} --bind 0.0.0.0 --directory \"\$HOME\" \
    >/tmp/sparkbox-http-${ANY_HTTP_PORT}.log 2>&1 </dev/null &
  echo \$! > /tmp/sparkbox-http-${ANY_HTTP_PORT}.pid
"
http_servers_started=1

proxy_deadline=$((SECONDS + 90))
default_response=""
any_response=""
while [[ "${SECONDS}" -lt "${proxy_deadline}" ]]; do
  default_response="$(curl --fail --silent --show-error --max-time 10 \
    -H "Host: ${SANDBOX_NAME}.${PROXY_DOMAIN}" \
    "http://${gateway_ip}:8081/sparkbox-http.txt" 2>/dev/null || true)"
  any_response="$(curl --fail --silent --show-error --max-time 10 \
    -H "Host: ${SANDBOX_NAME}.${PROXY_DOMAIN}" \
    "http://${gateway_ip}:${ANY_HTTP_PORT}/sparkbox-http.txt" 2>/dev/null || true)"
  if [[ "${default_response}" == "sparkbox-http-route-ok" \
    && "${any_response}" == "sparkbox-http-route-ok" ]]; then
    break
  fi
  sleep 5
done
[[ "${default_response}" == "sparkbox-http-route-ok" ]] \
  || die "gateway :8081 route did not reach guest :${DEFAULT_HTTP_PORT}"
[[ "${any_response}" == "sparkbox-http-route-ok" ]] \
  || die "gateway :${ANY_HTTP_PORT} REDIRECT did not reach the same guest port"
{
  printf 'default_route=%s\n' "${default_response}"
  printf 'any_port_route=%s\n' "${any_response}"
} | tee "${result_dir}/http-routing.txt"
pass "default HTTP route and arbitrary-port REDIRECT"

{
  printf 'result=PASS\n'
  printf 'machine=%s\n' "${MACHINE_NAME}"
  printf 'gateway_ip=%s\n' "${gateway_ip}"
  printf 'sandbox=%s\n' "${SANDBOX_NAME}"
  printf 'proxy_domain=%s\n' "${PROXY_DOMAIN}"
  printf 'guest_arch=aarch64\n'
  printf 'metadata_cross_slot_status=403\n'
  printf 'default_http_port=%s\n' "${DEFAULT_HTTP_PORT}"
  printf 'arbitrary_http_port=%s\n' "${ANY_HTTP_PORT}"
} > "${result_dir}/summary.txt"

echo
echo "macOS nested Sparkbox smoke test passed"
echo "  sandbox: ${SANDBOX_NAME}"
echo "  evidence: ${result_dir}"
