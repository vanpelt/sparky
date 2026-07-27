package sshgw

// Tests for hanging up sessions attached to a pausing sandbox: the terminal
// must be restored, the session must always end, and the manager must never be
// able to deadlock against a session teardown.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSession is a sessionConn that records what was written to it and whether
// it was closed. blockWrites makes it emulate a client that has stopped reading.
type fakeSession struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	closed     bool
	blockWrite chan struct{} // non-nil: writes block until this is closed
}

func (f *fakeSession) Write(p []byte) (int, error) {
	if f.blockWrite != nil {
		<-f.blockWrite
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}

func (f *fakeSession) Read([]byte) (int, error) { return 0, io.EOF }
func (f *fakeSession) Stderr() io.ReadWriter    { return f }

func (f *fakeSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if f.blockWrite != nil {
		select { // unblock a stuck writer, exactly as a real channel close does
		case <-f.blockWrite:
		default:
			close(f.blockWrite)
		}
	}
	return nil
}

func (f *fakeSession) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeSession) written() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

// waitFor polls until cond holds, so tests don't race the hang-up goroutines.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestHangUpRestoresTerminal is the fix for the wedged-terminal bug: a PTY
// client must receive the escape sequences that undo mouse reporting and the
// alternate screen, because the program that turned them on is being killed
// off without a chance to clean up.
func TestHangUpRestoresTerminal(t *testing.T) {
	gw, _, _ := newDoorGateway(t)
	sess := &fakeSession{}
	gw.trackSession("box", sess, true)

	if n := gw.CloseSandboxSessions("box", "went idle for 30m"); n != 1 {
		t.Fatalf("closed %d sessions, want 1", n)
	}
	waitFor(t, "session close", sess.isClosed)

	got := sess.written()
	for _, want := range []struct{ seq, why string }{
		{"\x1b[?1000l", "disable mouse tracking"},
		{"\x1b[?1003l", "disable any-event mouse tracking"},
		{"\x1b[?1006l", "disable SGR mouse encoding"},
		{"\x1b[?2004l", "disable bracketed paste"},
		{"\x1b[?1049l", "leave alternate screen"},
		{"\x1b[?25h", "show cursor"},
	} {
		if !strings.Contains(got, want.seq) {
			t.Errorf("goodbye missing %s (%q)", want.why, want.seq)
		}
	}
	if !strings.Contains(got, "went idle for 30m") {
		t.Errorf("goodbye did not explain why: %q", got)
	}
	// Raw mode: a bare \n would leave the cursor mid-line.
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Errorf("goodbye contains a bare \\n, which misaligns a raw-mode terminal: %q", got)
	}
}

// TestHangUpNonPTYSkipsEscapes: a `ssh box cmd` session has no terminal to
// restore, and injecting escape sequences would corrupt the command's output.
func TestHangUpNonPTYSkipsEscapes(t *testing.T) {
	gw, _, _ := newDoorGateway(t)
	sess := &fakeSession{}
	gw.trackSession("box", sess, false)

	gw.CloseSandboxSessions("box", "was paused")
	waitFor(t, "session close", sess.isClosed)

	if strings.Contains(sess.written(), "\x1b[") {
		t.Errorf("non-PTY session got terminal escapes: %q", sess.written())
	}
}

// TestHangUpClosesDespiteStuckWrite: the whole point of this path is that the
// session ends. A client that has stopped reading must not be able to keep its
// session alive by blocking the goodbye write.
func TestHangUpClosesDespiteStuckWrite(t *testing.T) {
	gw, _, _ := newDoorGateway(t)
	sess := &fakeSession{blockWrite: make(chan struct{})}
	gw.trackSession("box", sess, true)

	start := time.Now()
	gw.CloseSandboxSessions("box", "was paused")
	waitFor(t, "close of a non-reading client", sess.isClosed)

	if elapsed := time.Since(start); elapsed < closeGrace {
		t.Fatalf("closed after %v, before the %v write grace elapsed", elapsed, closeGrace)
	}
}

