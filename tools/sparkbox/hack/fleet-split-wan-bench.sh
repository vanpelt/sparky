#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != Linux ]]; then
  echo "fleet-split-wan-bench: Linux is required (tc netem + network namespaces)" >&2
  exit 1
fi
for cmd in awk ip sed sha256sum sudo tc; do
  command -v "$cmd" >/dev/null || { echo "fleet-split-wan-bench: missing $cmd" >&2; exit 1; }
done
if [[ -z "${BENCH_HOST_NAME:-}" ]]; then
  echo "fleet-split-wan-bench: set BENCH_HOST_NAME to the named Linux host producing this result" >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
module_dir="$(cd "$script_dir/.." && pwd)"
output_dir="${OUTPUT_DIR:-$module_dir/docs/benchmarks/fleet-split-$(date -u +%Y%m%dT%H%M%SZ)}"
scratch="$(mktemp -d)"
netns="sparkbox-split-wan-$$"
control_ip="100.100.0.2"
guest_ip="10.250.0.2"
control_addr="${control_ip}:24443"
guest_prefix="10.250.0.0/20"

cleanup() {
  sudo ip netns del "$netns" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT INT TERM

mkdir -p "$output_dir"
if [[ -n "${BENCH_BINARY:-}" ]]; then
  if [[ ! -x "$BENCH_BINARY" ]]; then
    echo "fleet-split-wan-bench: BENCH_BINARY is not executable: $BENCH_BINARY" >&2
    exit 1
  fi
  cp "$BENCH_BINARY" "$scratch/grpccontrol.test"
  git_sha="${BENCH_GIT_SHA:-unknown-prebuilt-source}"
  go_version="${BENCH_GO_VERSION:-unknown-prebuilt-toolchain}"
  compiler_host="${BENCH_COMPILER_HOST:-unknown-prebuilt-host}"
  binary_source="$BENCH_BINARY"
else
  for cmd in go git; do
    command -v "$cmd" >/dev/null || { echo "fleet-split-wan-bench: missing $cmd (or set BENCH_BINARY)" >&2; exit 1; }
  done
  (
    cd "$module_dir"
    go test -c -o "$scratch/grpccontrol.test" ./internal/grpccontrol
  )
  git_sha="$(git -C "$module_dir" rev-parse HEAD)"
  go_version="$(go env GOVERSION)"
  compiler_host="$(uname -n) ($(go env GOHOSTOS)/$(go env GOHOSTARCH))"
  binary_source="built by runner"
fi
{
  echo "git_sha=$git_sha"
  echo "go_version=$go_version"
  echo "compiler_host=$compiler_host"
  echo "kernel=$(uname -a)"
  echo "benchmark_host=$BENCH_HOST_NAME"
  echo "command=$0"
  echo "binary_source=$binary_source"
  echo "binary_sha256=$(sha256sum "$scratch/grpccontrol.test" | awk '{print $1}')"
  echo "control_path=grpc_mtls"
  echo "data_path=routed_tcp"
} >"$output_dir/environment.txt"

sudo ip netns add "$netns"
sudo ip netns exec "$netns" ip link set lo up
sudo ip netns exec "$netns" ip address add "${control_ip}/32" dev lo
sudo ip netns exec "$netns" ip address add "${guest_ip}/32" dev lo

if [[ "${QUICK:-0}" == 1 ]]; then
  rtts=(20)
  losses=(0)
  bandwidths=(100)
  streams=(10)
else
  rtts=(0 20 50 100)
  losses=(0 0.5 2)
  bandwidths=(10 100 1000)
  streams=(0 10 100 511)
fi

results="$output_dir/results.ndjson"
: >"$results"

shape() {
  local rtt="$1" loss="$2" bandwidth="$3" half_delay
  half_delay="$(awk "BEGIN { print $rtt / 2 }")"
  sudo ip netns exec "$netns" tc qdisc replace dev lo root handle 1: prio bands 3
  sudo ip netns exec "$netns" tc qdisc replace dev lo parent 1:3 handle 30: \
    netem delay "${half_delay}ms" loss "${loss}%" rate "${bandwidth}mbit"
  sudo ip netns exec "$netns" tc filter del dev lo parent 1: >/dev/null 2>&1 || true
  sudo ip netns exec "$netns" tc filter replace dev lo protocol ip parent 1: prio 3 u32 \
    match ip src "$control_ip/32" flowid 1:3
  sudo ip netns exec "$netns" tc filter replace dev lo protocol ip parent 1: prio 4 u32 \
    match ip dst "$control_ip/32" flowid 1:3
  sudo ip netns exec "$netns" tc filter replace dev lo protocol ip parent 1: prio 5 u32 \
    match ip src "$guest_ip/32" flowid 1:3
  sudo ip netns exec "$netns" tc filter replace dev lo protocol ip parent 1: prio 6 u32 \
    match ip dst "$guest_ip/32" flowid 1:3
}

run_cell() {
  local rtt="$1" loss="$2" bandwidth="$3" concurrency="$4" stalled="${5:-0}" output
  shape "$rtt" "$loss" "$bandwidth"
  output="$(sudo ip netns exec "$netns" env \
    SPARKBOX_SPLIT_WAN_BENCH=1 \
    SPARKBOX_WAN_BENCH_HOST="$BENCH_HOST_NAME" \
    SPARKBOX_SPLIT_WAN_CONTROL_ADDR="$control_addr" \
    SPARKBOX_SPLIT_WAN_GUEST_IP="$guest_ip" \
    SPARKBOX_SPLIT_WAN_GUEST_PREFIX="$guest_prefix" \
    SPARKBOX_WAN_RTT_MS="$rtt" \
    SPARKBOX_WAN_LOSS_PERCENT="$loss" \
    SPARKBOX_WAN_BANDWIDTH_MBPS="$bandwidth" \
    SPARKBOX_WAN_CONCURRENCY="$concurrency" \
    SPARKBOX_WAN_SAMPLES="${SAMPLES:-50}" \
    SPARKBOX_WAN_STALLED_READER="$stalled" \
    "$scratch/grpccontrol.test" -test.run '^TestSplitWANBenchmarkCell$' -test.v)"
  echo "$output" >&2
  echo "$output" | sed -n 's/^.*SPLIT_WANBENCH_RESULT //p' >>"$results"
}

for rtt in "${rtts[@]}"; do
  for loss in "${losses[@]}"; do
    for bandwidth in "${bandwidths[@]}"; do
      for concurrency in "${streams[@]}"; do
        run_cell "$rtt" "$loss" "$bandwidth" "$concurrency"
      done
    done
  done
done

run_cell 50 0.5 100 10 1

echo "fleet-split-wan-bench: wrote $output_dir" >&2
