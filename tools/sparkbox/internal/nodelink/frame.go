package nodelink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
)

// The shape of the link itself.
const (
	// User is the SSH username a node connects as. It is the same string as
	// sshgw.NodeUser; restating it here keeps this package free of an import it
	// would otherwise need only for a constant.
	User = "node"
	// Protocol is the version a hello must carry. Bumping it is the last
	// resort: every message below is additive-by-omitempty precisely so a
	// mixed-version fleet keeps working through an upgrade.
	Protocol = 1
	// LinkCommand is what a node execs on its session. A human who types
	// `ssh node@gateway` runs no command and is answered with a sentence
	// instead of silence, which is why the door checks for this exact string
	// rather than accepting anything.
	LinkCommand = "sparkbox-link/1"
	// DataCommand is the registration session on a dedicated SSH data
	// connection. It is intentionally a sibling of LinkCommand rather than a
	// replacement: peers which do not negotiate the data pool keep carrying
	// streams on the control connection exactly as before.
	DataCommand = "sparkbox-data/1"
	// CapabilitySSHDataPoolV1 opts both peers into separate SSH connections for
	// control and guest data. Capabilities are additive and omitted on the
	// wire by older builds, so a rolling upgrade falls back to the combined
	// link without a protocol-version bump.
	CapabilitySSHDataPoolV1 = "ssh_data_pool_v1"
	// CapabilityGRPCControlV1 says the node can host the typed mTLS control
	// service after certificate enrollment. It is advertised only; transport
	// preference remains a gateway rollout decision.
	CapabilityGRPCControlV1 = "grpc_control_v1"
	// CapabilityRoutedGuestV1 says the node's approved guest prefix is routed
	// to the gateway and its current HostIP values may be used for direct data.
	CapabilityRoutedGuestV1 = "routed_guest_v1"
	// StreamChannel is the data channel type the gateway opens back toward a
	// node, one per tunneled TCP connection.
	StreamChannel = "sandbox-stream@sparkbox"
	// MaxFrameBytes bounds one control line. A longer one is not truncated: it
	// closes the link, because a framing desync is not something either side
	// can reason its way out of.
	MaxFrameBytes = 1 << 20
)

// The cadences and ceilings. They are constants rather than options because
// both ends must agree on them and there is no negotiation in the handshake
// beyond the heartbeat interval the gateway states in its Welcome.
const (
	// DefaultHeartbeat is how often a node reports capacity. It doubles as the
	// keepalive cadence: the node also sends an SSH global request on this
	// beat, which is its only way to notice a half-open TCP.
	DefaultHeartbeat = 15 * time.Second
	// DefaultPingEvery and DefaultPingBudget are the gateway's own liveness
	// probe, which unlike the heartbeat demands an answer.
	DefaultPingEvery  = 30 * time.Second
	DefaultPingBudget = 10 * time.Second
	// DefaultGrace is three missed heartbeats. Past it a node is marked
	// offline — which never deletes an index row and never reschedules
	// anything, because the rootfs is still on that machine.
	DefaultGrace = 45 * time.Second
	// StreamDialTimeout bounds the node's own TCP dial into a guest.
	StreamDialTimeout = 10 * time.Second
	// MaxLiveStreams and MaxInFlightOps bound what one link may ask of a node.
	// MaxInFlightOps is a ceiling on concurrency, not a queue depth: a request
	// that arrives while every slot is held is answered CodeNodeBusy rather
	// than parked, because a peer — hostile, or merely a gateway with a stuck
	// operator script — would otherwise mint one goroutine holding one frame
	// body per line it can write, and MaxFrameBytes is a megabyte.
	MaxLiveStreams = 512
	MaxInFlightOps = 8
	// DefaultDataLanes is the number of independently supervised SSH data
	// connections a capable node maintains for each control generation.
	DefaultDataLanes = 2
	// LinkMargin is subtracted from a caller's remaining budget before it rides
	// as deadline_ms, so the responder gives up fractionally before the
	// requester does and the requester gets a typed answer rather than a
	// timeout it has to guess about.
	LinkMargin = 2 * time.Second
)

