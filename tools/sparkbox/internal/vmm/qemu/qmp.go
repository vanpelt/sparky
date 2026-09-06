//go:build linux

package qemu

// The QMP client. This file owns type qmpConn and nothing else; see the
// contract block in qemu.go.
//
// QMP is newline-delimited JSON over a Unix stream socket, and it is not a
// request/response protocol: the server interleaves asynchronous events with
// command replies on the same connection, and it sends one unsolicited greeting
// before anything else. Three consequences shape everything below.
//
//  1. The greeting must be consumed BEFORE qmp_capabilities. A client that
//     treats the first line as a reply is off by one message forever, and the
//     symptom is every command appearing to return the previous command's
//     answer — which reads as a driver bug, not a framing bug.
//
//  2. Exactly one goroutine owns the decoder for the connection's whole life
//     and always drains it. Events (BALLOON_CHANGE at the balloon stats polling
//     rate, plus STOP/RESUME/MIGRATION/SHUTDOWN) arrive whether or not a command
//     is outstanding, so a client that reads only while awaiting a reply leaves
//     them in the socket; QMP is a chardev, and a reader that stalls eventually
//     stalls the monitor. That is why replies are routed to waiters by id rather
//     than read inline by the calling goroutine.
//
//  3. The reply/event discriminator is the presence of the "event" key. Nothing
//     else distinguishes them — an event has no "id" even when the outstanding
//     command did.
//
// Events are decoded and discarded. Draining them is the load-bearing part —
// an undrained monitor stops delivering replies — and nothing in the driver
// needs to observe one, so there is no event stream to subscribe to. If a
// consumer ever appears (BALLOON_CHANGE is the likely first), add the channel
// with it: a handler must never issue a QMP command from inside the reader's
// goroutine, because the reply could only be read by the goroutine the handler
// is blocking.
//
// In-band commands execute serially, so replies do in fact arrive in request
// order and an id-free client "works" — right up until the first event lands
// between a command and its reply. Ids are used anyway.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

const (
	// qmpHandshakeTimeout bounds greeting + qmp_capabilities when the caller's
	// context carries no deadline of its own.
	qmpHandshakeTimeout = 10 * time.Second
	// qmpWriteTimeout bounds a single command write for the same reason. A write
	// only blocks if the VMM has stopped reading its monitor.
	qmpWriteTimeout = 10 * time.Second
	// qmpCommandTimeout bounds one command's REPLY when the caller's context
	// carries no deadline, exactly as qmpWriteTimeout does for the write half.
	// Deadline-less callers are the common case rather than the exotic one:
	// host.Manager.RunReaper drives Pause and SetBalloonTarget on the
	// process-lifetime context, and vmmtest polls BalloonStats on
	// context.Background(). Every command this driver issues is answered by
	// QEMU's main loop immediately or not at all — even `migrate` and `balloon`
	// reply before doing the work — so a wedged main loop with a socket still
	// open is the only thing this can fire on, and without it that wedge blocks
	// a caller forever while it holds the driver-wide mutex.
	qmpCommandTimeout = 30 * time.Second
	// qmpMigrationPollInterval paces AwaitMigration. The spike polled at 100ms and
	// a 1 GiB guest's file: migration completed well inside its 30s budget.
	qmpMigrationPollInterval = 100 * time.Millisecond
	// qmpRunnablePollInterval paces AwaitRunnable, which only ever waits out the
	// tail of an incoming migration load.
	qmpRunnablePollInterval = 50 * time.Millisecond

	// qmpRunStateInMigrate is the runstate a destination sits in while it loads an
	// incoming migration stream. `cont` fails until it leaves.
	qmpRunStateInMigrate = "inmigrate"

	// qmpBalloonStatUnset is what an unsampled virtio-balloon statistic reads as:
	// QEMU initialises the stat array to -1 and marshals the fields as uint64.
	// Decoding these into int64 fails outright and into float64 quietly yields
	// ~1.8e19, so they are decoded as uint64 and this value means "no sample".
	qmpBalloonStatUnset = ^uint64(0)
)

// errQMPClosed reports that the monitor connection went away — the VMM exited,
// something closed the socket, or the decoder hit a framing error. Quit expects
// it (QEMU may exit before it manages to reply) and everything else treats it
// as fatal to the connection.
var errQMPClosed = errors.New("qmp: connection closed")

