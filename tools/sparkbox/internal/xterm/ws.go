package xterm

// The WebSocket half of the browser terminal: the origin gate, the upgrade,
// and the bridge that pumps bytes between the socket and a PTY in the guest.
//
// Wire protocol, deliberately minimal and documented in the OpenAPI spec:
//
//	client -> server  binary frame  raw stdin bytes
//	client -> server  text frame    {"type":"resize","rows":N,"cols":N}
//	server -> client  binary frame  raw PTY output
//	server -> client  text frame    {"type":"status","state":"starting"|"ready",...}
//	                                {"type":"exit","code":N}
//	                                {"type":"error","message":"..."}
//
// Bytes ride in *binary* frames in both directions because PTY output is a byte
// stream, not text: a read boundary will eventually split a multi-byte rune,
// and a WebSocket text frame must be valid UTF-8 or the browser fails the whole
// connection. xterm.js takes a Uint8Array and reassembles the rune itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Close codes. 4001 is the one borrowed value: the terminal-over-HTTPS design
// already promises it to exe-ssh clients for "the shell exited normally", and
// two meanings for one code on one server would be worse than a gap in the
// numbering. The rest distinguish the cases the page reacts to differently —
// a hang-up offers "reconnect", an attach failure does not retry by itself.
const (
	statusShellExited   websocket.StatusCode = 4001
	statusSandboxHungUp websocket.StatusCode = 4002
	statusAttachFailed  websocket.StatusCode = 4003
)

const (
	// startTimeout bounds EnsureRunning. It is the archive budget, not the dial
	// budget, because a sandbox whose rootfs is parked in object storage has to
	// be downloaded and unpacked before it can be attached to — and the page
	// shows a real "starting…" state throughout, so the wait is visible rather
	// than a hang.
	startTimeout = 15 * time.Minute
	// dialTimeout bounds reaching the guest's sshd once it is running.
	// DialUpstream retries until the context expires, so an unbounded one is an
	// infinite loop, not a patient caller.
	dialTimeout = 30 * time.Second

	// writeTimeout bounds one frame to the browser. Without it a tab that has
	// stopped reading wedges the guest-output pump, which back-pressures the
	// SSH channel window and eventually stalls the process inside the VM — one
	// hung laptop taking a sandbox down with it.
	writeTimeout = 15 * time.Second
	// pingInterval keeps the connection alive through cloudflared and any
	// intermediate proxy, whose idle budgets are far shorter than a person's
	// thinking time. Ping is safe here only because the read loop is always
	// running: the pong is consumed by whoever is reading.
	pingInterval = 30 * time.Second
	pingTimeout  = 10 * time.Second

	// readLimit is per message. The 32 KiB default turns a large paste into a
	// closed connection (StatusMessageTooBig) rather than a large paste.
	readLimit = 1 << 20
	// outBuf sizes the guest-output reads. Big enough that a chatty build is
	// not one frame per line, small enough that an interactive keystroke echo
	// is not waiting on it.
	outBuf = 32 * 1024

	// touchInterval throttles marking the sandbox active. See touch().
	touchInterval = time.Minute
)

// ws is the browser entry point: GET /ws on <name>.xterm.<domain>.
//
// The origin gate runs before the ownership check, not after, and the order is
// load-bearing: answering a foreign origin 404-for-someone-else's-box but
// 403-for-your-own would tell a cross-site page which sandboxes its visitor
// owns. Refusing every cross-origin handshake identically says nothing.
func (h *Handler) ws(w http.ResponseWriter, r *http.Request) {
	if !h.upgradable(w, r) {
		return
	}
	box, ok := h.resolve(w, r)
	if !ok {
		return
	}
	sess, _ := edgeauth.From(r.Context())
	h.Bridge(w, r, box.Name, sess)
}