// Frame types. A reply is TypeReply whatever the request was; the correlation
// is the ID, not the name.
const (
	TypeReply     = "reply"
	TypeHello     = "hello"
	TypeBye       = "bye"
	TypeHeartbeat = "heartbeat"
	TypePing      = "ping"
	TypeCancel    = "cancel"
	TypeInventory = "inventory"
	TypeChanged   = "changed"
	TypeGone      = "gone"
	TypePaused    = "paused"

	TypeCreate        = "sandbox.create"
	TypeEnsureRunning = "sandbox.ensure_running"
	TypePause         = "sandbox.pause"
	TypeArchive       = "sandbox.archive"
	TypeResize        = "sandbox.resize"
	TypeReboot        = "sandbox.reboot"
	TypeRename        = "sandbox.rename"
	TypeDestroy       = "sandbox.destroy"
	TypeSetPinned     = "sandbox.set_pinned"
	TypeResyncEnv     = "sandbox.resync_env"
	TypeTouch         = "sandbox.touch"
	TypeRecordKey     = "sandbox.record_key"
	TypeVitals        = "sandbox.vitals"

	TypeSnapshotCreate = "snapshot.create"
	TypeSnapshotDelete = "snapshot.delete"
	TypeSnapshotFork   = "snapshot.fork"

	// The egress plane, gateway -> node. Policy is pushed because the rules
	// that produce it live in the gateway's store; usage is pulled because the
	// meter that produces it is attached to the node's own taps.
	TypeNetPolicy = "net.policy"
	TypeNetUsage  = "net.usage"

	// Workload identity, NODE -> gateway. The only two requests that travel
	// that way, and they do so because the fleet's OIDC signing key is the one
	// piece of gateway material that never leaves the gateway.
	TypeIdentityToken = "identity.token"
	TypeIdentityDoc   = "identity.describe"

	// Certificate enrollment, NODE -> gateway. The SSH control link is the
	// bootstrap authentication: the request carries no node name because the
	// gateway binds the CSR to the roster name authenticated by that link.
	TypeCertificateEnroll = "certificate.enroll"
)

