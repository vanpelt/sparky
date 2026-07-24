# Multi-node sparkbox — final implementation blueprint

Target tree: `/Users/vanpelt/Development/sparky/tools/sparkbox`
Verified against the working tree on 2026-07-22 (go1.25.4, `gliderlabs/ssh v0.3.8`, `golang.org/x/crypto v0.54.0`).

Every file path, line number, type name and signature below was read out of the real source. Where the
design doc (`docs/multi-node-design.md`) disagrees with the code, the code wins and the disagreement is
called out.

---

## 1. Architecture summary

**Design B, federated nodes.** The gateway keeps the whole control plane — identity (`internal/users`),
secrets + KEK, OIDC signing key, routes, schedules, the edge (`internal/proxy`, both consoles,
`internal/restapi`, `internal/xterm`), the SSH gateway and `internal/ctlops`. A *node* runs a
`*host.Manager` over a real `vmm.Driver` and nothing else: no users DB, no secrets, no OIDC key, no
edge, no consoles, no inbound SSH listener. A node dials **out** to the gateway's existing `:2222`
as SSH user `node`, and that one TCP connection carries both a JSON control channel (one SSH session
channel) and N `sandbox-stream@sparkbox` data channels the gateway opens back toward the node.

**Three seams, in this order.**

1. **`internal/fleet.Node`** — the interface that crosses a machine boundary. *Every* method carries a
   `context.Context` and returns an `error`. It is deliberately **not** `ctlops.Sandboxes`, four of whose
   methods (`Get`, `ListByOwner`, `Touch`, `ArchivingEnabled`) carry neither, and one of which
   (`SetPinned`) carries an error but no context. `ctlops.owned` (ops.go:304) calls `Get` on the hot path
   of every authorized operation, and `xterm.Handler.resolve` (xterm.go:266) calls it per browser
   request — a blocking RPC there would put an uncancellable network call inside every ownership check.
   `LocalNode` wraps the gateway's own `*host.Manager`; `*nodelink.Client` is the remote implementation.
2. **`internal/fleet.Fleet`** — the router. It satisfies `ctlops.Sandboxes`, `ctlops.Templates`,
   `sshgw.Sandboxes` and `xterm.Attacher` verbatim, answering the four context-free reads from a local
   cache plus the durable placement ledger, and dispatching everything else to the owning `Node`. It
   also owns the fleet dialer, `Fleet.DialContext(ctx, network, addr)`.
3. **`internal/nodelink`** — the wire. Framing, request/reply correlation, events, the data-channel
   payload and its `net.Conn` adapter, both ends.

**Two durable gateway tables**, both fifth/sixth writers on the existing `<state-dir>/sparkbox.db`,
copied structurally from `internal/routes/store.go` (same DSN, same redundant PRAGMA loop, own
`*sql.DB`, own `sync.Mutex`, own private `addColumnIfMissing`): `internal/placement` (name → node; its
`PRIMARY KEY(name)` **is** the fleet-wide name allocation) and `internal/nodes` (the SSH-key roster with
pending/approved/disabled).

**Addressing.** Every node mints identical guest IPs `172.30.<idx>.2` (fc.go:146-147), so an address is
not a fleet-wide name and must never leave the node it means something on. The wire therefore carries
**no** `HostIP` and **no** `SSHAddr`. For a remote record the Fleet synthesises

```
HostIP  = "<sandbox>.<node>.sandbox.invalid"
SSHAddr = "<sandbox>.<node>.sandbox.invalid:ssh"
```

`.invalid` is RFC 6761 guaranteed-NXDOMAIN, so any surviving raw `net.Dial` fails closed instead of
hitting the gateway's own VM at the same address; and because `http.Transport` pools idle connections by
URL host string with `MaxIdleConnsPerHost: 64` (proxy.go:76), the per-sandbox host name is what stops
node B's request reusing an idle connection to node A's sandbox — a silent cross-tenant response bleed
with no error and no log line. The literal port `"ssh"` is legal for `net.SplitHostPort` and means "this
sandbox's sshd, resolved node-side from its own record", which is required because firecracker reports
`172.30.<idx>.2:22` (fc.go:933) while the mock driver reports an ephemeral loopback port (mock.go:594).

**Trust.** The placement ledger's `owner` and `node` columns are gateway-authored and are the only
authorization inputs. On every inbound record the Fleet **overwrites** `Owner` and `Node` from the
ledger row before it touches the index; a node's `state`, `disk_mb`, `last_active`, `net_*_bytes` are
node-authored and display-only. That makes "a node lying can only affect its own sandboxes" structural.

**Masking.** `ctlops.owned` runs before any node lookup, so a caller who does not own a sandbox gets the
byte-identical masked `no sandbox named %q` and never learns that a node exists, is down, or holds
anything. Four tests pin that wording (ctlops/ownership_test.go:157, restapi/server_test.go:353,
xterm/xterm_test.go:224, sshgw/control_golden_test.go:250) and all four must keep passing untouched.

**Node-offline is not a new taxonomy.** `internal/restapi/openapi.json` has a hard `enum` of the ten
`kind` strings (components.schemas.Error), and `errors_test.go` pins kind/exit/status; adding a `Kind`
means editing both, so we do not. `fleet.Unreachable` builds a `*ctlops.Error{Kind: KindCapacity, Code:
"node_unreachable", Exit: 1, Status: 503, Verbatim: true}` — `KindCapacity` already yields exit 1 and
HTTP 503, which is exactly what design §6 asks for, and the friendly-capacity guidance in
`sshgw.failStart` (gateway.go:559) and `xterm.startFailure` (ws.go:454) branches on `errors.As` against
the *concrete* `*host.CapacityError`, never on `Kind`, so nothing misrenders. There is also **no** new
`vmm.State`; `host.Sandbox` gains a gateway-only `Unreachable bool`.

**Risk order, not milestone order.** The two things that could invalidate the design ship first as dead
code nothing imports: the typed-error round trip (W1) and the SSH stream transport with its
head-of-line-blocking proof (W2). Only then does the M0 plumbing land, and every data-path dialer seam is
widened while it is still provably a no-op `net.Dial`, so M3 is wiring rather than surgery.

---

## 2. The wire protocol

### 2.0 Transport and auth handshake

The node dials the gateway's existing SSH listener (`--ssh-addr`, default `:2222`):

```go
cfg := &xssh.ClientConfig{
    User:            nodelink.User,            // "node"
    Auth:            []xssh.AuthMethod{xssh.PublicKeys(nodeKey)},
    HostKeyCallback: pinnedOrTOFU,             // see below
    Timeout:         10 * time.Second,
}
```

* `nodeKey` is ed25519 at `<key-dir>/node_key.pem`, minted on first boot with
  `sshgw.LoadOrCreateKey(keysIn, "node_key")`. It is **deliberately not** routed through the
  `--require-keys` `LoadOrCreateKey→LoadKey` switch (main.go:158-162): that switch exists so a missing
  *fleet* key is a hard failure instead of a silently-minted new fleet identity, whereas a node's own
  identity is meant to be minted once on first boot.
* Host key: `--gateway-host-key <path>` pre-seeds a pin. Unset means trust-on-first-use — log the
  fingerprint, persist `<state-dir>/gateway_host_key.pub`, refuse any later change with both
  fingerprints printed.
* `NodeUser = "node"` is a new const in `internal/sshgw`. It is **not** appended to
  `sshgw.ReservedUsers`: cmd/sparkbox/main.go:410 iterates that slice to mint a front-door IPv6 and
  publish a public Cloudflare record per entry, and `resolveDoor` (gateway.go:361) matches it by
  destination IP — joining it would publish `node.<domain>` and make the fleet door reachable by address.
* `"node"` **is** added to `internal/reserved`'s `names` map. The design doc asserts at §13.1 that this
  is already true; verified false — `grep -n node internal/reserved/reserved.go` returns nothing, while
  `gateway`, `edge`, `proxy` and `vpn` are reserved.

On the connection:

* **Control channel** — the node opens exactly one `session` channel and `exec`s the literal string
  `sparkbox-link/1`. The gateway compares `s.Command()` to `[]string{"sparkbox-link/1"}`; anything else
  gets a plain-English sentence on stderr and `Exit(2)`, so a human who types `ssh node@gw` is told what
  the door is. No PTY is requested. Node→gateway frames ride the session's stdin (what the gateway reads
  from `s`); gateway→node frames ride stdout (what the gateway writes to `s`). `s.Stderr()` carries only
  human diagnostics and is never parsed.
* **Data channels** — type `sandbox-stream@sparkbox`, opened by the **gateway** on the
  `*gossh.ServerConn` recovered from `s.Context().Value(gssh.ContextKeyConn)` (gliderlabs sets it at
  server.go:305 from `gossh.NewServerConn`; there is no accessor). The node serves them off
  `client.HandleChannelOpen(nodelink.StreamChannel)`.

`Gateway.Server` (gateway.go:197) needs **no** `ChannelHandlers` change: the gateway only ever *opens*
channels and gliderlabs does not inspect outbound ones. If that ever changes, the literal must
re-register `"session": gssh.DefaultSessionHandler` by hand, because gliderlabs copies
`DefaultChannelHandlers` only when the map is nil (server.go:104-107). `gssh.Server` also sets no
`IdleTimeout` and no `MaxTimeout` today, which is the only reason a long-lived node session survives — a
guard test pins that.

### 2.1 Framing

`internal/nodelink/frame.go`:

```go
const (
    User          = "node"                      // the SSH username
    Protocol      = 1
    LinkCommand   = "sparkbox-link/1"           // what the node execs on its session
    StreamChannel = "sandbox-stream@sparkbox"
    MaxFrameBytes = 1 << 20                     // a longer line closes the link

    DefaultHeartbeat   = 15 * time.Second       // node -> gateway capacity + keepalive
    DefaultPingEvery   = 30 * time.Second       // gateway -> node ping request
    DefaultPingBudget  = 10 * time.Second
    DefaultGrace       = 45 * time.Second       // 3 missed heartbeats
    StreamDialTimeout  = 10 * time.Second       // node -> guest TCP dial
    MaxLiveStreams     = 512                    // per link
    MaxInFlightOps     = 8                      // per link
    LinkMargin         = 2 * time.Second        // subtracted from a caller budget before it rides
)

// Frame is one newline-delimited JSON message. A request carries a non-empty ID
// and Type=<name>; its reply carries the same ID and Type "reply" with exactly
// one of Body/Err. An event carries no ID and is never replied to.
type Frame struct {
    ID         string            `json:"id,omitempty"`
    Type       string            `json:"type"`
    DeadlineMS int64             `json:"deadline_ms,omitempty"` // requests only
    Body       json.RawMessage   `json:"body,omitempty"`
    Err        *ctlops.WireError `json:"err,omitempty"`
}
```

Encoding is one `*json.Encoder` (`SetEscapeHTML(false)`) guarded by a `sync.Mutex` per direction —
`json.Encoder.Encode` appends exactly one `\n` and escapes embedded newlines, so one frame is always one
line. Decoding is a `bufio.Scanner` with a `MaxFrameBytes` buffer in one reader goroutine that dispatches
by `ID` to per-request waiters and by `Type` to handlers.

Request IDs are 16 hex chars **prefixed by side**: gateway IDs begin `g`, node IDs begin `n`. Each side
only ever looks up IDs it issued, so the two ID spaces cannot collide.

**Deadline rule (load-bearing).** A request carries `deadline_ms` = the requester's remaining ctx budget
minus `LinkMargin`. The responder derives its work context from its own **process** context plus that
deadline, **never** from the link. If the link drops mid-request the responder runs the operation to
completion and discards the reply. This is the same rule `firecracker.Driver.boot` already applies at
fc.go:310 (binding a VM's lifetime to a request-scoped ctx would SIGTERM the microVM when the creating
SSH session closed), and it is what makes design §8's "running VMs must never be torn down for want of a
gateway" true. `{"type":"cancel","body":{"id":"g0a1…"}}` is a gateway→node event, never replied to; the
deadline is the backstop.

Budgets are unchanged and are *ceilings*, not additions: `ctlops.PauseTimeout` 3m, `ArchiveTimeout` 15m,
`ResizeTimeout` 10m, `DialTimeout` 15s (ops.go:277-282). A hop must fit inside them.

### 2.2 Handshake

Node → gateway, request `hello` (must arrive within 10s of the exec or the gateway closes):

```go
type Hello struct {
    Protocol    int      `json:"protocol"`     // 1
    Node        string   `json:"node"`         // --node-name; ^[a-z0-9][a-z0-9-]{0,62}$
    Arch        string   `json:"arch"`         // runtime.GOARCH
    OS          string   `json:"os"`           // runtime.GOOS
    Release     string   `json:"release"`      // release tag from the host manifest, "" if unknown
    Version     string   `json:"version"`      // sparkbox build version
    Driver      string   `json:"driver"`       // "firecracker" | "mock"
    Images      []string `json:"images"`       // rootfs template basenames, sorted
    Archiving   bool     `json:"archiving"`    // Manager.ArchivingEnabled()
    Snapshots   bool     `json:"snapshots"`    // Manager.Snapshotter()
    GuestSubnet string   `json:"guest_subnet"` // "172.30.0.0/16"
    StartedAt   time.Time `json:"started_at"`
}
```

Gateway → node, reply body:

```go
type Welcome struct {
    Protocol           int    `json:"protocol"`
    Node               string `json:"node"`                  // canonical name (== the roster row's)
    GatewayUpstreamPub string `json:"gateway_upstream_pub"`  // authorized_keys line, PUBLIC only
    Domain             string `json:"domain"`
    HeartbeatSeconds   int    `json:"heartbeat_seconds"`     // 15
}
```

`GatewayUpstreamPub` is `sshgw.PublicKeyLine(g.upstreamKey)` and is the **only** gateway material that
ever crosses the link. The node persists it to `<state-dir>/gateway_upstream_key.pub` and installs it
with `Manager.SetGatewayPublicKey`.

Refusals are a `reply` carrying `Err`, followed by a `bye` and a close. Codes (all `Kind` `denied`,
`conflict` or `invalid`):

| code | meaning |
|---|---|
| `node_pending` | key enrolled, awaiting `ssh ctl@<gw> node approve <SHA256:...>` (also the answer to the very first hello, which performs the enrolment). Keyed on the fingerprint, never the name — a node names itself, so a name cannot carry an approval |
| `node_disabled` | roster row status is `disabled` |
| `node_name_taken` | that node name is registered to a different key |
| `node_name_mismatch` | `hello.node` != the roster row's name (the row is authoritative) |
| `node_enrol_full` | more than `nodes.MaxPending` (32) rows are pending, or `--no-node-enrol` |
| `bad_node_name` | name fails the regex |
| `protocol_unsupported` | `hello.protocol` != 1 |

**One link per node, new wins.** If a link already exists for that name the gateway sends
`bye{code:"superseded"}` on the **old** one and closes it. Incumbent-wins would let one half-open TCP
socket lock a node out permanently. The node holds exactly one gateway link by construction (one
`--gateway` value), which is design §8's other half.

`bye` (either direction, event, then close):

```go
type Bye struct {
    Code string `json:"code"` // superseded | shutting_down | protocol_error | <a refusal code>
    Msg  string `json:"msg,omitempty"`
}
```

### 2.3 Liveness

* Node → gateway, event `heartbeat`, every `HeartbeatSeconds` (15s), and once immediately after Welcome.
* Node → gateway, SSH global request `SendRequest("keepalive@openssh.com", true, nil)` on the **same**
  cadence. This is the only mechanism by which the *node* detects a half-open TCP and reconnects; without
  it the node writes heartbeats into a black hole forever. gliderlabs services incoming global requests
  on its own goroutine (`go srv.handleRequests(ctx, reqs)`, server.go:308), so the reply arrives.
* Gateway → node, request `ping` every 30s with a 10s budget, body `{"nonce":"…"}` echoed back. Two
  consecutive failures close the link.
* The gateway marks a node offline when its link is gone **or** `DefaultGrace` (45s = 3 heartbeats) has
  elapsed with no frame. Offline never deletes an index row and never reschedules — the rootfs is on that
  machine.
* Node reconnect backoff: 1s, ×2, capped at 60s, ±20% jitter, forever. The link supervisor **must never**
  write to `serve()`'s 5-buffered `errCh` (main.go:440, drained at :645) — `serve` returns on the first
  value there, so under `Restart=always` a routine gateway restart would cold-restart every VM on the
  node.

```go
type Heartbeat struct {
    Capacity host.NodeCapacity `json:"capacity"` // marshalled verbatim; already JSON-tagged
    At       time.Time         `json:"at"`
}
```

### 2.4 Inventory and events (node → gateway)

The wire uses its own projections, **not** `*host.Sandbox`. That keeps an internal struct from becoming a
same-release wire contract (the reason `ctlops.SandboxInfo` exists, types.go:6-11) and — more
importantly — lets the wire deliberately omit every address.