// TestCloseSandboxSessionsDoesNotBlock guards the interface contract that keeps
// the manager from deadlocking: host.Manager calls this holding its lock, while
// the session goroutines being torn down take that same lock on the way out.
// If this ever waits for them, pausing an attached sandbox wedges the daemon.
func TestCloseSandboxSessionsDoesNotBlock(t *testing.T) {
	gw, _, _ := newDoorGateway(t)
	for i := 0; i < 4; i++ {
		gw.trackSession("box", &fakeSession{blockWrite: make(chan struct{})}, true)
	}

	done := make(chan int, 1)
	go func() { done <- gw.CloseSandboxSessions("box", "was paused") }()
	select {
	case n := <-done:
		if n != 4 {
			t.Fatalf("closed %d sessions, want 4", n)
		}
	case <-time.After(closeGrace / 2):
		t.Fatal("CloseSandboxSessions blocked on unresponsive clients; it must return immediately")
	}
}

// markableSession is a fakeSession that also implements HungUpMarker, the way
// internal/xterm's browser terminal does.
type markableSession struct {
	fakeSession
	markedC chan struct{}
}

func newMarkableSession() *markableSession {
	return &markableSession{markedC: make(chan struct{}, 1)}
}

func (m *markableSession) MarkHungUp() {
	select {
	case m.markedC <- struct{}{}:
	default:
	}
}

func (m *markableSession) wasMarked() bool {
	select {
	case <-m.markedC:
		return true
	default:
		return false
	}
}

// TestCloseSandboxSessionsMarksBeforeReturning is the ordering guarantee a
// browser terminal depends on, and the one this path did not used to give.
//
// The manager calls this and then immediately pauses the driver, which kills
// the guest's sshd and unwinds every attached session. The goodbye goroutines
// below may not run until after all of that. If the claim rode along with them,
// the session would reach its own exit path first and report "shell exited" for
// a sandbox that was paused — no goodbye, no terminal restore, wrong close code.
//
// The write is blocked here on purpose: it makes the goroutine that writes the
// goodbye incapable of being what marks the session, so a passing test can only
// mean the claim was taken on the caller's own goroutine.
func TestCloseSandboxSessionsMarksBeforeReturning(t *testing.T) {
	gw, _, _ := newDoorGateway(t)
	sess := newMarkableSession()
	sess.blockWrite = make(chan struct{})
	defer sess.Close() //nolint:errcheck // unblocks the stuck goodbye writer
	gw.trackSession("box", sess, true)

	gw.CloseSandboxSessions("box", "was paused")

	// No polling: "eventually" is precisely the property that is not enough.
	if !sess.wasMarked() {
		t.Fatal("CloseSandboxSessions returned to the manager without claiming " +
			"the session; the pause that follows will race the hang-up")
	}
}

// TestCloseAllSessionsSpansSandboxes covers the redeploy case: a control-plane
// restart never pauses anything, so shutdown has to sweep every sandbox's
// sessions rather than one named box's.
func TestCloseAllSessionsSpansSandboxes(t *testing.T) {
	gw, _, _ := newDoorGateway(t)
	a, b := &fakeSession{}, &fakeSession{}
	gw.trackSession("box-a", a, true)
	gw.trackSession("box-b", b, true)

	if n := gw.CloseAllSessions("was interrupted", time.Second); n != 2 {
		t.Fatalf("closed %d sessions across sandboxes, want 2", n)
	}
	for name, s := range map[string]*fakeSession{"box-a": a, "box-b": b} {
		if !s.isClosed() {
			t.Errorf("%s session left open by shutdown", name)
		}
		if !strings.Contains(s.written(), "\x1b[?1006l") {
			t.Errorf("%s terminal not restored: %q", name, s.written())
		}
		// Each session names its own sandbox in the goodbye.
		if !strings.Contains(s.written(), name) {
			t.Errorf("%s goodbye named the wrong sandbox: %q", name, s.written())
		}
	}
}

