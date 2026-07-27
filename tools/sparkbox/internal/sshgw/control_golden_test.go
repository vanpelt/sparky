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
	"errors"
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

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
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

// fakeRoster is a node roster in a slice. It is not the real store on purpose:
// what these rows have to exercise — a machine that is online, one still
// waiting for approval, one that would strand sandboxes if it were removed — is
// a join of the roster and the live fleet that no single store can produce on
// its own, and the ctl@ rendering is what this file is about.
type fakeRoster struct{ nodes []ctlops.NodeInfo }

func (f *fakeRoster) ListNodes() ([]ctlops.NodeInfo, error) {
	return append([]ctlops.NodeInfo(nil), f.nodes...), nil
}

func (f *fakeRoster) ApproveNode(fp, by string) (ctlops.NodeInfo, error) {
	for i := range f.nodes {
		if f.nodes[i].FP != "" && f.nodes[i].FP == fp {
			f.nodes[i].Status, f.nodes[i].ApprovedBy = "approved", by
			return f.nodes[i], nil
		}
	}
	return ctlops.NodeInfo{}, errors.New("no such node")
}

func (f *fakeRoster) RemoveNode(name string) error {
	for i := range f.nodes {
		if f.nodes[i].Name == name {
			f.nodes = append(f.nodes[:i:i], f.nodes[i+1:]...)
			return nil
		}
	}
	return errors.New("no such node")
}

// The fixture's fingerprints, full length: `node approve` checks the shape of
// what it is given, so a short stand-in would be refused as malformed rather
// than looked up.
const (
	fpNodeB    = "SHA256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fpNewcomer = "SHA256:ccccccccccccccccccccccccccccccccccccccccccc"
	// fpNobody is well-formed and belongs to no row.
	fpNobody = "SHA256:ddddddddddddddddddddddddddddddddddddddddddd"
)

// testRoster is the fleet every ctl@ node row is rendered against: this
// gateway, an approved machine holding sandboxes, and one that has enrolled and
// is waiting to be let in.
func testRoster() *fakeRoster {
	return &fakeRoster{nodes: []ctlops.NodeInfo{
		{Name: "here", Status: "approved", Online: true, Local: true, Arch: "arm64", Sandboxes: 1},
		{Name: "node-b", Status: "approved", Online: true, FP: fpNodeB, Arch: "amd64", Sandboxes: 2},
		{Name: "newcomer", Status: "pending", FP: fpNewcomer},
	}}
}

// ctlStack is a gateway wired to real stores on a temp dir and the mock VM
// driver. It deliberately has no secrets store, so the "tagging is not enabled"
// rendering is exercised rather than assumed.
type ctlStack struct {
	gw     *Gateway
	mgr    *host.Manager
	users  *users.Store
	roster *fakeRoster
	key    gssh.PublicKey
	log    *slog.Logger
}

func newCtlStack(t *testing.T) *ctlStack {
	t.Helper()
	return newCtlStackWith(t, testRoster())
}