```go
// SandboxRow is a sandbox as the gateway is allowed to see it. There is no
// HostIP and no SSHAddr on purpose: every node mints 172.30.<idx>.2, so an
// address is not a fleet-wide name, and sending one only invites something on
// the gateway to net.Dial it. The Fleet synthesises the addresses it hands out.
type SandboxRow struct {
    Name        string    `json:"name"`
    Owner       string    `json:"owner"`        // advisory; the ledger overwrites it
    Image       string    `json:"image"`
    State       string    `json:"state"`        // vmm.State: running|paused|archived
    VCPUs       int64     `json:"vcpus"`
    MemMB       int64     `json:"mem_mb"`
    DiskMB      int64     `json:"disk_mb,omitempty"`
    DiskTotalMB int64     `json:"disk_total_mb,omitempty"`
    Pinned      bool      `json:"pinned,omitempty"`
    Ballooned   bool      `json:"ballooned,omitempty"`
    SSHUser     string    `json:"ssh_user,omitempty"`
    KeyFP       string    `json:"key_fp,omitempty"`
    NetRxBytes  uint64    `json:"net_rx_bytes,omitempty"`
    NetTxBytes  uint64    `json:"net_tx_bytes,omitempty"`
    ArchivedAt  time.Time `json:"archived_at,omitempty"`
    CreatedAt   time.Time `json:"created_at"`
    LastActive  time.Time `json:"last_active"`
}

type SnapshotRow struct {
    Name      string    `json:"name"`
    Owner     string    `json:"owner"`
    Image     string    `json:"image"`
    FromBox   string    `json:"from_box"`
    CreatedAt time.Time `json:"created_at"`
}

// InventoryMsg is the node's full picture. Sent after Welcome, in reply to a
// gateway `inventory` request, and whenever the node's event queue overflows.
type InventoryMsg struct {
    Node      string            `json:"node"`
    Sandboxes []SandboxRow      `json:"sandboxes"`
    Snapshots []SnapshotRow     `json:"snapshots"`
    Capacity  host.NodeCapacity `json:"capacity"`
    At        time.Time         `json:"at"`
}

type InventoryAck struct {
    Orphaned    []string `json:"orphaned,omitempty"`
    Quarantined []string `json:"quarantined,omitempty"`
}
```

Events, node → gateway, no ID:

```go
// ChangedMsg is emitted on every lifecycle transition the node's manager makes,
// including the reaper's pause and balloon.
type ChangedMsg struct {
    Node    string     `json:"node"`
    Sandbox SandboxRow `json:"sandbox"`
    Reason  string     `json:"reason"` // created|resumed|paused|reaped|ballooned|deflated|
                                       // archived|restored|resized|rebooted|renamed|pinned|
                                       // unpinned|touched|disk
    At      time.Time  `json:"at"`
}

type GoneMsg struct {
    Node   string `json:"node"`
    Name   string `json:"name"`
    Reason string `json:"reason"` // "destroyed"
}

// PausedMsg is emitted from the node manager's SessionCloser hook, BEFORE the
// driver pause, so the gateway can hang up sessions attached to a remotely
// reaped sandbox with the node's own wording.
type PausedMsg struct {
    Node    string `json:"node"`
    Name    string `json:"name"`
    Reason  string `json:"reason"` // e.g. "went idle for 30m"
}
```

**Event backpressure.** The node's emitter holds a 256-deep buffered channel. On overflow it drops the
event, sets a dirty flag and sends one `inventory` instead. Correctness never depends on any individual
event.

Reserved for M4 (declared now so `Protocol` need not be bumped) — node → gateway requests:

```go
type MintReq struct{ Sandbox, Owner, Image, KeyFP, Audience string }
type MintResp struct{ Token string; ExpiresAt time.Time }

type NetAllowReq  struct{ Sandbox, Owner string }
// Governed is semantically distinct from an empty Allow: governed with an empty
// list is a deliberate deny-all, and collapsing the two into one nullable list
// silently converts a deny-all sandbox into an unrestricted one.
type NetAllowResp struct{ Allow []string; Governed bool }
```

### 2.5 Lifecycle requests (gateway → node)

One message type per `fleet.Node` method, so the catalogue is derivable from the interface rather than
hand-maintained.

| type | body | reply body |
|---|---|---|
| `sandbox.create` | `CreateReq{Name,Owner,Image,VCPUs,MemMB}` | `SandboxResp{Sandbox}` |
| `sandbox.ensure_running` | `NameReq{Name}` | `SandboxResp` |
| `sandbox.pause` | `NameReq` | `EmptyResp` |
| `sandbox.archive` | `NameReq` | `EmptyResp` |
| `sandbox.resize` | `ResizeReq{Name,SizeMB}` | `EmptyResp` |
| `sandbox.reboot` | `NameReq` | `EmptyResp` |
| `sandbox.rename` | `RenameReq{Name,NewName,Owner}` | `EmptyResp` |
| `sandbox.destroy` | `NameReq` | `EmptyResp` |
| `sandbox.set_pinned` | `PinReq{Name,Pinned}` | `EmptyResp` |
| `sandbox.resync_env` | `NameReq` | `EmptyResp` |
| `snapshot.create` | `SnapshotReq{Sandbox,Snapshot,Owner}` | `SnapshotResp{Snapshot}` |
| `snapshot.delete` | `DeleteSnapshotReq{Snapshot,Owner}` | `EmptyResp` |
| `snapshot.fork` | `ForkReq{Snapshot,Name,Owner,VCPUs,MemMB}` | `SandboxResp` |
| `inventory` | `{}` | `InventoryMsg` |
| `ping` | `PingReq{Nonce}` | `PingReq` |

Two gateway → node **events** (no ID, no reply), because they are the highest-frequency writes and must
never block a caller:

| type | body |
|---|---|
| `sandbox.touch` | `NameReq{Name}` |
| `sandbox.record_key` | `KeyReq{Name,KeyFP}` |

`Manager.Touch` does a full `sandboxes.json` rewrite (manager.go:1452→`save()`) and is called on every
SSH session teardown (`defer g.mgr.Touch`, gateway.go:321) and every xterm keystroke batch
(ws.go:364). It is the highest-frequency write in the system and must never be a synchronous hop.

The node caps in-flight ops at `MaxInFlightOps` (8) per link and replies
`capacity`/`node_busy` beyond that. Each op runs on its own goroutine; replies may interleave.

The node performs **no** ownership check (that is the gateway's, always) and **no** name policy, and
re-runs everything that is its own hardware's business — its RAM admission, producing
`*host.CapacityError`.

### 2.6 Data streams

Channel type `sandbox-stream@sparkbox`. Extra data is `gossh.Marshal`'d (RFC 4254 §5.1 convention, the
same encoding `direct-tcpip` uses), **not** JSON:

```go
type StreamOpen struct {
    Sandbox string
    Kind    string // "ssh" | "tcp"
    Port    uint32 // guest port when Kind=="tcp"; 0 for "ssh"
    Nonce   string // correlates the node's log line with the gateway's
}
```

The payload names a **sandbox**, never an address. The node re-resolves from its own manager, so a stale
gateway cache can never make a node dial an arbitrary node-local address:

* `Kind == "ssh"` → dial `box.SSHAddr` verbatim. This is what keeps the mock driver (sshd on an ephemeral
  127.0.0.1 port) working and keeps "port 22" knowledge node-side.
* `Kind == "tcp"` → dial `net.JoinHostPort(box.HostIP, itoa(Port))`.

Node-side rejections use `gossh.NewChannel.Reject`:

| reason | message | when |
|---|---|---|
| `gossh.UnknownChannelType` | `""` | channel type mismatch |
| `gossh.ConnectionFailed` | `"bad stream request"` | unmarshal failed |
| `gossh.Prohibited` | `"unknown sandbox"` | not in this node's manager (a gateway bug — it already authorized; log at Error) |
| `gossh.Prohibited` | `"sandbox not running"` | state != running, or the resolved address is empty |
| `gossh.ConnectionFailed` | `<dial error>` | `net.DialTimeout` failed inside the guest |
| `gossh.ResourceShortage` | `"stream limit"` | more than `MaxLiveStreams` live |

A rejection reaches the gateway as `*xssh.OpenChannelError{Reason, Message}` — that typed carrier is what
lets `proxy.ErrorHandler` tell "the node said no such sandbox" (503) from "connection refused inside the
guest" (the existing 502 "Nothing is listening on port N").

On accept: `go xssh.DiscardRequests(reqs)`, then bidirectional `io.Copy` with `ch.CloseWrite()` on each
half-close; both sides closed when either ends.

**Head-of-line blocking — the one real hazard.** On the node side, `x/crypto/ssh`'s mux does a *blocking*
send into a `chanSize`-buffered queue (`m.incomingChannels <- c`, mux.go:330; `chanSize = 16`,
handshake.go:26) **from the mux loop goroutine**. If the node fails to drain the queue returned by
`HandleChannelOpen`, a burst of 17 concurrent proxy opens freezes heartbeats, lifecycle RPCs and every
other stream on that connection. The node therefore runs a dedicated accept goroutine whose only job is
`for nc := range chans { go serveOne(nc) }` — it must never do work inline. (Inbound channel *data* is
not a hazard: it lands in an unbounded linked-list buffer bounded only by the 2 MB per-channel window, so
bulk transfer never stalls the mux loop. W2's acceptance test measures control-channel ping latency under
load anyway, so a regression here is caught.)

The gateway side needs no such care: `*gossh.ServerConn.OpenChannel` is safe for concurrent use, and
gliderlabs already dispatches each inbound channel with `go handler(...)` (server.go:319).

### 2.7 `net.Conn` over a channel

`nodelink.streamConn` wraps a `gossh.Channel`:

* `LocalAddr`/`RemoteAddr` return `fleetAddr{network: "sandbox", addr: "<node>/<sandbox>:<port>"}` so log
  lines stay legible.
* `SetDeadline`/`SetReadDeadline`/`SetWriteDeadline` are implemented as **close-on-expiry**: an
  `time.AfterFunc` that closes the channel, disarmed by the zero time. This is coarser than
  `*net.TCPConn` (an expired deadline is not recoverable) and is *better* than x/crypto's own `chanConn`,
  which returns `errors.New("ssh: tcpChan: deadline not supported")` from both setters (tcpip.go:535-545).
  It is safe because the only setters in this tree are (a) `DialUpstreamVia`, which arms one deliberately
  around `xssh.NewClientConn`, and (b) `net/http`'s client transport — verified on go1.25.4 that
  `$GOROOT/src/net/http/transport.go` calls **no** `SetDeadline`/`SetReadDeadline`/`SetWriteDeadline` at
  all (only `net/http/server.go` and `h2_bundle.go` do). W2's acceptance test pins that assumption.

**The `context.AfterFunc` asymmetry — write this down or someone will "simplify" it back into a bug.**
Bounding a tunneled stream with `context.AfterFunc(requestCtx, conn.Close)` is correct for the SSH, PTY
and envsync callers, which do not pool (envsync already does exactly this at sync.go:266). It is
*wrong* for the proxy, whose `http.Transport` reuses the connection after the dialing request finishes —
it produces intermittent resets under load with no error and no log line. So: `Fleet.DialContext` honours
the caller's ctx for the **channel open** and installs **no** close-bound; the conn's lifetime belongs to
whoever holds it (the transport's pool, or the SSH client's `Close`). `Fleet.Close()` tears down every
live stream.

### 2.8 Error encoding

`internal/ctlops/wire.go` — the projection lives next to the taxonomy, for the same reason `Budgets` are
exported from ctlops. `restapi`'s `apiError` (server.go:398) is the HTTP projection of the same thing;
this is the link projection.

```go
// WireError is *Error projected for a transport that cannot carry Go types.
// Kind rides as its String() because the Kind constants are iota-ordered and a
// future insertion would silently renumber a released wire format.
type WireError struct {
    Kind     string         `json:"kind"`
    Op       string         `json:"op,omitempty"`
    Code     string         `json:"code,omitempty"`
    Msg      string         `json:"msg"`
    Hint     string         `json:"hint,omitempty"`
    Details  map[string]any `json:"details,omitempty"`
    Verbatim bool           `json:"verbatim,omitempty"`
    Exit     int            `json:"exit,omitempty"`
    Status   int            `json:"status,omitempty"`
    Host     *WireHostError `json:"host,omitempty"`
}

// WireHostError carries the concrete internal/host error a *Error wraps, with
// typed fields rather than a Details map. This is not tidiness: json.Unmarshal
// turns every number in a map[string]any into a float64, and
// sshgw.failStart (gateway.go:559-578) dereferences limit.Running[0] with no
// nil guard — so a LimitError rebuilt out of Details would either panic on a
// type assertion or arrive with an empty Running slice and panic the gateway
// session.
type WireHostError struct {
    Type        string   `json:"type"` // limit|capacity|quota|missing|state|disabled|name
    Max         int      `json:"max,omitempty"`
    Running     []string `json:"running,omitempty"`
    RequestedMB int64    `json:"requested_mb,omitempty"`
    UsedMB      int64    `json:"used_mb,omitempty"`
    BudgetMB    int64    `json:"budget_mb,omitempty"`
    PoolMB      int64    `json:"pool_mb,omitempty"`
    Owner       string   `json:"owner,omitempty"`
    Noun        string   `json:"noun,omitempty"`
    Name        string   `json:"name,omitempty"`
    Problem     int      `json:"problem,omitempty"` // host.NameProblem
    Code        string   `json:"code,omitempty"`
    Msg         string   `json:"msg,omitempty"`
}

func ToWire(op string, err error) *WireError  // AsError(op, err), project, walk Unwrap for the host type
func FromWire(w *WireError) *Error            // rebuild, incl. the concrete host.* value on Err
func ParseKind(s string) (Kind, bool)         // exact inverse of Kind.String()
```

`FromWire` sets `Error.Err` to a freshly constructed `*host.LimitError` / `*host.CapacityError` /
`*host.DiskQuotaError` / `*host.MissingError` / `*host.StateError` / `*host.DisabledError` /
`*host.NameError`. Because `(*Error).Unwrap()` returns it, the shipped `errors.As` switches in
`sshgw.failStart` (gateway.go:559,570) and `xterm.startFailure` (ws.go:454,459) keep firing after the
hop, and **not one line of either renderer changes**. And because `AsError`'s first branch is
`if errors.As(err, &e) { … return e }` (errors.go:236), an already-classified `*Error` passes through the
gateway untouched.

`FromWire` always returns a **fresh** `*Error`, never a shared sentinel, which sidesteps `AsError`'s
documented in-place `Op` mutation (errors.go:240-242 — the reason `restapi/terminal.go:43` copies the
value before re-stamping).

No consumer may type-assert an int out of `Details`; a test pins the float64 hazard.

**Node offline.** One constructor, in `internal/fleet`:

```go
// Unreachable is the canonical "that machine is not answering" error. It reuses
// KindCapacity deliberately: KindCapacity already yields exit 1 and HTTP 503,
// which is what design §6 asks for, and adding a Kind would mean editing both
// the pinned taxonomy table in ctlops/errors_test.go AND the hard `kind` enum in
// the hand-authored, //go:embed'ed restapi/openapi.json (which is parsed at
// package init, so a bad edit panics the binary at startup). The friendly
// capacity guidance in sshgw and xterm branches on errors.As against the
// concrete *host.CapacityError, never on Kind, so nothing misrenders.
func Unreachable(op, sandbox, node string) *ctlops.Error {
    return &ctlops.Error{
        Kind: ctlops.KindCapacity, Op: op, Code: "node_unreachable",
        Msg:  fmt.Sprintf("sandbox %q lives on node %q, which is offline", sandbox, node),
        Hint: "It will be reachable again when the node reconnects; nothing was lost.",
        Details:  map[string]any{"node": node},
        Verbatim: true, Exit: 1, Status: http.StatusServiceUnavailable,
    }
}

// IsNodeUnreachable is the predicate proxy and both consoles branch on.
func IsNodeUnreachable(err error) bool
```

This error is only ever reachable **after** `ctlops.owned` has passed, so it can never be an existence
oracle.

`ctlops.notFoundMsg` (errors.go:155) gains `"node": "no node named %q"` so `ctl@ node rm ghost` renders
correctly instead of falling through to the generic `no node %q`.

---

## 3. New packages

### 3.1 `internal/placement`

The gateway's durable name → node index. A fifth writer on `<state-dir>/sparkbox.db`, copied
structurally from `internal/routes/store.go:80-150`: the exact DSN
`file:<path>?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)`,
the redundant explicit PRAGMA loop, an inline `CREATE TABLE IF NOT EXISTS`, its **own private copy** of
`addColumnIfMissing` (each store keeps one on purpose — see `var _ = addColumnIfMissing` in
secrets/store.go:209), and a `mu sync.Mutex` serialising writes. There is no schema-version table
anywhere in this project and this store must not invent one.

Note: `<state-dir>/schedules.db` is a *separate* file (main.go:252) despite what schedule/store.go's
comment says. Placements go in `sparkbox.db`.

