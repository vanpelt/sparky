#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != Linux ]]; then
  echo "fleet-wan-bench: Linux is required (tc netem + network namespaces)" >&2
  exit 1
fi
for cmd in go git ip tc sudo sed; do
  command -v "$cmd" >/dev/null || { echo "fleet-wan-bench: missing $cmd" >&2; exit 1; }
done
if [[ -z "${BENCH_HOST_NAME:-}" ]]; then
  echo "fleet-wan-bench: set BENCH_HOST_NAME to the named Linux host producing this baseline" >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
module_dir="$(cd "$script_dir/.." && pwd)"
output_dir="${OUTPUT_DIR:-$module_dir/docs/benchmarks/fleet-ssh-baseline-$(date -u +%Y%m%dT%H%M%SZ)}"
scratch="$(mktemp -d)"
netns="sparkbox-wan-$$"

cleanup() {
  sudo ip netns del "$netns" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT INT TERM

mkdir -p "$output_dir"
(
  cd "$module_dir"
  go test -c -o "$scratch/nodelink.test" ./internal/nodelink
  {
    echo "git_sha=$(git rev-parse HEAD)"
    echo "go_version=$(go env GOVERSION)"
    echo "kernel=$(uname -a)"
    echo "benchmark_host=$BENCH_HOST_NAME"
    echo "command=$0"
  } >"$output_dir/environment.txt"
)

sudo ip netns add "$netns"
sudo ip netns exec "$netns" ip link set lo up

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
  sudo ip netns exec "$netns" tc filter replace dev lo protocol ip parent 1: prio 3 u32 \
    match ip sport 22222 0xffff flowid 1:3
  sudo ip netns exec "$netns" tc filter replace dev lo protocol ip parent 1: prio 4 u32 \
    match ip dport 22222 0xffff flowid 1:3
}

run_cell() {
  local rtt="$1" loss="$2" bandwidth="$3" concurrency="$4" stalled="${5:-0}" output
  shape "$rtt" "$loss" "$bandwidth"
  output="$(sudo ip netns exec "$netns" env \
    SPARKBOX_WAN_BENCH=1 \
    SPARKBOX_WAN_BENCH_HOST="$BENCH_HOST_NAME" \
    SPARKBOX_WAN_GATEWAY_ADDR=127.0.0.1:22222 \
    SPARKBOX_WAN_RTT_MS="$rtt" \
    SPARKBOX_WAN_LOSS_PERCENT="$loss" \
    SPARKBOX_WAN_BANDWIDTH_MBPS="$bandwidth" \
    SPARKBOX_WAN_CONCURRENCY="$concurrency" \
    SPARKBOX_WAN_SAMPLES="${SAMPLES:-50}" \
    SPARKBOX_WAN_STALLED_READER="$stalled" \
    "$scratch/nodelink.test" -test.run '^TestWANBenchmarkCell$' -test.v)"
  echo "$output" >&2
  echo "$output" | sed -n 's/^.*WANBENCH_RESULT //p' >>"$results"
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

# A separate, explicit head-of-line case: one stream's peer accepts and stops
# reading while control samples continue.
run_cell 50 0.5 100 10 1

echo "fleet-wan-bench: wrote $output_dir" >&2