// Frame is one newline-delimited JSON message. A request carries a non-empty
// ID and its own Type; the reply carries the same ID, Type "reply", and
// exactly one of Body or Err. An event carries no ID and is never replied to —
// which is what lets the two highest-frequency writes in the system (touch and
// record_key) cost a caller nothing.
type Frame struct {
	ID string `json:"id,omitempty"`
	// Type is the message name on a request or event, and TypeReply on a reply.
	Type string `json:"type"`
	// DeadlineMS is the requester's remaining budget, less LinkMargin. The
	// responder bounds its work with it and with nothing else: binding the work
	// to the link would tear down a running VM the moment a gateway restarted.
	DeadlineMS int64             `json:"deadline_ms,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
	Err        *ctlops.WireError `json:"err,omitempty"`
}

// encoder renders frames. json.Encoder.Encode appends exactly one newline and
// escapes any embedded one, so a frame is always a line; the mutex is what
// keeps two goroutines' frames from interleaving halfway through that line.
//
// It encodes into a buffer of its own rather than straight at the transport,
// and the mutex is therefore held only for as long as marshalling takes —
// never across a write. See writer for why that distinction is the difference
// between a live link and a wedged one.
type encoder struct {
	mu  sync.Mutex
	buf bytes.Buffer
	enc *json.Encoder
	w   io.Writer
}

func newEncoder(w io.Writer) *encoder {
	e := &encoder{w: w}
	e.enc = json.NewEncoder(&e.buf)
	// The link carries sandbox names, owners and error sentences, never HTML.
	// Escaping < and > here would only make the wire harder to read in a log.
	e.enc.SetEscapeHTML(false)
	return e
}

// line renders one frame as the bytes that go on the wire.
func (e *encoder) line(f *Frame) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.buf.Reset()
	if err := e.enc.Encode(f); err != nil {
		return nil, err
	}
	// Cloned because the buffer is reused by the next frame, and the line is
	// about to be handed to a goroutine that has not written it yet.
	return bytes.Clone(e.buf.Bytes()), nil
}

// writeLine puts an already-rendered line on the transport. It takes no lock,
// deliberately: its only caller is the single writer goroutine, and a lock held
// here is exactly the mistake the rest of this file is arranged to prevent.
func (e *encoder) writeLine(line []byte) error {
	_, err := e.w.Write(line)
	return err
}

// encode renders and writes one frame on the calling goroutine. It is for the
// door — the refusal and the bye written onto a session the same goroutine is
// about to close, where there is no second writer to interleave with and
// nothing else alive to park. Every frame on a live link goes through writer.
func (e *encoder) encode(f *Frame) error {
	line, err := e.line(f)
	if err != nil {
		return err
	}
	return e.writeLine(line)
}

// ErrLinkBacklogged is what a frame nobody is waiting on reports when the
// transport is so far behind that the queue is full. It is an answer rather
// than a silent drop: the emitter logs it, and a caller that cares can resync.
var ErrLinkBacklogged = errors.New("nodelink: the link's write queue is full")

const (
	// writeQueueDepth bounds how many frames may wait for the transport at
	// once. A healthy link's backlog is one frame; this is sized for the burst
	// a reconnect makes — a full inventory, a heartbeat, and every reply in
	// flight — and it is finite because the only alternative to refusing a
	// frame is holding the goroutine that offered it.
	writeQueueDepth = 128
	// flushGrace is how long Close lets the writer put its backlog on the wire
	// before the transport is shut underneath it. It exists for one frame: the
	// bye that says why a link is ending, which a peer that never receives it
	// has to wait out a grace period to infer. A peer that is reading drains in
	// microseconds; a peer that is not is the exact case this design exists to
	// survive, so the wait is short and unconditional rather than a promise.
	flushGrace = 250 * time.Millisecond
)

// writer owns one direction of a live link.
//
// Every frame is rendered by its caller and handed to a single goroutine that
// does nothing but write, and no lock is held while that write runs. This is
// not tidiness. The transport in production is an SSH channel: once a peer
// stops reading, roughly 2 MiB rides unacknowledged and then Write parks, with
// no deadline and no context, for as long as the peer cares to stay quiet. A
// mutex held across that write would put the ping loop, the hangup and every
// reply behind a machine that has merely gone silent — which would make the one
// mechanism that notices a dead node the first casualty of one. Handing the
// write to a goroutine is also what lets a caller keep its own context: it
// abandons the frame, rather than the frame keeping it.
type writer struct {
	enc             *encoder
	q               chan []byte
	metrics         *fleetmetrics.Registry
	node, transport string

	// started is closed when the goroutine exists; dead when it has gone. The
	// pair is what lets shutdown tell "nothing was ever written on this link"
	// from "the backlog is still going out".
	start   sync.Once
	started chan struct{}
	stopped sync.Once
	stop    chan struct{}
	dead    chan struct{}

	// fail carries a write error to the rest of the link. A socket that cannot
	// be written to is dead however healthy its reading half looks — a
	// half-open TCP accepts reads forever — so the waiters are told at once
	// instead of each discovering it by deadline.
	fail func(error)

	mu  sync.Mutex
	err error
}

func (w *writer) setMetrics(m *fleetmetrics.Registry, node, transport string) {
	w.metrics, w.node, w.transport = m, node, transport
	w.metrics.SetWriteQueueDepth(w.node, w.transport, len(w.q))
}

func (w *writer) queued() {
	w.metrics.SetWriteQueueDepth(w.node, w.transport, len(w.q))
}

func newWriter(w io.Writer, fail func(error)) *writer {
	if fail == nil {
		fail = func(error) {}
	}
	return &writer{
		enc:     newEncoder(w),
		q:       make(chan []byte, writeQueueDepth),
		started: make(chan struct{}),
		stop:    make(chan struct{}),
		dead:    make(chan struct{}),
		fail:    fail,
	}
}

// send queues one frame, waiting for room when the transport is behind. The
// wait ends on ctx or on the writer's death and never on nothing, which is the
// whole reason a caller may sit behind a network hop: its context is the only
// thing standing between it and a peer that has stopped reading.
func (w *writer) send(ctx context.Context, f *Frame) error {
	line, err := w.enc.line(f)
	if err != nil {
		return err
	}
	w.run()
	select {
	case <-w.dead:
		return w.reason()
	default:
	}
	select {
	case w.q <- line:
		w.queued()
		return nil
	case <-w.dead:
		return w.reason()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// post queues one frame or says it could not, without ever waiting. Everything
// nobody is blocked on goes out this way — events, and the replies handlers
// produce — because the goroutine offering one is a heartbeat, a reaper hook,
// or the read loop itself, and none of them may be parked by a quiet peer.
func (w *writer) post(f *Frame) error {
	line, err := w.enc.line(f)
	if err != nil {
		return err
	}
	w.run()
	select {
	case <-w.dead:
		return w.reason()
	default:
	}
	select {
	case w.q <- line:
		w.queued()
		return nil
	case <-w.dead:
		return w.reason()
	default:
		kind := "event"
		if f.Type == TypeReply {
			kind = "reply"
		}
		w.metrics.IncDropped(w.node, w.transport, kind)
		return ErrLinkBacklogged
	}
}

// run starts the writer on first use — lazily, so a Conn that never writes a
// frame costs no goroutine at all.
func (w *writer) run() {
	w.start.Do(func() {
		close(w.started)
		go w.loop()
	})
}

// loop is the only goroutine that ever touches the transport. It holds nothing
// anyone else needs, so a write that never returns costs this goroutine and
// this goroutine alone.
func (w *writer) loop() {
	defer close(w.dead)
	for {
		select {
		case line := <-w.q:
			w.queued()
			if err := w.write(line); err != nil {
				return
			}
		case <-w.stop:
			w.drain()
			return
		}
	}
}

func (w *writer) write(line []byte) error {
	err := w.enc.writeLine(line)
	if err != nil {
		w.mu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.mu.Unlock()
		w.fail(err)
	}
	return err
}

// drain puts what is already queued on the wire on the way out. The last frame
// either side sends is usually the bye that explains the close, and dropping it
// would turn a planned hangup into a silence the peer has to wait out.
func (w *writer) drain() {
	for {
		select {
		case line := <-w.q:
			w.queued()
			if err := w.write(line); err != nil {
				return
			}
		default:
			return
		}
	}
}

// shutdown stops the writer and gives it flushGrace to finish the backlog.
// Bounded, because the peer this matters most for — the one being hung up on —
// is also the one most likely not to be reading.
func (w *writer) shutdown() {
	w.stopped.Do(func() { close(w.stop) })
	select {
	case <-w.started:
	default:
		// Nothing was ever written on this link, so there is nothing to flush
		// and no goroutine that will ever close dead.
		return
	}
	t := time.NewTimer(flushGrace)
	defer t.Stop()
	select {
	case <-w.dead:
	case <-t.C:
	}
}

// reason is why a frame could not be queued. ErrLinkClosed is the fallback
// because a writer that stopped without a write error stopped on Close.
func (w *writer) reason() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	return ErrLinkClosed
}

// decoder reads frames off the other direction. bufio.Scanner is used for its
// hard ceiling: a peer that never sends a newline is refused at MaxFrameBytes
// instead of being buffered until the process dies.
type decoder struct {
	sc *bufio.Scanner
}

func newDecoder(r io.Reader) *decoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxFrameBytes)
	return &decoder{sc: sc}
}

// next returns the next frame, io.EOF at a clean end of stream, and a bounded
// error for anything else. A blank line is skipped rather than treated as a
// desync, because a hand-typed probe at this door is a likely first contact.
func (d *decoder) next() (*Frame, error) {
	for {
		if !d.sc.Scan() {
			if err := d.sc.Err(); err != nil {
				if errors.Is(err, bufio.ErrTooLong) {
					return nil, fmt.Errorf("nodelink: frame exceeds %d bytes: %w", MaxFrameBytes, err)
				}
				return nil, err
			}
			return nil, io.EOF
		}
		line := d.sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			return nil, fmt.Errorf("nodelink: malformed frame: %w", err)
		}
		return &f, nil
	}
}

// --- handshake ---

// Hello is a node introducing itself. Everything in it is a fact about that
// machine; nothing in it is a claim about authorization, because the node's
// name and status come from the gateway's roster, which the SSH key already
// resolved before this frame was read.
type Hello struct {
	Protocol    int      `json:"protocol"`
	Node        string   `json:"node"`
	Arch        string   `json:"arch"`
	OS          string   `json:"os"`
	Release     string   `json:"release"`
	Version     string   `json:"version"`
	Driver      string   `json:"driver"`
	Images      []string `json:"images"`
	Archiving   bool     `json:"archiving"`
	Snapshots   bool     `json:"snapshots"`
	GuestSubnet string   `json:"guest_subnet"`
	// GRPCAddr is the tailnet address where this node will expose NodeControl
	// after enrollment. It is advisory at hello; the approved roster row
	// remains authoritative for dialing.
	GRPCAddr  string    `json:"grpc_addr,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// Sluice reports whether this machine has an egress gateway to enforce
	// against and meter from. It is stated rather than discovered because both
	// answers are silent otherwise: a gateway that pushes policy to a machine
	// with no sluice gets a cheerful nil back and believes a tagged sandbox is
	// filtered when it is not, and an owner reading a bandwidth panel for a VM
	// on such a machine sees zeroes that look like an idle box rather than an
	// unmetered one. An older node omits the field and reads as false, which is
	// the honest answer for a build that had no way to install one.
	Sluice bool `json:"sluice,omitempty"`
	// Capabilities are optional, independently negotiated transport features.
	Capabilities []string `json:"capabilities,omitempty"`
}

