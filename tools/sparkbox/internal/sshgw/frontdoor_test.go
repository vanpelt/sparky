package sshgw

// Unit tests for destination-IP (front-door) routing: the gateway must map a
// connection's dialed address back to a sandbox, honor the reserved new/ctl
// doors, and fall back to username routing for out-of-range addresses.

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/frontdoor"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

func newDoorGateway(t *testing.T) (*Gateway, *host.Manager, *frontdoor.Mapper) {
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
	doors, err := frontdoor.New("2001:db8:702:1c7::/64")
	if err != nil {
		t.Fatal(err)
	}
	gw := New(GatewayOptions{
		Manager: mgr, HostKey: hostKey, UpstreamKey: upstreamKey,
		Logger: log, Doors: doors, Domain: "hivemind.tools",
	})
	return gw, mgr, doors
}

func tcp(ip net.IP) net.Addr { return &net.TCPAddr{IP: ip, Port: 2222} }

func TestResolveDoor(t *testing.T) {
	gw, mgr, doors := newDoorGateway(t)
	if _, err := mgr.Create(context.Background(), "fuzzy-otter", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}

	// Existing sandbox by its front door.
	name, ok, inRange := gw.resolveDoor(tcp(doors.Addr("fuzzy-otter")))
	if !inRange || !ok || name != "fuzzy-otter" {
		t.Fatalf("resolveDoor(sandbox door) = %q, %v, %v", name, ok, inRange)
	}

	// Reserved doors resolve even with no matching sandbox.
	for _, reserved := range []string{NewSandboxUser, ControlUser} {
		name, ok, inRange = gw.resolveDoor(tcp(doors.Addr(reserved)))
		if !inRange || !ok || name != reserved {
			t.Fatalf("resolveDoor(%s door) = %q, %v, %v", reserved, name, ok, inRange)
		}
	}

	// In range but no such sandbox: inRange without ok (caller rejects, no
	// username fallback — the client explicitly dialed a door).
	name, ok, inRange = gw.resolveDoor(tcp(doors.Addr("ghost")))
	if !inRange || ok {
		t.Fatalf("resolveDoor(unknown door) = %q, %v, %v", name, ok, inRange)
	}

	// Out of range: username routing applies.
	for _, ip := range []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("2001:db8:702:1c7::3")} {
		if _, ok, inRange := gw.resolveDoor(tcp(ip)); ok || inRange {
			t.Fatalf("resolveDoor(%s) should be out of range", ip)
		}
	}
}

func TestResolveDoorDisabled(t *testing.T) {
	gw, _, doors := newDoorGateway(t)
	gw.doors = nil
	if _, ok, inRange := gw.resolveDoor(tcp(doors.Addr("fuzzy-otter"))); ok || inRange {
		t.Fatal("resolveDoor should be inert without a mapper")
	}
}
