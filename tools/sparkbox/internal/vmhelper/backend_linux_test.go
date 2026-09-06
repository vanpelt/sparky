//go:build linux

package vmhelper

import "testing"

// A helper serves a backend if and only if it was given a binary for it. That
// is the whole privilege argument for letting a request name one: an operator
// who does not want QEMU started passes no --qemu-bin, and then no request can
// conjure it.
func TestAHelperOnlyServesTheBackendsItWasGivenBinariesFor(t *testing.T) {
	both := qemuTestServer(t)
	both.opts.FirecrackerBin = "/srv/assets/firecracker"
	qemuOnly := qemuTestServer(t)
	fcOnly := &server{opts: ServerOptions{
		Backend: BackendFirecracker, FirecrackerBin: "/srv/assets/firecracker",
	}}

	for _, tc := range []struct {
		name    string
		s       *server
		backend Backend
		want    bool
	}{
		{"both serves qemu", both, BackendQEMU, true},
		{"both serves firecracker", both, BackendFirecracker, true},
		{"qemu-only refuses firecracker", qemuOnly, BackendFirecracker, false},
		{"qemu-only serves qemu", qemuOnly, BackendQEMU, true},
		{"firecracker-only refuses qemu", fcOnly, BackendQEMU, false},
		{"firecracker-only serves firecracker", fcOnly, BackendFirecracker, true},
		{"nobody serves a backend that does not exist", both, Backend("cloud-hypervisor"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.serves(tc.backend); got != tc.want {
				t.Errorf("serves(%q) = %v, want %v", tc.backend, got, tc.want)
			}
		})
	}
}

// Two drivers allocating from their own view of the world both pick slot 0.
// They must collide in the helper, which is the only process that sees both.
func TestASlotIsOwnedAcrossBackendsNotWithinOne(t *testing.T) {
	s := qemuTestServer(t)
	s.active[3] = activeVM{name: "alice-box", pid: 1234, backend: BackendQEMU}

	// The reading is taken from a pid this map owns, so a firecracker driver
	// asking about slot 3 must not be handed the QEMU VM's answer even when it
	// happens to use the same sandbox name.
	if _, err := s.cpuTime(Request{Name: "alice-box", Slot: 3}, BackendFirecracker); err == nil {
		t.Error("cpu-time crossed backends on a shared slot")
	}
	if err := s.prepareSnapshotOutputs(Request{Name: "alice-box", Slot: 3}, BackendFirecracker); err == nil {
		t.Error("snapshot-outputs crossed backends on a shared slot")
	}
}

// The selection is the SOCKET, and these are the rules that keep it honest: a
// second path may only serve a backend the operator installed, and it may not
// be the same path as the first.
func TestASecondSocketOnlyServesAnInstalledBackend(t *testing.T) {
	both := qemuTestServer(t)
	both.opts.FirecrackerBin = "/srv/assets/firecracker"
	both.opts.SocketPath = "/run/sparkbox-vmm/qemu.sock"
	both.opts.SecondSocketPath = "/run/sparkbox-vmm/firecracker.sock"

	got, err := both.sockets()
	if err != nil {
		t.Fatalf("sockets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("sockets = %v, want two", got)
	}
	if got[0].backend != BackendQEMU || got[1].backend != BackendFirecracker {
		t.Errorf("sockets = %v, want the default first and the other second", got)
	}

	// A helper with only QEMU installed cannot be given a firecracker door.
	bare := qemuTestServer(t)
	bare.opts.SocketPath = "/run/a.sock"
	bare.opts.SecondSocketPath = "/run/b.sock"
	if _, err := bare.sockets(); err == nil {
		t.Error("a second socket was accepted for a backend with no binary")
	}

	same := both
	same.opts.SecondSocketPath = same.opts.SocketPath
	if _, err := same.sockets(); err == nil {
		t.Error("two backends were accepted on one path")
	}
}

// A single-backend helper is untouched: one socket, the default backend, which
// is every deployment that exists today.
func TestOneBackendStillMeansOneSocket(t *testing.T) {
	s := qemuTestServer(t)
	s.opts.SocketPath = "/run/sparkbox-vmm/helper.sock"
	got, err := s.sockets()
	if err != nil {
		t.Fatalf("sockets: %v", err)
	}
	if len(got) != 1 || got[0].path != "/run/sparkbox-vmm/helper.sock" || got[0].backend != BackendQEMU {
		t.Fatalf("sockets = %v, want the one default socket", got)
	}
}
