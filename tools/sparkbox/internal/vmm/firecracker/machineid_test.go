//go:build linux

package firecracker

import (
	"encoding/hex"
	"strings"
	"testing"
)

// The machine id a fork boots with has to differ from its parent's and stay the
// same across the sandbox's own boots. Both halves matter and they pull in
// opposite directions, which is why this is derived from the name rather than
// random (would churn on every resume) or constant (every fork a twin).
//
// A template is a byte-for-byte copy of somebody's rootfs, /etc/machine-id
// included, and PID 1 reads that file before any unit can rewrite it — so the
// cmdline is the only place this can come from and still be right on a fork's
// FIRST boot.
func TestMachineIDForIsPerSandboxAndStable(t *testing.T) {
	parent := machineIDFor("brave-otter")
	if got := machineIDFor("brave-otter"); got != parent {
		t.Errorf("machine id changed between boots of one sandbox: %q then %q", parent, got)
	}
	fork := machineIDFor("brave-otter-2")
	if fork == parent {
		t.Errorf("a fork got its parent's machine id (%q); journald, dbus and systemd all key on it", parent)
	}

	// systemd accepts exactly 32 lowercase hex characters and rejects an
	// all-zero id. A malformed value is not a loud failure: systemd ignores the
	// argument and the guest silently keeps whatever the template had.
	for name, id := range map[string]string{"parent": parent, "fork": fork} {
		if len(id) != 32 {
			t.Errorf("%s machine id %q is %d characters, want 32", name, id, len(id))
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Errorf("%s machine id %q is not hex: %v", name, id, err)
		}
		if strings.ToLower(id) != id {
			t.Errorf("%s machine id %q is not lowercase", name, id)
		}
		if strings.Trim(id, "0") == "" {
			t.Errorf("%s machine id is all zeroes, which systemd rejects", name)
		}
	}
}
