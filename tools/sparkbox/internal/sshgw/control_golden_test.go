package sshgw

// The ctl@ channel's output is a shipped interface: people script against it,
// read its error sentences, and branch on its exit codes. Moving the logic out
// into internal/ctlops must not move any of that, so this file drives
// handleControl through a fake SSH session and pins what comes back — stream by
// stream, byte for byte, exit code included.
//
// The cases that matter most are the boring-looking ones: "no sandbox named" is
// identical for a name that does not exist and a name that belongs to someone
// else, and `resize` reports it *before* it complains about a missing size, so
// the usage line cannot confirm that a stranger's sandbox is real.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// ---------------------------------------------------------------------------
// A gssh.Session that is a pair of buffers
// ---------------------------------------------------------------------------

// ctlContext is the slice of gssh.Context a ctl session actually reads: the
// values the public-key handler stashed, and a cancellable context for the
// command's timeout budget.
type ctlContext struct {
	context.Context
	sync.Mutex
	vals map[any]any
}

func (c *ctlContext) User() string          { return ControlUser }
func (c *ctlContext) SessionID() string     { return "test-session" }
func (c *ctlContext) ClientVersion() string { return "SSH-2.0-test" }
func (c *ctlContext) ServerVersion() string { return "SSH-2.0-sparkbox" }
func (c *ctlContext) RemoteAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *ctlContext) LocalAddr() net.Addr   { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *ctlContext) Permissions() *gssh.Permissions {
	return &gssh.Permissions{Permissions: &xssh.Permissions{}}
}
func (c *ctlContext) SetValue(k, v any) { c.vals[k] = v }
func (c *ctlContext) Value(k any) any   { return c.vals[k] }

// ctlSession records what a command wrote to each stream and the exit code it
// asked for. Only the first Exit is kept: a real session is closed by it, so
// anything a buggy handler wrote afterwards would never reach a client either.
type ctlSession struct {
	cmd    []string
	ctx    *ctlContext
	out    bytes.Buffer
	stderr bytes.Buffer
	code   int
	exited bool
}

func (s *ctlSession) Read([]byte) (int, error)    { return 0, nil }
func (s *ctlSession) Write(p []byte) (int, error) { return s.out.Write(p) }
func (s *ctlSession) Close() error                { return nil }
func (s *ctlSession) CloseWrite() error           { return nil }
func (s *ctlSession) Stderr() io.ReadWriter       { return &s.stderr }
func (s *ctlSession) SendRequest(string, bool, []byte) (bool, error) {
	return true, nil
}
func (s *ctlSession) User() string         { return ControlUser }
func (s *ctlSession) RemoteAddr() net.Addr { return s.ctx.RemoteAddr() }
func (s *ctlSession) LocalAddr() net.Addr  { return s.ctx.LocalAddr() }
func (s *ctlSession) Environ() []string    { return nil }
func (s *ctlSession) Command() []string    { return s.cmd }
func (s *ctlSession) RawCommand() string   { return strings.Join(s.cmd, " ") }
func (s *ctlSession) Subsystem() string    { return "" }
func (s *ctlSession) PublicKey() gssh.PublicKey {
	k, _ := s.ctx.Value(authedKeyKey).(gssh.PublicKey)
	return k
}
func (s *ctlSession) Context() gssh.Context { return s.ctx }
func (s *ctlSession) Permissions() gssh.Permissions {
	return gssh.Permissions{Permissions: &xssh.Permissions{}}
}
func (s *ctlSession) Pty() (gssh.Pty, <-chan gssh.Window, bool) { return gssh.Pty{}, nil, false }
func (s *ctlSession) Signals(chan<- gssh.Signal)                {}
func (s *ctlSession) Break(chan<- bool)                         {}

func (s *ctlSession) Exit(code int) error {
	if !s.exited {
		s.code, s.exited = code, true
	}
	return nil
}

// ---------------------------------------------------------------------------
// The stack under test
// ---------------------------------------------------------------------------

