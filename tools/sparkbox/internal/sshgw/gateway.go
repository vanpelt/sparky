// Package sshgw is the smart SSH proxy: clients run `ssh <sandbox>@gateway`,
// are identified by public key, and the gateway resumes the target sandbox if
// suspended (resume-on-connect) before piping the session through.
//
// SSH has no Host header, so the sandbox name travels in the SSH username —
// the simplest of the two routing schemes exe.dev's design allows (the other
// keys off destination IP from a shared pool; see docs/agentic-sandbox-design.md).
package sshgw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/frontdoor"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// NewSandboxUser is the reserved SSH username that creates a fresh sandbox on
// connect: `ssh new@gateway`.
const NewSandboxUser = "new"

// ControlUser is the reserved SSH username for the out-of-band control channel:
// `ssh ctl@gateway list` / `ssh ctl@gateway pause <name>`. It never routes to a
// VM, so no sandbox may be named "ctl".
const ControlUser = "ctl"

const controlUsage = "usage: ssh ctl@<gateway> <command>\r\n" +
	"  list            list your sandboxes and their state\r\n" +
	"  pause <name>    pause a running sandbox to free a slot\r\n"

const authedUserKey = "sparkbox-user"

// pauseTimeout bounds ctl pause. Pausing writes the guest's full memory
// snapshot (GBs for a warm sandbox) — the 15s dial timeout wedged an 8GB
// guest mid-pause. Sized for the slowest plausible snapshot, not a dial.
const pauseTimeout = 3 * time.Minute

type Gateway struct {
	mgr          *host.Manager
	users        *Users
	log          *slog.Logger
	hostKey      xssh.Signer
	upstreamKey  xssh.Signer // authenticates the gateway into VMs
	defaultImage string
	dialTimeout  time.Duration
	doors        *frontdoor.Mapper // optional: hostname (dest-IP) routing
	domain       string            // base domain for user-facing hints, e.g. "hivemind.tools"
}

type GatewayOptions struct {
	Manager      *host.Manager
	Users        *Users
	HostKey      xssh.Signer
	UpstreamKey  xssh.Signer
	DefaultImage string
	Logger       *slog.Logger
	// Doors, if set, enables hostname routing: connections whose destination
	// address is a front-door IPv6 are routed to the matching sandbox, and the
	// SSH username no longer has to carry the sandbox name.
	Doors *frontdoor.Mapper
	// Domain is the base DNS domain used in user-facing hints (optional).
	Domain string
}

func New(opts GatewayOptions) *Gateway {
	return &Gateway{
		mgr: opts.Manager, users: opts.Users, log: opts.Logger,
		hostKey: opts.HostKey, upstreamKey: opts.UpstreamKey,
		defaultImage: opts.DefaultImage, dialTimeout: 15 * time.Second,
		doors: opts.Doors, domain: opts.Domain,
	}
}

// Server builds the gliderlabs/ssh server; callers run Serve/ListenAndServe.
func (g *Gateway) Server(addr string) *gssh.Server {
	srv := &gssh.Server{
		Addr:    addr,
		Handler: g.handle,
		PublicKeyHandler: func(ctx gssh.Context, key gssh.PublicKey) bool {
			user, ok := g.users.Lookup(key)
			if !ok {
				g.log.Warn("rejected unknown key", "remote", ctx.RemoteAddr())
				return false
			}
			ctx.SetValue(authedUserKey, user)
			return true
		},
	}
	srv.AddHostKey(g.hostKey)
	return srv
}

