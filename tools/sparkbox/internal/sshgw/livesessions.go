package sshgw

// Live-session tracking, so the control plane can hang up cleanly on the
// interactive sessions attached to a sandbox it is about to pause.
//
// Without this, pausing a sandbox out from under an attached terminal leaves
// the client wedged: the TCP connection to the gateway stays open with nothing
// on the far end, so the ssh client never exits and never restores the local
// terminal. The visible damage comes from the *remote* application's terminal
// modes — a full-screen agent CLI or editor turns on mouse reporting and
// bracketed paste, and those live in the terminal emulator, not in termios, so
// even a clean ssh exit wouldn't undo them. The user is left with a terminal
// that spews escape sequences on every mouse move and no obvious way back.

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// closeGrace bounds how long we wait for the goodbye message to reach a client
// before closing anyway. A wedged or vanished client must not hold the close
// open — the point of this path is to guarantee the session ends.
const closeGrace = 2 * time.Second

// terminalRestore undoes the terminal modes an interactive program may have
// turned on but never got to turn off, because we are about to end its session
// from the outside. Ordinary shell output does not need this; a full-screen TUI
// does, and that is exactly what runs on these boxes.
//
// Sent only to sessions that requested a PTY — on a plain `ssh box cmd` these
// bytes would be corruption in the middle of the command's output.
// Ordered deliberately: the modes that make the terminal *emit* input come
// first, because those are what fill a disconnected terminal with garbage like
// "35;24;36M" on every mouse move. Cosmetic state follows.
const terminalRestore = "" +
	"\x1b[?9l\x1b[?1000l\x1b[?1001l\x1b[?1002l\x1b[?1003l" + // every mouse tracking mode
	"\x1b[?1004l" + // focus in/out reporting (emits \e[I and \e[O)
	"\x1b[?1005l\x1b[?1006l\x1b[?1015l" + // the extended coordinate encodings
	"\x1b[?2004l" + // bracketed paste
	"\x1b[?1l" + // application cursor keys
	"\x1b[?1049l" + // leave the alternate screen (restores the user's scrollback)
	"\x1b[r" + // drop any scrolling region the TUI set up
	"\x1b(B\x0f" + // G0 back to ASCII and shift-in, undoing line-drawing charsets
	"\x1b[?25h" + // cursor back on
	"\x1b[?7h" + // autowrap back on
	"\x1b[0m" // default colours and attributes

// SessionConn is the slice of gssh.Session that hanging up needs: somewhere to
// write the goodbye and a way to end the session. Kept narrow so the hang-up
// path can be tested without standing up a real SSH server.
//
// It is exported because the browser terminal in internal/xterm must register
// in *this* registry rather than start a second one: host.Manager takes exactly
// one SessionCloser, so a second registry would mean pausing a sandbox silently
// strands every browser terminal attached to it. See TrackTerminal.
type SessionConn interface {
	Stderr() io.ReadWriter
	Close() error
}

// connectVia records how a tracked session reached its sandbox, because that is
// what decides whether "reconnect with" can honestly print an ssh(1) command. A
// browser tab handed one would have nothing to do with it.
type connectVia int

const (
	// viaFrontDoor is the hostname-routed SSH form, `ssh <name>.<domain>`. It is
	// the zero value because it is what every hung-up SSH session has always been
	// told, whichever way it actually connected.
	viaFrontDoor connectVia = iota
	// viaGateway is the username-routed SSH form, `ssh <name>@<domain>`.
	viaGateway
	// viaBrowser is the terminal page, `https://<name>.<xterm>.<domain>`.
	viaBrowser
)

// liveSession is one tracked interactive session.
type liveSession struct {
	sess    SessionConn
	sandbox string
	isPTY   bool
	via     connectVia
}

// trackSession registers a session as attached to a sandbox and returns the
// function that unregisters it. Safe to call for both PTY and exec sessions.
func (g *Gateway) trackSession(sandbox string, s SessionConn, isPTY bool) func() {
	return g.track(sandbox, s, isPTY, viaFrontDoor)
}

// TrackSession is trackSession, exported as the seam internal/xterm registers a
// browser terminal through — its Config.Track field has exactly this shape.
// Prefer TrackTerminal for a browser: it also picks the reconnect wording.
func (g *Gateway) TrackSession(sandbox string, s SessionConn, isPTY bool) func() {
	return g.trackSession(sandbox, s, isPTY)
}

// TrackTerminal registers a browser terminal. It is always a PTY — that is what
// a terminal is, and getting it wrong would skip the escape sequences that undo
// mouse reporting — and its goodbye names a URL rather than an ssh command.
func (g *Gateway) TrackTerminal(sandbox string, s SessionConn) func() {
	return g.track(sandbox, s, true, viaBrowser)
}

