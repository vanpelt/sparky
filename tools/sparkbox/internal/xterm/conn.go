package xterm

// The adapter that makes a browser terminal look like an attached SSH session
// to the control plane, so pausing a sandbox hangs it up the same way and with
// the same courtesies.

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// hangUpGrace bounds the goodbye write. It mirrors sshgw's closeGrace, and for
// the same reason: the gateway's hang-up path calls Stderr().Write and then
// Close under its own 2-second budget, so a write that could block longer than
// that would make one wedged tab delay the pause of a sandbox.
const hangUpGrace = 2 * time.Second

// wsSession presents a live WebSocket terminal as the sshgw session registry's
// sessionConn: a place to write the parting message, and a Close that ends it.
//
// Close is called from the gateway's goroutine, concurrently with this
// package's own pumps, so it is idempotent and — this is the load-bearing part
// — it must not block. CloseNow drops the connection immediately rather than
// waiting out a close handshake with a client that may be gone, and cancelling
// the bridge context is what unblocks a frame write already in flight. Blocking
// here would deadlock the manager, which calls the whole path under its lock.
type wsSession struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
	once   sync.Once
	hung   atomic.Bool
}

func newSessionConn(conn *websocket.Conn, cancel context.CancelFunc) *wsSession {
	return &wsSession{conn: conn, cancel: cancel}
}

// HungUp reports that the gateway ended this session, so the normal exit path
// knows the close code has already been chosen and does not race a second one.
func (s *wsSession) HungUp() bool { return s.hung.Load() }

// Stderr is where the gateway writes the terminal-restore escape sequences and
// the "sandbox paused" line. Those bytes belong in the terminal itself, not in
// a JSON status frame: they are the same escape sequences a real terminal
// needs, and xterm.js latches a TUI's mouse-reporting and bracketed-paste modes
// exactly like a physical emulator does — a terminal hung up mid-vim without
// them would spend the rest of its life emitting coordinates on mouse-move.
func (s *wsSession) Stderr() io.ReadWriter { return (*wsSessionStderr)(s) }

// Close ends the session. See the type comment: idempotent, non-blocking.
func (s *wsSession) Close() error {
	s.once.Do(func() {
		s.hung.Store(true)
		// The close handshake cannot happen on this goroutine. It writes a
		// close frame and then waits for the peer's reply, and that wait needs
		// the read lock this connection's own read loop is holding — up to
		// twenty seconds if the tab is gone. The manager is calling us with its
		// lock held, so we hand the handshake to a goroutine and return: the
		// contract is that the session *will* end, not that it has.
		go func() {
			// A distinct close code so the page can tell "your sandbox was
			// paused, press reconnect" from "your shell exited".
			closeWith(s.conn, statusSandboxHungUp, "sandbox hung up")
			s.conn.CloseNow() //nolint:errcheck
		}()
		s.cancel()
	})
	return nil
}

// wsSessionStderr is wsSession viewed as the io.ReadWriter Stderr() promises.
// A distinct type rather than methods on wsSession itself, because Read here
// means "read from the client's stderr", which for a terminal is nothing.
type wsSessionStderr wsSession

// Read reports EOF: a terminal has no separate stderr coming back from the
// browser, and the registry never reads this — it only needs the interface.
func (*wsSessionStderr) Read([]byte) (int, error) { return 0, io.EOF }

// Write frames the goodbye as terminal output. Bounded, because the caller's
// grace period is shorter than a stalled write could be.
func (s *wsSessionStderr) Write(p []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hangUpGrace)
	defer cancel()
	if err := s.conn.Write(ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
