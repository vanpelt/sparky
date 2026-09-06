package ctlops

import (
	"context"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// runnerBoxes is a single-machine sandbox store that knows which VMM it runs —
// what *host.Manager is to a gateway with no fleet. It exists to exercise the
// localRunner branch of build(), which is the only refusal a deployment with no
// placer can make.
type runnerBoxes struct {
	*fakeSandboxes
	runner vmm.Runner
}

func (r runnerBoxes) Runner() vmm.Runner { return r.runner }

func withLocalRunner(r *rig, runner vmm.Runner) {
	r.ops.boxes = runnerBoxes{fakeSandboxes: r.boxes, runner: runner}
}

func TestRunnerAgrees(t *testing.T) {
	for _, tc := range []struct {
		name       string
		want, have vmm.Runner
		ok         bool
	}{
		{"agreement", vmm.RunnerQEMU, vmm.RunnerQEMU, true},
		{"disagreement", vmm.RunnerQEMU, vmm.RunnerFirecracker, false},
		{"no requirement", vmm.RunnerAny, vmm.RunnerFirecracker, true},
		// A host that was never told which VMM it runs is every mock and every
		// test double. Unknown is not a refusal, here as everywhere else.
		{"a host that did not say", vmm.RunnerQEMU, vmm.RunnerAny, true},
		{"a mock host", vmm.RunnerQEMU, vmm.RunnerMock, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runnerAgrees("create", tc.want, tc.have)
			if (err == nil) != tc.ok {
				t.Fatalf("runnerAgrees(%q, %q) = %v", tc.want, tc.have, err)
			}
		})
	}
}

// The single-machine refusal, and the property that matters most about it: a
// create that cannot possibly succeed leaves no sandbox behind.
func TestCreateRefusesAnEnvironmentTheHostCannotRun(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateReady)
	mustRunner(t, e.SetRunner("alice", "web", vmm.RunnerQEMU))
	withLocalRunner(r, vmm.RunnerFirecracker)

	_, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "box", Env: "web"})
	if err == nil {
		t.Fatal("a qemu environment was built on a firecracker host")
	}
	ce := AsError("create", err)
	if ce.Code != "no_node_runs_runner" {
		t.Errorf("code = %q, want no_node_runs_runner", ce.Code)
	}
	// Both halves of the disagreement, because the user chose neither of them
	// in this command and has no other way to see which is which.
	for _, want := range []string{"firecracker", "qemu"} {
		if !strings.Contains(ce.Msg, want) {
			t.Errorf("refusal %q does not mention %q", ce.Msg, want)
		}
	}
	if _, ok := r.boxes.boxes["box"]; ok {
		t.Error("a refused create left a sandbox behind")
	}
}

func TestCreateAllowsAnEnvironmentTheHostCanRun(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateReady)
	mustRunner(t, e.SetRunner("alice", "web", vmm.RunnerQEMU))
	withLocalRunner(r, vmm.RunnerQEMU)

	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "box", Env: "web"}); err != nil {
		t.Fatalf("a qemu environment on a qemu host: %v", err)
	}
}

// Two environments on one sandbox that disagree about the VMM. This mirrors
// resolveTemplate's rule on the same tag list, so the assertion that matters is
// the other one below: agreement is ordinary and must not be refused.
func TestCreateRefusesTwoEnvironmentsThatDisagreeAboutTheRunner(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateReady)
	seedEnv(t, e, "alice", "ci", envs.StateReady)
	mustRunner(t, e.SetRunner("alice", "web", vmm.RunnerQEMU))
	mustRunner(t, e.SetRunner("alice", "ci", vmm.RunnerFirecracker))

	_, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Env: "web", Tags: []string{"ci"},
	})
	if err == nil {
		t.Fatal("two environments requiring different VMMs produced a sandbox")
	}
	ce := AsError("create", err)
	if ce.Code != "ambiguous_runner" {
		t.Fatalf("code = %q, want ambiguous_runner", ce.Code)
	}
	// Before the first write, like every other refusal in Create: no sandbox
	// and no tag rows for something that never existed.
	if _, ok := r.boxes.boxes["box"]; ok {
		t.Error("a refused create left a sandbox behind")
	}
	if tags := r.tagger.tags["box"]; len(tags) > 0 {
		t.Errorf("a refused create left tag rows %v", tags)
	}
}

func TestTwoEnvironmentsAgreeingAboutTheRunnerIsOrdinary(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateReady)
	seedEnv(t, e, "alice", "ci", envs.StateReady)
	mustRunner(t, e.SetRunner("alice", "web", vmm.RunnerQEMU))
	mustRunner(t, e.SetRunner("alice", "ci", vmm.RunnerQEMU))
	withLocalRunner(r, vmm.RunnerQEMU)

	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Env: "web", Tags: []string{"ci"},
	}); err != nil {
		t.Fatalf("two environments agreeing on qemu: %v", err)
	}
}

// An environment requiring nothing is the overwhelming majority, and it must
// stay on exactly the path it was on: no lookup that can fail, no refusal.
func TestAnEnvironmentRequiringNothingIsPlacedAnywhere(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateReady)
	withLocalRunner(r, vmm.RunnerFirecracker)

	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "box", Env: "web"}); err != nil {
		t.Fatalf("an unconstrained environment was refused: %v", err)
	}
	if got, _ := e.Get("alice", "web"); got.Runner != vmm.RunnerAny {
		t.Fatalf("the seeded environment requires %q", got.Runner)
	}
}

// PutEnvironment's pointer discipline, which is the whole reason EnvArgs.Runner
// is a pointer: an edit to anything else must not silently drop the
// requirement. If it did, the environment would keep working — on the wrong
// VMM, which is the failure this field exists to prevent.
func TestEditingSomethingElseLeavesTheRunnerAlone(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	qemu := "qemu"
	if _, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name: "web", Runner: &qemu,
	}); err != nil {
		t.Fatalf("create with a runner: %v", err)
	}
	desc := "the web environment"
	got, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name: "web", Description: &desc,
	})
	if err != nil {
		t.Fatalf("edit the description: %v", err)
	}
	if got.Runner != "qemu" {
		t.Fatalf("runner = %q after an unrelated edit, want qemu", got.Runner)
	}

	// And sending it empty is how somebody deliberately goes back to "anywhere".
	clear := ""
	got, err = r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name: "web", Runner: &clear,
	})
	if err != nil {
		t.Fatalf("clear the runner: %v", err)
	}
	if got.Runner != "" {
		t.Fatalf("runner = %q after being cleared", got.Runner)
	}
}

func TestPutEnvironmentRefusesARunnerNobodyRuns(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	for _, bad := range []string{"mock", "cloud-hypervisor", "QEMU"} {
		_, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
			Name: "web", Runner: &bad,
		})
		if err == nil {
			t.Errorf("--runner %q was accepted", bad)
			continue
		}
		if code := AsError("create", err).Code; code != "bad_runner" {
			t.Errorf("--runner %q gave code %q, want bad_runner", bad, code)
		}
	}
}

func mustRunner(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("SetRunner: %v", err)
	}
}
