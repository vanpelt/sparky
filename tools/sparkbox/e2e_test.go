package sparkbox_test

// End-to-end test of the full MVP stack with the mock driver: manager + SSH
// gateway wired together in-process, exercised by a real SSH client. This is
// the same path a user takes: ssh new@gateway, run commands, get paused,
// resume on next connect.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

type testStack struct {
	mgr     *host.Manager
	addr    string
	userKey xssh.Signer
	users   *users.Store
}

func newStack(t *testing.T) *testStack {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	hostKey, err := sshgw.LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	upstreamKey, err := sshgw.LoadOrCreateKey(dir, "gateway_upstream_key")
	if err != nil {
		t.Fatal(err)
	}

	// One test user with a fresh client key.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userKey, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := xssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	usersPath := filepath.Join(dir, "users.conf")
	line := fmt.Sprintf("tester %s", xssh.MarshalAuthorizedKey(sshPub))
	if err := os.WriteFile(usersPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	// The gateway authenticates against the sqlite identity store, seeded from
	// users.conf exactly as a real host bootstraps itself.
	userStore, err := users.Open(filepath.Join(dir, "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { userStore.Close() })
	if err := users.SeedFile(usersPath, userStore, log); err != nil {
		t.Fatal(err)
	}

	driver := mock.New(dir, hostKey)
	t.Cleanup(func() { driver.Close() })

	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver,
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}

	gw := sshgw.New(sshgw.GatewayOptions{
		Manager: mgr, Users: userStore, HostKey: hostKey, UpstreamKey: upstreamKey,
		DefaultImage: "ubuntu", Logger: log,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.Server("")
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })

	return &testStack{mgr: mgr, addr: ln.Addr().String(), userKey: userKey, users: userStore}
}

func (ts *testStack) dial(t *testing.T, sshUser string) *xssh.Client {
	t.Helper()
	client, err := xssh.Dial("tcp", ts.addr, &xssh.ClientConfig{
		User:            sshUser,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(ts.userKey)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), //nolint:gosec // test
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial %s@%s: %v", sshUser, ts.addr, err)
	}
	return client
}

func (ts *testStack) run(t *testing.T, sshUser, cmd string) (string, string) {
	t.Helper()
	client := ts.dial(t, sshUser)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Run(cmd); err != nil {
		t.Fatalf("run %q: %v (stderr: %s)", cmd, err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

func TestEndToEnd(t *testing.T) {
	ts := newStack(t)
	ctx := context.Background()

	// Create via the manager (the API server is a thin layer over it).
	if _, err := ts.mgr.Create(ctx, "demo", "tester", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}

	// Run a command through the gateway; state must persist across sessions.
	ts.run(t, "demo", "echo hello-from-sandbox > marker.txt")
	out, _ := ts.run(t, "demo", "cat marker.txt")
	if strings.TrimSpace(out) != "hello-from-sandbox" {
		t.Fatalf("marker roundtrip failed, got %q", out)
	}

	// Suspend, then resume-on-connect must bring it back with disk intact.
	if err := ts.mgr.Pause(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if box, _ := ts.mgr.Get("demo"); box.State != vmm.StatePaused {
		t.Fatalf("expected paused, got %s", box.State)
	}
	out, _ = ts.run(t, "demo", "cat marker.txt")
	if strings.TrimSpace(out) != "hello-from-sandbox" {
		t.Fatalf("post-resume marker read failed, got %q", out)
	}
	if box, _ := ts.mgr.Get("demo"); box.State != vmm.StateRunning {
		t.Fatalf("expected running after resume-on-connect, got %s", box.State)
	}
}

func TestNewSandboxOnConnect(t *testing.T) {
	ts := newStack(t)

	// A plain connect (no command) creates a sandbox and drops into its
	// shell; immediate stdin EOF ends the shell so the session returns.
	client := ts.dial(t, sshgw.NewSandboxUser)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	sess.Stdin = strings.NewReader("")
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("shell: %v (stderr: %s)", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "created sandbox") {
		t.Fatalf("expected creation banner on stderr, got %q", stderr.String())
	}

	boxes := ts.mgr.List()
	if len(boxes) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(boxes))
	}
	// Names are playful "adjective-noun" (optionally with a -hex suffix on
	// collision), owned by the connecting user.
	if boxes[0].Owner != "tester" || !nameRe.MatchString(boxes[0].Name) {
		t.Fatalf("unexpected sandbox record: %+v", boxes[0])
	}

	// new@'s arguments are tags, never a guest command. This stack wires no
	// tag store, so the door must refuse the word — not run it in the guest.
	tagClient := ts.dial(t, sshgw.NewSandboxUser)
	defer tagClient.Close()
	tagSess, err := tagClient.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer tagSess.Close()
	var tagErr bytes.Buffer
	tagSess.Stderr = &tagErr
	if err := tagSess.Run("true"); err == nil {
		t.Fatal("expected tag-create to fail on a host without tagging")
	}
	if !strings.Contains(tagErr.String(), "tagging is not enabled") {
		t.Fatalf("expected tagging-disabled error, got %q", tagErr.String())
	}
	if got := len(ts.mgr.List()); got != 1 {
		t.Fatalf("failed tag-create must not leave a sandbox, have %d", got)
	}
}

var nameRe = regexp.MustCompile(`^[a-z]+-[a-z]+(-[0-9a-f]+)?$`)

func TestOwnershipAndAuth(t *testing.T) {
	ts := newStack(t)
	ctx := context.Background()

	// A sandbox owned by someone else must be invisible to our user.
	if _, err := ts.mgr.Create(ctx, "theirs", "someone-else", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	client := ts.dial(t, "theirs")
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	if err := sess.Run("true"); err == nil {
		t.Fatal("expected failure accessing another user's sandbox")
	}
	if !strings.Contains(stderr.String(), "no sandbox named") {
		t.Fatalf("ownership failure should look like not-found, got %q", stderr.String())
	}

	// An unknown key must be rejected outright.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	badKey, _ := xssh.NewSignerFromKey(priv)
	_, err = xssh.Dial("tcp", ts.addr, &xssh.ClientConfig{
		User:            "theirs",
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(badKey)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), //nolint:gosec // test
		Timeout:         5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected auth failure for unknown key")
	}
}

func TestIdleReaper(t *testing.T) {
	ts := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := ts.mgr.Create(ctx, "sleepy", "tester", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	// balloonAfter=0 disables the balloon stage; this exercises pause + resume.
	go ts.mgr.RunReaper(ctx, 0, 50*time.Millisecond, 25*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if box, _ := ts.mgr.Get("sleepy"); box.State == vmm.StatePaused {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reaper never paused the idle sandbox")
}

// syncBuf merges a session's stdout and stderr into one transcript. It must be
// synchronised: x/crypto/ssh copies each stream in its own goroutine, so
// handing both the same bare bytes.Buffer races and silently drops output.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// newClientKey returns a fresh, unregistered client key.
func newClientKey(t *testing.T) (xssh.Signer, xssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := xssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return signer, sshPub
}

// signup drives the interactive signup dialog with a PTY, feeding it answers
// and returning everything the gateway printed. Answers are terminated with
// \r: that is what an Enter key sends over a PTY, and what the line editor
// reading them recognises.
func (ts *testStack) signup(t *testing.T, signer xssh.Signer, answers ...string) string {
	t.Helper()
	client, err := xssh.Dial("tcp", ts.addr, &xssh.ClientConfig{
		User:            sshgw.SignupUser,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), //nolint:gosec // test
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial signup@: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm", 40, 80, xssh.TerminalModes{xssh.ECHO: 0}); err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuf
	sess.Stdout = &out
	sess.Stderr = &out
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	go func() {
		for _, a := range answers {
			stdin.Write([]byte(a + "\r")) //nolint:errcheck
			time.Sleep(20 * time.Millisecond)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-done: // exit status is asserted by callers via the transcript
	case <-time.After(10 * time.Second):
		t.Fatal("signup dialog hung")
	}
	return out.String()
}

func TestSignupRegistersAnUnregisteredKey(t *testing.T) {
	ts := newStack(t)
	// "tester" is seeded from users.conf, which makes them the operator.
	code, err := ts.users.NewInvite("tester")
	if err != nil {
		t.Fatal(err)
	}
	signer, pub := newClientKey(t)

	// invite code, handle, blank to skip the GitHub link (which would
	// otherwise reach out to github.com from a unit test), then an email.
	out := ts.signup(t, signer, code, "cvp", "", "cvp@example.com")
	if !strings.Contains(out, `registered as "cvp"`) {
		t.Fatalf("signup did not confirm registration:\n%s", out)
	}
	if h, ok := ts.users.Lookup(pub); !ok || h != "cvp" {
		t.Fatalf("Lookup after signup = %q, %v; want cvp, true", h, ok)
	}
	if u, err := ts.users.Get("cvp"); err != nil || u.Email != "cvp@example.com" {
		t.Errorf("email after signup = %q, %v; want cvp@example.com", u.Email, err)
	}

	// The freshly registered key now opens the normal doors as its own account.
	client := ts.dialAs(t, signer, sshgw.ControlUser)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var whoami bytes.Buffer
	sess.Stdout = &whoami
	if err := sess.Run("whoami"); err != nil {
		t.Fatalf("ctl whoami as the new user: %v", err)
	}
	if !strings.Contains(whoami.String(), "handle:  cvp") {
		t.Errorf("the new key does not identify as cvp:\n%s", whoami.String())
	}
}

// dialAs connects with an arbitrary key rather than the stack's default user.
func (ts *testStack) dialAs(t *testing.T, signer xssh.Signer, sshUser string) *xssh.Client {
	t.Helper()
	client, err := xssh.Dial("tcp", ts.addr, &xssh.ClientConfig{
		User:            sshUser,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), //nolint:gosec // test
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial %s@: %v", sshUser, err)
	}
	return client
}

// The invite is what gates who may join; without one, an unregistered key gets
// no account.
func TestSignupWithoutAnInviteIsRefused(t *testing.T) {
	ts := newStack(t)
	signer, pub := newClientKey(t)
	out := ts.signup(t, signer, "not-real", "also-bad", "still-no")
	if strings.Contains(out, "registered as") {
		t.Fatalf("signup succeeded without an invite:\n%s", out)
	}
	if _, ok := ts.users.Lookup(pub); ok {
		t.Fatal("an account was created without a valid invite")
	}
}

// A code is single-use: the second person to present it gets nothing.
func TestInviteCannotBeUsedTwice(t *testing.T) {
	ts := newStack(t)
	code, err := ts.users.NewInvite("tester")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := newClientKey(t)
	if out := ts.signup(t, first, code, "alice", "", ""); !strings.Contains(out, `registered as "alice"`) {
		t.Fatalf("first signup failed:\n%s", out)
	}
	second, pub := newClientKey(t)
	out := ts.signup(t, second, code, "bob", "")
	if strings.Contains(out, "registered as") {
		t.Fatalf("a spent invite was accepted:\n%s", out)
	}
	if _, ok := ts.users.Lookup(pub); ok {
		t.Fatal("a second account was created from one single-use invite")
	}
}

// Handles are immutable and appear in the OIDC subject, so they must be
// first-come.
func TestSignupRefusesATakenHandle(t *testing.T) {
	ts := newStack(t)
	code, err := ts.users.NewInvite("tester")
	if err != nil {
		t.Fatal(err)
	}
	signer, pub := newClientKey(t)
	// "tester" is already seeded; the dialog should re-ask, then accept "cvp".
	out := ts.signup(t, signer, code, "tester", "cvp", "", "")
	if !strings.Contains(out, "is taken") {
		t.Errorf("dialog did not report the handle as taken:\n%s", out)
	}
	if h, ok := ts.users.Lookup(pub); !ok || h != "cvp" {
		t.Errorf("Lookup = %q, %v; want cvp", h, ok)
	}
}

// Re-running signup with a key that already has an account should say so
// rather than start a second registration.
func TestSignupOnAnAlreadyRegisteredKey(t *testing.T) {
	ts := newStack(t)
	out := ts.signup(t, ts.userKey)
	if !strings.Contains(out, "already registered") {
		t.Errorf("expected an 'already registered' notice:\n%s", out)
	}
}

func TestControlWhoamiAndKeys(t *testing.T) {
	ts := newStack(t)
	out, _ := ts.run(t, sshgw.ControlUser, "whoami")
	if !strings.Contains(out, "handle:  tester") {
		t.Errorf("whoami:\n%s", out)
	}
	// The subject is what a relying party's service account binds to.
	if !strings.Contains(out, "sparkbox:user:tester") {
		t.Errorf("whoami did not report the OIDC subject:\n%s", out)
	}

	// A second key can be added, and then authenticates as the same account.
	_, pub := newClientKey(t)
	line := strings.TrimSpace(string(xssh.MarshalAuthorizedKey(pub)))
	ts.run(t, sshgw.ControlUser, "keys add "+line+" work-laptop")
	if h, ok := ts.users.Lookup(pub); !ok || h != "tester" {
		t.Errorf("added key authenticates as %q, %v; want tester", h, ok)
	}
	out, _ = ts.run(t, sshgw.ControlUser, "keys list")
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 2 {
		t.Errorf("keys list showed %d keys, want 2:\n%s", n, out)
	}
}

// Only operators may invite by default (--invites-per-user defaults to 0).
func TestOnlyOperatorsCanInvite(t *testing.T) {
	ts := newStack(t)
	out, _ := ts.run(t, sshgw.ControlUser, "invite")
	if !strings.Contains(out, "invite code:") {
		t.Fatalf("the seeded operator could not invite:\n%s", out)
	}
	code := strings.Fields(strings.SplitN(out, "invite code: ", 2)[1])[0]

	signer, _ := newClientKey(t)
	if o := ts.signup(t, signer, code, "guest", "", ""); !strings.Contains(o, `registered as "guest"`) {
		t.Fatalf("guest signup failed:\n%s", o)
	}
	// The invited user is not an operator, so they get no invites of their own.
	client := ts.dialAs(t, signer, sshgw.ControlUser)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if err := sess.Run("invite"); err == nil {
		t.Fatalf("a non-operator minted an invite:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "only operators") {
		t.Errorf("unexpected refusal: %q", stderr.String())
	}
}
