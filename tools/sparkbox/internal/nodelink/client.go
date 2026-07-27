package nodelink

// The node half of a link: dial out to the gateway, prove this machine's
// identity with its own key, and hold one control channel open for as long as
// the process lives.
//
// The file is a supervisor more than a protocol. A node's link is the least
// reliable thing in the system — it crosses a network to a machine that gets
// restarted — and the property everything else here is arranged around is that
// none of that reaches the VMs on this box: RunClient returns only when its
// context is cancelled, so nothing it experiences can be mistaken by
// cmd/sparkbox for a reason to stop serving. A gateway can be down for a week
// and the sandboxes on this machine keep running.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// keepaliveRequest is OpenSSH's global keepalive, and it is the node's only
// half-open detector. Heartbeats are events, so a node whose TCP connection has
// quietly died writes them into a black hole forever and never learns anything;
// this one demands an answer. gliderlabs services incoming global requests on
// its own goroutine, so the answer arrives even while the gateway is busy.
const keepaliveRequest = "keepalive@openssh.com"

const (
	// DefaultBackoffMin and DefaultBackoffMax bound the reconnect delay: one
	// second, doubling, capped at a minute, jittered, forever. The cap is what
	// keeps a node that has been away all night back within a minute of the
	// gateway returning; the jitter is what keeps a rack of them from
	// rediscovering it in lockstep.
	DefaultBackoffMin = 1 * time.Second
	DefaultBackoffMax = 60 * time.Second

	// dialTimeout bounds the TCP connect and SSH handshake together.
	dialTimeout = 10 * time.Second
	// inventoryBudget bounds one full-picture report. It is generous because
	// the gateway writes a ledger transaction against it, and stingy enough
	// that a wedged gateway does not hold the link's only writer forever.
	inventoryBudget = 30 * time.Second
)

// Manager is the node's own host manager, narrowed to what a link asks of it:
// what this machine has left, what is on it, and the lifecycle operations a
// gateway may ask it to run (see nodeops.go).
//
// It is an interface rather than *host.Manager because everything outside this
// list is policy this package must not grow — no ownership check, no name
// policy, no placement — and because it lets a test drive a link without a
// driver, a state dir or a VM. The assertion below is what keeps the narrowing
// honest: every signature here is *host.Manager's own, including the four that
// carry no context or no error, which are exactly the four the fleet's Node
// interface has to adapt.
type Manager interface {
	Capacity() host.NodeCapacity
	List() []*host.Sandbox
	AllSnapshots() []*host.Snapshot

	// Get is the whole of the data plane's node half: a stream names a sandbox
	// and this is where the address comes from. It is a read of one record and
	// deliberately not List, because it runs on the path of every proxy request
	// to every guest on this machine.
	Get(name string) (*host.Sandbox, bool)

	// Vitals is the live-counter read. Unlike everything below it this is not a
	// lifecycle operation and changes nothing; it is here because a balloon and
	// a VMM process can only be asked of the machine running them, which is the
	// same reason every other method on this interface exists.
	Vitals(ctx context.Context, name string) (host.Vitals, error)

	Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error)
	EnsureReady(ctx context.Context, name string) (*host.Sandbox, error)
	Pause(ctx context.Context, name string) error
	Archive(ctx context.Context, name string) error
	Resize(ctx context.Context, name string, sizeMB int64) error
	Reboot(ctx context.Context, name string) error
	Rename(ctx context.Context, oldName, newName, owner string) error
	Destroy(ctx context.Context, name string) error
	SetPinned(name string, pinned bool) error
	ResyncEnv(ctx context.Context, name string)
	MarkActive(name string)
	RecordKey(name, fp string)

	Snapshot(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapName, owner string) error
	Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error)
}

var _ Manager = (*host.Manager)(nil)