// TestCloseAllSessionsWaits: at shutdown the process is about to exit, so bytes
// still in flight are lost. Unlike the pause path, this one must block until the
// sessions are actually done.
func TestCloseAllSessionsWaits(t *testing.T) {
	gw, _, _ := newDoorGateway(t)
	sess := &fakeSession{}
	gw.trackSession("box", sess, true)

	gw.CloseAllSessions("was interrupted", 2*time.Second)

	// No waitFor here — that is the point. It must already be closed on return.
	if !sess.isClosed() {
		t.Fatal("CloseAllSessions returned before the session was closed; the restore bytes would be lost on exit")
	}
}

// TestTerminalRestoreDisablesInputModes pins the modes that actually produce the
// garbage a user sees. The reported symptom was "35;24;36M" spew on mouse move,
// which is SGR mouse reporting (1006) still latched after the connection died.
func TestTerminalRestoreDisablesInputModes(t *testing.T) {
	for _, m := range []struct{ seq, why string }{
		{"\x1b[?9l", "X10 mouse"},
		{"\x1b[?1000l", "normal mouse tracking"},
		{"\x1b[?1002l", "button-event tracking"},
		{"\x1b[?1003l", "any-event tracking"},
		{"\x1b[?1004l", "focus reporting"},
		{"\x1b[?1006l", "SGR mouse encoding — the reported symptom"},
		{"\x1b[?2004l", "bracketed paste"},
		{"\x1b[?1049l", "alternate screen"},
	} {
		if !strings.Contains(terminalRestore, m.seq) {
			t.Errorf("terminalRestore does not disable %s (%q)", m.why, m.seq)
		}
	}
	// Input-generating modes must be cleared before the cosmetic ones: those are
	// what spew into a terminal the user is still typing at.
	if strings.Index(terminalRestore, "\x1b[?1006l") > strings.Index(terminalRestore, "\x1b[0m") {
		t.Error("mouse reporting is disabled after the cosmetic reset; clear input modes first")
	}
}

// TestTrackSessionUnregisters: a session that ended on its own must not be
// hung up later, and the per-sandbox map must not leak entries.
func TestTrackSessionUnregisters(t *testing.T) {
	gw, _, _ := newDoorGateway(t)
	sess := &fakeSession{}
	release := gw.trackSession("box", sess, true)
	release()

	if n := gw.CloseSandboxSessions("box", "was paused"); n != 0 {
		t.Fatalf("closed %d sessions after release, want 0", n)
	}
	gw.liveMu.Lock()
	_, leaked := gw.live["box"]
	gw.liveMu.Unlock()
	if leaked {
		t.Fatal("live-session map leaked an empty entry for the sandbox")
	}
}

// TestPauseClosesAttachedSessions wires the real manager to the real gateway:
// pausing a sandbox must release the terminal attached to it. This is the
// end-to-end version of the bug — and it deadlocks rather than fails if the
// non-blocking contract is ever broken.
func TestPauseClosesAttachedSessions(t *testing.T) {
	gw, mgr, _ := newDoorGateway(t)
	mgr.SetSessions(gw)
	ctx := context.Background()
	if _, err := mgr.Create(ctx, "box", "alice", "ubuntu", 1, 1024); err != nil {
		t.Fatal(err)
	}
	sess := &fakeSession{}
	gw.trackSession("box", sess, true)

	done := make(chan error, 1)
	go func() { done <- mgr.Pause(ctx, "box") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pause deadlocked against the session-close hook")
	}

	waitFor(t, "attached session to be closed by pause", sess.isClosed)
	if !strings.Contains(sess.written(), "\x1b[?1000l") {
		t.Errorf("paused sandbox left the terminal unrestored: %q", sess.written())
	}
}