// Welcome is the gateway's reply to an approved hello.
//
// GatewayUpstreamPub is the only gateway material that ever crosses this link:
// it is the public half of the key the gateway logs into guests with, which a
// node must install on every VM it boots. Node is the canonical roster name,
// not the one the hello asked for — the row is authoritative.
type Welcome struct {
	Protocol           int      `json:"protocol"`
	Node               string   `json:"node"`
	GatewayUpstreamPub string   `json:"gateway_upstream_pub"`
	Domain             string   `json:"domain"`
	HeartbeatSeconds   int      `json:"heartbeat_seconds"`
	Capabilities       []string `json:"capabilities,omitempty"`
	// ControlGeneration is an opaque, per-control-link token. Dedicated data
	// connections must present it when registering, which prevents a late lane
	// from an old control link attaching to its replacement.
	ControlGeneration string `json:"control_generation,omitempty"`
}

// Bye is the last frame either side sends. It is an event: there is nothing to
// reply to, and waiting for an acknowledgement would only delay the close.
type Bye struct {
	Code string `json:"code"`
	Msg  string `json:"msg,omitempty"`
}

// Refusal codes. They are stable tokens rather than sentences because the node
// logs a sentence of its own choosing around them — most importantly the exact
// `ssh ctl@<gateway> node approve <name>` an operator has to run next.
const (
	CodeNodePending         = "node_pending"
	CodeNodeDisabled        = "node_disabled"
	CodeNodeNameTaken       = "node_name_taken"
	CodeNodeNameMismatch    = "node_name_mismatch"
	CodeNodeEnrolFull       = "node_enrol_full"
	CodeBadNodeName         = "bad_node_name"
	CodeProtocolUnsupported = "protocol_unsupported"

	// Bye codes that are not refusals.
	CodeSuperseded    = "superseded"
	CodeShuttingDown  = "shutting_down"
	CodeProtocolError = "protocol_error"

	// CodeNodeBusy answers a request that arrived while every one of a link's
	// MaxInFlightOps slots was held. It rides as a capacity refusal because
	// that is what it is — the far machine has no room for this work right now
	// — and because a caller told "busy" can retry or place the work
	// elsewhere, where one silently queued can only wait out a deadline it
	// cannot see.
	CodeNodeBusy = "node_busy"
)