func (g *Gateway) handle(s gssh.Session) {
	user, _ := s.Context().Value(authedUserKey).(string)
	sandboxName := s.User()

	// Hostname routing: when the client dialed a sandbox's front-door IPv6
	// (ssh myvm.<domain>), the destination address names the target and the
	// SSH username is not consulted for routing. Outside the front-door range
	// (or when doors are disabled) the username carries the name, as ever.
	viaDoor := false
	if name, ok, inRange := g.resolveDoor(s.LocalAddr()); inRange {
		if !ok {
			log := g.log.With("user", user, "dest", s.LocalAddr().String(), "remote", s.RemoteAddr())
			fail(s, log, "lookup", fmt.Errorf("no sandbox at this address — create one with: ssh new.%s", g.domainHint()))
			return
		}
		sandboxName = name
		viaDoor = true
	}
	log := g.log.With("user", user, "sandbox", sandboxName, "remote", s.RemoteAddr())

	if sandboxName == ControlUser {
		g.handleControl(s, user, log)
		return
	}

	ctx, cancel := context.WithTimeout(s.Context(), g.dialTimeout)
	defer cancel()

	if sandboxName == NewSandboxUser {
		name := g.newName()
		if _, err := g.mgr.Create(ctx, name, user, g.defaultImage, 0, 0); err != nil {
			g.failStart(s, log, "create sandbox", err)
			return
		}
		if viaDoor {
			fmt.Fprintf(s.Stderr(), "sparkbox: created sandbox %q — reconnect with: ssh %s.%s\r\n", name, name, g.domainHint())
		} else {
			fmt.Fprintf(s.Stderr(), "sparkbox: created sandbox %q — reconnect with: ssh %s@<gateway>\r\n", name, name)
		}
		sandboxName = name
	}

	box, ok := g.mgr.Get(sandboxName)
	if !ok {
		fail(s, log, "lookup", fmt.Errorf("no sandbox named %q", sandboxName))
		return
	}
	if box.Owner != user {
		// Same message as not-found: don't leak other users' sandbox names.
		fail(s, log, "lookup", fmt.Errorf("no sandbox named %q", sandboxName))
		return
	}

	// Resume-on-connect: the user perceives an always-on machine; suspended
	// sandboxes cost only disk.
	box, err := g.mgr.EnsureRunning(ctx, sandboxName)
	if err != nil {
		g.failStart(s, log, "resume", err)
		return
	}
	defer g.mgr.Touch(sandboxName)

	client, err := g.dialUpstream(ctx, box.SSHAddr, box.SSHUser)
	if err != nil {
		fail(s, log, "dial vm", err)
		return
	}
	defer client.Close()

	exitCode, err := g.pipeSession(s, client)
	if err != nil {
		log.Warn("session ended with error", "err", err)
	}
	s.Exit(exitCode) //nolint:errcheck
}

// resolveDoor maps the connection's destination address to a sandbox name.
// inRange reports whether the address is a front-door address at all; ok
// whether it named a reserved user or an existing sandbox. When the address
// is out of range (or doors are disabled) the caller falls back to routing by
// the SSH username.
func (g *Gateway) resolveDoor(addr net.Addr) (name string, ok, inRange bool) {
	if g.doors == nil {
		return "", false, false
	}
	ta, isTCP := addr.(*net.TCPAddr)
	if !isTCP || !g.doors.Contains(ta.IP) {
		return "", false, false
	}
	// Reserved names first: their doors exist before any sandbox does (and no
	// sandbox may claim them for routing).
	for _, r := range []string{NewSandboxUser, ControlUser} {
		if g.doors.Addr(r).Equal(ta.IP) {
			return r, true, true
		}
	}
	names := make([]string, 0, 16)
	for _, b := range g.mgr.List() {
		names = append(names, b.Name)
	}
	if n, found := g.doors.Resolve(ta.IP, names); found {
		return n, true, true
	}
	return "", false, true
}

// domainHint is the base domain for user-facing "ssh <x>.<domain>" hints.
func (g *Gateway) domainHint() string {
	if g.domain != "" {
		return g.domain
	}
	return "<domain>"
}

