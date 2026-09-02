# Fleet WAN transport benchmarks

These harnesses compare both sides of the v0.5.0 transport boundary under the
same Linux `tc netem` matrix.

## Combined SSH baseline

`hack/fleet-wan-bench.sh` measures the legacy combined SSH control/data link
under Linux `tc netem`. It builds the existing real-SSH nodelink fixture, runs
it in a temporary network namespace, and shapes only packets whose source or
destination is the fixture's fixed SSH port.

Run a smoke cell first:

```sh
BENCH_HOST_NAME=linux-runner-01 QUICK=1 SAMPLES=10 ./hack/fleet-wan-bench.sh
```

Run the accepted Phase 1 matrix:

```sh
BENCH_HOST_NAME=linux-runner-01 ./hack/fleet-wan-bench.sh
```

The full run covers RTT 0/20/50/100 ms, loss 0/0.5/2%, bandwidth
10/100/1,000 Mbps, and 0/10/100/511 already-live streams. A final separate
cell opens a stream whose TCP peer accepts and stops reading. The script writes
`environment.txt` and newline-delimited JSON results under a timestamped
directory; set `OUTPUT_DIR` to choose a stable destination.
`BENCH_HOST_NAME` is required and is written to both `environment.txt` and
every result row. Use a stable, operator-recognizable name for the Linux host,
not a laptop label or an anonymous CI container.

The delay applied in each direction is half the requested RTT. Loss is the
configured per-direction netem packet loss, which should be stated exactly
when comparing runs. The namespace and qdisc are removed on exit, including
failure.

Each result row records measured p50/p95/p99 values for:

- control RPC round trips;
- new guest-stream opens;
- cold HTTP TTFB, including a new guest stream for every request;
- warm HTTP TTFB, after an unrecorded preflight establishes one persistent
  HTTP connection through the guest stream.

The HTTP samples run through the same real SSH link and node-side
`StreamResolver` as the stream-open samples. The HTTP listener is reachable
only through the mock guest's resolved host address; the benchmark does not
substitute a direct client connection.

`transport_totals` is parsed from the gateway-side transport-neutral registry
after the samples. It contains `disconnects_total`, `dropped_total`, and the
final `write_queue_depth` gauge for `node-b`/`ssh`. An unpublished counter
series means the registry observed no such event and is reported as zero.
These are observed values, not placeholders; the queue value is a final depth,
not a claimed high-water mark.

## Split gRPC control and routed guest data

`hack/fleet-split-wan-bench.sh` runs the same matrix against the v0.5.0 split
architecture:

- a reused TLS 1.3/mTLS HTTP/2 connection carrying real
  `NodeControl.Health` RPCs;
- independent ordinary TCP connections opened by `routedguest.Dialer` for
  stream-open and HTTP samples.

Run a smoke cell first, then the full matrix:

```sh
BENCH_HOST_NAME=linux-runner-01 QUICK=1 SAMPLES=10 ./hack/fleet-split-wan-bench.sh
BENCH_HOST_NAME=linux-runner-01 ./hack/fleet-split-wan-bench.sh
```

The fixed control and guest addresses are installed only in the temporary
network namespace and both are shaped by the same qdisc. The data fixture
exercises the routed guest dialer and ordinary TCP behavior, but deliberately
does not start a Tailscale daemon or measure WireGuard encryption overhead.

The runner also accepts an executable Linux test binary in `BENCH_BINARY`.
That permits a cross-compiled binary to run on a minimal benchmark host while
recording `BENCH_GIT_SHA`, `BENCH_GO_VERSION`, and the binary SHA-256 in
`environment.txt`.

Absolute control latency is not a microbenchmark of identical application
work: the split harness calls the production health RPC, while the SSH harness
uses its transport ping. Compare RTT scaling, loss sensitivity, isolation, and
capacity directly; treat small sub-millisecond control differences as
implementation detail.

Captured result directories must include the named host, kernel, Go version,
git revision, and raw machine-readable output.

Captured Sparky runs:

- [`fleet-ssh-v0.5.0-sparky-20260728T005558Z`](fleet-ssh-v0.5.0-sparky-20260728T005558Z/README.md):
  combined SSH baseline;
- [`fleet-split-v0.5.0-sparky-20260728T014327Z`](fleet-split-v0.5.0-sparky-20260728T014327Z/README.md):
  split gRPC control and routed TCP comparison.
