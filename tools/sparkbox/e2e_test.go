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
	"strings"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

type testStack struct {
	mgr     *host.Manager
	addr    string
	userKey xssh.Signer
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
	users, err := sshgw.LoadUsers(usersPath)
	if err != nil {
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
		Manager: mgr, Users: users, HostKey: hostKey, UpstreamKey: upstreamKey,
		DefaultImage: "ubuntu", Logger: log,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.Server("")
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })

	return &testStack{mgr: mgr, addr: ln.Addr().String(), userKey: userKey}
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

	_, stderr := ts.run(t, sshgw.NewSandboxUser, "true")
	if !strings.Contains(stderr, "created sandbox") {
		t.Fatalf("expected creation banner on stderr, got %q", stderr)
	}

	boxes := ts.mgr.List()
	if len(boxes) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(boxes))
	}
	if boxes[0].Owner != "tester" || !strings.HasPrefix(boxes[0].Name, "tester-") {
		t.Fatalf("unexpected sandbox record: %+v", boxes[0])
	}
}

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
	go ts.mgr.RunReaper(ctx, 50*time.Millisecond, 25*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if box, _ := ts.mgr.Get("sleepy"); box.State == vmm.StatePaused {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reaper never paused the idle sandbox")
}
