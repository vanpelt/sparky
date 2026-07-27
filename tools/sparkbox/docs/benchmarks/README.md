# Fleet SSH WAN baselines

`hack/fleet-wan-bench.sh` measures the current combined SSH control/data link
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

No baseline measurements are committed here because they must come from a
named Linux host with its host name, kernel, Go version, and git revision
recorded by the script. The schema is complete, but actual Phase 1 baseline
output still requires running the matrix on that named Linux host.