// ClientOptions is everything a node needs to reach its gateway.
type ClientOptions struct {
	// Gateway is the host:port of the gateway's SSH listener.
	Gateway string
	// NodeName is what this machine calls itself. The roster's name wins over
	// it on the gateway; a mismatch is refused rather than silently resolved,
	// so this is the name an operator approved.
	NodeName string
	// Key is this node's own identity, minted on first boot and never rotated
	// by anything but an operator.
	Key xssh.Signer
	// HostKey pre-seeds the gateway host key pin; nil trusts the first key
	// offered and pins that.
	HostKey xssh.PublicKey
	// HostKeyPath is where a trusted-on-first-use pin is remembered. Empty
	// means the pin lives only as long as the process, which makes every
	// restart a fresh first use.
	HostKeyPath string

	Manager Manager
	Emitter *Emitter
	// Uplink, if set, is bound to each live link so this node can make requests
	// OF its gateway — today the two workload-identity calls. Nil is a node
	// that never asks for anything, which is every node before this existed.
	Uplink *Uplink
	// Net, if set, is the egress gateway on this machine: what a pushed policy
	// is applied to and what a usage request reads. Nil answers both with the
	// typed refusal in nodeops, so a gateway learns this machine meters
	// nothing rather than being handed an empty report it would render as an
	// idle VM.
	Net NetControl

	// Hello supplies the facts about this machine that only the process
	// assembling it knows — its build version, its release tag, which driver it
	// runs. Protocol and Node are stamped here whatever it returns.
	Hello func() Hello
	// OnWelcome runs before the link carries anything. A failure here fails the
	// link: the only thing a node does with a welcome is install the key its
	// guests will trust, and a machine that could not do that must not go on to
	// boot VMs nobody can log into.
	OnWelcome func(Welcome) error

	BackoffMin, BackoffMax time.Duration
	// Heartbeat overrides the reported cadence. Settable for the same reason
	// the server's ping interval is: a test must be able to drive the liveness
	// machinery without waiting fifteen seconds for each beat.
	Heartbeat time.Duration
	Log       *slog.Logger
	// Metrics is optional. Nil keeps the historical zero-instrumentation path.
	Metrics *fleetmetrics.Registry
}

// RunClient dials, links, serves and reconnects until ctx is done.
//
// It returns only ctx.Err(), and that is a contract rather than an accident:
// cmd/sparkbox's serve() returns on the first value in its error channel, so a
// supervisor that reported a transport failure upward would turn a routine
// gateway restart — under Restart=always — into a cold restart of every VM on
// this machine.
func RunClient(ctx context.Context, opts ClientOptions) error {
	c, err := newNodeClient(opts)
	if err != nil {
		return err
	}
	delay := c.backoffMin
	for ctx.Err() == nil {
		linked, err := c.runOnce(ctx)
		if ctx.Err() != nil {
			break
		}
		var refusal *ctlops.Error
		switch {
		case linked:
			// The link worked and then ended, which is what a gateway restart
			// looks like. Starting over from the minimum is what gets this node
			// back within a second of the gateway returning.
			delay = c.backoffMin
			if c.opts.Metrics != nil {
				c.opts.Metrics.IncReconnect(c.name, "ssh")
			}
			c.log.Info("gateway link ended; reconnecting", "err", err)
		case errors.As(err, &refusal):
			// The gateway answered and said no. The handshake has already
			// printed its sentence — repeating it here would double every line
			// an operator has to read.
			c.log.Debug("retrying after a refusal", "code", refusal.Code, "retry_in", delay)
		default:
			c.log.Warn("cannot reach the gateway", "retry_in", delay, "err", err)
		}
		if !sleepFor(ctx, jitter(delay)) {
			break
		}
		delay = min(delay*2, c.backoffMax)
	}
	c.log.Info("node link supervisor stopped", "gateway", c.opts.Gateway)
	return ctx.Err()
}

// jitter spreads a delay by ±20% so a fleet coming back from a shared outage
// does not arrive as one thundering herd.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // spreading retries, not keying anything
}

