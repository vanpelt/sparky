package vmm

import "testing"

func TestParseRunnerAcceptsEveryDriverNameAndTheEmptyOne(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Runner
	}{
		{"", RunnerAny},
		{"mock", RunnerMock},
		{"firecracker", RunnerFirecracker},
		{"qemu", RunnerQEMU},
	} {
		got, err := ParseRunner(tc.in)
		if err != nil {
			t.Fatalf("ParseRunner(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseRunner(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseRunnerRejectsAnythingElse(t *testing.T) {
	for _, in := range []string{"QEMU", "firecraker", "cloud-hypervisor", " qemu"} {
		if _, err := ParseRunner(in); err == nil {
			t.Errorf("ParseRunner(%q) accepted an unknown runner", in)
		}
	}
}

// An environment may not require mock: it would ask the platform for a sandbox
// that runs nothing. A NODE may still be mock — that asymmetry is the whole
// reason ParseRequirement exists alongside ParseRunner.
func TestParseRequirementRefusesMockButKeepsTheRealOnes(t *testing.T) {
	if _, err := ParseRequirement("mock"); err == nil {
		t.Fatal("ParseRequirement accepted mock; an environment must not be able to require it")
	}
	for _, in := range []string{"", "firecracker", "qemu"} {
		if _, err := ParseRequirement(in); err != nil {
			t.Errorf("ParseRequirement(%q): %v", in, err)
		}
	}
}

func TestSatisfiedBy(t *testing.T) {
	for _, tc := range []struct {
		name string
		want Runner
		have Runner
		ok   bool
	}{
		{"unspecified takes any node", RunnerAny, RunnerFirecracker, true},
		{"unspecified takes a qemu node", RunnerAny, RunnerQEMU, true},
		{"unspecified takes a mock node", RunnerAny, RunnerMock, true},
		{"qemu takes a qemu node", RunnerQEMU, RunnerQEMU, true},
		{"qemu refuses a firecracker node", RunnerQEMU, RunnerFirecracker, false},
		{"firecracker refuses a qemu node", RunnerFirecracker, RunnerQEMU, false},
		{"firecracker takes a firecracker node", RunnerFirecracker, RunnerFirecracker, true},

		// The dev-path rule. Without it, setting a runner on an environment
		// makes it unplaceable under `serve --driver mock`.
		{"qemu takes a mock node", RunnerQEMU, RunnerMock, true},
		{"firecracker takes a mock node", RunnerFirecracker, RunnerMock, true},

		// A node that has not reported a runner yet — an older node in a
		// fleet mid-upgrade — satisfies only the environments that ask for
		// nothing. Placing a qemu-requiring sandbox on a node that has not
		// said what it runs is exactly the guess this field exists to stop.
		{"qemu refuses a silent node", RunnerQEMU, RunnerAny, false},
		{"unspecified takes a silent node", RunnerAny, RunnerAny, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.want.SatisfiedBy(tc.have); got != tc.ok {
				t.Errorf("Runner(%q).SatisfiedBy(%q) = %v, want %v", tc.want, tc.have, got, tc.ok)
			}
		})
	}
}