// errBalloonNoSample reports that virtio-balloon has not produced a guest stats
// sample yet: the polling interval is a QOM property that must be set and one
// interval must elapse. Callers must surface this as an error rather than
// substituting zeros — zeroed stats make host.Manager.MemStats charge every
// sandbox its full ceiling and balloon innocent ones.
var errBalloonNoSample = errors.New("qmp: balloon guest stats have no sample yet")

// qmpError is a command failure reported by QEMU, carrying both halves of the
// QMP error object. Only `desc` is descriptive; `class` is a coarse enum
// (GenericError, CommandNotFound, DeviceNotActive, DeviceNotFound,
// KVMMissingCap in 8.2) worth branching on in only two cases — DeviceNotActive
// for "there is no balloon" and CommandNotFound for version skew.
type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *qmpError) Error() string {
	if e.Class == "" {
		return "qmp: " + e.Desc
	}
	return "qmp: " + e.Class + ": " + e.Desc
}

// qmpMessage is any line the server sends. Which kind it is, is decided by
// which fields are present: "QMP" for the greeting, "event" for an event,
// otherwise "return" or "error" for a reply.
type qmpMessage struct {
	Greeting json.RawMessage `json:"QMP"`
	Event    string          `json:"event"`
	Data     json.RawMessage `json:"data"`
	ID       uint64          `json:"id"`
	Return   json.RawMessage `json:"return"`
	Error    *qmpError       `json:"error"`
}

// qmpRequest is one command. `id` is echoed verbatim in the reply, which is
// what lets the reader route replies to waiters.
type qmpRequest struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
	ID        uint64 `json:"id"`
}

// qmpConn is a live monitor connection. It is safe for concurrent use: the
// driver holds one per running VM and calls it from whichever goroutine is
// servicing a request.
type qmpConn struct {
	conn net.Conn
	dec  *json.Decoder

	// wmu serializes writers. QMP frames are newline-delimited, so two
	// interleaved writes would corrupt both.
	wmu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan *qmpMessage
	readErr error

	done      chan struct{} // closed once the reader goroutine has exited
	closeOnce sync.Once
}