// sleepFor waits, reporting false if the wait was cut short by shutdown.
func sleepFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// nodeClient is one node's side of the link, reused across reconnects. The
// Client type in this package is the gateway's handle on a node; this is the
// node's handle on its gateway.
type nodeClient struct {
	opts      ClientOptions
	log       *slog.Logger
	name      string
	mgr       Manager
	emitter   *Emitter
	pin       *hostKeyPin
	heartbeat time.Duration

	backoffMin, backoffMax time.Duration
	startedAt              time.Time
	streams                *StreamLimiter
}

func newNodeClient(opts ClientOptions) (*nodeClient, error) {
	switch {
	case opts.Gateway == "":
		return nil, errors.New("nodelink: a node needs a gateway address to link to")
	case opts.NodeName == "":
		return nil, errors.New("nodelink: a node needs a name to introduce itself by")
	case opts.Key == nil:
		return nil, errors.New("nodelink: a node needs its own key to link with")
	case opts.Manager == nil:
		return nil, errors.New("nodelink: a node needs its host manager to report on")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("node", opts.NodeName, "gateway", opts.Gateway)

	pin, err := newHostKeyPin(opts.HostKey, opts.HostKeyPath, log)
	if err != nil {
		return nil, err
	}
	emitter := opts.Emitter
	if emitter == nil {
		emitter = NewEmitter(log)
	}
	emitter.SetMetrics(opts.Metrics, opts.NodeName)
	return &nodeClient{
		opts:       opts,
		log:        log,
		name:       opts.NodeName,
		mgr:        opts.Manager,
		emitter:    emitter,
		pin:        pin,
		heartbeat:  orDuration(opts.Heartbeat, DefaultHeartbeat),
		backoffMin: orDuration(opts.BackoffMin, DefaultBackoffMin),
		backoffMax: orDuration(opts.BackoffMax, DefaultBackoffMax),
		startedAt:  time.Now(),
		streams:    NewStreamLimiter(MaxLiveStreams),
	}, nil
}

// runOnce holds one link from dial to death. It reports whether the link ever
// came up, which is all the supervisor needs to tell "the gateway restarted"
// from "the gateway is not there".
func (c *nodeClient) runOnce(ctx context.Context) (linked bool, err error) {
	ssh, err := c.dial(ctx)
	if err != nil {
		return false, err
	}
	defer ssh.Close()

	linkCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The accept loop starts before the handshake, not after. An unserved
	// channel queue blocks x/crypto's mux loop — and with it every frame on
	// this connection — so the one thing a node must never do is leave an
	// inbound open unaccepted, even for the moment it takes to say hello.
	var controlStreams atomic.Bool
	go ServeStreamsWithOptions(
		linkCtx,
		ssh.HandleChannelOpen(StreamChannel),
		StreamResolver(c.mgr),
		c.log,
		c.opts.Metrics,
		c.name,
		"ssh",
		c.streams,
		&controlStreams,
	)

	sess, err := ssh.NewSession()
	if err != nil {
		return false, err
	}
	defer sess.Close() //nolint:errcheck // closing the connection is what ends the link
	stdin, err := sess.StdinPipe()
	if err != nil {
		return false, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := sess.Start(LinkCommand); err != nil {
		return false, err
	}

	conn := NewConn(stdout, writerOnly{stdin}, "n", c.log)
	conn.SetMetrics(c.opts.Metrics, c.name, "ssh")
	// Handlers derive their work from the PROCESS context, never the link's:
	// a fifteen-minute archive that a dropped connection could cancel would
	// tear down a running VM because a gateway restarted.
	conn.SetBaseContext(ctx)
	c.register(linkCtx, conn)
	defer conn.Close()

	served := make(chan error, 1)
	go func() { served <- conn.Serve(linkCtx) }()

	welcome, err := c.handshake(ctx, conn)
	if err != nil {
		return false, err
	}
	c.log.Info("linked to the gateway", "domain", welcome.Domain, "heartbeat_s", welcome.HeartbeatSeconds)
	if welcome.SupportsDataPool() {
		for lane := 0; lane < DefaultDataLanes; lane++ {
			go c.superviseDataLane(linkCtx, welcome.ControlGeneration, fmt.Sprintf("lane-%d", lane+1))
		}
	} else {
		// Combined-link fallback for either side of a rolling upgrade.
		controlStreams.Store(true)
	}

	detach := c.emitter.attach(conn, c.name)
	defer detach()
	// The uplink is bound at the same moment and released at the same moment,
	// and deliberately AFTER the handshake: a request made against a link whose
	// welcome has not landed would be answered by a gateway that has not yet
	// decided this machine is who it says it is.
	if c.opts.Uplink != nil {
		release := c.opts.Uplink.attach(conn)
		defer release()
	}
	// The first inventory is the gateway's whole picture of this machine, and
	// it goes before the first heartbeat so nothing arrives about a sandbox the
	// gateway has not been told exists.
	c.sendInventory(ctx, conn)
	go c.beat(linkCtx, conn, ssh)

	select {
	case err := <-served:
		return true, err
	case <-ctx.Done():
		// A planned shutdown says so, so the gateway marks this machine offline
		// now instead of waiting out the grace period wondering.
		if berr := conn.Event(TypeBye, Bye{Code: CodeShuttingDown, Msg: "this node is shutting down"}); berr != nil {
			c.log.Debug("nodelink: goodbye not delivered", "err", berr)
		}
		return true, ctx.Err()
	}
}

// dial opens the SSH connection the whole link rides on.
func (c *nodeClient) dial(ctx context.Context) (*xssh.Client, error) {
	d := net.Dialer{Timeout: dialTimeout}
	raw, err := d.DialContext(ctx, "tcp", c.opts.Gateway)
	if err != nil {
		return nil, err
	}
	// The handshake gets its own bound. ClientConfig.Timeout would not give it
	// one — it covers the TCP connect, which has already happened here — so a
	// machine that accepts and then says nothing would hold this attempt open
	// for as long as it cared to.
	if err := raw.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		raw.Close()
		return nil, err
	}
	sc, chans, reqs, err := xssh.NewClientConn(raw, c.opts.Gateway, &xssh.ClientConfig{
		User:            User,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(c.opts.Key)},
		HostKeyCallback: c.pin.check,
	})
	if err != nil {
		raw.Close()
		return nil, err
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		sc.Close()
		return nil, err
	}
	return xssh.NewClient(sc, chans, reqs), nil
}