```sql
CREATE TABLE IF NOT EXISTS placements (
  name       TEXT PRIMARY KEY,
  owner      TEXT NOT NULL,
  node       TEXT NOT NULL,
  image      TEXT NOT NULL DEFAULT '',
  arch       TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS placements_node  ON placements(node);
CREATE INDEX IF NOT EXISTS placements_owner ON placements(owner);
```

```go
// Package placement is the gateway's durable answer to "which machine holds
// this sandbox name". The PRIMARY KEY on name IS the fleet-wide name
// allocation: Reserve is a bare INSERT, so a key conflict is the taken-name
// answer and no read-then-write window exists.
//
// sandboxes.json stays NODE-LOCAL truth. This table's owner and node columns
// are gateway-authored and are the only authorization inputs; its state column
// is a cache, and a stale one is a display artifact, never an authorization
// input.
package placement

const (
    StateOK         = ""           // normal
    StateOrphaned   = "orphaned"   // the node is up and does not have it
    StateQuarantine = "quarantine" // two nodes claim the name
)

var (
    ErrTaken    = errors.New("that sandbox name is already placed")
    ErrNoSuchRow = errors.New("no such placement")
)

type Row struct {
    Name      string    `json:"name"`
    Owner     string    `json:"owner"`
    Node      string    `json:"node"`
    Image     string    `json:"image"`
    Arch      string    `json:"arch"`
    State     string    `json:"state"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

type Store struct{ /* mu sync.Mutex; db *sql.DB */ }

func Open(path string) (*Store, error)
func (s *Store) Close() error

func (s *Store) Reserve(name, owner, node, image, arch string) error // INSERT; ErrTaken on conflict
func (s *Store) Release(name string) error
func (s *Store) Rename(old, new string) error                       // DELETE+INSERT in one immediate tx
func (s *Store) SetNode(name, node string) error                    // migration (M6)
func (s *Store) SetRowState(name, state string) error
func (s *Store) Get(name string) (Row, bool, error)
func (s *Store) List() ([]Row, error)
func (s *Store) ByNode(node string) ([]Row, error)
func (s *Store) ByOwner(owner string) ([]Row, error)
```

### 3.2 `internal/nodes`

```go
// Package nodes is the fleet's node roster: an SSH public key, a node name and
// an approval status, in sparkbox.db. It is deliberately separate from
// internal/users so a node key can never satisfy a user lookup and a user key
// can never open the node door. Same store template as internal/routes.
package nodes

const (
    StatusPending  = "pending"
    StatusApproved = "approved"
    StatusDisabled = "disabled"
)

// MaxPending bounds self-enrolment the way an invite quota bounds signup.
const MaxPending = 32

var (
    ErrNoSuchNode     = errors.New("no such node")
    ErrNameTaken      = errors.New("that node name is registered to a different key")
    ErrTooManyPending = errors.New("too many nodes are awaiting approval")
    ErrBadName        = errors.New("invalid node name")
)

type Node struct {
    Name       string     `json:"name"`
    Wire       string     `json:"-"`   // base64(key.Marshal()) — never rendered
    FP         string     `json:"fp"`  // SHA256:… for the operator to compare out of band
    Status     string     `json:"status"`
    Arch       string     `json:"arch,omitempty"`
    Release    string     `json:"release,omitempty"`
    ApprovedBy string     `json:"approved_by,omitempty"`
    FirstSeen  time.Time  `json:"first_seen"`
    LastSeen   *time.Time `json:"last_seen,omitempty"`
    ApprovedAt *time.Time `json:"approved_at,omitempty"`
}

func ValidName(s string) bool // ^[a-z0-9][a-z0-9-]{0,62}$

type Store struct{ /* mu sync.Mutex; db *sql.DB */ }

func Open(path string) (*Store, error)
func (s *Store) Close() error

// Lookup returns the row for a key WHATEVER its status — unlike users.Lookup,
// which filters to active — because the node door must be able to answer
// "pending" distinguishably from "unknown".
func (s *Store) Lookup(key xssh.PublicKey) (Node, bool)
func (s *Store) PendingCount() (int, error)
// Enroll is idempotent for the same key: a reconnecting pending node gets its
// own row back. ErrNameTaken when the name belongs to a different key.
func (s *Store) Enroll(name string, key xssh.PublicKey) (Node, error)
func (s *Store) Approve(name, by string) error
func (s *Store) Remove(name string) error
func (s *Store) List() ([]Node, error)
func (s *Store) Get(name string) (Node, error)
func (s *Store) Seen(name, arch, release string, at time.Time) error
```

Schema:

```sql
CREATE TABLE IF NOT EXISTS nodes (
  name        TEXT PRIMARY KEY,
  wire        TEXT NOT NULL,
  fp          TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'pending',
  arch        TEXT NOT NULL DEFAULT '',
  release     TEXT NOT NULL DEFAULT '',
  approved_by TEXT NOT NULL DEFAULT '',
  first_seen  TIMESTAMP NOT NULL,
  approved_at TIMESTAMP,
  last_seen   TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS nodes_wire ON nodes(wire);
```

`wire` is `base64(key.Marshal())`, a private copy of `users.wireOf` (store.go:206) — the exact wire form
the client proves possession of, not a fingerprint.

### 3.3 `internal/nodelink`

```go
// Package nodelink is the node link wire: control-channel framing over an SSH
// session channel, request/reply correlation, events, and the
// sandbox-stream@sparkbox data channel adapted to net.Conn. Pure protocol — it
// holds no policy and no stores.
//
// Import DAG: host <- ctlops <- nodelink <- fleet <- sshgw/proxy/xterm/cmd.
// nodelink must NEVER import internal/fleet: the gateway half takes plain
// callbacks (Hooks), not a fleet type, which is what keeps the DAG acyclic.
package nodelink

// --- constants, Frame, Hello/Welcome/Bye/Heartbeat/Inventory*/Changed/Gone/
//     Paused/Cancel, the request bodies, StreamOpen: all as in section 2 ---

// Conn is one framed control channel. Transport-agnostic, so its whole test
// suite runs over net.Pipe.
type Conn struct{ /* ... */ }

func NewConn(r io.Reader, w io.Writer, idPrefix string, log *slog.Logger) *Conn
// Serve runs the reader until EOF or a protocol error, then fails every pending
// waiter with the error Fail returns.
func (c *Conn) Serve(ctx context.Context) error
// Request stamps DeadlineMS from ctx (minus LinkMargin) and waits on
// {reply, ctx.Done, link death}. On ctx cancellation it emits a `cancel` event.
func (c *Conn) Request(ctx context.Context, typ string, body, out any) error
func (c *Conn) Event(typ string, body any) error
// Handle registers a request handler. The handler's ctx is derived from the
// Conn's PROCESS context plus the frame deadline, never from the link.
func (c *Conn) Handle(typ string, fn func(ctx context.Context, body json.RawMessage) (any, error))
func (c *Conn) OnEvent(typ string, fn func(body json.RawMessage))
// Fail sets the error pending and future requests are failed with. The gateway
// side sets fleet.Unreachable-shaped errors; the node side sets io.EOF.
func (c *Conn) Fail(err error)
func (c *Conn) Close() error

// --- streams ---

// OpenStream opens a sandbox-stream channel on conn and adapts it to net.Conn.
// An *xssh.OpenChannelError propagates untouched so callers can tell a node's
// refusal from a dial failure inside the guest.
func OpenStream(conn gossh.Conn, req StreamOpen) (net.Conn, error)

// Resolver answers "what address is this sandbox's <kind> port on THIS node".
type Resolver func(sandbox, kind string, port int) (addr string, err error)

// ServeStreams drains chans forever, dispatching each open to its own goroutine.
// It MUST be given the channel from Client.HandleChannelOpen and MUST NOT do
// work inline: x/crypto's mux blocks on a 16-deep queue from its loop goroutine
// (mux.go:330, chanSize=16 handshake.go:26), so a stalled accept loop freezes
// heartbeats and every other stream on the connection.
func ServeStreams(ctx context.Context, chans <-chan gossh.NewChannel, resolve Resolver, log *slog.Logger)

// --- gateway half ---

// Hooks is what the gateway integrator supplies. Plain funcs, not a fleet
// interface, so this package never imports internal/fleet.
type Hooks struct {
    OnInventory func(node string, inv InventoryMsg) InventoryAck
    OnHeartbeat func(node string, hb Heartbeat)
    OnChanged   func(node string, m ChangedMsg)
    OnGone      func(node string, m GoneMsg)
    OnPaused    func(node string, m PausedMsg)
}

type ServerOptions struct {
    Node    string            // the AUTHENTICATED roster name; body.node is advisory
    Session io.ReadWriter     // the gliderlabs session
    Stderr  io.Writer
    Conn    gossh.Conn        // for OpenChannel
    Welcome Welcome
    Hooks   Hooks
    Log     *slog.Logger
}

// Client is the gateway's handle on one connected node.
type Client struct{ /* ... */ }

// Serve performs the handshake and owns the link for its lifetime. hello is the
// already-read first frame; the caller (sshgw) has done roster policy.
func Serve(ctx context.Context, opts ServerOptions, hello Hello) (*Client, func() error, error)

func (c *Client) Name() string
func (c *Client) Hello() Hello
func (c *Client) Online() bool
func (c *Client) LastSeen() time.Time
func (c *Client) Capacity() host.NodeCapacity
func (c *Client) Snapshot() (boxes []SandboxRow, snaps []SnapshotRow) // last inventory + events
func (c *Client) Do(ctx context.Context, typ string, body, out any) error
func (c *Client) Cast(typ string, body any)
func (c *Client) DialSandbox(ctx context.Context, sandbox, kind string, port int) (net.Conn, error)
func (c *Client) Close() error

// --- node half ---

// Emitter is the host.Observer + host.SessionCloser the node installs on its
// manager. Sends are non-blocking into a 256-deep queue; on overflow it drops
// the event and asks for a full inventory instead.
type Emitter struct{ /* ... */ }
func NewEmitter(log *slog.Logger) *Emitter
func (e *Emitter) SandboxChanged(b *host.Sandbox, reason string)
func (e *Emitter) SandboxGone(name string)
func (e *Emitter) CloseSandboxSessions(sandbox, reason string) int // returns 0 immediately

type ClientOptions struct {
    Gateway     string        // host:port
    NodeName    string
    Key         xssh.Signer
    HostKeyPath string        // TOFU pin file; "" disables pinning
    HostKey     xssh.PublicKey // pre-seeded pin; nil enables TOFU
    Manager     *host.Manager
    Emitter     *Emitter
    Hello       func() Hello
    OnWelcome   func(Welcome) error // node persists GatewayUpstreamPub here
    BackoffMin, BackoffMax time.Duration
    Log         *slog.Logger
}

// RunClient dials, registers, serves and reconnects with jittered backoff until
// ctx is done. It NEVER returns on a transport error, so it can never poison
// cmd/sparkbox's errCh (main.go:440), which serve() returns on.
func RunClient(ctx context.Context, opts ClientOptions) error
```

### 3.4 `internal/fleet`

```go
// Package fleet routes control-plane operations to the machine that holds a
// sandbox. It declares the Node interface (the ONLY thing that crosses a
// machine boundary), the LocalNode adapter over *host.Manager, and the Fleet
// type that satisfies ctlops.Sandboxes, ctlops.Templates, sshgw.Sandboxes and
// xterm.Attacher.
package fleet

// Suffix is the RFC 6761 .invalid label the gateway's synthetic sandbox
// addresses live under. Nothing will ever resolve it, which is the point: a
// path that still net.Dials a remote record fails closed instead of reaching
// the gateway's OWN sandbox at the same 172.30.<idx>.2 address, and the
// per-sandbox host string is what keeps http.Transport's idle-connection pool
// from serving node B's request over an idle connection to node A.
const Suffix = ".sandbox.invalid"

// SSHPort is the literal port half of a synthetic SSHAddr. It is a name, not a
// number, because only the owning node knows where its guests' sshd listens:
// firecracker reports 172.30.<idx>.2:22 (fc.go:933), the mock driver reports an
// ephemeral 127.0.0.1 port (mock.go:594).
const SSHPort = "ssh"

func Host(sandbox, node string) string                    // "<sandbox>.<node>.sandbox.invalid"
func SplitHost(h string) (sandbox, node string, ok bool)

// Node is one machine that runs sandboxes. EVERY method takes a context and
// returns an error — deliberately unlike ctlops.Sandboxes, whose Get,
// ListByOwner, Touch and ArchivingEnabled can report no network failure at all
// and whose Get sits inside every authorization decision (ctlops/ops.go:304).
// That difference is the entire reason this interface exists: Fleet answers the
// context-free reads from its own state and crosses a machine boundary only
// through here.
type Node interface {
    Name() string
    Facts() Facts
    Online() bool

    // Snapshot is the node's last known inventory, served from cache. It is how
    // Fleet answers Get/List/ListByOwner/Snapshots without a network call.
    Snapshot() ([]*host.Sandbox, []*host.Snapshot)
    Capacity() host.NodeCapacity

    Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error)
    EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error)
    Pause(ctx context.Context, name string) error
    Archive(ctx context.Context, name string) error
    Resize(ctx context.Context, name string, sizeMB int64) error
    Reboot(ctx context.Context, name string) error
    Rename(ctx context.Context, oldName, newName, owner string) error
    Destroy(ctx context.Context, name string) error
    SetPinned(ctx context.Context, name string, pinned bool) error
    ResyncEnv(ctx context.Context, name string) error
    Touch(ctx context.Context, name string) error
    RecordKey(ctx context.Context, name, fp string) error

    Snapshotter(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error)
    DeleteSnapshot(ctx context.Context, snapName, owner string) error
    Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error)

    // DialGuest opens a stream to a port inside a sandbox. kind is "ssh"
    // (port ignored) or "tcp".
    DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error)
}

type Facts struct {
    Node, Arch, OS, Release, Version, Driver, GuestSubnet string
    Archiving, Snapshots bool
    Images    []string
    StartedAt time.Time
}

// Local adapts the gateway's own *host.Manager to Node. Every method is a
// direct pass-through; DialGuest uses the same net.Dialer proxy has always
// used, so a single-box deployment dials exactly as it did before.
func Local(name string, mgr *host.Manager) Node
func LocalManager(n Node) (*host.Manager, bool) // the gateway still needs the concrete one

// Remote wraps a *nodelink.Client as a Node. It is deliberately dumb: no
// retries, no caching, no policy. It DOES enforce two invariants, one line per
// return path: every record it returns has Node overwritten with the
// AUTHENTICATED link name, and a request on a dead link returns
// Unreachable(...), never io.EOF.
func Remote(c *nodelink.Client) Node

type Options struct {
    Local     *host.Manager     // required: the gateway's own machine
    LocalName string            // --node-name; "" -> "local"
    LocalArch string            // runtime.GOARCH
    Index     *placement.Store  // nil: single-node, no ledger, no remote nodes
    Grace     time.Duration     // 0 -> nodelink.DefaultGrace (45s)
    Log       *slog.Logger
    Now       func() time.Time
}

type Fleet struct{ /* ... */ }

func New(opts Options) (*Fleet, error)
func (f *Fleet) Close() error

// The compile assertions live HERE. internal/ctlops/fakes_test.go is
// `package ctlops`, and fleet imports ctlops, so an in-package assertion would
// be an import cycle. (An external `package ctlops_test` file would also work;
// keeping them here means one place to look.)
var (
    _ ctlops.Sandboxes = (*Fleet)(nil)
    _ ctlops.Templates = (*Fleet)(nil)
    _ Node             = (*localNode)(nil)
    _ Node             = (*remoteNode)(nil)
)

// --- ctlops.Sandboxes, signatures verbatim ---
func (f *Fleet) Get(name string) (*host.Sandbox, bool)
func (f *Fleet) ListByOwner(owner string) []*host.Sandbox
func (f *Fleet) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error)
func (f *Fleet) EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error)
func (f *Fleet) Pause(ctx context.Context, name string) error
func (f *Fleet) Archive(ctx context.Context, name string) error
func (f *Fleet) Resize(ctx context.Context, name string, sizeMB int64) error
func (f *Fleet) Reboot(ctx context.Context, name string) error
func (f *Fleet) Rename(ctx context.Context, oldName, newName, owner string) error
func (f *Fleet) Destroy(ctx context.Context, name string) error
func (f *Fleet) SetPinned(name string, pinned bool) error   // bounded internally: 10s
func (f *Fleet) ResyncEnv(ctx context.Context, name string) // fire-and-forget for remote
func (f *Fleet) Touch(name string)                          // fire-and-forget, coalesced
func (f *Fleet) ArchivingEnabled() bool                     // OR over local + online nodes

// --- ctlops.Templates ---
func (f *Fleet) Snapshots(owner string) []*host.Snapshot
func (f *Fleet) Snapshot(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error)
func (f *Fleet) DeleteSnapshot(ctx context.Context, snapName, owner string) error
func (f *Fleet) Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error)
func (f *Fleet) Snapshotter() bool                          // OR over local + online nodes

// --- sshgw.Sandboxes / proxy.Resumer / xterm.Attacher extras ---
func (f *Fleet) List() []*host.Sandbox
func (f *Fleet) RecordKey(name, fp string)                  // fire-and-forget, coalesced

