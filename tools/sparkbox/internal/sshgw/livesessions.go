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
const terminalRestore = "" +
	"\x1b[?1000l\x1b[?1002l\x1b[?1003l" + // mouse tracking: normal, button-event, any-event
	"\x1b[?1005l\x1b[?1006l\x1b[?1015l" + // and the extended coordinate encodings
	"\x1b[?2004l" + // bracketed paste
	"\x1b[?1l" + // application cursor keys
	"\x1b[?1049l" + // leave the alternate screen (restores the user's scrollback)
	"\x1b[?25h" + // cursor back on
	"\x1b[?7h" + // autowrap back on
	"\x1b[0m" // default colours and attributes

// sessionConn is the slice of gssh.Session that hanging up needs: somewhere to
// write the goodbye and a way to end the session. Kept narrow so the hang-up
// path can be tested without standing up a real SSH server.
type sessionConn interface {
	Stderr() io.ReadWriter
	Close() error
}

// liveSession is one tracked interactive session.
type liveSession struct {
	sess  sessionConn
	isPTY bool
}

// trackSession registers a session as attached to a sandbox and returns the
// function that unregisters it. Safe to call for both PTY and exec sessions.
func (g *Gateway) trackSession(sandbox string, s sessionConn, isPTY bool) func() {
	ls := &liveSession{sess: s, isPTY: isPTY}
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
	set := g.live[sandbox]
	victims := make([]*liveSession, 0, len(set))
	for ls := range set {
		victims = append(victims, ls)
	}
	g.liveMu.Unlock()

	for _, ls := range victims {
		go g.hangUp(ls, sandbox, reason)
	}
	if len(victims) > 0 {
		g.log.Info("closed attached sessions", "sandbox", sandbox, "count", len(victims), "reason", reason)
	}
	return len(victims)
}

// hangUp writes the parting message and closes one session. The write is
// bounded by closeGrace: a client that has stopped reading must not keep the
// session alive, and Close unblocks the writer if it is still stuck.
func (g *Gateway) hangUp(ls *liveSession, sandbox, reason string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var msg string
		if ls.isPTY {
			msg = terminalRestore
		}
		// \r\n throughout: the client's terminal is in raw mode, so a bare \n
		// would step down a line without returning to column zero.
		msg += fmt.Sprintf("\r\nsparkbox: sandbox %q %s — reconnect with: ssh %s\r\n",
			sandbox, reason, g.reconnectHint(sandbox))
		fmt.Fprint(ls.sess.Stderr(), msg) //nolint:errcheck // best-effort goodbye
	}()
	select {
	case <-done:
	case <-time.After(closeGrace):
	}
	ls.sess.Close() //nolint:errcheck
}

// reconnectHint renders the address a user reconnects on, matching the form
// used elsewhere in the gateway's user-facing messages.
func (g *Gateway) reconnectHint(sandbox string) string {
	if d := g.domainHint(); d != "" {
		return sandbox + "." + d
	}
	return sandbox + "@<gateway>"
}