// ctlStack is a gateway wired to real stores on a temp dir and the mock VM
// driver. It deliberately has no secrets store, so the "tagging is not enabled"
// rendering is exercised rather than assumed.
type ctlStack struct {
	gw    *Gateway
	mgr   *host.Manager
	users *users.Store
	key   gssh.PublicKey
	log   *slog.Logger
}

func newCtlStack(t *testing.T) *ctlStack {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	hostKey, err := LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	upstreamKey, err := LoadOrCreateKey(dir, "gateway_upstream_key")
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: mock.New(dir, hostKey),
		GatewayPublicKey: PublicKeyLine(upstreamKey), Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	userStore, err := users.Open(filepath.Join(dir, "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { userStore.Close() }) //nolint:errcheck
	routeStore, err := routes.Open(filepath.Join(dir, "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { routeStore.Close() }) //nolint:errcheck
	schedStore, err := schedule.Open(filepath.Join(dir, "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { schedStore.Close() }) //nolint:errcheck

	// alice's key is the one the fake session presents, so `keys list` marks it
	// and `whoami` echoes it.
	aliceKey := PublicKeyLine(upstreamKey)
	pub, _, _, _, err := xssh.ParseAuthorizedKey([]byte(aliceKey))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"alice", "mallory"} {
		k := pub
		if h == "mallory" {
			// A second, distinct key: the store refuses to link one key twice.
			s, err := LoadOrCreateKey(dir, h+"_key")
			if err != nil {
				t.Fatal(err)
			}
			k, _, _, _, err = xssh.ParseAuthorizedKey([]byte(PublicKeyLine(s)))
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := userStore.Create(h, k, h+"@example.test", "signup", "someone"); err != nil {
			t.Fatal(err)
		}
	}

	gw := New(GatewayOptions{
		Manager: mgr, Users: userStore, HostKey: hostKey, UpstreamKey: upstreamKey,
		Logger: log, Domain: "hivemind.tools",
		Routes: routeStore, Schedules: schedStore,
	})
	return &ctlStack{gw: gw, mgr: mgr, users: userStore, key: pub, log: log}
}

// run drives one ctl command as handle, returning the session it wrote into.
func (st *ctlStack) run(t *testing.T, handle string, args ...string) *ctlSession {
	t.Helper()
	ctx := &ctlContext{Context: context.Background(), vals: map[any]any{}}
	ctx.SetValue(authedUserKey, handle)
	ctx.SetValue(authedKeyKey, st.key)
	s := &ctlSession{cmd: args, ctx: ctx}
	st.gw.handleControl(s, handle, st.log)
	return s
}

// ---------------------------------------------------------------------------
// The golden table
// ---------------------------------------------------------------------------

func TestControlGolden(t *testing.T) {
	st := newCtlStack(t)
	ctx := context.Background()
	if _, err := st.mgr.Create(ctx, "alice-box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if _, err := st.mgr.Create(ctx, "mallory-box", "mallory", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		handle   string
		args     []string
		wantOut  string
		wantErr  string
		wantExit int
	}{{
		name: "no command prints usage to stderr", handle: "alice", args: nil,
		wantErr: controlUsage, wantExit: 2,
	}, {
		name: "help prints the same text to stdout", handle: "alice", args: []string{"help"},
		wantOut: controlUsage, wantExit: 0,
	}, {
		name: "unknown command names it", handle: "alice", args: []string{"nope"},
		wantErr: "unknown command \"nope\"\r\n" + controlUsage, wantExit: 2,
	}, {
		// Owner-scoped: mallory-box exists and is not listed.
		name: "list is owner-scoped", handle: "alice", args: []string{"list"},
		wantOut:  "alice-box                running  scale-to-zero\r\n",
		wantExit: 0,
	}, {
		name: "list with nothing of your own", handle: "nobody", args: []string{"list"},
		wantOut:  "no sandboxes yet — create one with: ssh new@<gateway>\r\n",
		wantExit: 0,
	}, {
		name: "pause without a name is a usage error", handle: "alice", args: []string{"pause"},
		wantErr: "usage: ssh ctl@<gateway> pause <name>\r\n", wantExit: 2,
	}, {
		name: "pause of a sandbox that does not exist", handle: "alice", args: []string{"pause", "ghost"},
		wantErr: "sparkbox: no sandbox named \"ghost\"\r\n", wantExit: 1,
	}, {
		// The masking invariant: byte-identical to the case above, so a stranger
		// cannot tell a real name from an invented one.
		name: "pause of someone else's sandbox", handle: "alice", args: []string{"pause", "mallory-box"},
		wantErr: "sparkbox: no sandbox named \"mallory-box\"\r\n", wantExit: 1,
	}, {
		// Ordering invariant: the not-found answer comes before the missing-size
		// usage line, or the usage line confirms the sandbox exists.
		name: "resize reports not-found before a missing size", handle: "alice", args: []string{"resize", "mallory-box"},
		wantErr: "sparkbox: no sandbox named \"mallory-box\"\r\n", wantExit: 1,
	}, {
		name: "resize without a size", handle: "alice", args: []string{"resize", "alice-box"},
		wantErr: "usage: ssh ctl@<gateway> resize <name> <size>   e.g. 25G\r\n", wantExit: 2,
	}, {
		name: "resize with an unreadable size", handle: "alice", args: []string{"resize", "alice-box", "big"},
		wantErr: "sparkbox: bad size \"big\" — use e.g. 25G or 512M\r\n", wantExit: 2,
	}, {
		name: "tags on a host with no secrets store", handle: "alice", args: []string{"tags", "alice-box"},
		wantErr: "sparkbox: tags failed: tagging is not enabled on this host\r\n", wantExit: 1,
	}, {
		name: "snapshot list when there are none", handle: "alice", args: []string{"snapshot", "list"},
		wantOut: "no snapshots — create one with:\r\n" +
			"  ssh ctl@hivemind.tools snapshot create <box> <name>\r\n",
		wantExit: 0,
	}, {
		name: "snapshot rm of a snapshot you don't have", handle: "alice", args: []string{"snapshot", "rm", "ghost"},
		wantErr: "sparkbox: no snapshot named \"ghost\"\r\n", wantExit: 1,
	}, {
		name: "snapshot with an unknown subcommand", handle: "alice", args: []string{"snapshot", "wat"},
		wantErr: "unknown snapshot command \"wat\"\r\n" + controlUsage, wantExit: 2,
	}, {
		name: "schedule list when there are none", handle: "alice", args: []string{"schedule", "list"},
		wantOut: "no scheduled jobs — add one with:\r\n" +
			"  ssh ctl@hivemind.tools schedule add <box> \"*/30 * * * *\" <cmd>\r\n",
		wantExit: 0,
	}, {
		name: "schedule add on someone else's sandbox", handle: "alice",
		args:    []string{"schedule", "add", "mallory-box", "*/5 * * * *", "echo", "hi"},
		wantErr: "sparkbox: no sandbox named \"mallory-box\"\r\n", wantExit: 1,
	}, {
		// The shipped exit code for a bad cron is 1, not the 2 every other
		// malformed invocation gets, because the store rejected it rather than
		// the parser. Preserved deliberately.
		name: "schedule add with an unparseable cron", handle: "alice",
		args:     []string{"schedule", "add", "alice-box", "not a cron", "echo", "hi"},
		wantExit: 1,
	}, {
		name: "schedule rm of an id you don't own", handle: "alice", args: []string{"schedule", "rm", "sc_ghost"},
		wantErr: "sparkbox: no schedule \"sc_ghost\"\r\n", wantExit: 1,
	}, {
		name: "fork without both names", handle: "alice", args: []string{"fork", "snap"},
		wantErr: "usage: ssh ctl@<gateway> fork <snapshot> <new-name> [--tag <t>]…\r\n" +
			"       list your snapshots with: ssh ctl@<gateway> snapshot list\r\n",
		wantExit: 2,
	}, {
		name: "fork from a snapshot you don't have", handle: "alice", args: []string{"fork", "ghost", "newbox"},
		wantErr: "sparkbox: no snapshot named \"ghost\"\r\n", wantExit: 1,
	}, {
		name: "share of a sandbox that does not exist", handle: "alice", args: []string{"share", "ghost"},
		wantErr: "sparkbox: no sandbox named \"ghost\"\r\n", wantExit: 1,
	}, {
		name: "share without a name", handle: "alice", args: []string{"share"},
		wantErr: "usage: ssh ctl@<gateway> share <name> [public|private]\r\n", wantExit: 2,
	}, {
		name: "share with a visibility that isn't one", handle: "alice", args: []string{"share", "alice-box", "sorta"},
		wantErr: "sparkbox: visibility must be 'public' or 'private', not \"sorta\"\r\n", wantExit: 2,
	}, {
		name: "share reports a sandbox with no routes", handle: "alice", args: []string{"share", "alice-box"},
		wantOut: "alice-box has no web routes yet.\r\n", wantExit: 0,
	}, {
		// The one deliberate exit-code inconsistency in the shipped CLI: a
		// malformed key line exits 1, where every other bad invocation exits 2.
		name: "keys add with something that isn't a key", handle: "alice", args: []string{"keys", "add", "not-a-key"},
		wantErr:  "sparkbox: that isn't a valid authorized_keys line: ssh: no key found\r\n",
		wantExit: 1,
	}, {
		name: "keys add with nothing to add", handle: "alice", args: []string{"keys", "add"},
		wantErr:  "usage: ssh ctl@<gateway> keys add \"ssh-ed25519 AAAA... label\"\r\n",
		wantExit: 2,
	}, {
		name: "keys rm of a fingerprint you don't have", handle: "alice", args: []string{"keys", "rm", "SHA256:nope"},
		wantErr: "sparkbox: no key SHA256:nope on this account\r\n", wantExit: 1,
	}, {
		name: "keys with no subcommand", handle: "alice", args: []string{"keys"},
		wantErr: controlUsage, wantExit: 2,
	}, {
		name: "keys import-github without a link", handle: "alice", args: []string{"keys", "import-github"},
		wantErr: "sparkbox: no GitHub account linked — link one with: " +
			"ssh ctl@hivemind.tools keys verify-github\r\n",
		wantExit: 1,
	}, {
		name: "keys verify-github with no login to check", handle: "alice", args: []string{"keys", "verify-github"},
		wantErr: "usage: ssh ctl@<gateway> keys verify-github <login>\r\n", wantExit: 2,
	}, {
		name: "passkey list when none are enrolled", handle: "alice", args: []string{"passkey", "list"},
		wantOut: "no passkeys — enroll one by signing in at https://login.hivemind.tools\r\n", wantExit: 0,
	}, {
		name: "passkey rm of an id that matches nothing", handle: "alice", args: []string{"passkey", "rm", "zz"},
		wantErr: "sparkbox: no passkey matches \"zz\" — see `passkey list`\r\n", wantExit: 1,
	}, {
		name: "passkey with an unknown subcommand", handle: "alice", args: []string{"passkey", "wat"},
		wantErr: "usage: ssh ctl@<gateway> passkey [list|rm <id>]\r\n", wantExit: 2,
	}, {
		name: "email when none is set", handle: "alice", args: []string{"email"},
		wantOut:  "no email set — add one with: ssh ctl@hivemind.tools email set you@example.com\r\n",
		wantExit: 0,
	}, {
		name: "email set with something that isn't an address", handle: "alice", args: []string{"email", "set", "nope"},
		wantErr: "sparkbox: that doesn't look like an email address\r\n", wantExit: 1,
	}, {
		name: "email set with nothing to set", handle: "alice", args: []string{"email", "set"},
		wantErr: "usage: ssh ctl@<gateway> email set you@example.com\r\n", wantExit: 2,
	}, {
		name: "email with an unknown subcommand", handle: "alice", args: []string{"email", "wat"},
		wantErr: "usage: ssh ctl@<gateway> email [set <addr>|clear]\r\n", wantExit: 2,
	}, {
		name: "invite from a non-operator on an operator-only host", handle: "alice", args: []string{"invite"},
		wantErr: "sparkbox: only operators can mint invite codes here.\r\n", wantExit: 1,
	}, {
		name: "session-token on a host without an edge signer", handle: "alice", args: []string{"session-token"},
		wantErr: "sparkbox: authenticated forwarding isn't enabled on this host.\r\n", wantExit: 1,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s := st.run(t, tc.handle, tc.args...)
			if tc.wantOut != "" || s.out.Len() > 0 {
				if got := s.out.String(); got != tc.wantOut {
					t.Errorf("stdout = %q, want %q", got, tc.wantOut)
				}
			}
			if tc.wantErr != "" {
				if got := s.stderr.String(); got != tc.wantErr {
					t.Errorf("stderr = %q, want %q", got, tc.wantErr)
				}
			}
			if s.code != tc.wantExit {
				t.Errorf("exit = %d, want %d (stderr %q)", s.code, tc.wantExit, s.stderr.String())
			}
			if !s.exited {
				t.Error("command never called Exit; the ssh client would hang")
			}
			// The client's terminal is in raw mode, so a bare \n would leave the
			// cursor mid-line on every message this channel prints.
			for stream, text := range map[string]string{"stdout": s.out.String(), "stderr": s.stderr.String()} {
				if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
					t.Errorf("%s contains a bare \\n: %q", stream, text)
				}
			}
		})
	}
}

// TestControlPauseSucceeds is the happy path the table can't hold: it mutates,
// so it runs on its own stack.
func TestControlPauseSucceeds(t *testing.T) {
	st := newCtlStack(t)
	if _, err := st.mgr.Create(context.Background(), "alice-box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if _, err := st.mgr.EnsureRunning(context.Background(), "alice-box"); err != nil {
		t.Fatal(err)
	}
	s := st.run(t, "alice", "pause", "alice-box")
	if s.code != 0 || s.out.String() != "paused alice-box\r\n" {
		t.Fatalf("pause = exit %d, stdout %q, stderr %q", s.code, s.out.String(), s.stderr.String())
	}
	if box, _ := st.mgr.Get("alice-box"); box.State != "paused" {
		t.Errorf("sandbox state = %q, want paused", box.State)
	}
}

// TestControlWhoami pins the five lines and their order; the subject and
// fingerprint are derived, so they are checked for shape rather than value.
func TestControlWhoami(t *testing.T) {
	st := newCtlStack(t)
	s := st.run(t, "alice", "whoami")
	if s.code != 0 {
		t.Fatalf("whoami exited %d: %q", s.code, s.stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(s.out.String(), "\r\n"), "\r\n")
	if len(lines) != 5 {
		t.Fatalf("whoami printed %d lines, want 5: %q", len(lines), s.out.String())
	}
	for i, prefix := range []string{"handle:  alice", "status:  ", "github:  not linked", "subject: ", "key:     SHA256:"} {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("whoami line %d = %q, want prefix %q", i, lines[i], prefix)
		}
	}
}

// TestControlUsageDocumentsTheOtherDoors: the ctl listing is the only place a
// user who lives in a terminal will ever learn that the same sandboxes are
// reachable from a browser and from HTTP.
func TestControlUsageDocumentsTheOtherDoors(t *testing.T) {
	for _, want := range []string{
		"https://<name>-xterm.<domain>",
		"https://api.<domain>",
		"/docs",
		"session-token",
	} {
		if !strings.Contains(controlUsage, want) {
			t.Errorf("ctl usage never mentions %q", want)
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(controlUsage, "\r\n"), "\r\n") {
		if strings.Contains(line, "\n") {
			t.Errorf("usage line has a bare \\n: %q", line)
		}
	}
}