// --- liveness ---

// Heartbeat is the node's unprompted "still here, and here is what I have
// left". It is an event: a capacity report nobody is waiting on must never
// occupy a request slot.
type Heartbeat struct {
	Capacity host.NodeCapacity `json:"capacity"`
	At       time.Time         `json:"at"`
}

// PingReq is both the request and the reply body of a ping. The gateway sends
// it because a heartbeat only proves the node's writer works, and a link can
// be dead in exactly one direction.
type PingReq struct {
	Nonce string `json:"nonce"`
}

// Cancel withdraws interest in an in-flight request. It is advisory — the
// responder's deadline is the backstop — because a cancellation that arrived
// after the work finished must not look like a failure.
type Cancel struct {
	ID string `json:"id"`
}

// --- inventory ---

// SandboxRow is a sandbox as the gateway is allowed to see it.
//
// There is no HostIP and no SSHAddr on purpose. Every node mints the same guest
// addresses, so an address is not a fleet-wide name; sending one would only
// invite something on the gateway to dial it and reach its own VM instead. The
// gateway synthesises the addresses it hands out, and a stream names a sandbox
// rather than an address so the owning node re-resolves it itself.
//
// Owner rides only as an advisory display value: the placement ledger's owner
// column is the authorization input, and the gateway overwrites this one from
// it before the record is indexed.
type SandboxRow struct {
	Name        string    `json:"name"`
	Owner       string    `json:"owner"`
	Image       string    `json:"image"`
	State       string    `json:"state"`
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

// SnapshotRow is a fork template as the gateway sees it. Snapshots are
// node-local: a fork happens on the machine that holds the template.
type SnapshotRow struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	Image     string    `json:"image"`
	FromBox   string    `json:"from_box"`
	CreatedAt time.Time `json:"created_at"`
}

