package vmhelper

import (
	"strings"
	"testing"
)

func validMachine() Request {
	return Request{
		Version: ProtocolVersion,
		Op:      OpLaunch,
		Name:    "box",
		Slot:    3,
		VCPUs:   2,
		MemMB:   1024,
		Cmdline: "console=ttyS0 root=/dev/vda rw ip=172.30.0.6::172.30.0.5:255.255.255.252::eth0:off " +
			"sparkbox_host=172.30.0.5 sparkbox_dns=172.30.0.53 sparkbox_fresh=1",
	}
}

func TestValidateMachineAcceptsARealCmdline(t *testing.T) {
	if err := ValidateMachine(validMachine()); err != nil {
		t.Fatalf("a command line the driver actually builds was rejected: %v", err)
	}
}

func TestValidateMachineBounds(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Request)
		want string
	}{
		{"zero vcpus", func(r *Request) { r.VCPUs = 0 }, "vcpus"},
		{"negative vcpus", func(r *Request) { r.VCPUs = -1 }, "vcpus"},
		{"absurd vcpus", func(r *Request) { r.VCPUs = MaxVCPUs + 1 }, "vcpus"},
		{"zero memory", func(r *Request) { r.MemMB = 0 }, "memory"},
		{"absurd memory", func(r *Request) { r.MemMB = MaxMemMB + 1 }, "memory"},
		{"empty cmdline", func(r *Request) { r.Cmdline = "" }, "required"},
		{"oversized cmdline", func(r *Request) { r.Cmdline = strings.Repeat("a", MaxCmdlineBytes+1) }, "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validMachine()
			tc.edit(&req)
			err := ValidateMachine(req)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A zero vcpu or memory value is the specific one worth naming: QEMU turns -smp 0
// into a cryptic startup failure and -m 0 into a guest with no RAM, and MemMB is
// also the balloon's baseline, so a zero there reports every guest as fully
// ballooned. The driver refuses them at its own door too; this is the privileged
// process refusing to build an argv it has not looked at.
func TestValidateMachineRejectsControlCharacters(t *testing.T) {
	for _, bad := range []string{
		"console=ttyS0\nroot=/dev/vda",   // a newline could split a log line in two
		"console=ttyS0\x00root=/dev/vda", // execve would reject this far less legibly
		"console=ttyS0; rm -rf /",        // never reaches a shell, but nobody should have to check
		"console=ttyS0 $(whoami)",
		"console=ttyS0 `id`",
		"console=ttyS0 |tee /etc/passwd",
		"console=ttyS0\trw",
	} {
		req := validMachine()
		req.Cmdline = bad
		if err := ValidateMachine(req); err == nil {
			t.Errorf("cmdline %q was accepted", bad)
		}
	}
}

func TestParseBackend(t *testing.T) {
	// Empty must mean Firecracker: every existing deployment passes no flag and
	// must keep its behaviour exactly.
	for in, want := range map[string]Backend{
		"":            BackendFirecracker,
		"firecracker": BackendFirecracker,
		"qemu":        BackendQEMU,
	} {
		got, err := ParseBackend(in)
		if err != nil {
			t.Fatalf("ParseBackend(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseBackend(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"QEMU", "kvm", "cloud-hypervisor", "qemu-system-x86_64"} {
		if _, err := ParseBackend(bad); err == nil {
			t.Errorf("ParseBackend(%q) was accepted", bad)
		}
	}
}