// upgradable applies the two checks that must pass before a request may become
// a WebSocket at all, writing the refusal itself and reporting whether to go
// on. Called twice on the browser path (once before the owner check, once
// inside Bridge) because it is a pure header test, and because Bridge is also
// reachable from internal/restapi, which must not be able to skip it.
func (h *Handler) upgradable(w http.ResponseWriter, r *http.Request) bool {
	// This is the single most security-critical check in the terminal, and it
	// lives here rather than in edgeauth because it is WebSocket-specific in a
	// way no other endpoint is.
	//
	// The session cookie's Domain is ".<zone>", so it rides every request to
	// every host under the zone — including one opened by script running on a
	// *sandbox's own web route*, which is arbitrary user code on a same-site
	// origin. A browser cannot attach an Authorization or custom header to a
	// WebSocket handshake, so RequireMutation's CSRF proof is unavailable, and
	// SameSite=Lax fences off nothing when the attacker is same-site. Origin is
	// the only thing left that a page cannot forge: the browser sets it, and it
	// names the page that opened the socket.
	//
	// So: an Origin, if present, must be exactly this host. A request with no
	// Origin is not a browser and must prove it by authenticating with a Bearer
	// token, which no cross-site browser request can carry. Getting this wrong
	// hands a root shell in someone's sandbox to any page they visit.
	if err := checkOrigin(r); err != nil {
		h.log.Warn("terminal upgrade refused", "err", err,
			"origin", r.Header.Get("Origin"), "host", r.Host)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return false
	}
	// Accept needs to hijack the connection, which HTTP/2 does not offer. The
	// edge negotiates http/1.1 for this in practice; the guard turns a
	// confusing hijack failure into an answerable error.
	if r.ProtoMajor > 1 {
		http.Error(w, "sparkbox: the terminal requires HTTP/1.1", http.StatusHTTPVersionNotSupported)
		return false
	}
	return true
}

// Bridge upgrades an authenticated request to a terminal WebSocket attached to
// the named sandbox. It is exported so internal/restapi can serve
// GET /v1/sandboxes/{name}/terminal from this exact code path rather than a
// second copy of it — but note what it does NOT do: it does not check
// ownership. The caller has already done that (h.resolve here, ctlops.Attach
// there), and doing it twice in two places is how the two eventually disagree.
func (h *Handler) Bridge(w http.ResponseWriter, r *http.Request, name string, sess edgeauth.Session) {
	if !h.upgradable(w, r) {
		return
	}
	log := h.log.With("sandbox", name, "handle", sess.Handle)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// No OriginPatterns and no InsecureSkipVerify: the library's own
		// default (Origin host must equal the request host) runs as a second,
		// independent check behind checkOrigin. Two implementations of the same
		// rule is cheap insurance on the one rule that must not be wrong.
		Subprotocols: []string{"sparkbox.terminal.v1"},
	})
	if err != nil {
		log.Warn("terminal upgrade failed", "err", err)
		return
	}
	conn.SetReadLimit(readLimit)
	// CloseNow is the backstop for every path that does not close cleanly; it
	// is a no-op once the connection is already closed.
	defer conn.CloseNow() //nolint:errcheck

	h.serve(r.Context(), conn, name, log)
}

