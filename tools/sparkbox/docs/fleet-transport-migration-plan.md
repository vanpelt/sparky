# Fleet transport migration plan

> **Status:** Accepted plan, 2026-07-27.
>
> Replace the fleet's combined SSH control/data link with a gRPC control plane
> and Tailscale-routed guest data plane. QUIC is deliberately out of scope.

## Outcome

The target architecture has two independent paths:

```text
User traffic -> Gateway -> direct TCP over Tailscale -> guest subnet
                         \-> gRPC/mTLS -> node control service
```

The public proxy, authentication, browser terminal, and user-facing SSH gateway
remain centralized. Tailnet members do not connect directly to guests; only the
gateway gets routed access.

## Decisions

- Tailscale, or an equivalent WireGuard overlay with subnet routing, is a
  deployment requirement.
- gRPC is control-plane only. Guest TCP must not be carried in gRPC streams.
- Every node receives a unique guest subnet.
- The existing SSH transport remains available during migration and rollback.
- The existing SSH node approval ceremony bootstraps gRPC mTLS certificates.
- Gateway and node upgrades remain independent through capability negotiation.
- QUIC, direct tailnet-client access to guests, multi-gateway HA, and removal of
  the user-facing SSH gateway are out of scope.

## Phases

| Phase | Deliverable | Exit criterion |
|---|---|---|
| 1 | Metrics and WAN benchmark harness | Current p50/p95/p99 behavior is recorded |
| 2 | Remove control RPCs from the warm request path | Warm HTTP requests issue zero `EnsureRunning` RPCs |
| 3 | Separate control and data; split SSH connections | Bulk data cannot delay control or mark a healthy node offline |
| 4 | gRPC/mTLS control with durable operations | All node control runs over gRPC with safe reconnect/retry semantics |
| 5 | Unique guest subnets and routed data | New guest connections use direct TCP; SSH tunneling is fallback only |

## Phase 1: instrument and establish a baseline

### Metrics

Add a transport-neutral metrics package and a Prometheus-compatible `/metrics`
endpoint to the gateway's private HTTP API. Nodes get a small private metrics
listener of their own.

Record:

- control RPC duration and outcome by operation and transport;
- pending and in-flight RPC counts;
- control write-queue depth;
- dropped replies and events;
- reconnects, liveness failures, and disconnect reasons;
- live guest streams by node and transport;
- stream-open duration and failures;
- stream bytes, without sandbox-name labels;
- `EnsureRunning` calls classified as warm, resume, restore, or retry;
- proxy time-to-first-byte for warm and cold sandboxes;
- manager save duration and activity-flush counts.

Avoid sandbox, owner, request, and operation IDs as metric labels. Node and
operation type are bounded enough for fleet-level diagnostics.

### WAN benchmark harness

Add a Linux `tc netem` harness around the existing real-SSH link fixture. Exercise:

- RTT: 0, 20, 50, and 100 ms;
- loss: 0%, 0.5%, and 2%;
- bandwidth: 10, 100, and 1,000 Mbps;
- concurrent streams: 0, 10, 100, and near the 512-stream limit;
- a peer that accepts a stream and stops reading.

Measure control p50/p95/p99, stream-open latency, warm and cold HTTP
time-to-first-byte, disconnects, and queue drops. Check the baseline into the
repository so later phases compare against measurements rather than the current
loopback-only expectation.

### Exit gate

- Metrics are available on both gateway and node.
- The harness is reproducible on a Linux development host.
- The baseline matrix and environment are documented.
- No migration performance claim is accepted without a before/after result.

## Phase 2: remove `EnsureRunning` from the warm path

Split the current behavior into two concepts:

- `EnsureReady`: synchronous and potentially expensive; resumes or restores a
  stopped sandbox.
- `MarkActive`: cheap, nonblocking, and coalesced; prevents an active sandbox
  from being reaped.

### Request flow

For HTTP, SSH, browser terminal, console attach, and scheduled jobs:

1. Read the sandbox from the fleet cache.
2. If it is running, continue immediately.
3. Coalesce `MarkActive` to at most once per sandbox every 10-15 seconds.
4. If it is paused or archived, call `EnsureReady`.
5. If the first guest dial indicates stale state or a stopped guest, singleflight
   one `EnsureReady` and retry the dial once.

Explicit resume commands continue to call `EnsureReady` directly.

### Manager changes

