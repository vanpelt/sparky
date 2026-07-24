# Multi-node sparkbox — gateway and nodes

Status: **proposal** (2026-07-22). Nothing below is built. It is written against
the v0.3.0 tree, and every claim about today's behaviour carries the file it
came from so the first implementation step is a diff, not an excavation.

The goal is one sentence: **a sandbox should be able to run on a machine that is
not the one serving `catnip.sh`, and nobody outside the control plane should be
able to tell.** Same `ssh new@`, same `https://<name>.catnip.sh`, same
`api.catnip.sh`, same browser terminal — a second box just means more capacity.

The shape the user asked for, and the one this document argues for:

- `sparkbox serve` **is the gateway** by default. That is exactly today's
  binary, unchanged, and a single-box deployment stays a single-box deployment.
- `SPARKBOX_GATEWAY=<gateway>:<port>` (or `--gateway`) flips a `sparkbox serve`
  into **node mode**: it dials *out* to the gateway, registers, and offers its
  capacity. It runs no edge, no identity store, no consoles.
- The gateway schedules new sandboxes onto nodes and proxies every data path —
  SSH, HTTP, terminals, env pushes — through to wherever the sandbox lives.

## 1. What single-host sparkbox already gives us

The reason this is tractable: **the data plane already dials sandboxes by
address**, and the address is a struct field, not an assumption baked into the
transport.

| Path | How it reaches a sandbox today | Where |
|---|---|---|
| SSH gateway | `g.dialUpstream(ctx, box.SSHAddr, box.SSHUser)` | `internal/sshgw/gateway.go:332`, `runner.go:25` |
| HTTP proxy | `&url.URL{Host: net.JoinHostPort(box.HostIP, port)}` | `internal/proxy/proxy.go:468` |
| Browser terminal | its own `dialPTY` over `box.SSHAddr` | `internal/xterm/` |
| Secret env push | dials the guest's sshd with the fleet upstream key | `internal/envsync/sync.go` |
| Scheduled jobs | same upstream dial | `internal/sshgw/runner.go` |

Every one of those is `host.Sandbox.SSHAddr` / `.HostIP`
(`internal/host/manager.go:156-159`), filled from `vmm.Instance`
(`internal/vmm/driver.go`). Nothing in the edge knows what a tap is. If those
addresses can be made reachable — or dialled through something that is — the
edge does not change at all.

The second gift is `internal/ctlops`: one transport-agnostic method per control
operation, already stated in terms of narrow interfaces (`Sandboxes`,
`Templates`, `Accounts`, …) that `*host.Manager` satisfies *structurally*
(`internal/ctlops/ops.go:44-80`). A fleet-aware implementation of `Sandboxes`
is a drop-in. The SSH `ctl@` surface, the REST API and the terminal's owner gate
all go through it, so they federate together or not at all.

The third: `host.Manager` already calls itself a node. It has a `nodeName`
(`manager.go:308`, defaulting to `"local"`) and reports `NodeCapacity{Node,
TotalVCPUs, TotalMemMB, UsedMemMB, EffectiveMemMB, UsedDiskMB, Running, …}`
(`manager.go:698`). The scheduler's input format exists.

## 2. Where the seam goes

Two designs were considered.

**A. Remote `vmm.Driver`.** A node runs a thin agent exposing
`Create/Pause/Resume/Destroy`; the gateway keeps one `host.Manager` holding
every sandbox record in the fleet.

**B. Federated nodes.** A node runs today's `sparkbox serve` with the edge and
the identity store switched off — its own `host.Manager`, its own driver, its
own reaper, its own sluice, its own images. The gateway holds identity, the
edge, and a placement index, and forwards control operations to the owning node.

**Take B.** Design A looks smaller until you enumerate what `host.Manager` does
that is irreducibly local: it samples `/proc` and tap byte counters for the
idleness reaper (`vitals`, `manager.go:310`), drives balloon reclaim, measures
disk through `vmm.DiskReporter`, packs multi-GB rootfs artifacts for archive,
and patches image templates. Under A each of those becomes a new remote call
with a new failure mode, and the reaper — which must tick every few seconds
against local counters — becomes a chatty cross-network loop. Under B the node
*is* a sparkbox, which is code we already ship, already test, and already run on
the DGX. Design A also strands the standalone box: today `sparkbox serve` on a
laptop is a complete product, and it should stay one.