// serve is everything after the upgrade: resume, register, dial, bridge.
//
// One rule governs every context below it, and it is not obvious: cancelling
// the context of a coder/websocket Read or Write *closes the connection*. So
// nothing that merely wants to unwind the session may cancel a context the
// socket is using — do that and the close code, the code the page reads to
// decide whether to offer "reconnect", is replaced by an abnormal termination.
// Frames therefore always carry their own budget from context.Background(), and
// the session context below only ever tears down the guest end.
func (h *Handler) serve(_ context.Context, conn *websocket.Conn, name string, log *slog.Logger) {
	box, ok := h.mgr.Get(name)
	if !ok {
		closeWith(conn, statusAttachFailed, "sandbox is gone")
		return
	}
	if box.State != vmm.StateRunning {
		// A real state, not a spinner over a hang: an archived sandbox has to
		// be fetched back out of object storage first, which takes minutes, and
		// the user deserves to know that is what is happening.
		sendJSON(conn, statusMsg{Type: "status", State: "starting", Sandbox: name,
			Note: "starting your sandbox…"})
	}

	// Detached from the request: after a hijack the server no longer cancels
	// r.Context(), so it is not a liveness signal, and the socket itself is.
	startCtx, stopStart := context.WithTimeout(context.Background(), startTimeout)
	box, err := h.mgr.EnsureRunning(startCtx, name)
	stopStart()
	if err != nil {
		log.Warn("terminal resume failed", "err", err)
		sendJSON(conn, statusMsg{Type: "error", Message: startFailure(err)})
		closeWith(conn, statusAttachFailed, "could not start the sandbox")
		return
	}
	// Deferred, never periodic: LastActive is what the idle reaper reads, and
	// refreshing it on a timer would turn a forgotten tab into a permanently
	// pinned VM. See touch() for the one throttled exception.
	defer h.mgr.Touch(name)

	// The session context is cancelled by exactly one thing — the gateway
	// hanging this terminal up because the sandbox is going away — and its only
	// job is to unblock the output pump, which is parked in pty.Read.
	sessCtx, endSession := context.WithCancel(context.Background())
	defer endSession()

	// Register before dialling, so a pause racing this connection still finds
	// the session and hangs it up rather than leaving a terminal pointed at a
	// VM that is no longer there.
	sc := newSessionConn(conn, endSession)
	if h.track != nil {
		// isPTY is true because a browser terminal always is one, and it is
		// what makes the hang-up path send the escape sequences that undo a
		// full-screen TUI's mouse reporting. xterm.js latches those modes
		// exactly like a physical emulator does.
		defer h.track(name, sc, true)()
	}

	dialCtx, stopDial := context.WithTimeout(context.Background(), dialTimeout)
	pty, err := h.open(dialCtx, box, "xterm-256color", 24, 80)
	stopDial()
	if err != nil {
		log.Warn("terminal dial failed", "err", err)
		sendJSON(conn, statusMsg{Type: "error", Message: "could not reach the sandbox's shell"})
		closeWith(conn, statusAttachFailed, "could not reach the sandbox")
		return
	}
	defer pty.Close() //nolint:errcheck
	go func() {
		<-sessCtx.Done()
		pty.Close() //nolint:errcheck // idempotent; this is what unparks pty.Read
	}()

	sendJSON(conn, statusMsg{Type: "status", State: "ready", Sandbox: name})
	log.Info("terminal attached")

	code, err := h.bridge(conn, pty, name)
	if err != nil {
		log.Info("terminal ended", "err", err)
	}
	// The hang-up path owns the close when it fired: it has already sent the
	// goodbye and a 4002, and a second close code here would just race it.
	if !sc.HungUp() {
		sendJSON(conn, statusMsg{Type: "exit", Code: code})
		closeWith(conn, statusShellExited, fmt.Sprintf("shell exited with %d", code))
	}
}

// bridge pumps the socket and the PTY into each other and returns the shell's
// exit code. Split out from serve, and taking the PTY as an interface, so the
// framing, the resize clamp and the close behaviour are testable over an
// io.Pipe with no VM, no SSH and no manager.
func (h *Handler) bridge(conn *websocket.Conn, pty PTY, name string) (int, error) {
	stopPing := make(chan struct{})
	defer close(stopPing)

	// Client -> guest. It reads on a context that is never cancelled, because
	// cancelling one here would drop the connection before the close code that
	// explains why. It ends when the socket does, and on the way out it hangs
	// up the guest so a shell blocked on stdin exits instead of lingering until
	// the reaper notices.
	go func() {
		defer pty.Close()      //nolint:errcheck
		defer pty.CloseWrite() //nolint:errcheck
		var last time.Time
		for {
			typ, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if _, err := pty.Write(data); err != nil {
					return
				}
				last = h.touch(name, last)
			case websocket.MessageText:
				if err := applyControl(pty, data); err != nil {
					return
				}
				// Deliberately no touch here: a resize can be fired by a
				// window manager with nobody at the keyboard.
			}
		}
	}()

	// Keepalive, so a person thinking for a minute does not lose their shell to
	// cloudflared's idle budget. Safe to run concurrently with the read loop
	// above — and only concurrently with it, because the pong is consumed by
	// whoever is reading.
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				pctx, stop := context.WithTimeout(context.Background(), pingTimeout)
				err := conn.Ping(pctx)
				stop()
				if err != nil {
					return
				}
			}
		}
	}()

	// Guest -> client, on this goroutine so the exit code is reported only
	// after the last byte of output has been framed. A build that prints and
	// immediately exits must not lose its last screenful.
	buf := make([]byte, outBuf)
	var failure error
	for {
		n, err := pty.Read(buf)
		if n > 0 {
			// Bounded, because a tab that has stopped reading would otherwise
			// wedge this pump, back-pressure the SSH channel window, and stall
			// the process inside the VM — one hung laptop taking a sandbox down
			// with it. A timeout here drops the socket, which is the right
			// outcome: the client is gone.
			wctx, stop := context.WithTimeout(context.Background(), writeTimeout)
			werr := conn.Write(wctx, websocket.MessageBinary, buf[:n])
			stop()
			if werr != nil {
				failure = werr
				break
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				failure = err
			}
			break
		}
	}
	return pty.Wait(), failure
}

