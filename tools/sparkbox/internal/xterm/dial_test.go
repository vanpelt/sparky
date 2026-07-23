package xterm

// The dialer seam. No guest here: the assertions are about which dialer the
// PTY path reaches for and with what, so a connection that dies on the SSH
// banner is enough.

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

func newDialHandler(t *testing.T, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *Handler {
	t.Helper()
	return New(Config{
		Sandboxes: newFakeManager(),
		Sessions:  edgeauth.NewSigner([]byte("test-oidc-ikm")),
		Domain:    testDomain,
		Log:       slog.New(slog.DiscardHandler),
		Dial:      dial,
	})
}

// A nil Dial is the single-box configuration and every unit test's, so it must
// not join New's panic-on-missing list.
func TestNewAcceptsANilDialer(t *testing.T) {
	h := newDialHandler(t, nil)
	if h.dial != nil {
		t.Fatal("nil Config.Dial did not stay nil")
	}
	if h.open == nil {
		t.Fatal("New left the PTY seam unset")
	}
}

func TestDialPTYUsesTheConfiguredDialer(t *testing.T) {
	var mu sync.Mutex
	var dialed []string
	h := newDialHandler(t, func(_ context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, network+" "+addr)
		mu.Unlock()
		// A peer that hangs up before the version exchange: the handshake
		// fails at once rather than waiting out a deadline.
		ours, theirs := net.Pipe()
		theirs.Close() //nolint:errcheck
		return ours, nil
	})

	// The retry loop in DialUpstreamVia runs until the context expires, so the
	// budget is what bounds this, not the failure.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	box := &host.Sandbox{Name: "demo", SSHAddr: "demo.node1.sandbox.invalid:ssh", SSHUser: "sparky"}
	if _, err := h.dialPTY(ctx, box, "xterm", 24, 80); err == nil {
		t.Fatal("dialPTY succeeded against a peer that never spoke SSH")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) == 0 {
		t.Fatal("dialPTY bypassed the configured dialer")
	}
	// The synthetic address is the fleet's routing key: passing anything else
	// (or resolving it here) would send the stream to the wrong machine.
	if dialed[0] != "tcp "+box.SSHAddr {
		t.Fatalf("dialer saw %q, want %q", dialed[0], "tcp "+box.SSHAddr)
	}
}