// InventoryMsg is the node's whole picture. It is sent after Welcome, in reply
// to an inventory request, and whenever the node's event queue overflows —
// which is what makes correctness independent of any individual event
// arriving.
type InventoryMsg struct {
	Node      string            `json:"node"`
	Sandboxes []SandboxRow      `json:"sandboxes"`
	Snapshots []SnapshotRow     `json:"snapshots"`
	Capacity  host.NodeCapacity `json:"capacity"`
	At        time.Time         `json:"at"`
}

// InventoryAck tells a node what the gateway made of its inventory. Both lists
// are reported rather than acted on — nothing is deleted on a node's say-so,
// and nothing is deleted on a gateway's either.
//
// Orphaned is the names the gateway's ledger places on this machine that the
// machine did not report. Their placements are kept, not released: a machine
// that has been wiped or rolled back is indistinguishable from one whose
// sandbox really is gone, and the row is the user's record of where their work
// went.
//
// Quarantined is the names this machine reported that the gateway will not
// place here: a name another machine already holds, a name this gateway itself
// holds, or one whose owner or spelling is not something the platform issues.
// Nothing routes to those, so the sandboxes stay running and reachable to
// nobody until an operator resolves it.
//
// Both are bounded (see the gateway's maxAckNames): this is a diagnostic for a
// log line, and the full disagreement is logged in one line per name at the end
// that computed it.
type InventoryAck struct {
	Orphaned    []string `json:"orphaned,omitempty"`
	Quarantined []string `json:"quarantined,omitempty"`
}

// ChangedMsg is emitted on every lifecycle transition a node's manager makes,
// the reaper's pause and balloon included.
type ChangedMsg struct {
	Node    string     `json:"node"`
	Sandbox SandboxRow `json:"sandbox"`
	Reason  string     `json:"reason"`
	At      time.Time  `json:"at"`
}