// touch marks the sandbox active, at most once per touchInterval, and returns
// the new stamp. It fires on real client input only.
//
// This is a deliberate policy difference from the SSH path, which touches only
// at session end: a browser tab left open on a shell prompt is not work, and
// touching on keepalives or on the socket merely being open would exempt it
// from the idle reaper forever. Typing is work; the reaper's own CPU and
// network sampling covers everything the person is not personally typing.
func (h *Handler) touch(name string, last time.Time) time.Time {
	now := time.Now()
	if now.Sub(last) < touchInterval {
		return last
	}
	h.mgr.Touch(name)
	return now
}

// control is the client's out-of-band message. Only resize exists today;
// unknown types are ignored rather than fatal, so a newer page against an older
// server degrades instead of dropping the connection.
type control struct {
	Type string `json:"type"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

type statusMsg struct {
	Type    string `json:"type"`
	State   string `json:"state,omitempty"`
	Sandbox string `json:"sandbox,omitempty"`
	Note    string `json:"note,omitempty"`
	Message string `json:"message,omitempty"`
	Code    int    `json:"code,omitempty"`
}

// maxDim clamps a requested window. The numbers go straight into a TIOCSWINSZ
// in the guest, so an absurd pair from a hostile or buggy client is a knob a
// stranger gets to turn on someone else's kernel; a terminal larger than this
// is not a terminal.
const maxDim = 1000

func applyControl(pty PTY, data []byte) error {
	var c control
	if err := json.Unmarshal(data, &c); err != nil {
		// Malformed JSON is a broken client, not an attack; ignoring it keeps
		// a stray frame from killing an otherwise working shell.
		return nil
	}
	if c.Type != "resize" {
		return nil
	}
	return pty.Resize(clampDim(c.Rows), clampDim(c.Cols))
}

func clampDim(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxDim {
		return maxDim
	}
	return n
}

// checkOrigin implements the rule described at the call site: a browser must
// name this exact host, and a non-browser must present a Bearer token.
func checkOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin means no browser — every browser sends one on a WebSocket
		// handshake. Insist on the credential a browser cannot attach, so the
		// absence of Origin can never be used to skip the check.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			return errors.New("a terminal WebSocket needs an Origin header or Bearer authentication")
		}
		return nil
	}
	if !strings.EqualFold(origin, requestOrigin(r)) {
		return errors.New("cross-origin terminal connections are refused")
	}
	return nil
}

// requestOrigin renders the origin this request was addressed to. The scheme
// comes from X-Forwarded-Proto when the edge terminated TLS, and otherwise from
// the connection — https is not assumed, so the same check works on a plain
// http://127.0.0.1 dev server without a hole to talk it out of https.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(fwd, ",")[0]))
	}
	return scheme + "://" + r.Host
}

// startFailure turns a manager error into one sentence for the terminal. The
// resource limits are expected user-facing conditions with an obvious remedy,
// not faults; everything else is deliberately vague, because this surface faces
// strangers and the detail belongs in the log.
func startFailure(err error) string {
	var limit *host.LimitError
	if errors.As(err, &limit) {
		return fmt.Sprintf("you already have %d running sandboxes (max %d): %s — pause one and reconnect",
			len(limit.Running), limit.Max, strings.Join(limit.Running, ", "))
	}
	var capacity *host.CapacityError
	if errors.As(err, &capacity) {
		return fmt.Sprintf("the host is at capacity (%d/%d MB allocated) — try again shortly",
			capacity.UsedMB, capacity.BudgetMB)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the sandbox took too long to start"
	}
	return "the sandbox could not be started"
}

func sendJSON(conn *websocket.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	ctx, stop := context.WithTimeout(context.Background(), writeTimeout)
	defer stop()
	conn.Write(ctx, websocket.MessageText, b) //nolint:errcheck // best-effort status
}

// closeWith sends a close frame, keeping the reason inside the 123-byte cap the
// protocol allows. The reason is a hint for the page's reconnect logic; the
// explanation the user reads arrives as terminal output.
func closeWith(conn *websocket.Conn, code websocket.StatusCode, reason string) {
	if len(reason) > 120 {
		reason = reason[:120]
	}
	conn.Close(code, reason) //nolint:errcheck // the connection may already be gone
}
