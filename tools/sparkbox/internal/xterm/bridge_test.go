package xterm

// Bridge tests: the wire protocol, the resize clamp, the close codes and the
// hang-up path, driven through a real WebSocket against a fake guest built on
// io.Pipe. No VM, no SSH, no manager beyond the in-memory fake — the PTY seam
// exists precisely so this file can exercise the framing without one.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// fakePTY is a guest shell made of two pipes. Unbuffered on purpose: it has the
// same backpressure behaviour as the real SSH-backed one, so a test that hangs
// here would have hung in production too.
type fakePTY struct {
	inR, outR *io.PipeReader
	inW, outW *io.PipeWriter

	mu       sync.Mutex
	resizes  [][2]int
	code     int
	exited   chan struct{}
	closed   bool
	closeSig chan struct{}
}

func newFakePTY() *fakePTY {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	return &fakePTY{inR: inR, inW: inW, outR: outR, outW: outW,
		exited: make(chan struct{}), closeSig: make(chan struct{})}
}

func (p *fakePTY) Read(b []byte) (int, error)  { return p.outR.Read(b) }
func (p *fakePTY) Write(b []byte) (int, error) { return p.inW.Write(b) }
func (p *fakePTY) CloseWrite() error           { return p.inW.Close() }

func (p *fakePTY) Resize(rows, cols int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resizes = append(p.resizes, [2]int{rows, cols})
	return nil
}

func (p *fakePTY) Wait() int {
	<-p.exited
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}

func (p *fakePTY) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	p.outR.Close() //nolint:errcheck
	p.inR.Close()  //nolint:errcheck
	close(p.closeSig)
	return nil
}

// say emits guest output, as a program inside the sandbox would.
func (p *fakePTY) say(t *testing.T, s string) {
	t.Helper()
	if _, err := p.outW.Write([]byte(s)); err != nil {
		t.Fatalf("guest write: %v", err)
	}
}

// exit ends the shell: output closes, then Wait unblocks with the status.
func (p *fakePTY) exit(code int) {
	p.mu.Lock()
	p.code = code
	p.mu.Unlock()
	p.outW.Close() //nolint:errcheck
	close(p.exited)
}

// typed reads what the browser sent, up to n bytes.
func (p *fakePTY) typed(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	got, err := io.ReadFull(p.inR, b)
	if err != nil {
		t.Fatalf("guest read: %v (got %q)", err, b[:got])
	}
	return string(b)
}

func (p *fakePTY) windows() [][2]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][2]int(nil), p.resizes...)
}

// ---------------------------------------------------------------------------

// wsHarness runs the real handler behind a real HTTP server (httptest's
// ResponseWriter is a Hijacker, which httptest.NewRecorder is not) and rewrites
// the Host so the suffix routing sees the name it would see on the edge.
type wsHarness struct {
	*harness
	srv  *httptest.Server
	pty  *fakePTY
	host string
	// session is the SessionConn the handler registered with the live-session
	// registry — the thing a pause would call Close on.
	sessionConn chan SessionConn
}

func newWSHarness(t *testing.T, state vmm.State) *wsHarness {
	t.Helper()
	hz := newHarness(t, &host.Sandbox{Name: "demo", Owner: "alice", State: state,
		SSHAddr: "127.0.0.1:1", SSHUser: "sparky"})
	wh := &wsHarness{harness: hz, pty: newFakePTY(),
		host: "demo.xterm." + testDomain, sessionConn: make(chan SessionConn, 1)}

	hz.h.open = func(_ context.Context, _ *host.Sandbox, _ string, _, _ int) (PTY, error) {
		return wh.pty, nil
	}
	hz.h.track = func(sandbox string, s SessionConn, isPTY bool) func() {
		if !isPTY {
			t.Errorf("browser terminal registered with isPTY=false")
		}
		wh.sessionConn <- s
		return func() {}
	}

	wh.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = wh.host
		hz.h.ServeHTTP(w, r)
	}))
	t.Cleanup(wh.srv.Close)
	t.Cleanup(func() { wh.pty.Close() }) //nolint:errcheck
	return wh
}