// dialQMP connects to a QEMU monitor socket, consumes the greeting and
// completes the capabilities handshake. The context bounds the whole of that;
// it does not bound the connection's later life.
func dialQMP(ctx context.Context, sockPath string) (*qmpConn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("qmp: dial %s: %w", sockPath, err)
	}
	c, err := newQMPConn(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// newQMPConn runs the handshake on an already-connected socket and starts the
// reader. Split out from dialQMP so tests can drive the client over net.Pipe.
// On error the caller owns closing conn.
func newQMPConn(ctx context.Context, conn net.Conn) (*qmpConn, error) {
	c := &qmpConn{
		conn:    conn,
		dec:     json.NewDecoder(conn),
		pending: map[uint64]chan *qmpMessage{},
		done:    make(chan struct{}),
	}
	if err := c.handshake(ctx); err != nil {
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

// handshake reads the unsolicited greeting and negotiates capabilities. It runs
// before the reader goroutine exists, so it owns the decoder outright and does
// its own deadline management.
func (c *qmpConn) handshake(ctx context.Context) error {
	deadline := time.Now().Add(qmpHandshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("qmp: handshake: %w", err)
	}
	defer func() { c.conn.SetDeadline(time.Time{}) }()
	// A deadline in the past fails any in-flight and subsequent I/O at once,
	// which is how a blocking Decode is made to honour cancellation.
	stop := context.AfterFunc(ctx, func() { c.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	var greeting qmpMessage
	if err := c.dec.Decode(&greeting); err != nil {
		return fmt.Errorf("qmp: reading greeting: %w", qmpCtxErr(ctx, err))
	}
	if greeting.Greeting == nil {
		return fmt.Errorf("qmp: first message was not a greeting (event %q)", greeting.Event)
	}

	c.nextID++
	id := c.nextID
	if err := c.writeRequest(qmpRequest{Execute: "qmp_capabilities", ID: id}); err != nil {
		return fmt.Errorf("qmp: negotiating capabilities: %w", qmpCtxErr(ctx, err))
	}
	for {
		var m qmpMessage
		if err := c.dec.Decode(&m); err != nil {
			return fmt.Errorf("qmp: negotiating capabilities: %w", qmpCtxErr(ctx, err))
		}
		// QEMU does not emit events before negotiation completes, but skipping
		// them here costs nothing and keeps the one framing rule uniform.
		if m.Event != "" {
			continue
		}
		if m.Greeting != nil {
			continue
		}
		if m.Error != nil {
			return fmt.Errorf("qmp: negotiating capabilities: %w", m.Error)
		}
		return nil
	}
}

// readLoop owns the decoder for the rest of the connection's life. It never
// blocks on anything but the socket.
func (c *qmpConn) readLoop() {
	for {
		var m qmpMessage
		if err := c.dec.Decode(&m); err != nil {
			c.shutdown(err)
			return
		}
		switch {
		case m.Event != "":
			// Decoded and dropped. Draining is what matters: an undrained
			// monitor stops delivering replies.
		case m.Greeting != nil:
			// Only a reconnected socket could produce a second greeting, and we
			// never reconnect in place. Ignore rather than tear down.
		case m.Return != nil || m.Error != nil:
			c.deliver(&m)
		}
	}
}

// deliver routes a reply to its waiter. A reply with no waiter is a command
// whose caller's context expired; dropping it is correct.
func (c *qmpConn) deliver(m *qmpMessage) {
	c.mu.Lock()
	ch := c.pending[m.ID]
	delete(c.pending, m.ID)
	c.mu.Unlock()
	if ch != nil {
		ch <- m // buffered with room for exactly this one reply
	}
}

// shutdown records why the connection ended, fails every waiter, and closes the
// socket. Idempotent, and safe to race with Close.
func (c *qmpConn) shutdown(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		if err == nil {
			err = errQMPClosed
		}
		c.readErr = err
		pending := c.pending
		c.pending = map[uint64]chan *qmpMessage{}
		c.mu.Unlock()
		for _, ch := range pending {
			close(ch)
		}
		close(c.done)
		c.conn.Close() //nolint:errcheck // the connection is already going away
	})
}

// Close tears down the connection and waits for the reader to exit, so a caller
// that closes and then reuses the socket path cannot race the old reader.
//
// Note that closing the monitor does NOT stop QEMU — there is no analogue of
// the Firecracker SDK's "cancel the context and the VMM dies". Only stopVMM
// ends a process.
func (c *qmpConn) Close() error {
	err := c.conn.Close()
	<-c.done
	return err
}

// closedErr explains a connection that has gone away, always wrapping
// errQMPClosed so callers can test for it with errors.Is.
func (c *qmpConn) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil && !errors.Is(c.readErr, errQMPClosed) {
		return fmt.Errorf("%w (%v)", errQMPClosed, c.readErr)
	}
	return errQMPClosed
}

// Execute issues one command and waits for its reply. args may be nil; out may
// be nil to discard the return value. The context bounds the whole call: on
// cancellation the waiter is dropped and the eventual reply discarded, so a
// wedged VMM costs one abandoned id rather than a stuck driver goroutine.
//
// A caller with no deadline of its own gets qmpCommandTimeout, so "no deadline"
// never means "wait forever holding d.mu".
func (c *qmpConn) Execute(ctx context.Context, cmd string, args, out any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("qmp %s: %w", cmd, err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, qmpCommandTimeout)
		defer cancel()
	}

	c.mu.Lock()
	if c.readErr != nil {
		c.mu.Unlock()
		return fmt.Errorf("qmp %s: %w", cmd, c.closedErr())
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *qmpMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.writeCommand(ctx, cmd, args, id); err != nil {
		c.abandon(id)
		return err
	}

	select {
	case <-ctx.Done():
		c.abandon(id)
		return fmt.Errorf("qmp %s: %w", cmd, ctx.Err())
	case <-c.done:
		return fmt.Errorf("qmp %s: %w", cmd, c.closedErr())
	case m, ok := <-ch:
		if !ok {
			return fmt.Errorf("qmp %s: %w", cmd, c.closedErr())
		}
		if m.Error != nil {
			return fmt.Errorf("qmp %s: %w", cmd, m.Error)
		}
		if out == nil {
			return nil
		}
		ret := m.Return
		if len(ret) == 0 {
			ret = []byte("{}")
		}
		if err := json.Unmarshal(ret, out); err != nil {
			return fmt.Errorf("qmp %s: decoding reply: %w", cmd, err)
		}
		return nil
	}
}

// abandon forgets a waiter whose caller gave up. The reader tolerates the reply
// arriving later with nobody to hand it to.
func (c *qmpConn) abandon(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *qmpConn) writeCommand(ctx context.Context, cmd string, args any, id uint64) error {
	req := qmpRequest{Execute: cmd, Arguments: args, ID: id}
	deadline := time.Now().Add(qmpWriteTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("qmp %s: %w", cmd, err)
	}
	// Reset the deadline on the way out; best effort, the connection is only
	// interesting while it works.
	defer func() { c.conn.SetWriteDeadline(time.Time{}) }()
	// A deadline in the past fails a blocked write at once, which is how
	// cancellation reaches an I/O call that does not take a context.
	stop := context.AfterFunc(ctx, func() { c.conn.SetWriteDeadline(time.Unix(1, 0)) })
	defer stop()

	if err := c.writeRequest(req); err != nil {
		// A write that failed part-way through has desynchronised the frame
		// stream, and one that failed outright means the monitor is gone. Either
		// way the connection is finished: tear it down so every other waiter
		// fails now rather than blocking on a reply that cannot come.
		c.shutdown(err)
		return fmt.Errorf("qmp %s: write: %w", cmd, qmpCtxErr(ctx, err))
	}
	return nil
}

// writeRequest marshals and writes one frame. Callers hold c.wmu (or, during
// the handshake, own the connection outright).
func (c *qmpConn) writeRequest(req qmpRequest) error {
	buf, err := json.Marshal(req)
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	_, err = c.conn.Write(buf)
	return err
}

// qmpCtxErr prefers the context's own error when a deadline we set on the socket
// on the context's behalf is what actually fired, so a cancelled call reports
// context.DeadlineExceeded rather than "i/o timeout".
func qmpCtxErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && qmpIsTimeout(err) {
		return cerr
	}
	return err
}

func qmpIsTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// qmpIsDisconnect reports whether err means the peer went away rather than
// refused something. Quit expects exactly this: QEMU may exit before its reply
// is flushed.
func qmpIsDisconnect(err error) bool {
	return errors.Is(err, errQMPClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

// ---------------------------------------------------------------------------
// The commands the driver actually issues. Typed wrappers, so no caller
// hand-builds JSON and no caller has to remember that `balloon` speaks bytes.
// ---------------------------------------------------------------------------

// Stop pauses the guest's vCPUs. It is the first step of Pause; the guest's
// memory is still in the VMM until a migration writes it out.
func (c *qmpConn) Stop(ctx context.Context) error {
	return c.Execute(ctx, "stop", nil, nil)
}

// Cont resumes the vCPUs. On a restore it fails until the incoming migration
// has finished loading — see AwaitRunnable.
func (c *qmpConn) Cont(ctx context.Context) error {
	return c.Execute(ctx, "cont", nil, nil)
}

// Quit asks the VMM to exit. A missing reply is success, not failure: QEMU may
// close the monitor as it goes. The caller must still wait for the process to
// be reaped before touching the rootfs or the migration file — both stay open
// until exit.
func (c *qmpConn) Quit(ctx context.Context) error {
	if err := c.Execute(ctx, "quit", nil, nil); err != nil && !qmpIsDisconnect(err) {
		return err
	}
	return nil
}

type qmpStatus struct {
	Status string `json:"status"`
}

// QueryStatus returns the RunState string: running, paused, inmigrate,
// postmigrate, prelaunch, shutdown, guest-panicked, and so on.
func (c *qmpConn) QueryStatus(ctx context.Context) (string, error) {
	var s qmpStatus
	if err := c.Execute(ctx, "query-status", nil, &s); err != nil {
		return "", err
	}
	return s.Status, nil
}

// AwaitRunnable blocks until the destination of an incoming migration has
// finished loading it. Issuing `cont` as soon as the monitor socket appears
// fails on every single first attempt with "Migration is not finalized"; the
// spike's probe only worked because it retried in a loop.
func (c *qmpConn) AwaitRunnable(ctx context.Context) error {
	ticker := time.NewTicker(qmpRunnablePollInterval)
	defer ticker.Stop()
	for {
		state, err := c.QueryStatus(ctx)
		if err != nil {
			return err
		}
		if state != qmpRunStateInMigrate {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("qmp: still loading the incoming migration after %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// MigrateToFile starts writing the VM's memory and device state to path. It
// returns as soon as QEMU has ACCEPTED the request: `migrate` is asynchronous
// and replies {} immediately, so treating it as synchronous and quitting next
// truncates the snapshot. Follow it with AwaitMigration.
//
// The file: URI landed in QEMU 8.2, which is this driver's version floor.
func (c *qmpConn) MigrateToFile(ctx context.Context, path string) error {
	args := struct {
		URI string `json:"uri"`
	}{URI: "file:" + path}
	return c.Execute(ctx, "migrate", args, nil)
}

type qmpMigrationInfo struct {
	// Status is OPTIONAL: before any migration has been started the reply is a
	// bare {}, so an empty string here means "none", not a decode failure.
	Status    string `json:"status"`
	ErrorDesc string `json:"error-desc"`
}

// QueryMigrate reports the current migration's progress.
func (c *qmpConn) QueryMigrate(ctx context.Context) (qmpMigrationInfo, error) {
	var info qmpMigrationInfo
	err := c.Execute(ctx, "query-migrate", nil, &info)
	return info, err
}

// awaitMigration polls query-migrate until done says to stop. done returns
// (true, nil) to finish successfully, (false, nil) to keep polling, or an error
// to fail; the two exported wrappers below differ only in that function. The
// context is the only bound on how long either will wait.
func (c *qmpConn) awaitMigration(ctx context.Context, done func(qmpMigrationInfo) (bool, error)) error {
	ticker := time.NewTicker(qmpMigrationPollInterval)
	defer ticker.Stop()
	// A file: migration only ever walks none -> setup -> active -> completed,
	// so an absent status means it has not started yet.
	last := "none"
	for {
		info, err := c.QueryMigrate(ctx)
		if err != nil {
			return err
		}
		stop, err := done(info)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
		if info.Status != "" {
			last = info.Status
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("qmp: migration still %s: %w", last, ctx.Err())
		case <-ticker.C:
		}
	}
}

// AwaitMigration polls query-migrate until the migration reaches a terminal
// state, distinguishing completed from failed and cancelled.
func (c *qmpConn) AwaitMigration(ctx context.Context) error {
	return c.awaitMigration(ctx, func(info qmpMigrationInfo) (bool, error) {
		switch info.Status {
		case "completed":
			return true, nil
		case "failed":
			desc := info.ErrorDesc
			if desc == "" {
				desc = "no reason given"
			}
			return false, fmt.Errorf("qmp: migration failed: %s", desc)
		case "cancelled", "cancelling":
			return false, errors.New("qmp: migration was cancelled")
		}
		return false, nil
	})
}

// MigrateCancel aborts an outgoing migration. It is what makes a failed Pause
// recoverable: `migrate` is asynchronous, so a snapshot that failed for any
// reason other than a terminal migration status — the caller's deadline
// expiring while the stream is still `active` is the realistic one — leaves
// QEMU writing. Issuing `cont` under that live migration succeeds (the source
// runstate during an outgoing migration is `paused`) and is then undone
// seconds later when migration_completion parks the VM in `postmigrate` with
// its vCPUs halted: a sandbox the driver reports as Running and nothing can
// reach. Cancel first, settle, and only then recover.
//
// A cancel with no migration in flight is not an error worth distinguishing;
// callers treat this as best-effort.
func (c *qmpConn) MigrateCancel(ctx context.Context) error {
	return c.Execute(ctx, "migrate_cancel", nil, nil)
}

// AwaitMigrationSettled polls query-migrate until nothing is in flight. Unlike
// AwaitMigration it does not care HOW the migration ended — a cancel makes
// "cancelled" the success case — only that QEMU has closed the output file, so
// the caller may unlink it without stranding a full guest's worth of hot tier
// in an unlinked inode.
func (c *qmpConn) AwaitMigrationSettled(ctx context.Context) error {
	return c.awaitMigration(ctx, func(info qmpMigrationInfo) (bool, error) {
		switch info.Status {
		case "", "completed", "failed", "cancelled":
			return true, nil
		}
		return false, nil
	})
}

// SetBalloonBytes sets the guest's target VISIBLE RAM in bytes — QEMU's units,
// which are the inverse of vmm.Ballooner's "RAM reclaimed from the guest, in
// MiB". Callers convert; see the balloon section of the contract in qemu.go.
func (c *qmpConn) SetBalloonBytes(ctx context.Context, bytes uint64) error {
	args := struct {
		Value uint64 `json:"value"`
	}{Value: bytes}
	return c.Execute(ctx, "balloon", args, nil)
}

// BalloonActualBytes reports the guest's currently visible RAM in bytes, again
// in QEMU's orientation: a fully deflated 1024 MiB guest reads 1073741824.
// Returns a DeviceNotActive error when the VM has no balloon device.
func (c *qmpConn) BalloonActualBytes(ctx context.Context) (uint64, error) {
	var r struct {
		Actual uint64 `json:"actual"`
	}
	if err := c.Execute(ctx, "query-balloon", nil, &r); err != nil {
		return 0, err
	}
	return r.Actual, nil
}

// QOMSet writes one QOM property.
func (c *qmpConn) QOMSet(ctx context.Context, path, property string, value any) error {
	args := struct {
		Path     string `json:"path"`
		Property string `json:"property"`
		Value    any    `json:"value"`
	}{Path: path, Property: property, Value: value}
	return c.Execute(ctx, "qom-set", args, nil)
}

// QOMGet reads one QOM property into out.
func (c *qmpConn) QOMGet(ctx context.Context, path, property string, out any) error {
	args := struct {
		Path     string `json:"path"`
		Property string `json:"property"`
	}{Path: path, Property: property}
	return c.Execute(ctx, "qom-get", args, out)
}

// EnableBalloonStats turns on the guest's periodic stats refresh. The interval
// is in SECONDS, and it is a QOM property set at runtime rather than part of
// virtio-balloon's migrated device state — so boot must re-issue this after a
// restore as well as a cold boot, or BalloonStats goes stale on a resumed
// sandbox.
func (c *qmpConn) EnableBalloonStats(ctx context.Context, intervalSecs int) error {
	return c.QOMSet(ctx, balloonQOMPath, "guest-stats-polling-interval", intervalSecs)
}

// GuestStats reads the guest's free and available memory, in bytes.
//
// It returns errBalloonNoSample when the guest has not reported yet: stats are
// empty until EnableBalloonStats has run and one interval has elapsed, and
// unsampled fields read as 0xFFFFFFFFFFFFFFFF. Returning zeros instead would be
// worse than an error — host.Manager.MemStats would charge every sandbox its
// full ceiling and start ballooning sandboxes that are doing nothing wrong.
func (c *qmpConn) GuestStats(ctx context.Context) (free, available uint64, err error) {
	var r struct {
		// Every stat-* field is an unsigned 64-bit byte count. Decoding the
		// unset sentinel needs uint64 specifically: int64 fails outright and
		// float64 silently produces ~1.8e19.
		Stats      map[string]uint64 `json:"stats"`
		LastUpdate int64             `json:"last-update"`
	}
	if err := c.QOMGet(ctx, balloonQOMPath, "guest-stats", &r); err != nil {
		return 0, 0, err
	}
	if r.LastUpdate == 0 {
		return 0, 0, fmt.Errorf("%w (no refresh has happened)", errBalloonNoSample)
	}
	free, ok := qmpBalloonStat(r.Stats, "stat-free-memory")
	if !ok {
		return 0, 0, fmt.Errorf("%w (stat-free-memory unset)", errBalloonNoSample)
	}
	available, ok = qmpBalloonStat(r.Stats, "stat-available-memory")
	if !ok {
		return 0, 0, fmt.Errorf("%w (stat-available-memory unset)", errBalloonNoSample)
	}
	return free, available, nil
}

func qmpBalloonStat(stats map[string]uint64, name string) (uint64, bool) {
	v, ok := stats[name]
	if !ok || v == qmpBalloonStatUnset {
		return 0, false
	}
	return v, true
}
