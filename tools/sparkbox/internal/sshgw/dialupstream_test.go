package sshgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
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

// The other half of the rule above, and the one that was missing: a node's
// ConnectionFailed is a connection REFUSED inside the guest, not a refusal to
// carry the connection. Every caller of DialUpstreamVia dials the guest's sshd
// on a VM it has just started, so that is the boot it is already waiting on and
// it has to be retried exactly like the local ECONNREFUSED it stands for.
//
// Treating it as final made a remote sandbox's first attach fail about a second
// after its create returned, and cost the same box its secret-env push — which
// fires once per transition to running, so the secrets did not arrive late,
// they never arrived at all.
func TestDialUpstreamViaRetriesAGuestStillBooting(t *testing.T) {
	var calls atomic.Int64
	// The node's own words for "nothing is listening in there yet"; see
	// internal/nodelink.serveStream.
	booting := &xssh.OpenChannelError{
		Reason:  xssh.ConnectionFailed,
		Message: "nothing accepted a connection on that port in the sandbox",
	}
	// Refuse the first few dials the way a node does while sshd comes up, then
	// give a FINAL answer, which is only there to end the loop promptly instead
	// of spending the whole budget proving a point it has already made.
	const refusals = 3
	settled := ctlops.Disabled("dial", "that machine is offline")
	dial := func(context.Context, string, string) (net.Conn, error) {
		if calls.Add(1) <= refusals {
			return nil, booting
		}
		return nil, settled
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := DialUpstreamVia(ctx, dial, "box.node.sandbox.invalid:ssh", "sparky", testSigner(t))
	if !errors.Is(err, settled) {
		t.Fatalf("dial ended with %v, want the final answer that followed the refusals", err)
	}
	if n := calls.Load(); n != refusals+1 {
		t.Fatalf("dialer called %d times, want %d: a booting guest's ConnectionFailed "+
			"was taken as a final answer and never retried", n, refusals+1)
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

// ---------------------------------------------------------------------------
// What a failed dial is allowed to say
// ---------------------------------------------------------------------------

// TestDialFailureNamesNoAddress pins the one error on this path that must not
// be printed as it came.
//
// A dial error is composed by whatever did the dialing, and what it names is an
// address: `dial tcp 172.30.5.2:22` on this machine, and the synthetic
// `<sandbox>.<node>.sandbox.invalid` the fleet dialer resolves for a sandbox
// held elsewhere. Neither is anything a user can act on, and the second is
// fleet topology. It matters beyond a terminal, too: this is the sentence
// RunInSandbox hands the scheduler, which stores it in a job's last_error
// column and renders it in `ctl schedule list` and in both consoles.
//
// A typed refusal is the deliberate exception. The router's node-offline
// sentence is curated, names no address, and is the only useful thing anyone
// can say about a machine that is not answering.
func TestDialFailureNamesNoAddress(t *testing.T) {
	offline := fleet.Unreachable("dial", "demo", "node-b")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "a machine that is not answering",
			err:  fmt.Errorf("vm ssh not reachable: %w", offline),
			want: `dial demo: sandbox "demo" lives on node "node-b", which is offline`,
		},
		{
			name: "a guest on this machine that refused",
			err:  errors.New("vm ssh not reachable: dial tcp 172.30.5.2:22: connect: connection refused"),
			want: "dial demo: " + unreachableShell,
		},
		{
			name: "a synthetic address nothing resolved",
			err:  errors.New("dial tcp: lookup demo.node-b.sandbox.invalid: no such host"),
			want: "dial demo: " + unreachableShell,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dialFailure("demo", tc.err)
			if got.Error() != tc.want {
				t.Fatalf("stored sentence = %q, want %q", got.Error(), tc.want)
			}
			for _, bad := range []string{"172.30.", "sandbox.invalid", "dial tcp"} {
				if strings.Contains(got.Error(), bad) {
					t.Errorf("the sentence carries %q: %q", bad, got.Error())
				}
			}
			// The cause stays reachable, so a log line and errors.As both still
			// have everything they had before.
			if !errors.Is(got, tc.err) {
				t.Error("the cause was dropped rather than wrapped")
			}
		})
	}
	// And the typed cause survives the wrap, which is what lets anything
	// downstream classify a node outage rather than parse a sentence.
	wrapped := dialFailure("demo", fmt.Errorf("vm ssh not reachable: %w", offline))
	if !ctlops.IsNodeUnreachable(wrapped) {
		t.Error("a node outage stopped being classifiable once it was rendered")
	}
}