// superviseDataLane keeps one independently replaceable SSH data connection
// attached for the lifetime of a control generation. A lane failure never
// reaches runOnce and therefore never marks the node offline or interrupts
// control RPCs.
func (c *nodeClient) superviseDataLane(ctx context.Context, generation, lane string) {
	delay := c.backoffMin
	for ctx.Err() == nil {
		linked, err := c.runDataLane(ctx, generation, lane)
		if ctx.Err() != nil {
			return
		}
		if linked {
			delay = c.backoffMin
			if c.opts.Metrics != nil {
				c.opts.Metrics.IncReconnect(c.name, "ssh-data")
			}
			c.log.Info("data lane ended; reconnecting", "lane", lane, "err", err)
		} else {
			c.log.Warn("cannot attach data lane", "lane", lane, "retry_in", delay, "err", err)
		}
		if !sleepFor(ctx, jitter(delay)) {
			return
		}
		delay = min(delay*2, c.backoffMax)
	}
}

func (c *nodeClient) runDataLane(ctx context.Context, generation, lane string) (bool, error) {
	ssh, err := c.dial(ctx)
	if err != nil {
		return false, err
	}
	defer ssh.Close()
	stop := context.AfterFunc(ctx, func() { _ = ssh.Close() })
	defer stop()

	go ServeStreamsWithOptions(
		ctx,
		ssh.HandleChannelOpen(StreamChannel),
		StreamResolver(c.mgr),
		c.log,
		c.opts.Metrics,
		c.name,
		"ssh-data",
		c.streams,
		nil,
	)

	sess, err := ssh.NewSession()
	if err != nil {
		return false, err
	}
	defer sess.Close() //nolint:errcheck
	stdin, err := sess.StdinPipe()
	if err != nil {
		return false, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := sess.Start(DataCommand); err != nil {
		return false, err
	}
	if err := json.NewEncoder(stdin).Encode(DataHello{
		Protocol: Protocol, Node: c.name, Generation: generation, Lane: lane,
	}); err != nil {
		return false, err
	}

	type result struct {
		welcome DataWelcome
		err     error
	}
	registered := make(chan result, 1)
	go func() {
		var welcome DataWelcome
		err := json.NewDecoder(stdout).Decode(&welcome)
		registered <- result{welcome: welcome, err: err}
	}()
	select {
	case got := <-registered:
		if got.err != nil {
			return false, fmt.Errorf("nodelink: data registration: %w", got.err)
		}
		if !got.welcome.Accepted {
			return false, fmt.Errorf("nodelink: data registration refused: %s", got.welcome.Error)
		}
	case <-time.After(HelloTimeout):
		return false, errors.New("nodelink: data registration timed out")
	case <-ctx.Done():
		return false, ctx.Err()
	}

	c.log.Info("data lane attached", "lane", lane)
	waited := make(chan error, 1)
	go func() { waited <- sess.Wait() }()
	ticker := time.NewTicker(c.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case err := <-waited:
			return true, err
		case <-ticker.C:
			if err := c.keepalive(ctx, ssh); err != nil {
				if c.opts.Metrics != nil {
					c.opts.Metrics.IncLivenessFailure(c.name, "ssh-data", "node")
				}
				return true, fmt.Errorf("nodelink: data lane keepalive: %w", err)
			}
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
}

// handshake introduces this machine and reads the answer.
//
// A refusal comes back as the typed error the rest of the control plane speaks,
// so it is rendered here rather than decoded: the gateway's sentence for a
// pending node carries the exact command that approves it, and this log line is
// the only place anyone will look for it.
func (c *nodeClient) handshake(ctx context.Context, conn *Conn) (Welcome, error) {
	hctx, cancel := context.WithTimeout(ctx, HelloTimeout)
	defer cancel()

	var w Welcome
	err := conn.Request(hctx, TypeHello, c.hello(), &w)
	if err != nil {
		var e *ctlops.Error
		if errors.As(err, &e) {
			// Waiting for approval is the normal first day of a node's life,
			// not a fault: an operator has to read a fingerprint and type a
			// command, and until then this node retries quietly.
			if e.Code == CodeNodePending {
				c.log.Info("this node is enrolled and waiting for approval",
					"code", e.Code, "detail", e.Msg)
			} else {
				c.log.Error("the gateway refused this node", "code", e.Code, "detail", e.Msg)
			}
		}
		return Welcome{}, err
	}
	if w.Protocol != Protocol {
		return Welcome{}, fmt.Errorf("nodelink: the gateway speaks node protocol %d, this node speaks %d", w.Protocol, Protocol)
	}

	if c.opts.OnWelcome != nil {
		if err := c.opts.OnWelcome(w); err != nil {
			return Welcome{}, fmt.Errorf("nodelink: welcome not accepted: %w", err)
		}
	}
	return w, nil
}

// hello is this machine's introduction, with the two fields no caller may state
// differently stamped over whatever it supplied: the protocol this build
// speaks, and the name this node was configured with.
func (c *nodeClient) hello() Hello {
	var h Hello
	if c.opts.Hello != nil {
		h = c.opts.Hello()
	}
	h.Protocol = Protocol
	h.Node = c.name
	if !hasCapability(h.Capabilities, CapabilitySSHDataPoolV1) {
		h.Capabilities = append(h.Capabilities, CapabilitySSHDataPoolV1)
	}
	if h.Arch == "" {
		h.Arch = runtime.GOARCH
	}
	if h.OS == "" {
		h.OS = runtime.GOOS
	}
	if h.StartedAt.IsZero() {
		h.StartedAt = c.startedAt
	}
	return h
}

// writerOnly hides a session's stdin pipe behind a plain io.Writer.
//
// Conn closes what it can when the link ends, from whichever goroutine noticed,
// and closing a stdin pipe means CloseWrite — which x/crypto implements by
// setting a field every in-flight Write reads without a lock (channel.go:248
// against :589). That is a data race the detector finds on the first heartbeat
// that overlaps a teardown. Closing the channel itself has no such problem, and
// the Conn still does it: the reader half handed to it above IS the channel.
type writerOnly struct{ w io.Writer }

func (w writerOnly) Write(p []byte) (int, error) { return w.w.Write(p) }

// register wires what the gateway may ask of this node: the link's own
// housekeeping here, and every lifecycle verb in nodeops.go. An unregistered
// type is answered with a sentence rather than silence, so a version skew reads
// as a log line instead of a hung caller.
//
// ctx is the LINK's context. It bounds only the bookkeeping drain registerOps
// starts — the handlers themselves derive their work from the PROCESS context
// installed by SetBaseContext, because a fifteen-minute archive that a dropped
// connection could cancel would tear down a running VM because a gateway
// restarted.
func (c *nodeClient) register(ctx context.Context, conn *Conn) {
	conn.Handle(TypePing, func(_ context.Context, raw json.RawMessage) (any, error) {
		var req PingReq
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, ctlops.Invalid(TypePing, "bad_ping", "that ping could not be read: %v", err)
		}
		return req, nil
	})

	conn.Handle(TypeInventory, func(context.Context, json.RawMessage) (any, error) {
		return c.inventory(), nil
	})

	// A gateway that says goodbye has superseded this link or is going away.
	// Closing now rather than waiting for the stream to end is what turns a
	// supersession into an immediate reconnect.
	conn.OnEvent(TypeBye, func(raw json.RawMessage) {
		var b Bye
		_ = json.Unmarshal(raw, &b)
		c.log.Info("the gateway said goodbye", "code", b.Code, "msg", b.Msg)
		_ = conn.Close()
	})

	registerOps(ctx, conn, c.mgr, c.log)
	registerNetOps(conn, c.name, c.opts.Net)
}

// beat is the node's own cadence: a capacity report the gateway does not have
// to ask for, and a keepalive it has to answer.
func (c *nodeClient) beat(ctx context.Context, conn *Conn, ssh xssh.Conn) {
	// One immediately, so a gateway that has just admitted this node knows what
	// it has before the first tick rather than fifteen seconds later.
	c.sendHeartbeat(conn)

	t := time.NewTicker(c.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.emitter.stale():
			// Events were dropped, so the incremental picture is wrong. One
			// full inventory replaces however many were lost.
			c.sendInventory(ctx, conn)
		case <-t.C:
			c.sendHeartbeat(conn)
			if err := c.keepalive(ctx, ssh); err != nil {
				if ctx.Err() != nil {
					return
				}
				// Nothing else on this link would ever have noticed: heartbeats
				// are events, and a half-open socket accepts them forever.
				c.log.Warn("the gateway stopped answering keepalives; dropping the link", "err", err)
				if c.opts.Metrics != nil {
					c.opts.Metrics.IncLivenessFailure(c.name, "ssh", "node")
				}
				conn.Fail(err)
				_ = conn.Close()
				return
			}
		}
	}
}

