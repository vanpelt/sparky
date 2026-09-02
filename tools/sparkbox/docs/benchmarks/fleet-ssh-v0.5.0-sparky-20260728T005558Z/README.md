# Fleet SSH WAN benchmark: v0.5.0 on sparky

Run at `2026-07-28T00:55:58Z` against the tagged v0.5.0 source
(`92900586ab80568316988b79dc518eb5c06515f8`).

Sparky did not have a Go toolchain installed, so the Linux/ARM64 test binary was
cross-compiled with Go 1.25.4 on the operator laptop and copied to Sparky. The
benchmark itself ran on Sparky under its Linux kernel and the same network
namespace and `tc netem` shaping used by `hack/fleet-wan-bench.sh`. See
`environment.txt` for the exact provenance.

## Outcome

- 145 cells attempted: 110 passed and 35 failed.
- Every 0-, 10-, and 100-stream matrix cell passed.
- The separate stalled-reader cell passed.
- 35 of the 36 cells with 511 already-live streams failed with
  `ssh: rejected: resource shortage (stream limit)`. Four failed during a
  sampled stream open and 31 during cold HTTP. One 511-stream cell happened to
  pass.
- Every successful cell recorded zero disconnects, zero dropped control
  messages, and a final write-queue depth of zero.

The 511-stream result exposes a harness edge. `MaxLiveStreams` is 512, so 511
held streams leave exactly one slot for each sampled connection. A sampled
connection can be closed by the client before its close has been accounted for
at the server; opening the next connection during that interval correctly hits
the 512-stream ceiling. The accepted script stops on the first such failure.
This run preserved that first result, then continued the other cells and wrote
each edge failure to `failures.tsv`.

## No-loss latency

The values below are the median p95 across the nine successful combinations of
10/100/1,000 Mbps and 0/10/100 live streams for each RTT.

| Shaped RTT | Control | Stream open | Warm HTTP TTFB | Cold HTTP TTFB |
|---:|---:|---:|---:|---:|
| 0 ms | 0.326 ms | 0.355 ms | 0.223 ms | 0.659 ms |
| 20 ms | 21.099 ms | 21.648 ms | 21.951 ms | 42.470 ms |
| 50 ms | 51.750 ms | 52.129 ms | 52.040 ms | 103.213 ms |
| 100 ms | 101.763 ms | 102.025 ms | 102.182 ms | 203.730 ms |

Control, stream open, and warm HTTP cost approximately one shaped RTT. Cold
HTTP costs approximately two because it includes opening a new guest stream.
At 50 ms RTT and zero loss, changing bandwidth and concurrency across the
tested successful values kept control p95 within 51.561–51.957 ms, stream-open
p95 within 51.817–52.340 ms, and cold HTTP p95 within 102.901–103.671 ms.

Packet loss primarily broadened the tail. Across bandwidth and the successful
concurrency values, the median 100 ms RTT / 2% per-direction-loss result was:

- control: 101.128 / 232.204 / 551.921 ms p50/p95/p99;
- stream open: 101.264 / 102.229 / 409.254 ms;
- warm HTTP: 101.383 / 102.894 / 409.367 ms;
- cold HTTP: 202.512 / 333.406 / 651.372 ms.

In the 50 ms RTT, 0.5% loss, 100 Mbps, 10-stream stalled-reader cell, control
p95 was 52.564 ms versus 52.423 ms in its matched non-stalled cell. Neither
cell recorded a disconnect, dropped control message, or final queued write.

## Scope

This harness benchmarks the legacy combined SSH control/data fixture. It is a
useful regression baseline for v0.5.0, but it does not exercise the live
gRPC/mTLS control plane or Tailscale-routed guest data plane.

Files:

- `results.ndjson`: 110 successful machine-readable results;
- `failures.tsv`: matrix coordinates and exact failure for 35 failed cells;
- `run.log`: raw Go test output;
- `environment.txt`: host, compiler, source, kernel, and binary provenance;
- `summary.txt`: attempted/pass/fail counts.