// dial opens the terminal socket the way the page does: cookie for the session,
// Origin naming this exact host.
func (wh *wsHarness) dial(t *testing.T) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	hdr := http.Header{}
	hdr.Set("Origin", "http://"+wh.host)
	hdr.Set("Cookie", edgeauth.CookieName+"="+wh.token(t, "alice"))
	conn, _, err := websocket.Dial(ctx, "ws://"+strings.TrimPrefix(wh.srv.URL, "http://")+"/ws",
		&websocket.DialOptions{HTTPHeader: hdr, Subprotocols: []string{"sparkbox.terminal.v1"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() }) //nolint:errcheck
	return conn, ctx
}

// readUntilReady drains the status frames and returns the states it saw.
func readUntilReady(ctx context.Context, t *testing.T, conn *websocket.Conn) []string {
	t.Helper()
	var states []string
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read while waiting for ready: %v", err)
		}
		if typ != websocket.MessageText {
			t.Fatalf("got a binary frame before ready: %q", data)
		}
		var m statusMsg
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("status frame is not JSON: %q", data)
		}
		states = append(states, m.State)
		if m.State == "ready" {
			return states
		}
	}
}

// ---------------------------------------------------------------------------

func TestBridgeStreamsBothWays(t *testing.T) {
	wh := newWSHarness(t, vmm.StateRunning)
	conn, ctx := wh.dial(t)
	readUntilReady(ctx, t, conn)

	// Guest -> browser arrives as a binary frame, because PTY output is bytes
	// and a text frame must be valid UTF-8 at every boundary.
	wh.pty.say(t, "hello\r\n")
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("output frame type = %v, want binary", typ)
	}
	if string(data) != "hello\r\n" {
		t.Fatalf("output = %q", data)
	}

	// Browser -> guest.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("echo hi\n")); err != nil {
		t.Fatal(err)
	}
	if got := wh.pty.typed(t, len("echo hi\n")); got != "echo hi\n" {
		t.Fatalf("guest received %q", got)
	}

	// Real input marks the sandbox active. The reaper reads LastActive, and a
	// person typing is exactly the activity a CPU sampler might miss.
	waitForTouch(t, wh, "typing never touched the sandbox")
}