// rejectingSSHServer is a real sshd that completes the handshake and then
// refuses every key, which is what a sandbox created before the gateway's
// identity changed does: its authorized_keys was baked into the rootfs at
// create time and nothing rewrites it afterwards.
//
// A real server rather than a canned error because the whole point is WHERE the
// failure lands. A rejected key does not fail the dial — it fails inside
// xssh.NewClientConn, on the far side of a successful connect, which is the one
// branch of the retry loop that used to classify nothing at all.
func rejectingSSHServer(t *testing.T) (net.Listener, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var handshakes atomic.Int64
	cfg := &xssh.ServerConfig{
		PublicKeyCallback: func(xssh.ConnMetadata, xssh.PublicKey) (*xssh.Permissions, error) {
			return nil, fmt.Errorf("unknown public key")
		},
	}
	cfg.AddHostKey(testSigner(t))
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			handshakes.Add(1)
			go func() {
				defer c.Close()
				// Errors are the expected outcome: every client here is
				// rejected. Discarded rather than reported, because a test
				// failure must come from what the CLIENT concluded.
				sc, chans, reqs, err := xssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go xssh.DiscardRequests(reqs)
				for ch := range chans {
					ch.Reject(xssh.Prohibited, "no") //nolint:errcheck
				}
				sc.Close()
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln, &handshakes
}

// A guest that refuses the gateway's key must end the loop on the first answer.
//
// This is the storm that hid its own cause on the Mac dev box. Retried every
// 250ms, each attempt paying a full key exchange, it crossed sshd's MaxStartups
// inside a few seconds — after which sshd drops connections before the banner,
// the honest "unable to authenticate" is overwritten by `handshake failed: EOF`,
// and the user is told the shell could not be reached. The guest was healthy the
// whole time and its journal held the real answer.
func TestDialUpstreamViaDoesNotRetryARefusedKey(t *testing.T) {
	ln, handshakes := rejectingSSHServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := time.Now()
	c, err := DialUpstreamVia(ctx, (&net.Dialer{}).DialContext, ln.Addr().String(), "sparky", testSigner(t))
	if c != nil {
		c.Close()
	}
	if err == nil {
		t.Fatal("dial succeeded against a server that refuses every key")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v to report a refused key, want the first answer", elapsed)
	}
	// The number is the point: at 250ms apart, spending the budget instead of
	// returning is worth dozens of key exchanges against a guest that can never
	// say yes.
	if n := handshakes.Load(); n != 1 {
		t.Errorf("sshd saw %d connections, want 1: a refused key is being retried", n)
	}
	if !AuthRejected(err) {
		t.Errorf("error %v is not classified as an auth rejection, so no door can name it", err)
	}
}

// What the user is told, at both doors. "It may still be starting" is actively
// wrong for a locked-out sandbox — the guest is healthy and waiting for a key
// nobody has any more — and it sends the reader to a log that only says the
// same thing again.
func TestDialFailureNamesAStaleGuestKey(t *testing.T) {
	// The shape x/crypto/ssh actually produces; see AuthRejected on why this is
	// matched as a string.
	refused := errors.New("vm ssh not reachable: ssh: handshake failed: " +
		"ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain")

	got := dialFailure("demo", refused).Error()
	if !strings.Contains(got, StaleGuestKey) {
		t.Fatalf("stored sentence = %q, want it to carry %q", got, StaleGuestKey)
	}
	if strings.Contains(got, unreachableShell) {
		t.Errorf("a locked-out sandbox is still being reported as one that may still be starting: %q", got)
	}
	// It has to say what to DO. This is the only dial failure where the reader
	// can fix it themselves, and a sentence that stops at the diagnosis leaves
	// them exactly where the old one did.
	for _, want := range []string{"recreate", "ctl rm"} {
		if !strings.Contains(got, want) {
			t.Errorf("the sentence never mentions %q, so it names a cause with no cure: %q", want, got)
		}
	}
}