// --- data plane ---
// DialContext is the fleet dialer. A "<sandbox>.<node>.sandbox.invalid" host
// routes through that node's link; anything else falls through to the package
// default net.Dialer, so a single-box deployment is byte-identical.
//
// It honours ctx for the channel open and installs NO close-bound: a pooled
// http.Transport connection outlives the request that dialed it, and
// context.AfterFunc(reqCtx, conn.Close) there produces intermittent resets
// under load. Non-pooled callers (SSH, PTY, envsync) install their own.
func (f *Fleet) DialContext(ctx context.Context, network, addr string) (net.Conn, error)

// --- fleet management ---
func (f *Fleet) Attach(n Node) (detach func(), err error)
func (f *Fleet) NodeOf(sandbox string) (node string, ok bool)
func (f *Fleet) Online(node string) bool
func (f *Fleet) Capacities() []host.NodeCapacity
func (f *Fleet) Nodes() []NodeStatus

// Reconcile folds a node's full inventory into the index. See section 5, W20.
func (f *Fleet) Reconcile(node string, boxes []*host.Sandbox, snaps []*host.Snapshot) (orphaned, quarantined []string)
func (f *Fleet) ApplyChanged(node string, b *host.Sandbox, reason string)
func (f *Fleet) ApplyGone(node, name string)
func (f *Fleet) ApplyPaused(node, name, reason string)

// SetSessions installs the hang-up path so a remote pause event releases the
// gateway's attached sessions, mirroring host.Manager.SetSessions.
func (f *Fleet) SetSessions(c host.SessionCloser)

// --- placement (M2: explicit override only; the Placer body is M5) ---
type Request struct{ Owner, Image, Arch, PreferNode string; VCPUs, MemMB, DiskMB int64 }
type Candidate struct{ Name string; Facts Facts; Capacity host.NodeCapacity }
type Placer interface{ Place(req Request, nodes []Candidate) (node string, err error) }
func (f *Fleet) SetPlacer(p Placer)
// CreateOn is Create with the node chosen by the caller (ctl@ / REST --node).
func (f *Fleet) CreateOn(ctx context.Context, node, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error)

type NodeStatus struct {
    nodes.Node
    Online    bool              `json:"online"`
    Local     bool              `json:"local"`
    Capacity  host.NodeCapacity `json:"capacity"`
    Sandboxes int               `json:"sandboxes"`
    Running   int               `json:"running"`
}

// --- errors ---
func Unreachable(op, sandbox, node string) *ctlops.Error
func IsNodeUnreachable(err error) bool
```

**`Fleet.Get` is the shape everything else follows:**

```go
func (f *Fleet) Get(name string) (*host.Sandbox, bool) {
    // The local manager is consulted FIRST and its answer is authoritative. A
    // single-box deployment therefore never reads a cache and never sees a
    // stale record — the index exists for remote nodes and for name
    // reservation, not for the local read path.
    if b, ok := f.localMgr.Get(name); ok {
        return b, true
    }
    return f.remoteGet(name) // index row -> node cache -> synthetic addresses
}
```

`ArchivingEnabled`/`Snapshotter` are OR across the local node and every **online** node, documented as
"some node in this fleet can". The per-node truth surfaces when the operation is actually placed, as a
`*host.DisabledError`, which `AsError` already renders correctly. Making the interface per-sandbox would
break `*host.Manager`'s structural satisfaction for no gain.

**Placement eligibility must charge `effectiveMemMB`.** `Manager.effectiveMemMB` (manager.go:594)
returns `reserveMB` when `0 < reserveMB < memMB`, and both admission and `Capacity()` charge that, not
`MemMB`. A gateway-side filter comparing `MemMB` to `BudgetMemMB` silently over- or under-packs a node.

---

## 4. Changes to existing packages, file by file

### 4.1 `internal/reserved/reserved.go`

Current (lines 43-45, inside `var names`):

```go
	"new":     true, // ssh new@<domain>      — create a sandbox
	"ctl":     true, // ssh ctl@<domain>      — control plane
	"signup":  true, // ssh signup@<domain>   — register
```

New:

```go
	"new":     true, // ssh new@<domain>      — create a sandbox
	"ctl":     true, // ssh ctl@<domain>      — control plane
	"signup":  true, // ssh signup@<domain>   — register
	"node":    true, // ssh node@<domain>     — a node joins the fleet