The cost of B is that sandbox records are distributed, so the gateway needs an
index and a reconciliation story. That is section 6.

## 3. Split of responsibilities

| Concern | Gateway | Node |
|---|---|---|
| Identity: users, keys, passkeys, invites (`internal/users`) | ✅ sole owner | — |
| Secrets + KEK (`internal/secrets`), OIDC signing key | ✅ sole owner | — never holds signing material |
| Routes (`internal/routes`), netrules (`internal/netrules`) | ✅ sole owner | receives pushes |
| Schedules (`internal/schedule`) | ✅ fires them | — |
| Edge: proxy, TLS/ACME, consoles, `login`, `api`, `-xterm`, `dnsedge` | ✅ | — |
| SSH gateway `:2222` (`new@`, `ctl@`, `<name>@`) | ✅ | — |
| Placement index (name → node), fleet name uniqueness | ✅ new | — |
| VM lifecycle, `vmm.Driver`, `/dev/kvm`, taps | — (except its own local node) | ✅ |
| Sandbox records + `sandboxes.json`, reaper, balloon, disk accounting | — | ✅ |
| Image templates, guest kernel, agent-CLI patch timer | — | ✅ per node, per arch |
| sluice (eBPF egress) + metadata service `:8967` | — | ✅ per node |
| Archive to object storage | orchestrates | ✅ does the upload |

The gateway keeps a **local node** of its own — the degenerate fleet is one
node called `local`, which is exactly today's deployment. `--driver mock` on the
gateway with nodes attached is the "pure control plane" configuration.

## 4. The node link

A node must reach the gateway from behind NAT (a laptop, a home box, a cloud VM
with no inbound rules), and the gateway must be able to open **arbitrary TCP
streams back into the node's sandbox network** — that is what makes the edge
work unchanged. So the link is one outbound connection carrying both control
RPCs and multiplexed data streams in the reverse direction.

Three transports were considered:

| Option | Verdict |
|---|---|
| **L3 over a tailnet** — nodes advertise their sandbox subnet as a Tailscale subnet route; the gateway dials guest IPs directly | Fastest path (no proxying at all) and the DGX is already on a tailnet — but it makes Tailscale a hard dependency, needs `--accept-routes` fleet-wide, and gives no control channel. Keep as an *optimization*, not the mechanism. |
| **mTLS + a stream muxer** (yamux/HTTP2) | Perfectly fine; costs a new CA, a new cert lifecycle, and a new auth model. |
| **SSH, node dials gateway** ✅ | The gateway already *is* an SSH server with a host key and public-key auth; `golang.org/x/crypto/ssh` implements `tcpip-forward` on both sides, which is precisely "the server opens streams toward the client". Node auth is an SSH public key in a roster — the same primitive users already authenticate with. |

**Recommendation: SSH.** The node connects to the gateway's existing `:2222` as
user `node@`, authenticating with its own key. On the connection:

- a **control channel** (one long-lived session channel, newline-delimited JSON
  both ways) carries registration, inventory, heartbeat/capacity, lifecycle RPCs
  gateway→node, and state-change events node→gateway;
- **data streams** are `forwarded-tcpip` channels the gateway opens toward the
  node, each addressed `(guest-ip, port)`, which the node dials on its own
  sandbox network and splices.

That last bullet is the whole trick: the gateway gets a
`func(ctx, sandbox, port) (net.Conn, error)` for every sandbox in the fleet,
and the edge stops caring where anything runs.

### Addressing

Guest IPs are `172.30.<idx>.2` (`internal/vmm/firecracker/fc.go:146-147`), so
every node mints the same addresses. That is fine *only* because the gateway
never routes to them — it names them inside a per-node stream request. Two
follow-ups:

- `vmm.Options.Subnet` exists and is dead: `hostIP`/`guestIP` hardcode
  `172.30.`. Make it real (~20 lines) and give each node a distinct /16 anyway.
  It costs nothing, and it is a prerequisite for ever taking the L3 shortcut.
  `internal/metadata`'s `guestNet` and `netpush.TapName` parse the same literal
  and must move with it.
- `host.Sandbox` grows a `Node` field. Everything that dials must go through the
  fleet dialer rather than `net.Dial`; `proxy.upstreamTransport` is a
  package-level `var` with a fixed `DialContext` (`proxy.go:74`) and becomes
  per-fleet.

### What the gateway must *not* send down the link