// GoneMsg reports a sandbox the node no longer has.
type GoneMsg struct {
	Node   string `json:"node"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// PausedMsg is emitted from the node manager's session-closer hook, BEFORE the
// driver pause, so the gateway can hang up the sessions it holds open against a
// remotely reaped sandbox using the node's own wording.
type PausedMsg struct {
	Node   string `json:"node"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// --- lifecycle bodies ---
//
// One message type per fleet.Node method, so the catalogue is derivable from
// the interface instead of hand-maintained. The node performs no ownership
// check on any of them — that is the gateway's, always — and no name policy,
// but it does re-run everything that is its own hardware's business, which is
// how a *host.CapacityError still comes from the machine that would have had
// to find the RAM.

type NameReq struct {
	Name string `json:"name"`
}

type CreateReq struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Image string `json:"image"`
	VCPUs int64  `json:"vcpus"`
	MemMB int64  `json:"mem_mb"`
}

type ResizeReq struct {
	Name   string `json:"name"`
	SizeMB int64  `json:"size_mb"`
}

type RenameReq struct {
	Name    string `json:"name"`
	NewName string `json:"new_name"`
	Owner   string `json:"owner"`
}

type PinReq struct {
	Name   string `json:"name"`
	Pinned bool   `json:"pinned"`
}

type KeyReq struct {
	Name  string `json:"name"`
	KeyFP string `json:"key_fp"`
}

type SnapshotReq struct {
	Sandbox  string `json:"sandbox"`
	Snapshot string `json:"snapshot"`
	Owner    string `json:"owner"`
}

type DeleteSnapshotReq struct {
	Snapshot string `json:"snapshot"`
	Owner    string `json:"owner"`
}

type ForkReq struct {
	Snapshot string `json:"snapshot"`
	Name     string `json:"name"`
	Owner    string `json:"owner"`
	VCPUs    int64  `json:"vcpus"`
	MemMB    int64  `json:"mem_mb"`
}

type SandboxResp struct {
	Sandbox SandboxRow `json:"sandbox"`
}

type SnapshotResp struct {
	Snapshot SnapshotRow `json:"snapshot"`
}

// EmptyResp is the reply body of every operation whose success carries no
// information. It exists so a handler can return a value rather than nil and
// the reply frame stays uniform.
type EmptyResp struct{}

// VitalsResp is one reading of a sandbox's live counters, taken on the machine
// that runs it. Its request is a bare NameReq.
//
// It is the one read in this catalogue that is not served from the inventory
// cache, and it has to be: the inventory a node pushes is a lifecycle picture —
// state, sizes, lifetime totals — refreshed when something changes, whereas
// these are instrument readings whose whole value is that they are current to
// the second. Folding them into the inventory would either make every node
// broadcast a CPU sample a second to a gateway with nobody watching, or make
// the meters as stale as the last lifecycle event.
//
// The fields mirror host.Vitals exactly, pointers included, because a missing
// reading and a genuine zero are different facts all the way down: an absent
// cpu_seconds means this machine has no CPU stats for that sandbox, and a
// present 0.0 means it has used none. Every field is omitempty, so a node that
// can answer nothing sends `{}` — which is the same thing a gateway with no
// link at all renders, and deliberately so.
type VitalsResp struct {
	CPUSeconds *float64 `json:"cpu_seconds,omitempty"`
	MemUsedMB  *int64   `json:"mem_used_mb,omitempty"`
	NetRxBytes *uint64  `json:"net_rx_bytes,omitempty"`
	NetTxBytes *uint64  `json:"net_tx_bytes,omitempty"`
}

// ---------------------------------------------------------------------------
// The egress plane
// ---------------------------------------------------------------------------