// waitForTouch polls because Touch happens on the server's goroutines, which
// the client has no ordering relationship with.
func waitForTouch(t *testing.T, wh *wsHarness, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, touches := wh.mgr.counts(); touches > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestPausedSandboxReportsStartingThenReady(t *testing.T) {
	wh := newWSHarness(t, vmm.StatePaused)
	conn, ctx := wh.dial(t)
	states := readUntilReady(ctx, t, conn)
	if len(states) != 2 || states[0] != "starting" || states[1] != "ready" {
		t.Fatalf("states = %v, want [starting ready]", states)
	}
	if resumes, _ := wh.mgr.counts(); resumes != 1 {
		t.Fatalf("EnsureRunning called %d times, want 1", resumes)
	}
}

func TestRunningSandboxSkipsTheStartingFrame(t *testing.T) {
	wh := newWSHarness(t, vmm.StateRunning)
	conn, ctx := wh.dial(t)
	states := readUntilReady(ctx, t, conn)
	if len(states) != 1 || states[0] != "ready" {
		t.Fatalf("states = %v, want [ready]", states)
	}
}

func TestResizeIsForwardedAndClamped(t *testing.T) {
	wh := newWSHarness(t, vmm.StateRunning)
	conn, ctx := wh.dial(t)
	readUntilReady(ctx, t, conn)

	for _, msg := range []string{
		`{"type":"resize","rows":40,"cols":120}`,
		`{"type":"resize","rows":99999,"cols":0}`,
		`{"type":"resize","rows":-5,"cols":-5}`,
		// Neither of these may reach the guest: an unknown type is a newer
		// client, and broken JSON is a broken one. Both must be survivable.
		`{"type":"whoknows","rows":7,"cols":7}`,
		`not json at all`,
	} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	// A byte after the control frames proves the session survived all of them
	// and gives the resizes time to land in order.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := wh.pty.typed(t, 1); got != "x" {
		t.Fatalf("session did not survive the control frames (read %q)", got)
	}

	want := [][2]int{{40, 120}, {1000, 1}, {1, 1}}
	got := wh.pty.windows()
	if len(got) != len(want) {
		t.Fatalf("windows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("window %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestLargePasteIsNotDropped(t *testing.T) {
	wh := newWSHarness(t, vmm.StateRunning)
	conn, ctx := wh.dial(t)
	readUntilReady(ctx, t, conn)

	// Well over coder/websocket's 32 KiB default read limit, which would
	// otherwise close the connection with StatusMessageTooBig instead of
	// pasting a file into an editor.
	paste := strings.Repeat("abcdefgh", 64*1024) // 512 KiB
	done := make(chan string, 1)
	go func() { done <- wh.pty.typed(t, len(paste)) }()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte(paste)); err != nil {
		t.Fatalf("write paste: %v", err)
	}
	select {
	case got := <-done:
		if got != paste {
			t.Fatalf("guest received %d bytes, want %d", len(got), len(paste))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the paste never reached the guest")
	}
}

func TestShellExitClosesWith4001(t *testing.T) {
	wh := newWSHarness(t, vmm.StateRunning)
	conn, ctx := wh.dial(t)
	readUntilReady(ctx, t, conn)

	wh.pty.say(t, "bye\r\n")
	wh.pty.exit(7)

	// The last byte of output must arrive before the exit frame: a build that
	// prints and immediately exits must not lose its final screenful.
	var sawOutput bool
	var exitCode = -1
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if code := websocket.CloseStatus(err); code != statusShellExited {
				t.Fatalf("close status = %v, want %v", code, statusShellExited)
			}
			break
		}
		if typ == websocket.MessageBinary {
			if !strings.Contains(string(data), "bye") {
				t.Fatalf("unexpected output %q", data)
			}
			sawOutput = true
			continue
		}
		var m statusMsg
		if err := json.Unmarshal(data, &m); err == nil && m.Type == "exit" {
			exitCode = m.Code
		}
	}
	if !sawOutput {
		t.Error("the shell's last output was lost")
	}
	if exitCode != 7 {
		t.Errorf("exit code = %d, want 7", exitCode)
	}
	// The session end marks the box active, exactly as the SSH path's deferred
	// Touch does.
	waitForTouch(t, wh, "session end did not touch the sandbox")
}

// TestHangUpClosesWith4002 is the pause path: the manager tells the gateway to
// close every session attached to a sandbox, the gateway writes its goodbye to
// Stderr() and calls Close, and the browser must get both — the escape
// sequences that undo a full-screen TUI, then a close code it can react to.
func TestHangUpClosesWith4002(t *testing.T) {
	wh := newWSHarness(t, vmm.StateRunning)
	conn, ctx := wh.dial(t)
	readUntilReady(ctx, t, conn)

	var sc SessionConn
	select {
	case sc = <-wh.sessionConn:
	case <-time.After(5 * time.Second):
		t.Fatal("the terminal never registered with the live-session registry")
	}

	// Exactly what sshgw.hangUp does.
	goodbye := "\x1b[?1000l\r\nsparkbox: sandbox \"demo\" paused after 30m idle\r\n"
	if _, err := sc.Stderr().Write([]byte(goodbye)); err != nil {
		t.Fatalf("goodbye write: %v", err)
	}
	// Close must not block: the manager calls this holding its own lock.
	closed := make(chan struct{})
	go func() { sc.Close(); close(closed) }() //nolint:errcheck
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked — this would deadlock host.Manager.pause")
	}
	// And it is idempotent: CloseAllSessions can race CloseSandboxSessions.
	if err := sc.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}

	var got strings.Builder
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if code := websocket.CloseStatus(err); code != statusSandboxHungUp {
				t.Fatalf("close status = %v, want %v", code, statusSandboxHungUp)
			}
			break
		}
		if typ == websocket.MessageBinary {
			got.WriteString(string(data))
		}
	}
	if !strings.Contains(got.String(), "\x1b[?1000l") {
		t.Errorf("the terminal-restore escapes never reached the browser: %q", got.String())
	}
	if !strings.Contains(got.String(), "paused after 30m idle") {
		t.Errorf("the goodbye never reached the browser: %q", got.String())
	}
}

func TestClientHangUpClosesTheGuestStdin(t *testing.T) {
	wh := newWSHarness(t, vmm.StateRunning)
	conn, ctx := wh.dial(t)
	readUntilReady(ctx, t, conn)

	conn.Close(websocket.StatusNormalClosure, "tab closed") //nolint:errcheck
	_ = ctx

	// A shell blocked on stdin must see EOF rather than linger until the
	// reaper notices it, so the browser going away really ends the session.
	done := make(chan error, 1)
	go func() {
		b := make([]byte, 1)
		_, err := wh.pty.inR.Read(b)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("guest stdin stayed open after the client hung up")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guest stdin was never closed")
	}
}
