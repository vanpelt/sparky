package xterm

// The guest side of a terminal: the PTY seam and the SSH session that
// implements it. Everything above this line (ws.go) speaks only to the
// interface, which is what lets the framing tests run with a fake guest.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
)

// PTY is one interactive session inside a sandbox: the byte stream in both
// directions, plus the two things a terminal needs that a stream cannot carry —
// a window size and an exit code.
type PTY interface {
	// Read returns guest output. It reports io.EOF when the shell is done.
	Read(p []byte) (int, error)
	// Write sends keystrokes.
	Write(p []byte) (int, error)
	// CloseWrite sends EOF on stdin without tearing down the session, so a
	// shell reading from stdin exits and its status can still be collected.
	CloseWrite() error
	// Resize applies a new window. rows and cols are already clamped.
	Resize(rows, cols int) error
	// Wait blocks until the shell exits and returns its status. It is called
	// once, after Read has returned, so both the last byte and the code are
	// reported in the right order.
	Wait() int
	// Close tears everything down. Idempotent.
	Close() error
}

// sshPTY is a PTY over the same SSH transport the gateway uses to reach a
// guest: same upstream key, same sshd, same trust relationship. The browser
// terminal is a second door onto that, not a second way in.
type sshPTY struct {
	client *xssh.Client
	sess   *xssh.Session
	stdin  io.WriteCloser
	out    *io.PipeReader

	closeOnce sync.Once
	waitOnce  sync.Once
	code      int
}

// dialPTY resumes nothing and assumes a running box: EnsureRunning has already
// happened, and the record it returns is the one carrying SSHAddr — the
// pre-resume copy has it cleared while paused, which is the classic way to get
// an empty address here.
func (h *Handler) dialPTY(ctx context.Context, box *host.Sandbox, term string, rows, cols int) (PTY, error) {
	client, err := sshgw.DialUpstream(ctx, box.SSHAddr, box.SSHUser, h.upstreamKey)
	if err != nil {
		return nil, fmt.Errorf("dial guest: %w", err)
	}
	sess, err := client.NewSession()
	if err != nil {
		client.Close() //nolint:errcheck
		return nil, fmt.Errorf("open session: %w", err)
	}
	if err := sess.RequestPty(term, rows, cols, xssh.TerminalModes{xssh.ECHO: 1}); err != nil {
		sess.Close()   //nolint:errcheck
		client.Close() //nolint:errcheck
		return nil, fmt.Errorf("request pty: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()   //nolint:errcheck
		client.Close() //nolint:errcheck
		return nil, fmt.Errorf("stdin: %w", err)
	}
	// One pipe for both streams. On a PTY session sshd usually merges them
	// already, but a guest sshd that does not would otherwise drop stderr on
	// the floor. io.Pipe is safe for concurrent writers and, being unbuffered,
	// gives us the backpressure we want for free: a browser that stops reading
	// stalls the SSH copier rather than growing a queue in the control plane.
	pr, pw := io.Pipe()
	sess.Stdout = pw
	sess.Stderr = pw
	if err := sess.Shell(); err != nil {
		pw.Close()     //nolint:errcheck
		sess.Close()   //nolint:errcheck
		client.Close() //nolint:errcheck
		return nil, fmt.Errorf("start shell: %w", err)
	}
	p := &sshPTY{client: client, sess: sess, stdin: stdin, out: pr}
	// Session.Wait also waits for the stdout/stderr copiers, so closing the
	// pipe after it returns is what turns "the shell exited" into an io.EOF on
	// the reader — the signal the output pump is blocked on.
	go func() {
		p.collect()
		pw.Close() //nolint:errcheck
	}()
	return p, nil
}

func (p *sshPTY) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *sshPTY) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *sshPTY) CloseWrite() error           { return p.stdin.Close() }

func (p *sshPTY) Resize(rows, cols int) error { return p.sess.WindowChange(rows, cols) }

// collect runs Wait exactly once and records the status. A non-zero exit is a
// normal outcome for a shell, so only a transport failure becomes exit 1 with
// no better information.
func (p *sshPTY) collect() {
	p.waitOnce.Do(func() {
		err := p.sess.Wait()
		if err == nil {
			return
		}
		var exitErr *xssh.ExitError
		if errors.As(err, &exitErr) {
			p.code = exitErr.ExitStatus()
			return
		}
		p.code = 1
	})
}

func (p *sshPTY) Wait() int {
	p.collect()
	return p.code
}

func (p *sshPTY) Close() error {
	p.closeOnce.Do(func() {
		p.out.Close()    //nolint:errcheck
		p.sess.Close()   //nolint:errcheck
		p.client.Close() //nolint:errcheck
	})
	return nil
}
