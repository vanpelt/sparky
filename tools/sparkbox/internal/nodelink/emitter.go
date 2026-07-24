package nodelink

// The node's outbound event stream: how a machine tells its gateway that
// something on it changed, without ever waiting to hear back.
//
// Every send in this file is non-blocking by construction, and that is the
// whole design. These hooks fire from inside the node manager's lock — both
// host.Observer and host.SessionCloser say in as many words that an
// implementation must not block — so an emitter that could wait on a dead link
// would stall every lifecycle operation on the machine, which is precisely the
// failure the fleet is arranged to make impossible.
//
// Events are therefore lossy on purpose. Nothing downstream may depend on any
// individual one arriving: a dropped event is answered with one full inventory,
// and a fresh link opens with one anyway.

import (
	"log/slog"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// The emitter is the node manager's whole view of the gateway.
var (
	_ host.Observer      = (*Emitter)(nil)
	_ host.SessionCloser = (*Emitter)(nil)
)

// eventQueue is how many events may be waiting for the link at once. It is
// generous enough that a burst of lifecycle transitions rides through
// untouched, and finite because the alternative to dropping is blocking the
// manager.
const eventQueue = 256

// event is one queued message: the type and the body, still unmarshalled. The
// JSON happens on the drain goroutine rather than in the manager's lock.
type event struct {
	typ  string
	body any
}

// Emitter is the host.Observer and host.SessionCloser a node installs on its
// manager. It holds no link of its own: RunClient binds it to each live link
// and unbinds it when that link ends, so a node with no gateway does no work at
// all per transition.
type Emitter struct {
	log *slog.Logger

	// resync carries "you missed something". One slot, because the answer to
	// any number of dropped events is the same single inventory.
	resync chan struct{}

	mu   sync.Mutex
	node string
	q    chan event
}

// NewEmitter returns an emitter with no link. It is safe to install on a
// manager before a gateway has ever been reached.
func NewEmitter(log *slog.Logger) *Emitter {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Emitter{log: log, resync: make(chan struct{}, 1)}
}

// SandboxChanged reports a lifecycle transition, the reaper's pause and balloon
// included. reason is the transition, not a sentence.
func (e *Emitter) SandboxChanged(b *host.Sandbox, reason string) {
	if b == nil {
		return
	}
	e.send(TypeChanged, func(node string) any {
		return ChangedMsg{Node: node, Sandbox: sandboxRow(b), Reason: reason, At: time.Now()}
	})
}

// SandboxGone reports a record that no longer exists here. The gateway does not
// act on it — nothing is deleted on a node's say-so — but a listing that still
// showed it would be a sandbox nobody can reach.
func (e *Emitter) SandboxGone(name string) {
	e.send(TypeGone, func(node string) any {
		return GoneMsg{Node: node, Name: name, Reason: "destroyed"}
	})
}

// CloseSandboxSessions is host.SessionCloser on a machine that holds no
// sessions. Every interactive session in a fleet terminates at the gateway, so
// there is nothing here to hang up; what the gateway needs is the news, early
// enough to hang up its own — this fires before the driver pause, carrying the
// node's own wording so the user reads the same sentence a local pause prints.
//
// It always returns zero, immediately. The count is only ever a log line, and
// the manager is holding its lock.
func (e *Emitter) CloseSandboxSessions(sandbox, reason string) int {
	e.send(TypePaused, func(node string) any {
		return PausedMsg{Node: node, Name: sandbox, Reason: reason}
	})
	return 0
}

// send queues one event for the link, or drops it.
//
// The body is built under the lock, and only when a link exists: the caller
// hands over a builder rather than a value so that an unlinked node — every
// single-box deployment, and every node between reconnects — pays nothing per
// transition beyond a mutex it was going to take anyway.
func (e *Emitter) send(typ string, build func(node string) any) {
	e.mu.Lock()
	q := e.q
	if q == nil {
		// No link. Not a drop worth recording: the next handshake sends the
		// whole picture, which supersedes anything that would have been queued.
		e.mu.Unlock()
		return
	}
	ev := event{typ: typ, body: build(e.node)}
	e.mu.Unlock()

	select {
	case q <- ev:
	default:
		e.log.Warn("nodelink: event queue full; asking the gateway to resync", "type", typ)
		e.markStale()
	}
}

// markStale asks for one full inventory. Non-blocking: a second drop while one
// is already pending needs no second inventory.
func (e *Emitter) markStale() {
	select {
	case e.resync <- struct{}{}:
	default:
	}
}

// stale fires when events were dropped and the gateway's picture has to be
// rebuilt from scratch.
func (e *Emitter) stale() <-chan struct{} { return e.resync }

// attach binds the emitter to a live link and returns the function that unbinds
// it. Each link gets its own queue: events that piled up against a link that
// has since died are stale by the time a new one exists, and the handshake
// sends a full inventory over the new one regardless.
func (e *Emitter) attach(conn *Conn, node string) func() {
	q := make(chan event, eventQueue)
	stop := make(chan struct{})

	e.mu.Lock()
	e.q, e.node = q, node
	e.mu.Unlock()

	go e.drain(conn, q, stop)

	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			// Only ever unbind the queue this attachment installed, so a link
			// tearing itself down cannot detach the one that replaced it.
			if e.q == q {
				e.q, e.node = nil, ""
			}
			e.mu.Unlock()
			close(stop)
		})
	}
}

// drain writes queued events onto the link. A write that fails means the link
// died under it; nothing is retried, because the next link's inventory is the
// retry.
func (e *Emitter) drain(conn *Conn, q <-chan event, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case ev := <-q:
			if err := conn.Event(ev.typ, ev.body); err != nil {
				e.log.Debug("nodelink: event not delivered", "type", ev.typ, "err", err)
			}
		}
	}
}

// sandboxRow projects a node's own record into the wire's view of it.
//
// The omissions are the point: HostIP, SSHAddr and GuestV6 do not cross the
// link, because every node mints the same guest addresses and an address the
// gateway holds is one something up there can dial. RenamedFrom and ArchiveKey
// stay home too — they are this machine's crash-recovery bookkeeping and mean
// nothing anywhere else.
func sandboxRow(b *host.Sandbox) SandboxRow {
	return SandboxRow{
		Name:        b.Name,
		Owner:       b.Owner,
		Image:       b.Image,
		State:       string(b.State),
		VCPUs:       b.VCPUs,
		MemMB:       b.MemMB,
		DiskMB:      b.DiskMB,
		DiskTotalMB: b.DiskTotalMB,
		Pinned:      b.Pinned,
		Ballooned:   b.Ballooned,
		SSHUser:     b.SSHUser,
		KeyFP:       b.KeyFP,
		NetRxBytes:  b.NetRxBytes,
		NetTxBytes:  b.NetTxBytes,
		ArchivedAt:  b.ArchivedAt,
		CreatedAt:   b.CreatedAt,
		LastActive:  b.LastActive,
	}
}

// snapshotRow projects a fork template. Its Node field is dropped rather than
// forwarded: the link the row arrives on is what says which machine holds the
// file, and a name in the payload could only ever disagree with it.
func snapshotRow(s *host.Snapshot) SnapshotRow {
	return SnapshotRow{
		Name:      s.Name,
		Owner:     s.Owner,
		Image:     s.Image,
		FromBox:   s.FromBox,
		CreatedAt: s.CreatedAt,
	}
}