The node never receives the OIDC signing key, the secrets KEK, or the users DB.
It receives the gateway's **public** upstream key (so its rootfs templates trust
the gateway's dials), which is not secret — it is already a CI `vars` entry
(`GATEWAY_UPSTREAM_PUBKEY`).

## 5. Guest identity across nodes

`internal/metadata` is the one place where "which sandbox is calling" is
established by network position: source address `172.30.<idx>.2` on the guest's
own tap, with the reasoning written out at the top of `server.go`. That property
is node-local and stays node-local — it cannot be relayed, because the source
address is the credential.

So: **the node keeps the metadata listener, the gateway keeps the signing key.**
The node authenticates the guest exactly as today, then asks the gateway over
the control channel: *"my sandbox `demo`, which I own, wants an id token for
audience X."* The gateway verifies the node genuinely holds `demo` in the
placement index, mints, and returns it. A compromised node can mint tokens for
its own sandboxes and no others, which is the same blast radius it already has
by virtue of running their VMs.

Same pattern for anything else keyed off guest identity.

## 6. State, index, and reconciliation

The gateway gains one table (in the existing `sparkbox.db`):

```
placements(name PK, owner, node, image, arch, state, created_at, updated_at)
```

Rules:

- **Names are allocated by the gateway, before dispatch.** `ctlops.nameIsFree`
  (`ctlops/sandbox.go:82`) and `host.reservedName` already centralize the
  question; it moves from "free on this host" to "free in this fleet", which the
  index answers. Same for routes and handles, which already share
  `internal/reserved`.
- **The node is the source of truth for its own sandboxes' runtime state.** The
  index caches `state` for listing; a stale row is a display artifact, never an
  authorization input.
- **On (re)connect, the node sends its full inventory** and the gateway
  reconciles: rows the node no longer has are marked orphaned (never
  auto-deleted — a wiped node must not silently destroy the user's record of
  what existed); sandboxes the node has that the index does not are adopted if
  the name is free and quarantined if not.
- **Node offline** → its sandboxes list as `unreachable`, `ctl@` operations on
  them fail with a typed `StateError` naming the node, and the edge serves a
  503 that says which node is down rather than a blank 502.

Fleet-wide policy that today reads one manager's state — `--max-running-per-owner`
and the pooled `--disk-pool-mb-per-owner` — moves to the gateway and is computed
from the index; the node keeps enforcing its own RAM admission
(`CapacityError`) because that is a property of its own hardware.

## 7. Placement

Scheduling input is `NodeCapacity` per node, which already exists. The first
policy should be deliberately boring:

1. Filter to nodes whose **arch matches the requested image**. This is not
   optional in this fleet: artifacts are per-arch all the way down
   (`hack/stage-artifacts.sh` names every asset `-<arch>`), and a snapshot
   template taken on the DGX cannot fork on an amd64 node. Snapshots and forks
   are therefore **pinned to their node's arch**, and a fork prefers the node
   holding the template.
2. Filter to nodes with the requested image, headroom under
   `--mem-admission-pct` counting `--mem-reserve-mb`, and disk.
3. Pick most-free-effective-RAM (worst-fit — it keeps big requests satisfiable).
4. Explicit override: `ssh new@gateway --node dgx`, and a per-owner or per-tag
   affinity later (GPU boxes are the obvious driver — the DGX is the only node
   that can offer one).

Placement is sticky for the sandbox's life. Moving one is section 9.

## 8. Failure modes worth designing for now

- **Gateway down.** Nodes keep their sandboxes running; nothing is reachable
  from outside (the edge is the gateway). Nodes retry with backoff and re-send
  inventory on reconnect. Running VMs must never be torn down for want of a
  gateway — this deserves an explicit test.
- **Node flaps.** The gateway must not thrash: mark unreachable after a grace
  period, keep the index rows, do not reschedule (the rootfs is on that node).
- **Split placement.** Two gateways must never claim one node; the node holds
  exactly one gateway link and refuses a second.
- **Name races** are gone by construction: the gateway allocates.
- **A node lying** about its inventory can only affect its own sandboxes,
  because every gateway-side authorization decision consults the index and the
  users DB, never the node's claim.

## 9. Migration and drain (nearly free)