func (g *Gateway) track(sandbox string, s SessionConn, isPTY bool, via connectVia) func() {
	ls := &liveSession{sess: s, sandbox: sandbox, isPTY: isPTY, via: via}
	g.liveMu.Lock()
	if g.live == nil {
		g.live = map[string]map[*liveSession]struct{}{}
	}
	if g.live[sandbox] == nil {
		g.live[sandbox] = map[*liveSession]struct{}{}
	}
	g.live[sandbox][ls] = struct{}{}
	g.liveMu.Unlock()

	return func() {
		g.liveMu.Lock()
		defer g.liveMu.Unlock()
		if set, ok := g.live[sandbox]; ok {
			delete(set, ls)
			if len(set) == 0 {
				delete(g.live, sandbox)
			}
		}
	}
}

// CloseSandboxSessions implements host.SessionCloser: it hangs up every session
// attached to a sandbox, restoring PTY clients' terminals on the way out, and
// reports how many it closed.
//
// It returns without waiting for those sessions to unwind, which is a contract
// and not an optimisation: the manager calls this while holding its lock, and
// the session goroutines being torn down take that same lock on their way out
// (Touch, in the handler's defer). Blocking here would deadlock the manager.
func (g *Gateway) CloseSandboxSessions(sandbox, reason string) int {
	g.liveMu.Lock()
	victims := make([]*liveSession, 0, len(g.live[sandbox]))
	for ls := range g.live[sandbox] {
		victims = append(victims, ls)
	}
	g.liveMu.Unlock()
	return g.closeAll(victims, reason, 0)
}

// CloseAllSessions hangs up every attached session on the gateway and blocks
// until they are done (or wait elapses). This is the process-shutdown path: a
// redeploy SIGTERMs the control plane and sshSrv.Close() drops every connection
// abruptly, which is precisely the wedged-terminal case — the sandbox is never
// paused, so CloseSandboxSessions never runs, and the client is left holding a
// dead socket with mouse reporting still latched on.
//
// Unlike the pause path this one *must* wait: the process is about to exit, and
// bytes still buffered when it does are simply lost.
func (g *Gateway) CloseAllSessions(reason string, wait time.Duration) int {
	g.liveMu.Lock()
	var victims []*liveSession
	for _, set := range g.live {
		for ls := range set {
			victims = append(victims, ls)
		}
	}
	g.liveMu.Unlock()
	return g.closeAll(victims, reason, wait)
}

// closeAll hangs up the given sessions. wait > 0 blocks until they finish or the
// deadline passes; wait == 0 returns immediately, which the pause path requires
// because the manager calls it holding a lock the teardown also takes.
func (g *Gateway) closeAll(victims []*liveSession, reason string, wait time.Duration) int {
	if len(victims) == 0 {
		return 0
	}
	var wg sync.WaitGroup
	for _, ls := range victims {
		wg.Add(1)
		go func(ls *liveSession) {
			defer wg.Done()
			g.hangUp(ls, reason)
		}(ls)
	}
	g.log.Info("closed attached sessions", "count", len(victims), "reason", reason)
	if wait <= 0 {
		return len(victims)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
		g.log.Warn("some sessions did not close before the deadline", "wait", wait)
	}
	return len(victims)
}

// hangUp writes the parting message and closes one session. The write is
// bounded by closeGrace: a client that has stopped reading must not keep the
// session alive, and Close unblocks the writer if it is still stuck.
func (g *Gateway) hangUp(ls *liveSession, reason string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var msg string
		if ls.isPTY {
			msg = terminalRestore
		}
		// \r\n throughout: the client's terminal is in raw mode, so a bare \n
		// would step down a line without returning to column zero.
		msg += fmt.Sprintf("\r\nsparkbox: sandbox %q %s — reconnect with: %s\r\n",
			ls.sandbox, reason, g.reconnectHint(ls.sandbox, ls.via))
		fmt.Fprint(ls.sess.Stderr(), msg) //nolint:errcheck // best-effort goodbye
	}()
	select {
	case <-done:
	case <-time.After(closeGrace):
	}
	ls.sess.Close() //nolint:errcheck
}

// reconnectHint renders the whole "reconnect with" clause, command included,
// because the three forms are not interchangeable addresses: an SSH client gets
// an ssh(1) invocation and a browser tab gets a URL. via picks between the
// front-door hostname form (`ssh <name>.<domain>`) the caller knows the user is
// already using, the gateway form (`ssh <name>@<domain>`), and the terminal
// page. With no domain configured there is no concrete address to print, so a
// <gateway> placeholder stands in — and a browser falls back to the SSH form
// rather than inventing a URL that would not resolve.
func (g *Gateway) reconnectHint(sandbox string, via connectVia) string {
	if via == viaBrowser && g.domain != "" && g.xtermSubdomain != "" {
		return "https://" + sandbox + "." + g.xtermSubdomain + "." + g.domain
	}
	if g.domain == "" {
		return "ssh " + sandbox + "@<gateway>"
	}
	if via == viaGateway {
		return "ssh " + sandbox + "@" + g.domain
	}
	return "ssh " + sandbox + "." + g.domain
}
