package sshgw

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// stallingListener accepts connections and then says nothing, which is what a
// wedged sshd (or a tunnel whose far end has gone away) looks like from here.
func stallingListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	held := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(held)
				return
			}
			held <- c
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		for c := range held {
			c.Close()
		}
	})
	return ln
}

func testSigner(t *testing.T) xssh.Signer {
	t.Helper()
	key, err := LoadOrCreateKey(t.TempDir(), "upstream_key")
	if err != nil {
		t.Fatalf("mint key: %v", err)
	}
	return key
}

// ClientConfig.Timeout bounds ssh.Dial's own net dial and nothing else, so the
// SSH handshake against a peer that accepts and then stalls used to block
// forever — past the caller's deadline, past the retry loop's own ctx check,
// with xterm's and envsync's budgets cancelling a select that was never
// reached.
func TestDialUpstreamViaBoundsAStalledHandshake(t *testing.T) {
	ln := stallingListener(t)
	key := testSigner(t)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		c, err := DialUpstreamVia(ctx, (&net.Dialer{}).DialContext, ln.Addr().String(), "sparky", key)
		if c != nil {
			c.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dial to a stalled peer succeeded")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("returned after %v, want soon after the 300ms deadline", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DialUpstreamVia hung past its context deadline")
	}
}

// The retry loop is for a guest whose sshd has not finished booting. A dialer
// that rejected the channel has already given its final answer, and an
// unreachable machine should not cost the caller the whole dial budget before
// it says so.
func TestDialUpstreamViaDoesNotRetryARejectedChannel(t *testing.T) {
	var calls atomic.Int64
	refuse := &xssh.OpenChannelError{Reason: xssh.Prohibited, Message: "unknown sandbox"}
	dial := func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, refuse
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := DialUpstreamVia(ctx, dial, "box.node.sandbox.invalid:ssh", "sparky", testSigner(t))
	if err == nil {
		t.Fatal("dial succeeded against a refusing dialer")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v to report a refusal, want immediate", elapsed)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("dialer called %d times, want 1", n)
	}
	var oce *xssh.OpenChannelError
	if !errors.As(err, &oce) {
		t.Fatalf("error %v does not unwrap to *ssh.OpenChannelError, so callers cannot tell a refusal from a dead guest", err)
	}
	if oce.Reason != xssh.Prohibited || oce.Message != "unknown sandbox" {
		t.Errorf("refusal arrived as %v/%q, want Prohibited/%q", oce.Reason, oce.Message, "unknown sandbox")
	}
}

// A nil dialer is the single-box deployment: it must still dial the host
// network, and a connection refused there must still be retried until the
// caller's budget runs out.
func TestDialUpstreamViaNilDialerUsesHostNetwork(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing is listening there now

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := DialUpstreamVia(ctx, nil, addr, "sparky", testSigner(t)); err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	if ctx.Err() == nil {
		t.Error("returned before the context expired, so the boot retry loop was skipped")
	}
}

// The retry loop is built around a short connect budget per attempt: a guest
// whose sshd is still booting has to be re-probed several times inside the
// caller's 15s dial budget. That budget used to live on the nil-dialer path
// only, so once every deployment started supplying the fleet dialer — which
// falls through to the host network with a 10s timeout — a single box went
// from about five attempts to one, and every unreachable guest cost the user
// ten seconds instead of three.
func TestDialUpstreamViaPerAttemptConnectBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		want time.Duration
	}{
		{"host network", "127.0.0.1:22", localDialTimeout},
		// fleet.Host is the address form the router mints for a sandbox held by
		// another machine; asking fleet for it keeps the two definitions of
		// "somewhere else" from drifting apart.
		{"through a node", net.JoinHostPort(fleet.Host("fuzzy-otter", "node-b"), "22"), fleetDialTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			var calls int
			var budget time.Duration
			dial := func(dctx context.Context, _, _ string) (net.Conn, error) {
				calls++
				if d, ok := dctx.Deadline(); ok {
					budget = time.Until(d)
				} else {
					t.Error("the dial context carries no deadline, so nothing bounds one connect attempt")
				}
				cancel() // one attempt is all this measures
				return nil, errors.New("connection refused")
			}
			if _, err := DialUpstreamVia(ctx, dial, tc.addr, "sparky", testSigner(t)); err == nil {
				t.Fatal("dial through a refusing dialer succeeded")
			}
			if calls != 1 {
				t.Fatalf("dialer called %d times, want 1", calls)
			}
			// The deadline is read inside the call, so it has already lost a
			// sliver of wall clock; anything within a second of the budget is
			// the budget.
			if budget > tc.want || budget < tc.want-time.Second {
				t.Errorf("per-attempt connect budget %v, want ~%v", budget, tc.want)
			}
		})
	}
}

// A gateway that looks sandboxes up through the fleet must not hand `ctl@` a
// control plane wired to the local manager: `ssh <name>@` would follow a
// sandbox to whichever machine holds it while every control command looked for
// it here. Nothing in production builds that combination today because
// cmd/sparkbox passes Ops explicitly, which is exactly why it needs a test.
func TestOpsConfigPrefersFleetOverManager(t *testing.T) {
	mgr := newTestManager(t)
	flt, err := fleet.New(fleet.Options{Local: mgr})
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}

	cfg := opsConfig(GatewayOptions{Manager: mgr, Fleet: flt})
	if cfg.Sandboxes != interface{}(flt) {
		t.Errorf("control plane reads %T, want the fleet", cfg.Sandboxes)
	}
	if cfg.Templates != interface{}(flt) {
		t.Errorf("snapshots read %T, want the fleet", cfg.Templates)
	}
	// The other half of this — a Fleet too narrow to back the control plane —
	// has no test because it has no runtime: GatewayOptions.Fleet is
	// sshgw.FleetSandboxes, so such a value does not compile.
}

func newTestManager(t *testing.T) *host.Manager {
	t.Helper()
	dir := t.TempDir()
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
		GatewayPublicKey: PublicKeyLine(upstreamKey),
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}