func (c *nodeClient) sendHeartbeat(conn *Conn) {
	hb := Heartbeat{Capacity: c.mgr.Capacity(), At: time.Now()}
	if err := conn.Event(TypeHeartbeat, hb); err != nil {
		c.log.Debug("nodelink: heartbeat not delivered", "err", err)
	}
}

// keepalive is bounded by hand because SendRequest is not: on a half-open
// socket it would block until the kernel gives up on its retransmits, which is
// minutes after the point of noticing.
func (c *nodeClient) keepalive(ctx context.Context, ssh xssh.Conn) error {
	done := make(chan error, 1)
	go func() {
		_, _, err := ssh.SendRequest(keepaliveRequest, true, nil)
		done <- err
	}()
	budget := max(2*c.heartbeat, 2*time.Second)
	t := time.NewTimer(budget)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		return fmt.Errorf("no keepalive answer within %s", budget)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendInventory reports this machine's whole picture and waits for the answer.
// It is a request rather than an event because the gateway's reply says which
// of these names it disagrees with, and a node that never heard back would keep
// resending the same picture.
func (c *nodeClient) sendInventory(ctx context.Context, conn *Conn) {
	ctx, cancel := context.WithTimeout(ctx, inventoryBudget)
	defer cancel()

	var ack InventoryAck
	if err := conn.Request(ctx, TypeInventory, c.inventory(), &ack); err != nil {
		c.log.Warn("the gateway did not take this node's inventory", "err", err)
		return
	}
	// Reported, never acted on: a name the gateway does not place here is a
	// disagreement for an operator to resolve, and deleting a rootfs over one
	// would be the fleet destroying user data on its own initiative. The
	// gateway's half is the same undertaking in the other direction — it marks
	// the placements it disagrees with and releases none of them — so a
	// disagreement costs both sides a log line and nobody a sandbox.
	if len(ack.Orphaned) > 0 || len(ack.Quarantined) > 0 {
		c.log.Warn("the gateway disagrees about what this node holds",
			"orphaned", ack.Orphaned, "quarantined", ack.Quarantined)
	}
}

func (c *nodeClient) inventory() InventoryMsg {
	inv := InventoryMsg{Node: c.name, Capacity: c.mgr.Capacity(), At: time.Now()}
	for _, b := range c.mgr.List() {
		inv.Sandboxes = append(inv.Sandboxes, sandboxRow(b))
	}
	for _, s := range c.mgr.AllSnapshots() {
		inv.Snapshots = append(inv.Snapshots, snapshotRow(s))
	}
	return inv
}

// hostKeyPin is trust-on-first-use for the gateway's host key.
//
// A node has no operator at the keyboard to answer the usual prompt, so the
// first key offered is taken and remembered, and any later change is refused
// with both fingerprints printed. That is weaker than a pin seeded out of band
// (--gateway-host-key) and much stronger than accepting whatever answers on
// that address today.
type hostKeyPin struct {
	log  *slog.Logger
	path string

	mu  sync.Mutex
	key xssh.PublicKey
}

func newHostKeyPin(key xssh.PublicKey, path string, log *slog.Logger) (*hostKeyPin, error) {
	p := &hostKeyPin{log: log, path: path, key: key}
	if key != nil || path == "" {
		return p, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	pinned, _, _, _, err := xssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, fmt.Errorf("nodelink: %s does not hold a usable gateway host key: %w", path, err)
	}
	p.key = pinned
	return p, nil
}

func (p *hostKeyPin) check(_ string, _ net.Addr, key xssh.PublicKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.key != nil {
		if bytes.Equal(p.key.Marshal(), key.Marshal()) {
			return nil
		}
		return fmt.Errorf("the gateway's host key changed: this node trusts %s, the machine answering offers %s.%s",
			xssh.FingerprintSHA256(p.key), xssh.FingerprintSHA256(key), p.rekeyHint())
	}

	p.key = key
	p.log.Info("trusting the gateway's host key on first use", "fp", xssh.FingerprintSHA256(key))
	if p.path != "" {
		if err := os.WriteFile(p.path, xssh.MarshalAuthorizedKey(key), 0o644); err != nil { //nolint:gosec // a public key
			// Not fatal: the pin holds for this process, and the alternative is
			// refusing to link because a file could not be written.
			p.log.Warn("could not remember the gateway's host key", "path", p.path, "err", err)
		}
	}
	return nil
}

// rekeyHint names the file to delete, when there is one. A pin that lives only
// in memory is undone by restarting the node, and saying so is more useful than
// naming an empty path.
func (p *hostKeyPin) rekeyHint() string {
	if p.path == "" {
		return " If the gateway was deliberately rekeyed, restart this node."
	}
	return fmt.Sprintf(" If the gateway was deliberately rekeyed, delete %s.", p.path)
}