The archive path already packs a sandbox's rootfs into object storage and
restores it on demand (`internal/host/archive.go`, R2 proven on the DGX). A
migration is therefore: pause → archive on node A → update the placement row →
restore on node B. Subject to the arch constraint in section 7, and it is a
cold move (memory snapshot is dropped) — which the archive path already is.

`ctl@ node drain <node>` = archive everything on it, then let normal scheduling
place the restores. That also makes "my laptop is a node when I'm home" viable.

## 10. Milestones

| # | Milestone | Shippable on its own? |
|---|---|---|
| **M0** | `Fleet` type in front of `ctlops.Sandboxes`/`Templates` with exactly one node (`local`). Placement table. Names allocated fleet-wide. `Node` on `host.Sandbox` and in console/API output. **No behaviour change.** | yes — merge before anything remote exists |
| **M1** | Node link: `--gateway`/`SPARKBOX_GATEWAY`, `node@` auth, roster (`ctl@ node ls/approve/rm`), registration + heartbeat + capacity. Node runs its manager; gateway does not schedule to it yet. | yes — observability only |
| **M2** | Remote lifecycle: create/pause/resume/destroy/rename/resize over the control channel; inventory reconcile; typed errors survive the hop. | yes — but the sandbox is unreachable, so operator-only |
| **M3** | Data plane: reverse-tunnel streams; fleet dialer threaded through `sshgw`, `proxy`, `xterm`, `envsync`, `runner`. **This is the milestone that makes a remote sandbox indistinguishable.** | yes — the actual feature |
| **M4** | Node-local services: metadata mint relay, netrules push to the node's sluice, per-node reaper events surfaced. | yes |
| **M5** | Scheduler policy (arch filter, worst-fit, `--node` override), fleet-wide per-owner caps, node column + capacity in both consoles. | yes |
| **M6** | Migration + drain over archive/restore. | yes |

M0–M3 is the useful cut. Everything after is quality.

## 11. Prerequisites and chores this surfaces

- Make `vmm.Options.Subnet` real; move the `172.30.0.0/16` literals out of
  `internal/metadata` and `internal/netpush` behind it.
- `proxy.upstreamTransport` must become per-fleet rather than a package `var`.
- Per-arch image distribution: each node needs its own kernel + rootfs +
  firecracker for *its* arch. `sparkbox setup --release <tag>` already does
  exactly this per-arch — a node bootstraps with the release pipeline we have,
  and `--gateway` is the only new flag. Nodes should report their release tag so
  the gateway can warn about a fleet running mixed versions.
- Node-side archive credentials (rclone remote per node) — or accept that
  archiving is per-node-configured, as it is today.

## 12. Non-goals

Not in this design: live migration of a running VM; a distributed scheduler with
no gateway; multi-gateway HA; cross-node guest networking (sandboxes talking to
each other by name); anything that requires the nodes to trust each other. Nodes
know the gateway and nothing else.

## 13. Decisions taken

Settled 2026-07-22, before any code:

1. **Transport: SSH, node dials gateway.** The node authenticates as `node@` with
   its own key on the gateway's existing `:2222`; one session channel carries
   control JSON, and the gateway opens `sandbox-stream@sparkbox` channels toward
   the node for data. Both halves are already vendored — `gliderlabs/ssh v0.3.8`
   server side (the gateway already routes on username, and `internal/reserved`
   keeps a sandbox from being named `node`), `golang.org/x/crypto/ssh` client
   side, whose `NewClientConn` hands the node a `<-chan NewChannel` it can serve
   arbitrary channel types on. A custom channel type is used rather than
   `forwarded-tcpip`, because we control both ends and the target
   `(sandbox, port)` belongs in a payload of our own rather than smuggled through
   a bind-address field. Rejected: mTLS+yamux (a new CA, cert lifecycle and auth
   model for no gain here); tailnet L3 (a hard Tailscale dependency, and no
   control channel — kept as an optimization, see §4).
2. **The gateway runs VMs**, as a node named `local`. The single-box deployment
   is then the same code path as the fleet, not a special case.
3. **Node admission: pubkey-in-roster + an operator `ctl@ node approve`.** It
   mirrors the invite flow users already go through, and it means a leaked
   pre-shared token cannot enrol a node on its own. Approval names the key by
   its fingerprint and never the node's name: a node picks its own name at
   enrolment, so whoever enrols a name first holds it, and an approval keyed on
   one would bless whichever machine got there first.

Still open: whether fleet-wide per-owner caps land at M2 or wait for M5.