// dialUpstream connects to the VM's sshd, retrying briefly since a freshly
// resumed/booted VM may not be accepting connections yet.
func (g *Gateway) dialUpstream(ctx context.Context, addr, user string) (*xssh.Client, error) {
	cfg := &xssh.ClientConfig{
		User: user,
		Auth: []xssh.AuthMethod{xssh.PublicKeys(g.upstreamKey)},
		// The gateway provisions the VM and owns the only route to it; there
		// is no prior host key to verify against on first boot.
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         3 * time.Second,
	}
	var lastErr error
	for {
		conn, err := net.DialTimeout("tcp", addr, cfg.Timeout)
		if err == nil {
			c, chans, reqs, err := xssh.NewClientConn(conn, addr, cfg)
			if err == nil {
				return xssh.NewClient(c, chans, reqs), nil
			}
			conn.Close()
			lastErr = err
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("vm ssh not reachable: %w", lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// pipeSession mirrors the client's session (PTY, env, command, window
// changes, streams) onto a session inside the VM and returns the exit code.
func (g *Gateway) pipeSession(s gssh.Session, client *xssh.Client) (int, error) {
	up, err := client.NewSession()
	if err != nil {
		return 1, err
	}
	defer up.Close()

	for _, kv := range s.Environ() {
		if k, v, ok := splitEnv(kv); ok {
			up.Setenv(k, v) //nolint:errcheck // VM sshd may restrict AcceptEnv
		}
	}

	ptyReq, winCh, isPty := s.Pty()
	if isPty {
		modes := xssh.TerminalModes{xssh.ECHO: 1}
		if err := up.RequestPty(ptyReq.Term, ptyReq.Window.Height, ptyReq.Window.Width, modes); err != nil {
			return 1, err
		}
		go func() {
			for win := range winCh {
				up.WindowChange(win.Height, win.Width) //nolint:errcheck
			}
		}()
	}

	stdin, err := up.StdinPipe()
	if err != nil {
		return 1, err
	}
	stdout, err := up.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stderr, err := up.StderrPipe()
	if err != nil {
		return 1, err
	}
	go func() {
		io.Copy(stdin, s) //nolint:errcheck
		stdin.Close()
	}()
	done := make(chan struct{}, 2)
	go func() { io.Copy(s, stdout); done <- struct{}{} }()          //nolint:errcheck
	go func() { io.Copy(s.Stderr(), stderr); done <- struct{}{} }() //nolint:errcheck

	if raw := s.RawCommand(); raw != "" {
		err = up.Start(raw)
	} else {
		err = up.Shell()
	}
	if err != nil {
		return 1, err
	}
	err = up.Wait()
	<-done
	<-done
	if err == nil {
		return 0, nil
	}
	if exitErr, ok := err.(*xssh.ExitError); ok {
		return exitErr.ExitStatus(), nil
	}
	return 1, err
}

func fail(s gssh.Session, log *slog.Logger, what string, err error) {
	log.Error(what+" failed", "err", err)
	fmt.Fprintf(s.Stderr(), "sparkbox: %s failed: %v\r\n", what, err)
	s.Exit(1) //nolint:errcheck
}

// failStart is fail plus friendly, actionable guidance for the resource-limit
// errors, which are expected user-facing conditions rather than faults.
func (g *Gateway) failStart(s gssh.Session, log *slog.Logger, what string, err error) {
	var limit *host.LimitError
	if errors.As(err, &limit) {
		log.Info("start refused: per-owner limit", "running", limit.Running)
		fmt.Fprintf(s.Stderr(),
			"sparkbox: you already have %d running sandboxes (max %d): %s\r\n"+
				"Pause one to free a slot, e.g.:  ssh %s@<gateway> pause %s\r\n",
			len(limit.Running), limit.Max, strings.Join(limit.Running, ", "),
			ControlUser, limit.Running[0])
		s.Exit(1) //nolint:errcheck
		return
	}
	var capacity *host.CapacityError
	if errors.As(err, &capacity) {
		log.Info("start refused: host at capacity", "used_mb", capacity.UsedMB, "budget_mb", capacity.BudgetMB)
		fmt.Fprintf(s.Stderr(),
			"sparkbox: host is at capacity (%d/%d MB allocated). Try again shortly, or pause a sandbox:  ssh %s@<gateway> list\r\n",
			capacity.UsedMB, capacity.BudgetMB, ControlUser)
		s.Exit(1) //nolint:errcheck
		return
	}
	fail(s, log, what, err)
}

// handleControl serves the `ctl@` out-of-band channel: managing sandboxes
// without dialing into one. It only ever touches the caller's own sandboxes.
func (g *Gateway) handleControl(s gssh.Session, user string, log *slog.Logger) {
	args := s.Command()
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), controlUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	case "list":
		n := 0
		for _, b := range g.mgr.List() {
			if b.Owner != user {
				continue
			}
			fmt.Fprintf(s, "%-24s %s\r\n", b.Name, b.State)
			n++
		}
		if n == 0 {
			fmt.Fprint(s, "no sandboxes yet — create one with: ssh new@<gateway>\r\n")
		}
		s.Exit(0) //nolint:errcheck
	case "pause":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), "usage: ssh ctl@<gateway> pause <name>\r\n")
			s.Exit(2) //nolint:errcheck
			return
		}
		name := args[1]
		box, ok := g.mgr.Get(name)
		if !ok || box.Owner != user {
			// Same message either way: don't leak other users' sandbox names.
			fmt.Fprintf(s.Stderr(), "sparkbox: no sandbox named %q\r\n", name)
			s.Exit(1) //nolint:errcheck
			return
		}
		ctx, cancel := context.WithTimeout(s.Context(), pauseTimeout)
		defer cancel()
		if err := g.mgr.Pause(ctx, name); err != nil {
			fail(s, log, "pause", err)
			return
		}
		fmt.Fprintf(s, "paused %s\r\n", name)
		s.Exit(0) //nolint:errcheck
	default:
		fmt.Fprintf(s.Stderr(), "unknown command %q\r\n%s", args[0], controlUsage)
		s.Exit(2) //nolint:errcheck
	}
}

func splitEnv(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}

// newName returns a fun, unused "adjective-noun" sandbox name. If the random
// pool keeps colliding with existing sandboxes, it falls back to appending a
// short hex suffix so a name is always produced.
func (g *Gateway) newName() string {
	for i := 0; i < 8; i++ {
		n := randomName()
		if _, exists := g.mgr.Get(n); !exists {
			return n
		}
	}
	return randomName() + "-" + randomSuffix()
}

func randomSuffix() string {
	b := make([]byte, 3)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}
