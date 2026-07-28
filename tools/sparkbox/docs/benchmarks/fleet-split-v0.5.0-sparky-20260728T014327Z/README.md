# Split fleet WAN benchmark: v0.5.0 on sparky

Run from `2026-07-28T01:43:27Z` through `02:16:58Z` on Sparky. The
Linux/ARM64 test binary was cross-compiled with Go 1.25.4 from
`74bb8c9c62d7dba194ce6b3b79442682a9c5e008` plus the uncommitted benchmark
harness. That commit is one unrelated secrets change after the v0.5.0
transport release. See `environment.txt` for exact hashes and host provenance.

The comparison is the earlier combined-SSH run in
`fleet-ssh-v0.5.0-sparky-20260728T005558Z`.

## Outcome

- All 145 cells passed. The SSH fixture passed 110 and failed 35.
- All 36 cells with 511 already-live routed connections passed. The SSH
  fixture passed one of 36 because its 512-channel ceiling raced each next
  sampled channel open.
- The routed dialer recorded 37,012 successful connections and no rejected,
  failed, or canceled attempts.
- The separate stalled-reader cell passed without moving control p95.

The architecture has a clear capacity and isolation win. It does not materially
change healthy-WAN latency once network RTT dominates. Independent TCP
connection setup has a meaningful tail-latency cost at high packet loss.

## No-loss latency

The table reports the median p95 across the nine like-for-like combinations of
10/100/1,000 Mbps and 0/10/100 already-live connections. Values are
`combined SSH → split gRPC/routed TCP`.

| Shaped RTT | Control | Stream open | Warm HTTP TTFB | Cold HTTP TTFB |
|---:|---:|---:|---:|---:|
| 0 ms | 0.326 → 0.415 ms | 0.355 → 0.129 ms | 0.223 → 0.159 ms | 0.659 → 0.337 ms |
| 20 ms | 21.099 → 21.216 ms | 21.648 → 21.758 ms | 21.951 → 22.019 ms | 42.470 → 42.586 ms |
| 50 ms | 51.750 → 51.970 ms | 52.129 → 51.635 ms | 52.040 → 51.819 ms | 103.213 → 102.944 ms |
| 100 ms | 101.763 → 102.096 ms | 102.025 → 101.710 ms | 102.182 → 102.032 ms | 203.730 → 202.945 ms |

Across 20–100 ms RTT, every median p95 moved by less than 1%. Control,
stream-open, and warm HTTP remain approximately one RTT; cold HTTP remains
approximately two RTTs.

At zero shaped RTT, routed TCP reduced stream-open p95 by 64%, warm TTFB by
29%, and cold TTFB by 49%. The split control result was 0.089 ms slower, but it
calls the production health RPC, including its event-journal read, while the
SSH fixture uses a transport ping. That sub-millisecond control difference is
not an apples-to-apples protocol regression.

## Packet-loss tails

At 100 ms RTT and 2% per-direction packet loss, medians across the same nine
like-for-like cells were:

| Operation | SSH p50 / p95 / p99 | Split p50 / p95 / p99 |
|---|---:|---:|
| Control | 101.128 / 232.204 / 551.921 ms | 101.380 / 200.979 / 505.062 ms |
| Stream open | 101.264 / 102.229 / 409.254 ms | 101.070 / 102.354 / 1,150.520 ms |
| Warm HTTP | 101.383 / 102.894 / 409.367 ms | 101.249 / 102.129 / 414.494 ms |
| Cold HTTP | 202.512 / 333.406 / 651.372 ms | 202.249 / 1,209.001 / 1,252.315 ms |

Reused gRPC control improved p95 by 13% and p99 by 8%; reused warm HTTP was
effectively unchanged. The new routed connection's median p99 grew to about
1.15 seconds, and cold HTTP's median p95 grew from 333 ms to 1.21 seconds.
That is the Linux initial TCP retransmit timeout surfacing when a SYN or
handshake packet is lost. At 0.5% loss it usually appears at p99; at 2% the
extra cold-connection packets make it frequent enough to reach p95.

Connection reuse is therefore important on the routed data plane. The result
also gives a concrete target for any future connection-pooling or transport
work: preserve the split architecture's capacity without paying a fresh TCP
handshake for every cold request.

## Stalled-reader isolation

At the fixed 50 ms RTT, 0.5% loss, 100 Mbps, and 10-connection coordinate,
split control p95 was 52.316 ms normally and 52.283 ms with a routed peer that
accepted a connection and stopped reading. The blocked data writer did not
delay the separate gRPC control connection.

## Scope

The control fixture uses the production TLS 1.3/mTLS gRPC server and a reused
HTTP/2 connection. The data fixture uses the production `routedguest.Dialer`
and ordinary TCP connections to a guest address inside the approved prefix.
Both addresses traverse the same shaped qdisc.

The fixture does not start Tailscale or measure WireGuard encryption overhead.
It isolates the transport-architecture behavior, not full live-fleet
end-to-end throughput.

Files:

- `results.ndjson`: all 145 machine-readable results;
- `run.log`: raw Go test output;
- `environment.txt`: host, compiler, source, timestamps, and hashes;
- `summary.txt`: pass and routed-outcome counts.