```

`internal/reserved/reserved_test.go`: add `"node"` to `TestLiveDoorsAreReserved`'s literal list.
`internal/host/reserved_test.go` pins a list too — check and extend.

### 4.2 `internal/ctlops/errors.go`

Current (line 155):

```go
var notFoundMsg = map[string]string{
	"sandbox":  "no sandbox named %q",
```

New: add `"node": "no node named %q",`.

Add at the end of the file (append-only; **do not** touch the `Kind` iota block, and **do not** add a
Kind — `openapi.json` has a hard `enum` of the ten strings):

```go
// ParseKind is the inverse of Kind.String(). The string form is what crosses
// the node link precisely because the Kind constants are iota-ordered: a future
// insertion would silently renumber a released wire format. New Kinds are
// APPEND-ONLY, and adding one also means editing components.schemas.Error's
// `kind` enum in internal/restapi/openapi.json.
func ParseKind(s string) (Kind, bool)
```

### 4.3 `internal/ctlops/wire.go` (new file)

`WireError`, `WireHostError`, `ToWire`, `FromWire` exactly as section 2.8.

### 4.4 `internal/ctlops/types.go`

Current (lines 13-27): `SandboxInfo`. Add after `State`:

```go
	// Node names the machine whose driver runs this VM. It is the first
	// internal-topology field info() deliberately does NOT drop: a user needs
	// to know which machine their sandbox is on to reason about its arch, its
	// accelerators and its outages, and unlike a guest address a node name is
	// not dialable. omitempty keeps a single-box payload byte-identical.
	Node string `json:"node,omitempty"`
	// Unreachable reports that the node holding this sandbox is not answering
	// the control plane. The sandbox is very likely still running.
	Unreachable bool `json:"unreachable,omitempty"`
```

### 4.5 `internal/ctlops/ops.go`

Current (lines 335-351):

```go
// info projects a manager record onto the public shape. SSHAddr, HostIP and
// GuestV6 are dropped here rather than at the transport, so no future edge can
// serialize the host's internal topology by forgetting to.
func (o *Ops) info(b *host.Sandbox) SandboxInfo {
	si := SandboxInfo{
		Name:       b.Name,
		Owner:      b.Owner,
		State:      string(b.State),
```

New:

```go
// info projects a manager record onto the public shape. SSHAddr, HostIP and
// GuestV6 are dropped here rather than at the transport, so no future edge can
// serialize the host's internal topology by forgetting to. Node and Unreachable
// are the deliberate exceptions — see SandboxInfo.
func (o *Ops) info(b *host.Sandbox) SandboxInfo {
	si := SandboxInfo{
		Name:        b.Name,
		Owner:       b.Owner,
		State:       string(b.State),
		Node:        b.Node,
		Unreachable: b.Unreachable,
```

Also add the `NodeRoster` interface, `Config.Nodes`, `Ops.nodes`, `Capabilities.Fleet` and the three
operator-gated methods — see W15.

### 4.6 `internal/host/manager.go`

**(a) `Sandbox`** — current ends at `RenamedFrom string` (line ~209). Add:

```go
	// Node names the machine whose driver runs this VM. A node's own manager
	// writes its own name here; the gateway's Fleet overwrites it from the
	// placement ledger, which is the only authorization input.
	Node string `json:"node,omitempty"`
	// Unreachable is set ONLY by the gateway's Fleet, never by a node's manager:
	// it means the node holding this sandbox is not answering. There is
	// deliberately no fourth vmm.State — every `b.State ==` switch in host,
	// envsync, netpush and both consoles treats "not running" as "safe to
	// ignore", which is right, and a fourth value would have to be handled in
	// all of them.
	Unreachable bool `json:"unreachable,omitempty"`
```

Both are strings/bools, so `copyOf` (manager.go:1736) stays a shallow-is-deep copy.

**(b) `Snapshot`** (snapshot.go:21) — add `Node string \`json:"node,omitempty"\``.

**(c) `Manager.Create`** (line ~512) — current:

```go
	b := &Sandbox{
		Name: name, Owner: owner, Image: image, VCPUs: vcpus, MemMB: memMB,
		State: inst.State, SSHAddr: inst.SSHAddr, SSHUser: inst.SSHUser,
		HostIP: inst.HostIP, GuestV6: inst.GuestV6, CreatedAt: now, LastActive: now,
	}
```

New: add `Node: m.nodeName,`. Same in `Manager.Snapshot` for the `Snapshot` record.

**(d) `NewManager` load backfill** — after `m.load()` / `m.loadSnapshots()` and **before** the
running→paused downgrade (line ~472), insert:

```go
	// Records written before the fleet existed carry no node name. Backfill
	// them, or every pre-existing sandbox on a live box becomes unplaceable.
	for _, b := range m.boxes {
		if b.Node == "" {
			b.Node = m.nodeName
		}
		b.Unreachable = false // a gateway-only flag; never load one off disk
	}
	for _, s := range m.snaps {
		if s.Node == "" {
			s.Node = m.nodeName
		}
	}
```

**(e) `NodeCapacity`** (line 676) — add, additive and safe (it is marshal-only; no client contract test
unmarshals it):

```go
	Arch       string     `json:"arch,omitempty"`
	Release    string     `json:"release,omitempty"`
	Online     bool       `json:"online"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
```

`Options` gains `Arch string` and `Release string`; `Capacity()` fills them and sets `Online: true`
(the local node always is).

**(f) accessor** — `func (m *Manager) NodeName() string { return m.nodeName }` (no lock needed;
`nodeName` is written once in `NewManager`).

**(g) `Observer`** — new optional hook, because `m.sessions.CloseSandboxSessions` is called from exactly
**one** site (manager.go:878, the pause path), so a SessionCloser alone would emit events on pause and on
nothing else:

```go
// Observer is told about every change to a sandbox record, so a node can relay
// it to its gateway. Optional; nil on a gateway, where nothing is listening.
//
// Implementations MUST return without blocking: these fire from lifecycle
// methods that hold m.mu, and a slow observer would stall the whole manager.
type Observer interface {
	SandboxChanged(b *Sandbox, reason string)
	SandboxGone(name string)
}

func (m *Manager) SetObserver(o Observer)
```

`Options.Observer Observer` too. Fire `m.observe(b, reason)` (a nil-guarded helper that passes
`copyOf(b)`) from: `Create` ("created"), `EnsureRunning` ("resumed"), `pause` ("paused"; the reaper's
reason string distinguishes it), `Archive` ("archived"), `restore` ("restored"), `Resize` ("resized"),
`Reboot` ("rebooted"), `Rename` ("renamed"), `SetPinned` ("pinned"/"unpinned"), `balloonDown`
("ballooned"), `deflate` ("deflated"), `applyVitals`'s activity reset ("touched"), `refreshDiskUsage`'s
change branch ("disk"), and `Destroy` → `SandboxGone`.

**(h) `SetGatewayPublicKey`** — a post-construction setter in the same family as `SetEnvSync`
(manager.go:792) and `SetSessions` (manager.go:801), because a node cannot know the gateway's public
upstream key until `Welcome` arrives, while `host.NewManager` is constructed before the link is dialed:

```go
// SetGatewayPublicKey installs the authorized_keys line new guests trust. A
// gateway sets it at construction (Options.GatewayPublicKey); a node may not
// know it until its first Welcome, so this setter exists — and Create refuses
// with a *DisabledError until it is set rather than booting a VM nobody can log
// into. It takes m.mu because Create reads gwPubKey under it.
func (m *Manager) SetGatewayPublicKey(line string)
```

`Create` gains, right after the name/reserved checks:

```go
	if m.gwPubKey == "" {
		return nil, &DisabledError{Code: "no_gateway_key",
			Msg: "this node has not yet learned the gateway's key; it will once the link is up"}
	}
```

A gateway always passes `Options.GatewayPublicKey`, so this is unreachable there. The node persists the
key to `<state-dir>/gateway_upstream_key.pub` and preloads it at boot, so a node reboot during a gateway
outage still resumes its pinned VMs (design §8).

### 4.7 `internal/sshgw/gateway.go`

**(a) `NodeUser`** — after `SignupUser` (line 46):

```go
// NodeUser is the reserved SSH username a fleet node connects as:
// `ssh node@gateway sparkbox-link/1`. It is DELIBERATELY absent from
// ReservedUsers: cmd/sparkbox iterates that slice to mint a front-door IPv6 and
// publish a public DNS record per entry (main.go:410), and resolveDoor matches
// it by destination IP — so joining it would publish node.<domain> and let
// anyone reach the fleet control door by address.
const NodeUser = "node"
```

`ReservedUsers` is unchanged. Add `const authedNodeKey = "sparkbox-node"`.

**(b) narrow the manager field.** Current (line 67 / 91):

```go
type Gateway struct {
	mgr *host.Manager
...
type GatewayOptions struct {
	Manager      *host.Manager
```

New — declare the interface, retype `Gateway.mgr`, and **keep `GatewayOptions.Manager` as
`*host.Manager`** while adding a `Fleet` field, so all seven existing construction sites
(cmd/sparkbox/main.go:388, e2e_test.go:94, proxy_test.go, proxy_auth_test.go,
internal/restapi/server_test.go, internal/userconsole/console_test.go, internal/console/console_test.go,
internal/envsync/sync_test.go) compile untouched:

```go
// Sandboxes is the slice of the sandbox store the interactive `ssh
// <name>@gateway` path drives directly. It deliberately does not go through
// ctlops because it must also RecordKey and dial. *host.Manager and
// *fleet.Fleet both satisfy it structurally.
type Sandboxes interface {
	Get(name string) (*host.Sandbox, bool)
	List() []*host.Sandbox
	EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error)
	Touch(name string)
	RecordKey(name, fp string)
}

// Dialer opens the raw connection to a guest port. It is net.Dialer.DialContext's
// shape. Nil means net.Dial, which is what a single-box deployment has always
// done.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

type Gateway struct {
	mgr  Sandboxes
	dial Dialer
	...
}

type GatewayOptions struct {
	Manager *host.Manager // the local machine; used for opsConfig's fallback
	// Fleet, if set, replaces Manager on every sandbox lookup and resume so a
	// sandbox on another machine is indistinguishable from a local one.
	Fleet Sandboxes
	// Dial, if set, routes upstream connections through the fleet.
	Dial Dialer
	...
}
```

In `New`: `g.mgr = opts.Manager` then `if opts.Fleet != nil { g.mgr = opts.Fleet }` (through the non-nil
guard — a typed-nil in an interface field is the trap `opsConfig`'s comment at gateway.go:158-166
already documents). `opsConfig` still reads `opts.Manager`.

**(c) `PublicKeyHandler`** (lines 201-216) — insert a third branch **after** the users lookup (so a key
that is both a user key and a node key resolves as its user) and before the signup exception:

```go
			if g.nodes != nil && g.isNodeDoor(ctx.User(), ctx.LocalAddr()) {
				if n, ok := g.nodes.Lookup(key); ok {
					// Status is checked in handleNodeLink, not here: a pending
					// node must be able to be told it is pending.
					ctx.SetValue(authedNodeKey, n)
					return true
				}
				// An unknown key at the node door may enrol and do nothing else,
				// exactly as an unknown key at the signup door may register and
				// nothing else.
				return g.nodeEnrol
			}
```

`isNodeDoor(user string, local net.Addr) bool` mirrors `isSignupDoor` (gateway.go:379) but returns
**false** for any front-door address — a node never dials a front door:

```go
func (g *Gateway) isNodeDoor(user string, local net.Addr) bool {
	if _, _, inRange := g.resolveDoor(local); inRange {
		return false
	}
	return user == NodeUser
}
```

**(d) `handle` dispatch** (lines 244-257) — insert between the `ControlUser` branch and the `user == ""`
guard. It **must** precede that guard, because a node has no handle:

```go
	if sandboxName == ControlUser {
		g.handleControl(s, user, log)
		return
	}
	if sandboxName == NodeUser && g.nodes != nil {
		g.handleNodeLink(s, log)
		return
	}
	// Only the signup door admits an unregistered key; anything else reaching
	// here without a handle is a bug rather than a user error.
	if user == "" {
```

**(e) `DialUpstreamVia`** — current body of `DialUpstream` (lines 419-447) moves in wholesale, with two
fixes:

```go
// DialUpstreamVia is DialUpstream with the TCP dial supplied by the caller, so
// a sandbox on another machine is reached through the fleet's reverse tunnel
// instead of the host network. dial is called afresh on every retry; nil means
// net.DialTimeout.
//
// The gateway used to provision every VM and own the only route to it, which is
// why the host key is ignored. With a node in the path that node can MITM its
// own guests; design §8 accepts that (a node lying can only affect its own
// sandboxes), but the premise is no longer "there is no other route".
func DialUpstreamVia(ctx context.Context, dial Dialer, addr, user string, key xssh.Signer) (*xssh.Client, error) {
	cfg := &xssh.ClientConfig{ /* unchanged */ }
	if dial == nil {
		dial = (&net.Dialer{Timeout: cfg.Timeout}).DialContext
	}
	var lastErr error
	for {
		conn, err := dial(ctx, "tcp", addr)
		if err == nil {
			// ClientConfig.Timeout bounds ssh.Dial's own net.DialTimeout, NOT
			// NewClientConn — so a peer that accepts and then stalls hangs the
			// handshake forever, ignoring ctx. xterm's 30s budget (ws.go:59)
			// and envsync's 3min (sync.go:58) cancel only the retry select, not
			// a blocked handshake. Bound it explicitly.
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			c, chans, reqs, err := xssh.NewClientConn(conn, addr, cfg)
			if err == nil {
				_ = conn.SetDeadline(time.Time{})
				return xssh.NewClient(c, chans, reqs), nil
			}
			conn.Close()
			lastErr = err
		} else {
			lastErr = err
			// The retry loop exists because a freshly booted guest's sshd is not
			// up yet — node-local reasoning. A node that refused the channel is
			// a fast, typed "no"; hammering it for the full 15s dial budget
			// helps nobody.
			var oce *xssh.OpenChannelError
			if errors.As(err, &oce) {
				return nil, fmt.Errorf("vm ssh not reachable: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("vm ssh not reachable: %w", lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// DialUpstream is DialUpstreamVia over the host network. Kept so
// internal/xterm and internal/envsync compile unchanged.
func DialUpstream(ctx context.Context, addr, user string, key xssh.Signer) (*xssh.Client, error) {
	return DialUpstreamVia(ctx, nil, addr, user, key)
}
```

`g.dialUpstream` (line 409) becomes `return DialUpstreamVia(ctx, g.dial, addr, user, g.upstreamKey)`.
Both call sites (gateway.go:332 and runner.go:25) are unchanged.

### 4.8 `internal/sshgw/nodedoor.go` (new file)

```go
// NodeRoster is the fleet's node registry as the SSH door needs it. Nil leaves
// the node@ door shut, which is a single-box deployment.
type NodeRoster interface {
	Lookup(key gssh.PublicKey) (nodes.Node, bool)
	Enroll(name string, key gssh.PublicKey) (nodes.Node, error)
	Seen(name, arch, release string, at time.Time) error
}

// NodeJoiner is the fleet's half of the door: everything past authentication.
type NodeJoiner interface {
	// ServeLink owns the session for the life of the link. node is the
	// AUTHENTICATED roster name.
	ServeLink(ctx context.Context, node string, s gssh.Session, conn gossh.Conn, hello nodelink.Hello) error
	// Welcome builds the reply for an approved node.
	Welcome(node string) nodelink.Welcome
}

func (g *Gateway) handleNodeLink(s gssh.Session, log *slog.Logger)
```

`handleNodeLink`:

1. require `s.Command()` == `["sparkbox-link/1"]`, else a sentence on stderr + `Exit(2)`;
2. recover `conn, _ := s.Context().Value(gssh.ContextKeyConn).(gossh.Conn)` — the only route to
   `OpenChannel`; nil is a refusal;
3. read the first frame with a 10s deadline; require `Type == "hello"`;
4. roster policy: unknown key → `Enroll` (refusing past `MaxPending` or when `--no-node-enrol`) and
   reply `node_pending`; `pending`/`disabled` → the matching refusal; name mismatch → `node_name_mismatch`;
5. `Seen(...)`, then `g.fleetNodes.ServeLink(...)`, then `Exit(0)`/`Exit(1)`.

### 4.9 `internal/proxy/proxy.go`

**(a)** Delete the package-level `var upstreamTransport` (lines 73-80) and replace with a constructor,
preserving every tuned field and its comment verbatim:

```go
// newUpstreamTransport dials guest apps. It is deliberately not
// http.DefaultTransport: [existing comment kept verbatim]
//
// It must stay a concrete *http.Transport and must never be wrapped in a
// RoundTripper: httputil.ReverseProxy's 101-upgrade path requires the response
// Body to implement io.ReadWriteCloser, and only *http.Transport returns one —
// a wrapper turns every WebSocket into "101 switching protocols response with
// non-writable body".
func newUpstreamTransport(dial Dialer) *http.Transport {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	return &http.Transport{
		DialContext:           dial,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}
}
```

**(b)** `Server.mgr` (line 98) changes type; `New`'s first parameter changes type. **No Config-struct
rewrite** — that would break the positional call sites at proxy_test.go:73 and proxy_auth_test.go:83:

```go
// Resumer is the one manager method the edge drives. *host.Manager and
// *fleet.Fleet both satisfy it.
type Resumer interface {
	EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error)
}

// Dialer is net.Dialer.DialContext's shape; see SetDialer.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

func New(mgr Resumer, store *routes.Store, domain string, log *slog.Logger) *Server
```

`Server` gains `transport *http.Transport`, set in `New` to `newUpstreamTransport(nil)`, and
`s.rp.Transport = s.transport` replaces `Transport: upstreamTransport` (line 294).

```go
// SetDialer routes upstream connections through the fleet. It rebuilds the
// transport, so it must be called before the server starts serving.
func (s *Server) SetDialer(d Dialer) { s.transport = newUpstreamTransport(d); s.rp.Transport = s.transport }
```

**(c)** target host (line 468). Current:

```go
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(box.HostIP, strconv.Itoa(port))}
```

Unchanged in code — but `box.HostIP` is already `<name>.<node>.sandbox.invalid` for a remote record, so
the dial key and the connection-pool key become node-unique automatically. Add the comment explaining
why that matters (identical guest IPs across nodes + `MaxIdleConnsPerHost: 64`) and note that the guest
never sees it because `pr.Out.Host = pr.In.Host` (line 309) and `SetXForwarded()` derives from `pr.In`.

**(d)** `ErrorHandler` (line 358) and the `EnsureRunning` failure branch (line 457) gain a
node-unreachable case: a 503 "The machine hosting this sandbox is offline" instead of the 502 "Nothing is
listening on port %d". Name the node **only** when the request carried an authenticated identity (the
`identityKey` context value) — a public route is served to strangers and the node name is fleet topology.
Detect with `fleet.IsNodeUnreachable(err)` or `errors.As(err, &*xssh.OpenChannelError{})`.

### 4.10 `internal/xterm/xterm.go`, `internal/xterm/pty.go`

`Config` gains:

```go
	// Dial routes the guest connection through the fleet. Nil dials box.SSHAddr
	// directly, which is what a single-box deployment and every unit test want
	// — so it is deliberately NOT on New's panic-on-missing list.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
```

`Handler` gains the matching `dial` field. `dialPTY` (pty.go:60) changes its first statement only:

```go
	client, err := sshgw.DialUpstreamVia(ctx, h.dial, box.SSHAddr, box.SSHUser, h.upstreamKey)
```

Nothing below pty.go:63 moves. `h.open = h.dialPTY` stays the test seam (bridge_test.go:136 replaces it
wholesale and is unaffected). `Attacher` (xterm.go:53) needs **no** change — `*fleet.Fleet` satisfies it
for free, being a strict subset of `ctlops.Sandboxes`.

`const dialTimeout = 30 * time.Second` (ws.go:59) now bounds channel open + node→guest TCP + SSH
handshake. Leave the number; add a comment saying it is the fleet stream budget.

### 4.11 `internal/envsync/sync.go`

`New`'s second parameter widens (all call sites pass `*host.Manager` and keep compiling):

```go
// Lister is the one manager method the syncer drives.
type Lister interface{ List() []*host.Sandbox }

func New(store *secrets.Store, mgr Lister, upstreamKey xssh.Signer, log *slog.Logger) *Syncer

// SetDialer routes secret delivery through the fleet, so a sandbox on another
// machine receives its environment. Post-construction, matching the
// mgr.SetEnvSync idiom, so the constructor's signature never moves.
func (s *Syncer) SetDialer(d sshgw.Dialer)
```

`deliverBlock` (sync.go:258) becomes `sshgw.DialUpstreamVia(ctx, s.dial, box.SSHAddr, box.SSHUser,
s.upstreamKey)`. The `context.AfterFunc(ctx, client.Close)` bound at sync.go:266 still works over a
tunneled conn because `Close` propagates to the channel.

### 4.12 `internal/console/console.go`, `internal/userconsole/console.go`

Both dial `b.HostIP` directly with `net.DialTimeout` for the route-listening badge (console.go:220,
userconsole/console.go:288). Route both through a settable dialer defaulting to `net.DialTimeout`, and
make `probeTimeout` (300ms in both, console.go:51 / userconsole/console.go:54) a field defaulting to
300ms — a tunneled dial is slower. Left alone these are a silent **wrong answer**, not a crash: with
overlapping 172.30 space the gateway would probe its own local sandbox at the same address and render a
false-positive green badge for a remote route.

`console.cluster` (console.go:269-274) — current:

```go
	writeJSON(w, http.StatusOK, clusterResponse{
		Domain: h.domain,
		Nodes:  []host.NodeCapacity{h.mgr.Capacity()},
	})
```

New: `Nodes: h.capacities()`, where `capacities` is a settable func defaulting to the one-element
literal, set by main to `flt.Capacities`. The SPA already does `nodes.reduce` over the array
(index.html:351-404), so the four capacity cards aggregate fleet-wide with **zero** JS change; only the
per-sandbox table needs a `Node` column (index.html:214-215 thead, `render()` at :424). Both consoles
embed `*host.Sandbox`, so `node` and `unreachable` land in their JSON for free.

### 4.13 `internal/restapi/openapi.json`

`components.schemas.Sandbox.properties` gains (NOT in `required` — both are `omitempty`):

```json
"node": {
  "type": "string",
  "description": "The machine this sandbox runs on.",
  "example": "local"
},
"unreachable": {
  "type": "boolean",
  "description": "The machine holding this sandbox is not answering the control plane right now. It is very likely still running."
}
```

The file is hand-authored, `//go:embed`ed and parsed at package init (openapi.go:26-31), so a malformed
edit **panics the binary at startup**, not at request time. `"local"` is not a URL, so
`TestSpecOnlyNamesThePlaceholderDomain` (openapi_test.go:213) is satisfied.

W15 additionally adds `components.schemas.Capabilities.properties.fleet` (and to its `required` list,
which currently names all seven), `components.schemas.Node`, `components.schemas.CreateRequest.node`, and
the three `/v1/nodes*` paths.

### 4.14 `cmd/sparkbox/main.go`

**(a) name collision.** `type fleet struct{ mgr *host.Manager }` (line 735) is the netpush adapter and
would shadow the imported package. Rename it and its use at line 357 to `netpushFleet`. It must stay
**node-local**: sluice runs per node and feeds guest IPs to a node-local eBPF enforcer, and
`PUT /policy` is a full replace of the per-tap set.

**(b) new flags** in the `serve` flag block (lines 90-138):

```go
nodeNameFlag = fs.String("node-name", "", "name this machine reports to the fleet (default: hostname)")
archFlag     = fs.String("arch", runtime.GOARCH, "CPU architecture this machine reports to the fleet")
gatewayAddr  = fs.String("gateway", os.Getenv("SPARKBOX_GATEWAY"), "run as a fleet NODE and link to this gateway (host:port). The only env-var-defaulted flag, because a node's provisioning unit carries it as an environment line rather than a flag bundle")
gatewayPub   = fs.String("gateway-pubkey", "", "node mode: the gateway's PUBLIC upstream authorized_keys line (or a path to it). Cached from the first Welcome when omitted")
gatewayHostK = fs.String("gateway-host-key", "", "node mode: pin the gateway's SSH host key (path or authorized_keys line); empty trusts on first use")
noNodeEnrol  = fs.Bool("no-node-enrol", false, "gateway mode: refuse enrolment of unknown node keys at the node@ door")
```

**(c) precondition** (lines 142-144) — current:

```go
	if *usersPath == "" {
		return errors.New("--users is required")
	}
```

New:

```go
	if *usersPath == "" && *gatewayAddr == "" {
		return errors.New("--users is required")
	}
```

and immediately after the log handler is built:

```go
	if *gatewayAddr != "" {
		return serveNode(nodeOptions{ /* ... */ })
	}
```

**(d) node name** (line 262) — current `nodeName, _ := os.Hostname()`. New: the flag when non-empty,
else the hostname, else `"local"`. **Do not** rename production nodes to `local`: the same string is the
`box` claim in every issued id token (metadata/server.go:265) and is externally observable.

**(e) the six substitutions**, each through a non-nil guard:

| line | current | new |
|---|---|---|
| 246 area | — | `placeStore, err := placement.Open(filepath.Join(*stateDir, "sparkbox.db"))`; `nodeStore, err := nodes.Open(same)` |
| after 338 | — | `flt, err := fleet.New(fleet.Options{Local: mgr, LocalName: nodeName, LocalArch: *archFlag, Index: placeStore, Log: log})` |
| 346 | `envsync.New(secretsStore, mgr, upstreamKey, log)` | same, then `syncer.SetDialer(flt.DialContext)` |
| 357 | `netpush.NewSyncer(sluiceClient, fleet{mgr}, …)` | `netpush.NewSyncer(sluiceClient, netpushFleet{mgr}, …)` — **stays on `mgr`** |
| 379 | `Sandboxes: mgr, Templates: mgr` | `Sandboxes: flt, Templates: flt`, plus `Nodes: flt` |
| 389 | `Manager: mgr, …` | `Manager: mgr, Fleet: flt, Dial: flt.DialContext, Nodes: nodeStore, NodeJoiner: flt, NodeEnrol: !*noNodeEnrol` |
| 399 | `mgr.SetSessions(gw)` | plus `flt.SetSessions(gw)` — still exactly **one** registry |
| 437 | `api.New(mgr, …)` | **unchanged**: the legacy API is unauthenticated, loopback-bound, and its package doc forbids growth |
| 449 | `metadata.New{Manager: mgr, …}` | **unchanged**: `GetByHostIP` must stay node-local, or node B's guest is attributed to node A's sandbox at the same `172.30.<idx>.2` |
| 487 | `proxy.New(mgr, …)` | `px := proxy.New(flt, routeStore, *proxyDomain, log)`; `px.SetDialer(flt.DialContext)` |
| 556 | `xterm.Config{Sandboxes: mgr, …}` | `Sandboxes: flt, Dial: flt.DialContext` |
| console | `console.New(… mgr …)` | plus `ch.SetCapacities(flt.Capacities)` and `ch.SetDialer(flt.DialContext)` |

**(f)** extend `warnSubdomainCollision`'s startup pass (main.go:687) to warn if an existing sandbox or
handle is literally named `node`, since `reserved.Name` runs only at create/rename time.

### 4.15 `cmd/sparkbox/node.go` (new file)

`serveNode` is a **new function**, not a branch threaded through `serve()`'s 550 lines.

Keeps: the state dir, the node key (`sshgw.LoadOrCreateKey(keysIn, "node_key")`, **unconditionally** —
`--require-keys` must not switch it or a node can never bootstrap), the vmm driver exactly as `serve`
builds it, `host.NewManager` with `Routes`/`Schedules`/`Tags`/`Archive`/`FrontDoor` all nil and
`GatewayPublicKey` from `--gateway-pubkey` or the cached
`<state-dir>/gateway_upstream_key.pub`, `Observer` = the nodelink emitter, `SetSessions` = the same
emitter, `mgr.ResumePinned(ctx)`, `go mgr.RunReaper(...)`, netpush with a `Rules` stub returning
`governed=false`, and `go nodelink.RunClient(ctx, ...)` writing only to its own log.

Skips: `users.Open`/`SeedFile`, the OIDC key load (critical — `--require-keys` swaps
`LoadOrCreateKey`→`LoadKey` globally at main.go:158-163 and would hard-fail on an
`oidc_signing_key.pem` a node must never hold), `edgeauth.NewSigner`, `secrets.Open`, `netrules.Open`,
`routes.Open`, `schedule.Open`, the frontdoor plumber/publisher, `ctlops.New`, `sshgw.New`, the SSH
server, the legacy api server, dnsedge, both consoles, restapi, xterm, and the whole proxy edge
(main.go:486-640). The metadata listener stays **off** in node mode until M4, with a one-line warning.

### 4.16 `internal/hostsetup` and the unit templates

`hostsetup.Config` gains `Gateway string`. `renderEnvFile` (steps.go:534) emits `GATEWAY_FLAG=` and both
`deploy/units/sparkbox-standalone.service.tmpl` and the hand-maintained `deploy/sparkbox.service`
reference it as **`$GATEWAY_FLAG` unbraced** — systemd turns an empty `${VAR}` into one empty argument,
which terminates Go's flag parsing and silently drops every flag after it (the warning is already in both
units). `stepUsersConf` (steps.go:317-347, 508-529) hard-fails without an operator key and must be
skipped when `Gateway` is set.

---

## 5. Ordered work items

Parallel groups: items sharing a `parallel_group` letter touch disjoint files and may be done
concurrently once their dependencies are met.

---

### W1 — [M0 · spike] `ctlops` wire projection: typed errors survive the hop
**Group A** · deps: none
**Files:** `internal/ctlops/wire.go` (new), `internal/ctlops/wire_test.go` (new), `internal/ctlops/errors.go`

Add `ParseKind` (append-only; **do not** touch the iota block, **do not** add a `Kind` —
`openapi.json` has a hard enum of the ten strings) and `"node": "no node named %q"` to `notFoundMsg`.
New `wire.go` with `WireError`, `WireHostError`, `ToWire(op, err)`, `FromWire(w)` exactly as §2.8.
`ToWire` calls `AsError(op, err)` first, then copies every field except `Err` (logged node-side, never
rendered to a client — the proxy's rule), then walks `errors.As` for each of the seven concrete
`internal/host` types to fill `Host`. `FromWire` rebuilds the concrete host value and assigns it to
`Error.Err`, and always returns a **fresh** `*Error`.

**Acceptance:** `go test ./internal/ctlops/ -run TestWire -race`. A table test enumerates every error
`AsError` classifies — all seven host types (Limit, Capacity, DiskQuota, Missing, State, Disabled, Name
across all three `NameProblem` values), the five `users`/`schedule` sentinels, `context.Canceled`,
`context.DeadlineExceeded`, and one each of `NotFound`/`Invalid`/`Disabled`/`Denied` — and asserts after
`ToWire → json.Marshal → json.Unmarshal → FromWire`: `Kind`, `Op`, `Code`, `Msg`, `Hint`, `Verbatim`,
`ExitCode()` and `HTTPStatus()` all equal, and `errors.As` back to the original concrete host type
succeeds with equal field values (specifically `LimitError.Max`/`.Running` and
`CapacityError.UsedMB`/`.BudgetMB`, which `sshgw.failStart` dereferences without nil guards). A second
test asserts `ParseKind(k.String())` round-trips all ten kinds and that an unknown string returns
`(KindInternal, false)`.

---

### W2 — [M0 · spike] `internal/nodelink` transport, wired to nothing
**Group B** · deps: W1
**Files:** `internal/nodelink/doc.go`, `frame.go`, `conn.go`, `stream.go` (all new), `conn_test.go`,
`stream_test.go` (new)

The whole transport as **dead code no production file imports**. `frame.go`: constants + `Frame` +
the codec (a `bufio.Scanner` with a `MaxFrameBytes` buffer; a mutex-guarded `json.Encoder` with
`SetEscapeHTML(false)`). `conn.go`: `Conn` with one reader goroutine, a `pending map[string]chan *Frame`,
side-prefixed monotonic IDs, `Request`/`Event`/`Handle`/`OnEvent`/`Serve`/`Fail`/`Close`, the
deadline-from-process-context rule, and `cancel` emission on ctx cancellation. `stream.go`: `StreamOpen`
(`gossh.Marshal`), `OpenStream`, `ServeStreams` (dedicated accept goroutine, one goroutine per open,
`MaxLiveStreams` cap, the reject table), and `streamConn` (close-on-expiry deadlines, `fleetAddr`).

**Acceptance:** `go test ./internal/nodelink/ -race`, using a **real** loopback pair (a `gssh.Server` on
`127.0.0.1:0` plus `xssh.Dial` from the same process, node side serving
`client.HandleChannelOpen(StreamChannelType)`), with the control session live throughout:

1. **HOL proof.** Copy 8 MiB each way over three concurrent streams while sending a `ping` request every
   100 ms; assert every ping replies within 200 ms and no ping is lost. Separately open 40 streams
   concurrently (past `chanSize`=16) and assert the heartbeat cadence is unaffected — this is the
   `mux.go:330` blocking-send hazard.
2. `http.Transport{DialContext: <stream opener>}` does a 200 GET against an `httptest.Server` on the
   node side **and** a 101 upgrade with bidirectional bytes after the upgrade.
3. `xssh.NewClientConn` handshakes over a stream and runs a command against a `mock.Driver` guest.
4. `SetDeadline` on a stalled stream unblocks `Read` (the close-on-expiry contract).
5. A pinned assertion that `net/http`'s client transport sets no conn deadline: drive 20 sequential
   keep-alive requests over one pooled stream and assert exactly one dial and zero deadline calls
   (count them in the `streamConn`).
6. `srv.Close()` on the gateway makes the node's serve loop return rather than hang.
7. 200 concurrent `Request` round-trips with no ID collisions, under `-race`.
8. A 2 MiB line closes the link with a bounded error, not an OOM.

If (1) fails, stop and escalate: the mitigation is a second SSH connection per node dedicated to data,
which is small here and enormous at W21.

---

### W3 — [M0] Reserve the name `node`; add `sshgw.NodeUser`
**Group A** · deps: none
**Files:** `internal/reserved/reserved.go`, `internal/reserved/reserved_test.go`,
`internal/host/reserved_test.go`, `internal/sshgw/gateway.go`, `cmd/sparkbox/main.go`

As §4.1 and §4.7(a). `NodeUser` is **not** appended to `ReservedUsers`. Extend
`warnSubdomainCollision`'s startup pass to warn about an existing sandbox named `node`.

**Before merging on a live box:** run `ssh ctl@<domain> list` and check for a sandbox literally named
`node` — validation is create/rename-time only, so an existing one keeps working and would shadow the
door.

**Acceptance:** `go test ./internal/reserved/ ./internal/host/ ./internal/sshgw/`; a new assertion that
`reserved.Name("node")` is true, that `routes.ValidSubdomain("node")` is false, and that `NodeUser` is
absent from `sshgw.ReservedUsers` (so no front door is minted for it).

---

### W4 — [M0] `Node`/`Unreachable` on host records; arch/release/online on `NodeCapacity`
**Group A** · deps: none
**Files:** `internal/host/manager.go`, `internal/host/snapshot.go`, `internal/host/node_test.go` (new)

§4.6 (a)–(f).

**Acceptance:** `go test ./internal/host/`. `TestLoadBackfillsNodeName` writes a `sandboxes.json` with no
`node` key, opens a `Manager` with `NodeName: "nodeb"`, and asserts every record comes back with
`Node == "nodeb"` and `Unreachable == false`. `TestCreateStampsNode` asserts a fresh create carries it.
`TestCapacityReportsArch` asserts `Capacity().Arch`/`.Online`.

---

### W5 — [M0] `host.Observer` and `SetGatewayPublicKey`
**Group B** · deps: W4
**Files:** `internal/host/manager.go`, `internal/host/observer_test.go` (new)

§4.6 (g)–(h). Note `m.sessions.CloseSandboxSessions` fires from exactly one site (manager.go:878), which
is why the Observer is a separate hook and not a reuse of `SessionCloser`. The observe helper passes
`copyOf(b)` and must not block.

**Acceptance:** `go test ./internal/host/ -race`. A recording observer over a mock-driver manager sees
exactly one event per transition for create/resume/pause/resize/reboot/rename/pin/unpin/archive/
restore/destroy, with the expected `reason`. A separate test asserts `Create` returns a
`*host.DisabledError{Code:"no_gateway_key"}` before `SetGatewayPublicKey` and succeeds after, and that
`SetGatewayPublicKey` under `-race` against a concurrent `Create` is clean.

---

### W6 — [M0] `node`/`unreachable` in the REST sandbox shape
**Group B** · deps: W4
**Files:** `internal/ctlops/types.go`, `internal/ctlops/ops.go`, `internal/restapi/openapi.json`

§4.4, §4.5 (the `info` half only), §4.13 (the `Sandbox` schema only).

**Acceptance:** `go test ./internal/ctlops/ ./internal/restapi/` — in particular
`TestEveryRefResolves`, `TestSpecDescribesExactlyTheRoutes` and
`TestSpecOnlyNamesThePlaceholderDomain`. `omitempty` means a single-box payload is byte-identical until
W11 starts stamping a node name; assert that with a golden JSON comparison on `ops.info` output for a
record with an empty `Node`.

---

### W7 — [M0] `internal/placement`
**Group A** · deps: none
**Files:** `internal/placement/store.go`, `internal/placement/store_test.go` (new)

§3.1. Copy `internal/routes/store.go:80-150` structurally, including the private `addColumnIfMissing`.

**Acceptance:** `go test ./internal/placement/ -race`. `TestReserveIsAtomic` launches 64 goroutines
reserving one name against one `Store` and asserts exactly one nil error and 63 `ErrTaken`.
`TestReleaseThenReserve`, `TestRenameRefusesTakenTarget` (and leaves both rows intact),
`TestReopenIsANoOp`, and `TestSharesTheDatabase` opens `routes.Store` + `users.Store` +
`placement.Store` on one file and writes to all three concurrently under `-race`.

---

### W8 — [M0] `internal/nodes`
**Group A** · deps: none
**Files:** `internal/nodes/store.go`, `internal/nodes/store_test.go` (new)

§3.2. Same template as W7.

**Acceptance:** `go test ./internal/nodes/ -race`. Enrolling twice with the same key is idempotent and
stays pending; enrolling a second key under a taken name returns `ErrNameTaken`; the 33rd pending
enrolment returns `ErrTooManyPending`; `Approve` flips status and stamps `approved_by`; a `disabled`
row's `Lookup` still returns the row (status is the caller's decision, not the store's); `Lookup` keys on
`base64(key.Marshal())`, not the fingerprint.

---

### W9a — [M0] `sshgw` seam: `Sandboxes` interface, `Dialer`, `DialUpstreamVia`
**Group C** · deps: W3
**Files:** `internal/sshgw/gateway.go`, `internal/sshgw/runner.go`

§4.7 (b) and (e). `GatewayOptions.Manager` keeps its `*host.Manager` type; a new `Fleet Sandboxes` field
takes precedence when non-nil. `g.dialUpstream` routes through `g.dial`.

**Acceptance:** `go build ./... && go test ./internal/sshgw/ ./...` with **zero test-file edits** — that
is the criterion. If any existing test needed changing, the seam was widened wrongly. Plus one new test:
`DialUpstreamVia` with an injected dialer that accepts and then stalls returns within the ctx deadline
instead of hanging (the `NewClientConn`-is-not-bounded fix), and one that returns an
`*xssh.OpenChannelError` is **not** retried.

---

### W9b — [M0] `proxy` seam: `Resumer`, per-server transport, `SetDialer`
**Group C** · deps: none
**Files:** `internal/proxy/proxy.go`, `internal/proxy/upstream_test.go` (new)

§4.9 (a)–(c). No Config-struct rewrite.

**Acceptance:** `go test ./internal/proxy/ ./...` with no edits to `proxy_test.go`,
`proxy_stream_test.go` or `proxy_auth_test.go`. New `TestUpstreamPoolIsPerSandbox`: two backends, two
sandboxes whose `HostIP` differs only by the synthetic host name, interleaved keep-alive requests, each
response must come from the correct backend. (This test fails against a shared package-level transport
keyed on a shared address, which is the point.)

---

### W9c — [M0] `xterm`, `envsync` and both consoles: dialer fields
**Group C** · deps: W9a
**Files:** `internal/xterm/xterm.go`, `internal/xterm/pty.go`, `internal/envsync/sync.go`,
`internal/console/console.go`, `internal/userconsole/console.go`

§4.10, §4.11, §4.12 (the probe dialer and `probeTimeout` halves; `cluster` moves in W11).

**Acceptance:** `go test ./internal/xterm/ ./internal/envsync/ ./internal/console/
./internal/userconsole/ ./...` with no test-file edits. `xterm.New` must **not** panic on a nil `Dial`.

---

### W10 — [M0] `internal/fleet` core: `Node`, `LocalNode`, `Fleet`, synthetic addressing, `DialContext`
**Group D** · deps: W4, W6, W7, W9a, W9b
**Files:** `internal/fleet/node.go`, `local.go`, `fleet.go`, `dial.go`, `errors.go`,
`fleet_test.go`, `parity_test.go` (all new)

§3.4. At this point there are no remote nodes: `Attach` exists but nothing calls it, and every method
resolves to the local node. `Create` order is: choose node (always local here) → `index.Reserve(...)`,
translating `ErrTaken` into `&host.NameError{Problem: host.NameTaken, Noun:"sandbox", Name:name}` so
`AsError` classifies it exactly as today → `node.Create` → `index.Release` on failure. `Destroy` releases
after the node succeeds. `Rename` renames the ledger row **first** (a crash must not strand the name) and
rolls it back on node failure. `New` runs a boot reconcile: insert a row for every local sandbox with
none, and release local-node rows the local manager no longer has (the local manager is the truth for
local). `Unreachable`/`IsNodeUnreachable` in `errors.go`.

**Acceptance:** `go test ./internal/fleet/ -race`. `parity_test.go` is the M0 safety net: a Fleet over a
real mock-driver `*host.Manager`, with `Index` both nil and set, must return results deep-equal to the
bare manager's for `Get`/`List`/`ListByOwner`/`Snapshots` after **every step** of a
create/pause/resume/rename/destroy matrix. `fleet_test.go` covers the reservation rollback, the
double-create `NameTaken`, the boot reconcile dropping a stranded row, and that `Get` on a 5000-row index
returns in microseconds (no network, no lock contention). The `var _ ctlops.Sandboxes = (*Fleet)(nil)`
block compiles.

---

### W11 — [M0] Wire the Fleet into `cmd/sparkbox`; node column in the operator console
**Group E** · deps: W10, W9c, W3
**Files:** `cmd/sparkbox/main.go`, `internal/console/console.go`, `internal/console/index.html`,
`internal/userconsole/index.html`, `internal/console/console_test.go`

§4.14 (a)–(f) minus the node-mode branch (that is W14), plus `console.cluster` → `flt.Capacities`.
Single-box behaviour must be **identical** after this item.

**Acceptance:** `go build ./... && go vet ./... && go test ./...`, with no test edits beyond
`console_test.go`'s cluster assertion. Manual single-box smoke on `--driver mock`: `ssh new@`,
`ssh ctl@ list` (golden output byte-identical), an HTTP route, and a browser terminal all behave exactly
as before, with `node` now reporting the hostname.

---

### W12 — [M1] The `node@` door: auth branch, `handleNodeLink`, `nodelink.Serve`, `Fleet.Attach`
**Group F** · deps: W2, W8, W11
**Files:** `internal/sshgw/gateway.go`, `internal/sshgw/nodedoor.go` (new),
`internal/nodelink/server.go` (new), `internal/fleet/link.go` (new),
`internal/sshgw/nodedoor_test.go` (new), `cmd/sparkbox/main.go`

§4.7 (c)–(d), §4.8, plus `nodelink.Serve` (handshake, hooks dispatch, `Client`) and `Fleet.ServeLink` /
`Fleet.Welcome` / `Fleet.Attach` / `Fleet.Nodes`. At M1 the Fleet still serves **only local rows** to
ctlops: `Remote` exists but `Get`/`List`/`ListByOwner` ignore remote index rows. A connected node is
observable through `Fleet.Nodes()` and `Capacities()` and nothing else — the doc's "observability only".
New-link-wins supersession lives here.

**Acceptance:** `go test ./internal/sshgw/ ./internal/nodelink/ -race`. An unknown key at `node@` enrols
once and gets `node_pending`; a second connection from the same key does not create a second row; an
unknown key at any **other** username is still refused exactly as today (`TestOwnershipAndAuth` must not
weaken, and the failure shape must be indistinguishable); an approved key completes hello→welcome→
inventory and appears in `Fleet.Capacities()`; a second link for the same name supersedes the first with
`bye{superseded}`; killing the link marks the node offline within the grace period and leaves every
placement row intact; the gateway process does not exit. Plus a guard test that `gw.Server("")` sets no
`IdleTimeout` and no `MaxTimeout`.

---

### W13 — [M1] The node-side client: dial, TOFU pin, reconnect, emitter
**Group F** · deps: W12, W5
**Files:** `internal/nodelink/client.go`, `internal/nodelink/emitter.go` (new),
`internal/nodelink/client_test.go` (new)

`RunClient` as §3.3, including the `keepalive@openssh.com` `SendRequest` on the heartbeat cadence (the
only half-open detector on the node side) and the **dedicated** stream accept goroutine (registered here
even though `ServeStreams`' resolver rejects everything until W21). Backoff 1s→60s ±20% jitter. Logs
`node_pending` at Info with the exact `ssh ctl@<gw> node approve <SHA256:...>` command, not at Error — a
gateway restart is routine. `Emitter` installs as both `host.Observer` and `host.SessionCloser`.

**Acceptance:** `go test ./internal/nodelink/ -race`. A real gateway (the sshgw node door on a loopback
listener) and a real client in one process: the handshake completes, capacity heartbeats arrive, killing
the listener causes reconnect with backoff, `RunClient` returns only when ctx is cancelled, and the
emitter's `CloseSandboxSessions` returns immediately (mirroring
`sshgw/livesessions_test.go:TestCloseSandboxSessionsDoesNotBlock`) and drops rather than blocks when the
queue is full.

---

### W14 — [M1] Node mode in `cmd/sparkbox`
**Group G** · deps: W13
**Files:** `cmd/sparkbox/node.go` (new), `cmd/sparkbox/main.go`, `internal/hostsetup/config.go`,
`internal/hostsetup/steps.go`, `deploy/units/sparkbox-standalone.service.tmpl`, `deploy/sparkbox.service`

§4.14 (b)–(c), §4.15, §4.16.

**Acceptance:** `go build ./...`.
`sparkbox serve --driver mock --gateway 127.0.0.1:2222 --state-dir /tmp/n1` starts, logs
`node_pending` with the approve command, and creates **no** users/secrets/routes tables in
`/tmp/n1/sparkbox.db` (assert by inspecting the file). `sparkbox serve` with no `--gateway` is
byte-identical to today and still requires `--users`. A test asserts the link supervisor never writes to
`errCh`: kill the gateway listener and assert the node process's `serve` has not returned after 3s.

---

### W15 — [M1] `ctl@ node ls|approve|rm` and `GET /v1/nodes`
**Group G** · deps: W12
**Files:** `internal/ctlops/node.go` (new), `internal/ctlops/ops.go`, `internal/ctlops/types.go`,
`internal/ctlops/ownership_test.go`, `internal/ctlops/fakes_test.go`, `internal/sshgw/control.go`,
`internal/sshgw/controlnode.go` (new), `internal/sshgw/control_golden_test.go`,
`internal/restapi/server.go`, `internal/restapi/nodes.go` (new), `internal/restapi/openapi.json`

New narrow interface on `ctlops.Config`, in the same shape as the other seven (nil makes every node
command answer `KindDisabled` with `"this host is not a fleet gateway."`):

```go
// NodeRoster is the fleet's node registry. Nil makes every node operation
// answer KindDisabled.
type NodeRoster interface {
	ListNodes() ([]NodeInfo, error)
	ApproveNode(name, by string) (NodeInfo, error)
	RemoveNode(name string) error
}
```

`Capabilities` gains `Fleet bool` (`o.nodes != nil`) — so `components.schemas.Capabilities` gains an
eighth property **and** an eighth entry in its `required` list, in the same commit.

`Ops.ListNodes` / `ApproveNode` / `RemoveNode` are all **operator-gated**, resolving the operator bit
from `o.accounts.Get(c.Handle).IsOperator()` **inside** the method exactly as `Ops.Invite` does
(account.go:347-377) — never from `ctlops.Caller`, which has no operator field on purpose. `ListNodes` is
operator-only too: node names are topology. `RemoveNode` refuses with a `KindConflict` naming the count
when the node still holds placements.

`sshgw`: `case "node": g.controlNode(s, c, args[1:], log)` in the switch at control.go:206-234, shaped
line-for-line like `controlSnapshot` (capability check first, `sub := "list"` default, exit 2 on a bad
subcommand). `controlUsage` (control.go:15-63) gains a `node` block under `other`, every line CRLF.

`restapi`: `{"GET","/v1/nodes","nodes.list",authRead,h.listNodes}`,
`{"POST","/v1/nodes/{name}/approve","nodes.approve",authMutate,h.approveNode}`,
`{"DELETE","/v1/nodes/{name}","nodes.rm",authMutate,h.removeNode}` in `routes()` **and** in
`openapi.json` in the same commit; `sample()` (server_test.go:219) gains a case for each.

`TestEveryMethodIsClassified` (ownership_test.go:230-262) reflects over `*Ops`: add the three names to
`notResourceScoped`. `fakes_test.go`'s `mutatingVerbs` (line 69) gains `"ApproveNode"` and
`"RemoveNode"`, plus a `fakeNodes`.

**Acceptance:** `go test ./internal/ctlops/ ./internal/sshgw/ ./internal/restapi/`. Golden rows for
`node` with no args, `node ls` as a non-operator (Denied, exit 1, one sentence), `node ls` as an
operator, `node approve` with no fingerprint (exit 2), `node approve <an unenrolled fingerprint>` (the
`no node in this fleet holds the key %s` phrasing), `node approve <a name>` (refused as malformed, and
without echoing the fingerprint that holds that name),
`node rm` with placements outstanding (conflict), and `node wat` (exit 2); the three `controlUsage`
assertions at control_golden_test.go:225/228/231 updated;
`TestControlUsageDocumentsTheOtherDoors` (no bare `\n`) still passes. openapi_test.go's four spec-parity
tests pass.

---

### W16 — [M1] The two-node in-process harness
**Group H** · deps: W14, W15
**Files:** `fleet_e2e_test.go` (new, package `sparkbox_test`, beside `e2e_test.go`)

`newFleetStack(t) (*testStack, *nodeStack)`. Gateway = the existing `newStack` (e2e_test.go:33-107) plus
a `placement.Store`, a `nodes.Store` seeded with the node's approved key, a `fleet.Fleet`, and
`Fleet`/`Dial`/`Nodes`/`NodeJoiner` on `GatewayOptions`. Node = a **second** `*host.Manager` over its
**own** `t.TempDir()` with its **own** `mock.New(dir2, hostKey2)` and `NodeName: "node-b"` — distinct
`StateDir` is mandatory or the two share one `sandboxes.json`. Node identity from the existing
`newClientKey` (e2e_test.go:312); the link is a real `nodelink.RunClient` over loopback. Two
`t.Cleanup(driver.Close)` calls. `waitOnline` uses the tree's existing `waitFor` polling idiom
(manager_test.go:207, livesessions_test.go:65) — no blind sleeps.

File header must note: both mock nodes report `HostIP "127.0.0.1"` (mock.go:597) and overlapping
ephemeral ports, so a fleet test can distinguish nodes only by **which link carried a stream**, never by
address — which is exactly the property the synthetic addressing enforces.

**Acceptance:** `go test ./ -run TestFleet -race`. The harness comes up; `ssh ctl@ node ls` as an
operator lists two nodes both online; node-b's reported capacity matches its manager's `Capacity()`.
Because `mock.Driver.Create` refuses a duplicate name only within one Driver (mock.go:61-63), both
managers would happily create `demo` — assert that the second create through the fleet fails with
`NameTaken` from the ledger. This is the fleet-wide name-allocation regression.

---

### W17 — [M2] Node-side lifecycle handlers and the remote `fleet.Node`
**Group I** · deps: W16
**Files:** `internal/nodelink/nodeops.go`, `internal/nodelink/rpc.go` (new),
`internal/fleet/remote.go` (new), `internal/nodelink/nodeops_test.go`,
`internal/fleet/remote_test.go` (new)

`rpc.go` declares every request/reply body from §2.5. `nodeops.go` registers one `Conn.Handle` per verb,
each unmarshalling, calling the node's `*host.Manager` against a context derived from the node's
**process** context plus `DeadlineMS`, and returning either the result body or `ctlops.ToWire(verb, err)`.
No ownership check, no name policy.

`rename` is the one that does not forward wholesale: `Manager.Rename` (manager.go:1041) fuses the
node-local VM-directory move + `DropSnapshots` with the gateway-owned side stores, and treats
`routes.RenameSandbox` as **fatal with rollback** (manager.go:1089-1108) because route rows carry the
Owner column that gates private-route auth. On a node those hooks are nil so the manager's own nil guards
skip them; the gateway does its half before dispatching and rolls it back if the node's half fails.
Document that split at the handler.

`fleet/remote.go` implements every `Node` method as one `Client.Do` call — ~15 near-identical five-line
methods, the payoff for `Node` being the remotable subset. Two invariants enforced here and nowhere else:
every returned record has `Node` overwritten with the **authenticated** link name, and a request on a
dead link returns `Unreachable(...)`, never `io.EOF`. `DialGuest` is stubbed to a `KindDisabled` error
until W21, so this item is independently mergeable.

**Acceptance:** `go test ./internal/nodelink/ ./internal/fleet/ -race`. A dispatcher over a real
mock-driver manager executes every verb and returns a row whose fields match the record; all seven host
error types survive the hop with identical `Msg`/`Code`/`ExitCode`/`HTTPStatus` **and** with
`LimitError.Running` and `CapacityError.BudgetMB` intact; an expired `DeadlineMS` produces
`context.DeadlineExceeded` classified as `KindInternal`/`timeout`; `touch` produces no reply;
`TestRemoteOverwritesNodeField` has the node return a record claiming `Node: "evil"` and asserts the
fleet sees the roster name; `TestOfflineNodeReturnsUnreachable` closes the pipe mid-flight.

---

### W18 — [M2] Fleet remote dispatch and the `--node` placement override
**Group I** · deps: W17
**Files:** `internal/fleet/fleet.go`, `internal/fleet/place.go` (new),
`internal/fleet/dispatch_test.go` (new), `internal/ctlops/types.go`, `internal/ctlops/sandbox.go`,
`internal/sshgw/tags.go`, `internal/sshgw/tags_test.go`, `internal/sshgw/gateway.go`,
`internal/restapi/sandboxes.go`, `internal/restapi/openapi.json`

Fill in the remote half of every Fleet method. `Get`/`List`/`ListByOwner`/`Snapshots` merge local records
with the owning node's **cached** rows (synthetic addresses), name-sorted like
`Manager.ListByOwner`. `Touch`/`RecordKey`/`ResyncEnv` are `Cast` (fire-and-forget, coalesced per
sandbox). `SetPinned` has an error but no ctx: bound it internally with
`context.WithTimeout(context.Background(), 10*time.Second)`. `Fork` resolves the node holding the
template from the cached snapshot inventory and places the create **there** — a snapshot is a file in
that node's image dir (snapshot.go:30) and is arch-pinned by construction. A remote+offline node yields
`Unreachable(...)`.

`place.go` declares `Placer`/`Request`/`Candidate` (so M5 swaps an implementation rather than editing
`Create`) with an M2 body of: prefer `PreferNode` if online, arch-matching and holding the image;
otherwise local. The eligibility filter must charge `effectiveMemMB`, not `MemMB`.

Placement override plumbing:
* `ctlops.CreateArgs` gains `Node string` ("" means the gateway's own node until the scheduler lands).
  `Ops.Create` type-asserts `o.boxes` to a `Placer`-shaped interface and calls `CreateOn` when
  `a.Node != ""`; an unknown node is `NotFound(op, "node", a.Node)`; a store that is not a placer is
  `KindDisabled`. The `nameIsFree → stampTags → Create → clearTags-on-failure` ordering
  (sandbox.go:51-62) is preserved exactly; only the final call changes.
* `parseTags` gains a **fourth** return value (`tags, node, rest []string/string, err error`) recognising
  `--node <v>` and `--node=<v>`. Its one production call site (gateway.go:271) and `tags_test.go` are
  updated. This matters because the `new@` door treats every bare word as a tag, so a bare `--node dgx`
  would otherwise be silently swallowed as the tags `node` and `dgx`.
* `restapi.createRequest` gains `Node string \`json:"node,omitempty"\``, passed through, added to
  `openapi.json`'s `CreateRequest`, **and** folded into the job `Ref.Args`
  (`Ref{Type:"sandbox", Name:name, Args:req.Node}`) — otherwise two creates of the same name onto
  different nodes collapse into one job (jobs.go:31-43).

**Acceptance:** `go test ./internal/fleet/ ./internal/ctlops/ ./internal/sshgw/ ./internal/restapi/ ./ -race`.
Two-node rig: `ssh -t new@gw -- --node node-b ml` creates on node-b with tag `ml`, the ledger says
`node-b`, and node-b's own `sandboxes.json` holds it; then pause/restore/resize/reboot/rename/rm/pin all
round-trip and mutate node-b's state file. A `*host.LimitError` raised by node-b renders the identical
friendly sentence `sshgw.failStart` prints locally. With the link killed, every one of those answers
`sparkbox: sandbox "x" lives on node "node-b", which is offline` at exit 1 / HTTP 503 — and a
**different** user asking about the same name still gets the byte-identical masked
`no sandbox named "x"` with **zero** node methods reached. `--node ghost` → `no node named "ghost"` /
404. `--node` with no value → exit 2.

---

### W19 — [M2] Events and the session hang-up relay
**Group J** · deps: W17
**Files:** `internal/nodelink/emitter.go`, `internal/nodelink/client.go`,
`internal/nodelink/server.go`, `internal/fleet/link.go`, `cmd/sparkbox/main.go`,
`internal/fleet/sessions_test.go` (new)

The node's `Emitter` emits `sandbox.changed` / `sandbox.gone` / `sandbox.paused`. On the gateway,
`nodelink.Client` updates its cache and calls the `Hooks`; `Fleet.ApplyChanged`/`ApplyGone` update the
index's cached state, and `Fleet.ApplyPaused` calls
`f.sessions.CloseSandboxSessions(name, reason)` with the node's own wording so the user reads the same
sentence a local pause produces.

The call must **not** block: it runs from the link's read loop, and `CloseSandboxSessions` is already
contractually non-blocking (livesessions.go:135-138). Do **not** spawn a goroutine — ordering matters for
the reason text.

`main.go` adds `flt.SetSessions(gw)` right after `mgr.SetSessions(gw)` (line 399). There is still exactly
**one** registry, which is the invariant `xterm.go:68` depends on.

**Acceptance:** `go test ./ -run TestFleetPause -race`. Attach an SSH session to a sandbox on node-b,
drive node-b's reaper white-box (`reapOnce` with `LastActive` pushed into the past), and assert the
gateway-side session is closed with the same goodbye text and terminal-restore sequence as the existing
local `TestPauseClosesAttachedSessions`. Repeat for a browser terminal (close code 4002). Confirm the
existing local test does not deadlock.

---

### W20 — [M2] Reconcile: orphan, adopt, quarantine, offline grace
**Group J** · deps: W19
**Files:** `internal/fleet/reconcile.go`, `internal/fleet/reconcile_test.go` (new), `fleet_e2e_test.go`

Four cases, in one transaction per node inventory:

1. Ledger row exists and the node reports it → refresh the cached record. **The node always wins on
   state**, because the index caches state and a stale row is a display artifact.
2. Ledger row exists for this node and the node does **not** report it → `StateOrphaned`, logged at
   Error, kept **forever**, surfaced as `Unreachable`/orphaned in listings and refused for mutation with a
   typed conflict. Never auto-delete: a wiped node must not silently destroy the user's record.
3. The node reports a name with no ledger row → **adopt** if the name is free (`Reserve` succeeds, owner
   from the record); **quarantine** (`StateQuarantine`, log, excluded from every listing, never served)
   if the name is held by another node. Adoption is what makes the orphaned-in-flight-create case
   converge: `restapi`'s jobs are in-memory and die with the process (jobs.go:44-47), but under a fleet a
   gateway restart no longer kills work running on a node.
4. Ledger rows for a node that has never connected → keep, offline.

On gateway boot the Fleet marks every row whose node is not the local one `Unreachable` until that node's
inventory arrives, and **never** runs `NewManager`'s running→paused downgrade (manager.go:472-479)
against them — that downgrade assumes the VMs died with the process, which is false for another machine.

Also enforce here: on every ingest, `Owner` and `Node` are overwritten from the ledger row before the
record touches the index.

**Acceptance:** `go test ./internal/fleet/ -run TestReconcile -race` covers all four cases with a fake
node, plus: a node going offline leaves its rows and their last-known state intact; a reconnect with a
changed inventory converges; the local node's records are never touched by ingest; `Owner` from a lying
node is discarded. Integration in `fleet_e2e_test.go`: create a sandbox on node-b, tear down and rebuild
the **gateway's** fleet + sshgw over the same `sparkbox.db`, assert it lists unreachable, let node-b
reconnect, assert it lists normally again and was never asked to pause.

---

### W21 — [M3] Reverse streams end to end
**Group K** · deps: W18, W20, W2
**Files:** `internal/nodelink/client.go`, `internal/nodelink/server.go`, `internal/fleet/remote.go`,
`internal/fleet/dial.go`, `internal/nodelink/stream_e2e_test.go` (new),
`internal/fleet/dial_test.go` (new)

Turn on the half of W2 that shipped dead. Node side: the resolver is the node's own `mgr.Get` —
`Kind=="ssh"` → `box.SSHAddr`, `Kind=="tcp"` → `net.JoinHostPort(box.HostIP, port)`, empty or
not-running → error. Gateway side: `Client.DialSandbox` opens the channel on its stored `gossh.Conn`; a
channel-open rejection is **not** retried. `Remote.DialGuest` stops being a stub.
`Fleet.DialContext` resolves `<sandbox>.<node>.sandbox.invalid` (port `"ssh"` → `Kind "ssh"`, numeric →
`Kind "tcp"`) through that node's link, returns `Unreachable` when the node has no live link, and falls
through to the package default `net.Dialer` for everything else. **No** `context.AfterFunc` close-bound
is installed (§2.7).

**Acceptance:** `go test ./internal/nodelink/ ./internal/fleet/ ./ -race`. With a sandbox on node-b:
`fleet.DialContext(ctx,"tcp","demo.node-b.sandbox.invalid:ssh")` yields a conn `xssh.NewClientConn`
handshakes over and runs `echo hi` in; `":8000"` reaches a listener started inside the mock guest; a
sandbox on the **local** node still dials its real 127.0.0.1 address through `net.Dialer`; dialing a
sandbox whose link is closed returns `node_unreachable` in under a second, not after the 15s dial budget;
50 concurrent streams all succeed while heartbeats keep flowing; and — the pooling regression —
`dial_test.go` issues two sequential keep-alive HTTP requests to a remote sandbox and asserts **one**
dial (the pooled connection survives the request that created it). `Fleet.Close()` tears every stream
down.

---

### W22 — [M3] Flip every data path onto the fleet dialer
**Group L** · deps: W21
**Files:** `cmd/sparkbox/main.go`, `internal/proxy/proxy.go`, `internal/console/console.go`,
`internal/userconsole/console.go`

The seams exist from W9; this is wiring plus the error pages. `main.go` passes `Fleet: flt` and
`Dial: flt.DialContext` to `sshgw.GatewayOptions` (so the interactive `ssh <name>@gateway` path at
gateway.go:303/316/321/324 and `RunInSandbox` at runner.go:20/25 become fleet-aware — scheduled jobs on
remote sandboxes start firing), `proxy.New(flt, …)` + `px.SetDialer(flt.DialContext)`,
`xterm.Config{Sandboxes: flt, Dial: flt.DialContext}`, `syncer.SetDialer(flt.DialContext)`, and both
consoles' probe dialers. `proxy`'s `ErrorHandler` and `EnsureRunning` branch gain the 503.

envsync being initiated gateway-side and dialing through the fleet is what makes secret env reach remote
sandboxes at all: the node has no secrets store, so its manager's fire-and-forget `pushEnv`
(manager.go:827) is a no-op there.

**Acceptance:** `go build ./... && go test ./... -race`. Integration on the two-node rig: with a sandbox
on node-b, `ssh <name>@gateway echo hi` returns `hi` and exit 0; `https://<name>.<domain>` proxies
including a WebSocket upgrade; `https://<name>-xterm.<domain>` opens a terminal and echoes keystrokes; a
`--tag`-selected secret lands in the remote guest's `/etc/environment`; a `ctl schedule add` job fires
inside it and its exit code is recorded. The same assertions against a **local** sandbox still pass.

---

### W23 — [M3] Indistinguishability
**Group M** · deps: W22
**Files:** `fleet_e2e_test.go`, `e2e_test.go`, `proxy_test.go`, `proxy_stream_test.go`,
`internal/xterm/bridge_test.go`

Factor the assertion bodies of `TestEndToEnd`, the proxy round-trip and streaming tests, and the xterm
bridge test into helpers parameterised by a `place func(name string) node`, then run each **twice**:
local, and forced onto node-b. This is the milestone's definition of done and the only construction that
catches a whole class of address-leak bugs — a path that still `net.Dial`s `box.HostIP` fails **only** in
the node-b pass.

Three negative tests alongside:
1. A stranger probing a name on node-b gets the byte-identical masked `no sandbox named %q` with zero
   mutating store calls, whether node-b is **online or offline**.
2. A sandbox on an offline node-b answers `ctl@` with the `node_unreachable` sentence at exit 1 and the
   edge with a 503 naming the node **only** for an authenticated request.
3. Neither answer differs for a caller who does not own the box, because `owned()` runs first.

Also assert `restapi`'s `TestEveryRouteAnswersCleanly` allow-set `{200,201,202,303,400,403,404,409,501}`
still holds: a 503 must never leak out of a ghost-sandbox probe, and the reason it cannot is the ordering
rule.

**Acceptance:** `go test ./... -race` with every parameterised case green in **both** placements, on
darwin, no root, no `/dev/kvm`, no `t.Skip`. The sentence that must be true: no test body differs between
the local and node-b passes except the placement argument.

---

### W24 — [M3] Gateway restart must not touch a node's VMs
**Group M** · deps: W23
**Files:** `fleet_e2e_test.go`

Design §8 says this "deserves an explicit test". Write it. From the two-node rig: create a sandbox on
node-b and confirm it is running, then `srv.Close()` the gateway's listener (which is what
main.go:664 does on shutdown, dropping node links abruptly). Assert (a) node-b's own manager still
reports it running, (b) `RunClient` has not returned and is backing off, (c) nothing fatal was fed to the
node process. Stand a new gateway up on the same address with the same state dir, let node-b reconnect,
and assert the index reconverges with the same owner, same node, and the ledger row untouched throughout.

Two more cases in the same file: destroy the sandbox on node-b while the gateway is down → on reconnect
the ledger row is marked **orphaned** and logged, never auto-deleted. Create a sandbox directly on
node-b's manager while the gateway is down → on reconnect it is **adopted**; repeat with the name
pre-claimed by another node → **quarantined**, not served.

**Acceptance:** `go test -run TestFleetSurvivesGatewayRestart ./... -race` green.

---

## 6. Test plan

There is **no CI running `go test`** in this repo (`.github/workflows` holds only `build-artifacts.yml`
and `pages.yml`). Every acceptance criterion is local-only. The verification loop after every item:

```
cd tools/sparkbox && go build ./... && go vet ./... && go test ./... \
  && go test -race ./internal/fleet/... ./internal/nodelink/... ./internal/placement/... ./internal/nodes/...
```

Not one test file in the tree calls `t.Skip`, and the whole suite runs on darwin with no root and no
`/dev/kvm`. That contract must survive. `internal/vmm/firecracker` is entirely behind `//go:build linux`,
so on darwin it contributes no files at all.

### Four idioms, all four must be fed

**1. Pure fakes** — `internal/fleet/fleet_test.go`, `dispatch_test.go`, `reconcile_test.go` use an
in-memory `fakeNode` implementing `fleet.Node`: no sqlite, no driver, no temp dir, mirroring
`ctlops/fakes_test.go`'s shape including a shared `calls` recorder. Coverage: atomic name claim (64
goroutines, one winner), dispatch landing on the right node, offline dispatch → `node_unreachable`,
`Owner`/`Node` overwrite on ingest, `ArchivingEnabled` as OR-across-online-nodes, `Touch` coalescing,
`SetPinned`'s internal 10s bound.

**2. Contract guards that fail the build** — these already exist and must be fed, never bypassed:
* `ctlops.TestEveryMethodIsClassified` (ownership_test.go:230) reflects over `*Ops`; `ListNodes`,
  `ApproveNode`, `RemoveNode` go in `notResourceScoped`, each with an operator-gate test modelled on
  `TestInviteIsTheOnlyOperatorGate`.
* `fakes_test.go`'s `mutatingVerbs` (line 69, stated positively on purpose) gains `"ApproveNode"` and
  `"RemoveNode"` or `TestCrossOwnerIsIndistinguishable` silently stops covering them.
* `restapi`'s four `routes()`-derived tests iterate through `sample()` (server_test.go:219); each
  `/v1/nodes` route needs a `sample()` case, a matching `openapi.json` entry, a documented 401, an
  `IdempotencyKey` reference for the two `authMutate` ops, and 202⇔`Prefer` parity.
* Compile assertions `var _ ctlops.Sandboxes = (*fleet.Fleet)(nil)` live in `internal/fleet`
  (fleet imports ctlops; an in-package `ctlops` test importing fleet would cycle).
* New: a guard asserting `gw.Server("")` sets no `IdleTimeout` and no `MaxTimeout`.

**3. Golden output** — `internal/sshgw/control_golden_test.go` pins ~40 `ctl@` invocations byte-for-byte
on stdout, stderr **and** exit code, and separately forbids a bare `\n` on any line. W15 adds rows and
updates the three `controlUsage` assertions; update the goldens, do not soften the test. Note the
buffer-based `ctlSession` fake stores context values in a plain map and therefore returns nil for
`gssh.ContextKeyConn` — that harness can test `ctl@ node` **output** and can never exercise a link.

**4. Full-stack loopback** — `internal/nodelink` over `net.Pipe` for the codec and over a **real**
loopback SSH server+client for streams; `fleet_e2e_test.go` for the two-node rig.

### File-by-file

| file | asserts |
|---|---|
| `internal/ctlops/wire_test.go` | every classified error round-trips `ToWire`→JSON→`FromWire` preserving Kind/Op/Code/Msg/Hint/Verbatim/ExitCode/HTTPStatus; `errors.As` recovers all seven concrete host types with equal fields; `LimitError.Running` is non-empty and `Running[0]` is safe; `Details` numbers are float64 and nothing asserts an int out of them; `ParseKind(k.String())` for all ten kinds |
| `internal/nodelink/conn_test.go` | request/reply over `net.Pipe` for every verb; 200 concurrent requests, no ID collision; a `WireError` reply rehydrates to the same `*Error`; ctx cancellation emits `cancel`; an oversized line closes the link; a closed link fails every pending waiter |
| `internal/nodelink/stream_test.go` | the eight W2 proofs, incl. the HOL latency measurement and the "http.Transport sets no conn deadline" pin |
| `internal/nodelink/nodeops_test.go` | every verb against a real mock-driver manager returns a row equal to a direct call; all seven host errors survive; expired deadline; `touch` produces no reply |
| `internal/nodelink/client_test.go` | handshake, heartbeat cadence, keepalive detects a half-open peer, reconnect backoff, `RunClient` returns only on ctx, emitter drops rather than blocks and `CloseSandboxSessions` returns immediately |
| `internal/placement/store_test.go` | `TestReserveIsAtomic` (64 goroutines, 1 winner), release/reserve, rename refuses a taken target, reopen is a no-op, shared-`sparkbox.db` concurrent writes under `-race` |
| `internal/nodes/store_test.go` | idempotent re-enrol, `ErrNameTaken`, `MaxPending`, approve stamps `approved_by`, disabled rows still `Lookup`, keying on the wire form |
| `internal/fleet/parity_test.go` | Fleet over a real manager, `Index` nil and set, deep-equal to the bare manager for `Get`/`List`/`ListByOwner`/`Snapshots` after every step of a lifecycle matrix |
| `internal/fleet/fleet_test.go` | reservation rollback, double-create `NameTaken`, boot reconcile, `Get` latency on a 5000-row index |
| `internal/fleet/remote_test.go` | every `Node` method round-trips over `net.Pipe`; `Node` field overwritten with the roster name; a dead link yields `Unreachable` |
| `internal/fleet/reconcile_test.go` | the four reconcile cases; offline preserves rows and state; reconnect converges; local records untouched; a lying `Owner` is discarded |
| `internal/fleet/dial_test.go` | a pooled HTTP connection survives its dialing request (one dial for two sequential keep-alive requests); `Fleet.Close()` tears streams down |
| `internal/fleet/sessions_test.go` | a remote pause event reaches `CloseSandboxSessions` with the node's reason, non-blocking |
| `internal/proxy/upstream_test.go` | `TestUpstreamPoolIsPerSandbox` — two sandboxes, interleaved keep-alives, each response from the correct backend (fails against a shared package-level transport) |
| `internal/sshgw/nodedoor_test.go` | unknown key enrols once and gets `node_pending`; unknown key at any other username is still refused identically; approved key handshakes; second link supersedes; no `IdleTimeout`/`MaxTimeout` |
| `internal/host/node_test.go` | `Node` backfill on load; `Create` stamps it; `Capacity().Arch`/`.Online` |
| `internal/host/observer_test.go` | one event per transition with the right reason; `Create` refuses before `SetGatewayPublicKey` |
| `fleet_e2e_test.go` | the harness; fleet-wide name uniqueness across two mock drivers; W19/W20/W22/W23/W24's integration cases |

### Invariants re-asserted, not merely preserved

The masking property is pinned in four places (ctlops/ownership_test.go:157-171 substitutes the caller's
own name and demands byte-identical messages plus exit 1 / status 404 / `Verbatim` true;
restapi/server_test.go:353; xterm/xterm_test.go:224; sshgw/control_golden_test.go:250). Add the
fleet-layer version: a cross-owner probe against a **remote** sandbox must reach zero node methods and
must not distinguish "unreachable node" from "no such sandbox", online or offline.

### Timing discipline

Nothing sleeps blindly. The idioms are `waitFor` polling (manager_test.go:207,
livesessions_test.go:65, bridge_test.go:233) and white-box single-tick calls (`m.reapOnce`). The node
heartbeat and the offline grace timer must expose a package-visible `tickOnce`-style entry point so no
fleet test waits on a real timer. Any SSH-driven test that merges stdout and stderr **must** use
`syncBuf` (e2e_test.go:294-309) — x/crypto copies the two streams on separate goroutines and a shared
bare `bytes.Buffer` races and silently drops output.

### Manual smoke before promoting an M3 release

The DGX is the primary box and is aarch64. Bring a second node up on the laptop over the tailnet as
`--driver mock --gateway <dgx>:2222`, approve it, create a sandbox on it with `--node`, and confirm
`https://<name>.catnip.sh`, `ssh <name>@catnip.sh` and `https://<name>-xterm.catnip.sh` all work.
Arch-aware scheduling is M5, so a mock node with no real images is the honest second node until then.

---

## 7. Open questions

Three, all deliberately scoped out of M0–M3 rather than undecided:

1. **Metadata / id tokens on remote sandboxes (M4).** `internal/metadata` keeps its listener node-side —
   its whole security model is source-address attribution (`guestNet`, `gatewayFor`, `GetByHostIP`) and
   cannot be relayed. But `Options.Issuer` is a concrete `*oidc.Issuer` and `claimsFor`
   (server.go:258-273) reads the users DB for the `github` claim, neither of which a node has. The
   `mint` message is already declared in §2.4 so `Protocol` need not be bumped: the node sends **facts**
   (`sandbox`, `owner`, `image`, `key_fp`, `aud`) and the gateway builds the `oidc.Claims` and verifies
   the placement. Until M4 lands, node mode runs with `--metadata-addr ""` and **a sandbox on a remote
   node has no id-token endpoint**. Decision needed at M4 planning, not now: whether to gate remote
   placement behind a flag until then, or ship the relay together with M3.
   Also at M4: metadata's 10 mints/minute/sandbox limiter (server.go:53-56) runs **before** the mint and
   stays node-side, so a compromised node can bypass its own limiter — the gateway needs an independent
   per-(node, sandbox) limit or §5's blast-radius claim weakens to "can farm unlimited tokens for its own
   sandboxes".

2. **Egress policy on remote sandboxes (M4).** `netpush`/sluice stays node-local (taps only exist where
   the VM runs, and `PUT /policy` is a **full replace** of the per-tap set, so a fleet-wide map would wipe
   every other node's taps). `internal/netrules` stays gateway-owned. Until the `netrules.allow` relay
   (declared in §2.4) lands, a node runs with a `Rules` stub returning `governed=false`, which means
   **a tagged sandbox on a remote node has unrestricted egress**. This is a security regression, not a
   missing feature: it must be in the M1 release note, or netrules-governed workloads must be refused
   remote placement until M4. `governed` must stay a separate boolean from the allow list — collapsing
   them turns a deliberate deny-all into unrestricted egress.

3. **Per-node guest subnets.** Not required by this plan — the fleet dialer names sandboxes, never
   addresses. Recorded because the design doc names `vmm.Options.Subnet` twice (§4, §11) and **that type
   does not exist**: `internal/vmm` declares only `Config`, `Instance`, `State`, `BalloonStats`, `Driver`
   and the eight capability interfaces. The dead field is `firecracker.Options.Subnet` (fc.go:45),
   defaulted at fc.go:83 and read nowhere, and unreachable from the CLI (no `--subnet` flag, no slot in
   either `newFirecrackerDriver` signature). The `172.30` literal exists in **four** independent forms
   that no shared constant ties together — `fmt.Sprintf` formats (fc.go:146-147), a `net.IPNet`
   (metadata/server.go:48), per-octet integer comparisons on a `netip.As4()` array (netpush.go:254), and
   shell strings (deploy/sparkbox-net.sh:19-38) — plus the `/30` mask asserted by convention in three
   more (fc.go:951, the kernel-arg literal `255.255.255.252` at fc.go:274, and `gatewayFor` at
   server.go:174). Changing one and not the others fails silently at runtime, not at compile time. Treat
   the doc's "~20 lines" as wrong and do not treat this as a blocker for anything in M0–M3.

Everything else is decided above. Notably decided, so nobody re-opens them:

* **No new `ctlops.Kind`** and **no new `vmm.State`**. Node-offline is `KindCapacity` +
  `Code: "node_unreachable"` + explicit `Status: 503`; unreachable-ness rides on
  `host.Sandbox.Unreachable`, a gateway-only bool.
* **`node` is not in `ReservedUsers`** (it would publish `node.<domain>` and expose the fleet door by
  destination IP), but it **is** in `internal/reserved`.
* **`internal/api` and `internal/metadata` stay on the local `*host.Manager`.** The legacy API is
  unauthenticated and loopback-bound; `GetByHostIP` fleet-wide would be a security bug, because guest IPs
  repeat across nodes.
* **The wire carries no addresses.** The Fleet synthesises `<sandbox>.<node>.sandbox.invalid`.
* **`Fleet.DialContext` installs no `context.AfterFunc` close-bound.** Pooled connections outlive the
  request that dialed them.