// NetPolicyReq is one machine's whole egress policy, keyed by SANDBOX NAME.
//
// Not by tap, which is what the node's own sluice client is keyed by and what
// sluice itself enforces on. A tap name is derived from a guest's slot index
// (sbtap<idx>, see internal/netpush), and slot indices are assigned by the
// machine that holds the VM — so sbtap3 on the gateway and sbtap3 on a node are
// different sandboxes belonging to different people. Sending tap names would be
// sending one machine's addressing to another and hoping; the name is the only
// identifier both ends agree on, and the node re-derives its own taps from it.
//
// Like the sluice PUT it ends up as, this is a FULL SNAPSHOT: a sandbox absent
// from Allow is one no rule governs, which leaves its egress unrestricted. A
// governed sandbox that resolves to nothing is present with an empty list,
// which is a deliberate deny-all — so the two cases stay distinguishable across
// the link exactly as they are on one machine.
type NetPolicyReq struct {
	Allow map[string][]string `json:"allow"`
}

// NetUsageResp is the node's per-sandbox bandwidth, already re-labelled from
// tap to name by the machine that knows the mapping.
//
// It carries netpush's own types rather than a third copy of them. There are
// already two — sluice's report structs and netpush's mirror of them — and a
// third would be a set of json tags that has to be kept in step with the other
// two by hand, for a payload that is passed straight through.
type NetUsageResp struct {
	VMs []netpush.VMUsage `json:"vms"`
}

// ---------------------------------------------------------------------------
// Workload identity
// ---------------------------------------------------------------------------

// IdentityReq is a node asking its gateway to speak for one of the sandboxes on
// it. The name and the audience are the whole request, and that is the point:
// everything else about a sandbox — who owns it, what image it runs, which
// machine it is on — the gateway resolves from its own ledger and its own cache
// of this node's inventory, so nothing a node could assert reaches the claims.
//
// Aud empty means the gateway's default audience.
type IdentityReq struct {
	Sandbox string `json:"sandbox"`
	Aud     string `json:"aud,omitempty"`
}

// IdentityTokenResp is a signed id token and its expiry.
type IdentityTokenResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// IdentityDocResp is the unsigned claim set behind /run/sparkbox/identity.json.
// Its fields mirror metadata.Doc; they are restated here for the same reason
// SandboxRow restates host.Sandbox — the wire is its own contract.
type IdentityDocResp struct {
	Issuer  string `json:"iss"`
	Subject string `json:"sub"`
	Owner   string `json:"owner"`
	GitHub  string `json:"github,omitempty"`
	KeyFP   string `json:"key_fp,omitempty"`
	Sandbox string `json:"sandbox"`
	Image   string `json:"image"`
	Box     string `json:"box"`
}

// ---------------------------------------------------------------------------
// Node control certificate enrollment
// ---------------------------------------------------------------------------

const (
	// MaxCSRPEMBytes is far above a P-256 CSR while keeping the node->gateway
	// signing request a small control operation independent of MaxFrameBytes.
	MaxCSRPEMBytes = 16 << 10
	// MaxCertificatePEMBytes bounds each half of the enrollment response.
	MaxCertificatePEMBytes    = 64 << 10
	MaxGatewayIdentityBytes   = 512
	MaxGatewayGRPCAddrBytes   = 1024
	MaxCertificateSerialBytes = 128
)

// CertificateEnrollRequest carries only proof of the node's durable private
// key. Identity is supplied by the authenticated SSH link, never this payload.
type CertificateEnrollRequest struct {
	CSRPEM []byte `json:"csr_pem"`
}

// CertificateEnrollResponse is everything the node needs to install its leaf
// and authenticate the gateway's exact cluster identity.
type CertificateEnrollResponse struct {
	CertificatePEM   []byte `json:"certificate_pem"`
	CACertificatePEM []byte `json:"ca_certificate_pem"`
	GatewayIdentity  string `json:"gateway_identity"`
	// GatewayGRPCAddr is optional for rolling compatibility. A new gateway
	// advertises the tailnet endpoint hosting GatewayIdentity; an old gateway
	// omits it and the node keeps relaying identity over the SSH control link.
	GatewayGRPCAddr string    `json:"gateway_grpc_addr,omitempty"`
	Serial          string    `json:"serial"`
	ExpiresAt       time.Time `json:"expires_at"`
}