- A warm `EnsureRunning` must not synchronously call `m.save()`.
- Keep `LastActive` current in memory.
- Flush dirty activity timestamps periodically and during graceful shutdown.
- Preserve immediate persistence for lifecycle transitions.
- Coalesce concurrent resume attempts for the same sandbox.

### Exit gate

- One hundred concurrent warm requests produce zero control RPCs.
- One hundred concurrent requests to a paused sandbox produce one resume.
- A stale running cache entry produces one failed dial, one resume, and one retry.
- Sustained traffic still prevents reaping.
- Activity timestamps survive a clean shutdown.
- Archive restore and environment synchronization retain their behavior.

## Phase 3: separate control and data transport

Introduce reusable seams before changing either protocol:

```go
type ControlPlane interface {
	// Lifecycle, inventory, policy, and identity operations.
}

type GuestDialer interface {
	DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error)
}
```

A remote node composes these interfaces instead of assuming they are backed by
the same SSH client. Phase 4 replaces `ControlPlane`; phase 5 replaces
`GuestDialer`.

### SSH data pool

- Keep one SSH connection exclusively for legacy control.
- Open two data-only SSH connections per node by default.
- Add a `sparkbox-data/1` SSH command for data-lane registration.
- Bind each data lane to the authenticated roster node and current control-link
  generation.
- Select the least-loaded healthy lane for new streams.
- Losing one data lane affects its streams but does not mark the node offline.
- Losing the control link invalidates and closes its associated data lanes.
- Once split mode is active, the control connection does not accept guest-stream
  channels.

Add an `ssh_data_pool_v1` handshake capability. New gateways continue accepting
the combined link, and new nodes fall back to it when the gateway lacks the
capability.

### Exit gate

- Saturating or stalling all data lanes keeps control p99 within twice idle p99.
- Bulk traffic causes no false liveness failure.
- Killing one data lane affects only streams on that lane.
- Killing control invalidates the entire node generation.
- Stream limits apply across the pool rather than independently per lane.
- Old/new gateway-node combinations pass compatibility tests.

## Phase 4: replace control with gRPC/mTLS

### Protobuf API

Add a versioned API at `api/node/v1/node.proto`.

`NodeControl` provides:

- `GetInventory` and `WatchEvents`;
- `GetCapacity` and `Health`;
- `EnsureRunning`;
- begin methods for create, pause, archive, resize, reboot, rename, destroy,
  snapshot, and fork;
- `ApplyNetworkPolicy` and `GetNetworkUsage`;
- `GetOperation` and `WatchOperation`.

`GatewayIdentity` provides the token issue and identity description calls needed
by a node's guest metadata service.

Commit generated Go code and pin the protobuf-generation toolchain.

The node runs `NodeControl` on its tailnet address. The gateway consumes
`WatchEvents`; the node calls `GatewayIdentity` when it needs the gateway signer.

### Identity and enrollment

Use a Sparkbox internal CA with short-lived certificates:

- gateway identity: `spiffe://sparkbox/gateway/<cluster-id>`;
- node identity: `spiffe://sparkbox/node/<roster-name>`.

Bootstrap through the existing approval flow:

1. The node enrolls with its SSH key.
2. An operator approves its fingerprint.
3. The node submits a CSR over the approved SSH control channel.
4. The gateway signs it and records serial and expiry in the roster.
5. The node starts its gRPC listener.
6. The gateway verifies gRPC identity and health before preferring it.

Revoking or disabling a roster row closes active clients and prevents renewal.
Replacing SSH enrollment itself is a separate future project.

### Durable operation semantics

Every mutation carries:

- a globally unique `operation_id`;
- an `idempotency_key`;
- a hash of immutable request fields;
- initiator and creation time.

Persist an operation journal on each node. A duplicate operation ID returns the
existing state or result when its request hash matches and fails when it does not.

Long operations return a durable handle immediately. The synchronous `fleet.Node`
adapter may initially wait on `WatchOperation`, preserving caller behavior while
allowing it to reattach after a gateway or link restart.

### Revisioned events

- Inventory contains the node's current monotonic revision.
- Every event carries a revision.
- `WatchEvents(after_revision)` resumes from a known point.
- If the revision is no longer buffered, the node reports a gap and the gateway
  fetches full inventory.
- The current revision survives node restart.

### Rollout

Add:

```text
--node-control-transport=auto|ssh|grpc
--node-grpc-addr=<tailnet-ip>:9443
```

Roll out in this order:

