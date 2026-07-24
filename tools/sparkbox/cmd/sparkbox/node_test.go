package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"

	_ "modernc.org/sqlite"
)

// deadAddr returns a loopback address nothing is listening on: a port is bound
// long enough to be handed out and then released, which is what a gateway that
// is down looks like from a node.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// testNodeOptions is a node with the mock driver and no gateway worth reaching.
func testNodeOptions(t *testing.T, gateway string) nodeOptions {
	t.Helper()
	return nodeOptions{
		gateway:     gateway,
		nodeName:    "node-under-test",
		arch:        "amd64",
		driverName:  "mock",
		stateDir:    t.TempDir(),
		idleBalloon: time.Minute,
		idleTimeout: 30 * time.Minute,
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// runNodeInBackground starts a node and returns its state dir plus the channel
// its return value will arrive on. Cancelling is left to the caller: half these
// tests are about what happens while it is still running.
func runNodeInBackground(t *testing.T, ctx context.Context, opts nodeOptions) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- runNode(ctx, opts) }()
	// The node key is minted before anything else, so its appearance is the
	// cheapest proof that runNode got past its own construction rather than
	// returning an error nobody has read yet.
	waitFor(t, "the node to mint its key", func() bool {
		_, err := os.Stat(filepath.Join(opts.stateDir, "node_key.pem"))
		return err == nil
	})
	return done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A node must keep serving through a gateway it cannot reach. This is the whole
// reason the link supervisor has no error channel: serve() returns on the first
// value in its errCh, so a supervisor that reported a transport failure upward
// would turn a routine gateway restart — under systemd's Restart=always — into
// a cold restart of every VM on this machine.
func TestNodeModeSurvivesAnUnreachableGateway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := testNodeOptions(t, deadAddr(t))
	done := runNodeInBackground(t, ctx, opts)

	// Long enough for several dial attempts to fail and be backed off from.
	select {
	case err := <-done:
		t.Fatalf("node mode returned while its gateway was unreachable: %v", err)
	case <-time.After(3 * time.Second):
	}

	// And it still stops when it is actually asked to.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("node mode did not return after its context was cancelled")
	}
}

// A node holds VMs, not the fleet. sparkbox.db is where accounts, secrets,
// routes, schedules, netrules, placements and the node roster all live, so the
// blunt form of "a node opens none of them" is that the file is not there at
// all — and if some later change does open one, this says which.
func TestNodeModeOpensNoGatewayStores(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := testNodeOptions(t, deadAddr(t))
	done := runNodeInBackground(t, ctx, opts)
	cancel()
	<-done

	dbPath := filepath.Join(opts.stateDir, "sparkbox.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return
	}
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		t.Errorf("node mode created the gateway table %q in %s", name, dbPath)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// The gateway's upstream key is the one piece of gateway material a node holds,
// and where it comes from decides whether a node that reboots during an outage
// can resume its pinned VMs.
func TestGatewayUpstreamKeyResolution(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "gateway_upstream_key.pub")
	flagKey := publicKeyLine(t)
	cachedKey := publicKeyLine(t)
	keyFile := filepath.Join(dir, "from-file.pub")
	if err := os.WriteFile(keyFile, []byte(flagKey), 0o600); err != nil {
		t.Fatal(err)
	}

	// First boot with nothing to go on: not an error, just an unknown key. The
	// link supplies it, and Create refuses in the meantime.
	got, err := gatewayUpstreamKey("", cache)
	if err != nil || got != "" {
		t.Fatalf("first boot = %q, %v; want empty and no error", got, err)
	}

	if err := os.WriteFile(cache, []byte(cachedKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err = gatewayUpstreamKey("", cache); err != nil || got != cachedKey {
		t.Fatalf("cached = %q, %v; want %q", got, err, cachedKey)
	}
	// An operator who names a key means it, cache or no cache — both as a
	// literal line and as a path to one.
	if got, err = gatewayUpstreamKey(flagKey, cache); err != nil || got != flagKey {
		t.Fatalf("literal flag = %q, %v; want %q", got, err, flagKey)
	}
	if got, err = gatewayUpstreamKey(keyFile, cache); err != nil || got != flagKey {
		t.Fatalf("flag as path = %q, %v; want %q", got, err, flagKey)
	}
	if _, err = gatewayUpstreamKey("not-a-key-and-not-a-file", cache); err == nil {
		t.Error("a value that is neither a key nor a file should be refused")
	}
}

// acceptWelcome is the only thing a node does with a welcome, so it fails the
// link rather than shrugging: a machine that cannot learn the key its guests
// will trust must not go on to boot VMs nobody can log into.
func TestAcceptWelcomeInstallsAndCachesTheKey(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "gateway_upstream_key.pub")
	mgr := testNodeManager(t, dir)
	line := publicKeyLine(t)

	if err := acceptWelcome(nodelink.Welcome{GatewayUpstreamPub: line}, mgr, cache); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != line {
		t.Errorf("cached %q, want %q", b, line)
	}
	// And the manager took it: the next boot preloads exactly what was cached.
	if got, err := gatewayUpstreamKey("", cache); err != nil || got != line {
		t.Errorf("preload after welcome = %q, %v; want %q", got, err, line)
	}

	for _, bad := range []string{"", "   ", "not a public key"} {
		if err := acceptWelcome(nodelink.Welcome{GatewayUpstreamPub: bad}, mgr, cache); err == nil {
			t.Errorf("welcome with key %q was accepted", bad)
		}
	}
}

func TestImageNamesListsRootfsTemplates(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"universal.ext4", "minimal.ext4", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "stale.ext4"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := imageNames(dir)
	if want := []string{"minimal", "universal"}; !slices.Equal(got, want) {
		t.Errorf("imageNames = %v, want %v", got, want)
	}
	// A node with no images is a node nothing gets placed on, not one that
	// refuses to start.
	if got := imageNames(filepath.Join(dir, "gone")); got != nil {
		t.Errorf("missing dir = %v, want nil", got)
	}
	if got := imageNames(""); got != nil {
		t.Errorf("unset dir = %v, want nil", got)
	}
}

// publicKeyLine mints a throwaway key and returns it in the exact shape the
// gateway sends over the link.
func publicKeyLine(t *testing.T) string {
	t.Helper()
	signer, err := sshgw.LoadOrCreateKey(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	return string(xssh.MarshalAuthorizedKey(signer.PublicKey()))
}

// testNodeManager is a manager shaped like a node's: no stores, no front door,
// and no gateway key until a welcome supplies one.
func testNodeManager(t *testing.T, dir string) *host.Manager {
	t.Helper()
	signer, err := sshgw.LoadOrCreateKey(dir, "node_key")
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: mock.New(dir, signer),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}