// newCtlStackWith builds the stack against a given roster; a nil one is a
// single-box host, where every node command is KindDisabled. Each tweak gets
// the config the gateway would have built for itself, before the Ops is
// constructed from it — the same window main has.
func newCtlStackWith(t *testing.T, roster *fakeRoster, tweaks ...func(*ctlops.Config)) *ctlStack {
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
	// opsy is seeded rather than invited, which is the only thing that makes an
	// operator — the node commands are the second operator gate in the tree.
	for _, h := range []string{"alice", "mallory", "opsy"} {
		k := pub
		if h != "alice" {
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
		invitedBy := "someone"
		if h == "opsy" {
			invitedBy = users.OperatorInviter
		}
		if err := userStore.Create(h, k, h+"@example.test", "signup", invitedBy); err != nil {
			t.Fatal(err)
		}
	}

	opts := GatewayOptions{
		Manager: mgr, Users: userStore, HostKey: hostKey, UpstreamKey: upstreamKey,
		Logger: log, Domain: "hivemind.tools",
		Routes: routeStore, Schedules: schedStore,
	}
	// The control plane is built from the gateway's own wiring and then given a
	// roster, so every other row in the golden table is rendered by exactly the
	// Ops the gateway would have built for itself.
	cfg := opsConfig(opts)
	if roster != nil {
		cfg.Nodes = roster
	}
	cfg.GatewayGuestSubnet = "10.200.0.0/20"
	for _, tweak := range tweaks {
		tweak(&cfg)
	}
	ops := ctlops.New(cfg)
	t.Cleanup(ops.Close)
	opts.Ops = ops

	gw := New(opts)
	return &ctlStack{gw: gw, mgr: mgr, users: userStore, roster: roster, key: pub, log: log}
}

// newCtlStackGitHub builds the stack with the GitHub halves wired: device is
// the OAuth device flow (nil models a host with no --github-client-id, which is
// what makes the key check the only path) and keys is github.com/<login>.keys.
//
// It is a separate constructor rather than fields on ctlStack because both are
// decided when the Ops is built, exactly as they are in main: a host either has
// an app configured at startup or it does not, and nothing switches it later.
func newCtlStackGitHub(t *testing.T, device ctlops.GitHubDeviceFlow, keys ctlops.GitHubKeys) *ctlStack {
	t.Helper()
	return newCtlStackWith(t, testRoster(), func(cfg *ctlops.Config) {
		cfg.GitHubDevice = device
		if keys != nil {
			cfg.GitHub = keys
		}
	})
}

// newSession builds an empty session for handle, for the handful of assertions
// that call a gateway method directly rather than through a ctl command.
func (st *ctlStack) newSession(handle string) *ctlSession {
	ctx := &ctlContext{Context: context.Background(), vals: map[any]any{}}
	ctx.SetValue(authedUserKey, handle)
	ctx.SetValue(authedKeyKey, st.key)
	return &ctlSession{ctx: ctx}
}

// run drives one ctl command as handle, returning the session it wrote into.
func (st *ctlStack) run(t *testing.T, handle string, args ...string) *ctlSession {
	t.Helper()
	s := st.newSession(handle)
	s.cmd = args
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
			"ssh ctl@hivemind.tools github link\r\n",
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
	}, {
		// `node` with no subcommand is `node ls`, so a non-operator meets the
		// same one-sentence refusal either way: a node name is fleet topology.
		name: "node with no subcommand", handle: "alice", args: []string{"node"},
		wantErr: "sparkbox: only operators can list the machines in this fleet.\r\n", wantExit: 1,
	}, {
		name: "node ls as a non-operator", handle: "alice", args: []string{"node", "ls"},
		wantErr: "sparkbox: only operators can list the machines in this fleet.\r\n", wantExit: 1,
	}, {
		name: "node ls as an operator", handle: "opsy", args: []string{"node", "ls"},
		wantOut: "here (this gateway)          approved  online   arm64    1 sandbox\r\n" +
			"node-b                       approved  online   amd64    2 sandboxes   " + fpNodeB + "\r\n" +
			"newcomer                     pending   offline  -        0 sandboxes   " + fpNewcomer + "\r\n",
		wantExit: 0,
	}, {
		name: "node approve without a fingerprint", handle: "opsy", args: []string{"node", "approve"},
		wantErr: "sparkbox: a node fingerprint is required.\r\n", wantExit: 2,
	}, {
		name: "node approve of a machine that never enrolled", handle: "opsy",
		args:    []string{"node", "approve", fpNobody, "--guest-subnet", "10.201.0.0/20"},
		wantErr: "sparkbox: no node in this fleet holds the key " + fpNobody + "\r\n", wantExit: 1,
	}, {
		// A name is not a way in, even a name that IS on the roster. The
		// refusal must not answer with the fingerprint that holds it: that
		// would turn the ceremony into a paste nobody compared.
		name: "node approve by name", handle: "opsy",
		args: []string{"node", "approve", "newcomer", "--guest-subnet", "10.201.0.0/20"},
		wantErr: "sparkbox: \"newcomer\" is not an SSH key fingerprint. A machine is approved by the key it " +
			"holds, not by the name it asked for — the name is chosen by whoever is enrolling, so approving " +
			"one would trust a stranger's word. Read the fingerprint off the machine itself (it prints one " +
			"at startup), check it against `node ls`, and approve that.\r\n", wantExit: 2,
	}, {
		// The count is the message: removing the row would not delete those
		// sandboxes, only strand them.
		name: "node rm while it still holds sandboxes", handle: "opsy", args: []string{"node", "rm", "node-b"},
		wantErr: "sparkbox: node \"node-b\" still holds 2 sandboxes.\r\n", wantExit: 1,
	}, {
		name: "node rm without a name", handle: "opsy", args: []string{"node", "rm"},
		wantErr: "usage: ssh ctl@<gateway> node rm <name>\r\n", wantExit: 2,
	}, {
		name: "node with an unknown subcommand", handle: "opsy", args: []string{"node", "wat"},
		wantErr: "unknown node command \"wat\"\r\n" + controlUsage, wantExit: 2,
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
	if _, err := st.mgr.EnsureReady(context.Background(), "alice-box"); err != nil {
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

// TestControlNodeOnASingleBox: a host nobody can join answers every node
// command with the same sentence, including the ones whose arguments are wrong,
// so an operator learns what the host is rather than what they typed.
func TestControlNodeOnASingleBox(t *testing.T) {
	st := newCtlStackWith(t, nil)
	const want = "sparkbox: this host is not a fleet gateway.\r\n"
	for _, args := range [][]string{
		{"node"}, {"node", "ls"}, {"node", "approve", "node-b"}, {"node", "rm"}, {"node", "wat"},
	} {
		s := st.run(t, "opsy", args...)
		if s.stderr.String() != want || s.code != 1 {
			t.Errorf("%v = exit %d, stderr %q; want exit 1 and %q", args, s.code, s.stderr.String(), want)
		}
	}
}

// TestControlNodeApproveAndRemove is the happy path the table cannot hold: both
// mutate the roster, so they run on their own stack.
func TestControlNodeApproveAndRemove(t *testing.T) {
	st := newCtlStack(t)

	s := st.run(t, "opsy", "node", "approve", fpNewcomer,
		"--guest-subnet", "10.201.0.0/20")
	if s.code != 0 || s.out.String() != "approved newcomer ("+fpNewcomer+") — it can carry sandboxes now\r\n" {
		t.Fatalf("approve = exit %d, stdout %q, stderr %q", s.code, s.out.String(), s.stderr.String())
	}
	if st.roster.nodes[2].Status != "approved" || st.roster.nodes[2].ApprovedBy != "opsy" {
		t.Errorf("roster row after approve = %+v", st.roster.nodes[2])
	}

	// A machine holding nothing can go; the sentence says it may come back,
	// because removal revokes the approval rather than banning the key.
	s = st.run(t, "opsy", "node", "rm", "newcomer")
	if s.code != 0 || s.out.String() != "removed node \"newcomer\" — it may enrol again and wait for approval\r\n" {
		t.Fatalf("rm = exit %d, stdout %q, stderr %q", s.code, s.out.String(), s.stderr.String())
	}
	if len(st.roster.nodes) != 2 {
		t.Errorf("roster still holds %d rows", len(st.roster.nodes))
	}

	// A non-operator reaches neither.
	s = st.run(t, "alice", "node", "rm", "node-b")
	if s.code != 1 || len(st.roster.nodes) != 2 {
		t.Errorf("a non-operator's rm = exit %d, roster %d rows", s.code, len(st.roster.nodes))
	}
}