1. Ship the gRPC server disabled.
2. Enable it on one node while SSH remains authoritative.
3. Shadow read-only inventory and compare results.
4. Move read-only operations to gRPC.
5. Move idempotent mutations.
6. Move destructive and long-running mutations.
7. Make `auto` prefer gRPC.
8. Keep SSH control fallback for at least one release.

### Exit gate

- All `fleet.Node` behavior has transport-parity tests.
- A reply lost after a successful mutation is recovered through `GetOperation`.
- Gateway restart during create or archive does not duplicate or lose the work.
- Event gaps force deterministic inventory reconciliation.
- Certificate renewal and revocation tests pass.
- Mixed SSH/gRPC fleet versions remain operable.

## Phase 5: unique subnets and routed guest traffic

### Addressing

Add:

```text
sparkbox serve --guest-subnet 10.200.0.0/20
sparkbox setup --guest-subnet 10.200.0.0/20
```

Use `net/netip` arithmetic rather than hardcoded `172.30` formatting:

- divide the configured prefix into `/30` guest networks;
- assign the host address at offset `+1`;
- assign the guest address at offset `+2`;
- use a `/20` by default for fleet nodes, providing 1,024 `/30` slots;
- keep `172.30.0.0/16` as the single-node compatibility default;
- require explicit, unique prefixes for fleet nodes.

Persist the approved prefix in the node roster. The gateway refuses a node prefix
that overlaps the gateway's local subnet or any online or offline roster node.

Update Firecracker address allocation, the mock driver, guest DNS expansion,
`sparkbox-net.sh`, host setup and doctor checks, sluice configuration, and network
accounting to use the configured prefix.

### Tailscale requirements

Each node advertises its prefix:

```text
tailscale set --advertise-routes=<node-guest-prefix>
```

The gateway accepts routes:

```text
tailscale set --accept-routes=true
```

Tailnet policy permits the gateway identity to reach node gRPC ports and node
guest prefixes. General tailnet members do not receive access to guest prefixes.

`sparkbox doctor` verifies:

- the node control address is tailnet-reachable;
- the guest prefix is advertised and approved;
- the gateway accepts routes;
- IP forwarding is enabled;
- prefixes do not overlap;
- the expected route exists through the node;
- mTLS health succeeds.

### Routed data

Add a `RoutedGuestDialer` that opens an ordinary TCP connection to the sandbox's
current `HostIP`. WireGuard protects the gateway-to-node leg; guest SSH retains
its own end-to-end SSH protection.

Add a `routed_guest_v1` capability and:

```text
--guest-data-transport=auto|ssh|routed
```

Roll out per node, then across 5%, 25%, and 100% of new connections. Existing
pooled connections drain naturally. During the migration window, `auto` may
fall back to the SSH data pool when route health fails. After one stable release,
routed data becomes the default and SSH fallback may be disabled operationally.

### Exit gate

- The gateway can reach every running guest through its routed address.
- New HTTP, terminal, job, and SSH connections use no node-link SSH channel.
- Route loss is observable and can fall back during the migration window.
- Tailnet members other than the gateway cannot reach guest prefixes.
- Existing proxy authorization, route visibility, wildcard TLS, and
  resume-on-connect behavior remain unchanged.

## Pull request sequence

1. Metrics registry and private `/metrics`.
2. Netem benchmark harness and baseline report.
3. Coalesced activity tracker and manager persistence changes.
4. Warm-path proxy/session refactor with singleflight resume.
5. `ControlPlane` and `GuestDialer` separation.
6. SSH data pool and capability negotiation.
7. Protobuf API and generated-code tooling.
8. Operation journal, idempotency, and event revisions.
9. mTLS enrollment and dual-stack gRPC control.
10. Control-plane gRPC cutover.
11. Configurable Firecracker subnet and `/30` allocator.
12. Tailscale setup/doctor integration and overlap validation.
13. Routed guest dialer, canary controls, and final cutover.

## Completion criteria

The migration is complete when:

- warm HTTP traffic performs no cross-node control RPC;
- bulk guest traffic cannot materially delay control liveness;
- retried mutations cannot execute twice or leave the gateway guessing;
- node control uses typed gRPC APIs authenticated with renewable mTLS identities;
- every node has a unique routed guest prefix;
- gateway-to-guest connections use no `sandbox-stream@sparkbox` SSH channel;
- the old SSH paths can be enabled independently for rollback;
- user-facing proxy, SSH, authorization, and scale-to-zero behavior are unchanged.
