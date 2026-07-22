package ctlops

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// ownCase drives one owner-scoped method against a resource of the given kind.
// target is substituted so the same closure can be run twice: once against a
// resource that exists but belongs to somebody else, once against one that does
// not exist at all. The whole point of the test is that those two runs are
// indistinguishable.
type ownCase struct {
	method string // the *Ops method name, for the completeness check below
	kind   string // "sandbox" | "snapshot" | "schedule" | "job"
	run    func(r *rig, c Caller, target string) error
}

func ownCases() []ownCase {
	ctx := context.Background()
	return []ownCase{
		{"Get", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Get(ctx, c, t)
			return err
		}},
		{"Pause", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Pause(ctx, c, t)
			return err
		}},
		{"Resume", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Resume(ctx, c, t)
			return err
		}},
		{"Archive", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Archive(ctx, c, t)
			return err
		}},
		{"Resize", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Resize(ctx, c, t, 25*1024)
			return err
		}},
		{"Reboot", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Reboot(ctx, c, t)
			return err
		}},
		{"Rename", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Rename(ctx, c, t, "stolen")
			return err
		}},
		{"Destroy", "sandbox", func(r *rig, c Caller, t string) error {
			return r.ops.Destroy(ctx, c, t)
		}},
		{"SetPinned", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.SetPinned(ctx, c, t, true)
			return err
		}},
		{"Tags", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Tags(ctx, c, t)
			return err
		}},
		{"SetTags", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.SetTags(ctx, c, t, []string{"pwned"})
			return err
		}},
		{"Attach", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Attach(ctx, c, t)
			return err
		}},
		{"CreateSnapshot", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.CreateSnapshot(ctx, c, t, "stolen-snap")
			return err
		}},
		{"AddSchedule", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.AddSchedule(ctx, c, ScheduleArgs{
				Sandbox: t, Spec: "*/5 * * * *", Command: "curl evil",
			})
			return err
		}},
		{"Visibility", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Visibility(ctx, c, t)
			return err
		}},
		{"SetVisibility", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.SetVisibility(ctx, c, t, "public")
			return err
		}},
		{"DeleteSnapshot", "snapshot", func(r *rig, c Caller, t string) error {
			return r.ops.DeleteSnapshot(ctx, c, t)
		}},
		{"Fork", "snapshot", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Fork(ctx, c, ForkArgs{Snapshot: t, Name: "stolen-fork"})
			return err
		}},
		{"DeleteSchedule", "schedule", func(r *rig, c Caller, t string) error {
			return r.ops.DeleteSchedule(ctx, c, t)
		}},
		{"Job", "job", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Job(c, t)
			return err
		}},
		{"CancelJob", "job", func(r *rig, c Caller, t string) error {
			_, err := r.ops.CancelJob(c, t)
			return err
		}},
	}
}

// existing names alice's resource of each kind; absent is a name nobody owns.
var ownTargets = map[string][2]string{
	"sandbox":  {"alicebox", "ghostbox"},
	"snapshot": {"alicesnap", "ghostsnap"},
	"schedule": {"sch-alice", "sch-ghost"},
	"job":      {"", "jobghost"}, // the existing job id is minted per-run
}

// TestCrossOwnerIsIndistinguishable is the security invariant of the package:
// somebody else's resource and a nonexistent one produce the byte-identical
// error, the same exit code and the same HTTP status, and neither reaches a
// mutating store method — so a probe can neither confirm a name nor wake a VM.
func TestCrossOwnerIsIndistinguishable(t *testing.T) {
	for _, tc := range ownCases() {
		t.Run(tc.method, func(t *testing.T) {
			targets := ownTargets[tc.kind]
			mine, absent := targets[0], targets[1]

			r := newRig(t)
			if tc.kind == "job" {
				j := r.ops.Go(alice(), "archive", Ref{Type: "sandbox", Name: "alicebox"},
					PauseTimeout, func(ctx context.Context) (any, error) { return nil, nil })
				r.ops.Await(context.Background(), j, time.Second)
				mine = j.ID
			}

			r.calls.reset()
			foreignErr := tc.run(r, mallory(), mine)
			foreignCalls := r.calls.mutating()

			r.calls.reset()
			absentErr := tc.run(r, mallory(), absent)
			absentCalls := r.calls.mutating()

			var fe, ae *Error
			if !errors.As(foreignErr, &fe) {
				t.Fatalf("foreign resource: want *ctlops.Error, got %v", foreignErr)
			}
			if !errors.As(absentErr, &ae) {
				t.Fatalf("absent resource: want *ctlops.Error, got %v", absentErr)
			}
			if fe.Kind != KindNotFound {
				t.Errorf("foreign resource: kind = %v, want KindNotFound", fe.Kind)
			}
			// The only thing that may differ between the two answers is the name
			// the caller themselves typed. Substituting it must make the two
			// messages identical — anything else is a distinguishing signal.
			if want := strings.Replace(fe.Msg, mine, absent, 1); want != ae.Msg {
				t.Errorf("messages differ beyond the echoed name — existence leaked:\n foreign: %q\n absent:  %q", fe.Msg, ae.Msg)
			}
			if fe.Code != ae.Code || fe.Kind != ae.Kind {
				t.Errorf("classification differs: foreign %v/%s, absent %v/%s", fe.Kind, fe.Code, ae.Kind, ae.Code)
			}
			if fe.ExitCode() != 1 || fe.HTTPStatus() != 404 {
				t.Errorf("exit/status = %d/%d, want 1/404", fe.ExitCode(), fe.HTTPStatus())
			}
			if !fe.Verbatim {
				t.Error("masked not-found must be Verbatim so ctl prints it unwrapped")
			}
			if len(foreignCalls) != 0 {
				t.Errorf("cross-owner request reached mutating store calls: %v", foreignCalls)
			}
			if len(absentCalls) != 0 {
				t.Errorf("absent-resource request reached mutating store calls: %v", absentCalls)
			}
		})
	}
}

// TestOwnerSeesOwnResources is the other half: the same table must SUCCEED for
// the owner, or a masking bug could pass by rejecting everything.
func TestOwnerSeesOwnResources(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	if _, err := r.ops.Get(ctx, alice(), "alicebox"); err != nil {
		t.Fatalf("Get own sandbox: %v", err)
	}
	if _, err := r.ops.Tags(ctx, alice(), "alicebox"); err != nil {
		t.Fatalf("Tags own sandbox: %v", err)
	}
	if err := r.ops.DeleteSchedule(ctx, alice(), "sch-alice"); err != nil {
		t.Fatalf("DeleteSchedule own schedule: %v", err)
	}
	if err := r.ops.DeleteSnapshot(ctx, alice(), "alicesnap"); err != nil {
		t.Fatalf("DeleteSnapshot own snapshot: %v", err)
	}
}

// TestListingsAreOwnerScoped checks the collection endpoints, which mask by
// filtering rather than by erroring.
func TestListingsAreOwnerScoped(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	boxes, err := r.ops.List(ctx, mallory())
	if err != nil || len(boxes) != 0 {
		t.Errorf("List(mallory) = %v, %v; want empty", boxes, err)
	}
	snaps, err := r.ops.ListSnapshots(ctx, mallory())
	if err != nil || len(snaps) != 0 {
		t.Errorf("ListSnapshots(mallory) = %v, %v; want empty", snaps, err)
	}
	scheds, err := r.ops.ListSchedules(ctx, mallory())
	if err != nil || len(scheds) != 0 {
		t.Errorf("ListSchedules(mallory) = %v, %v; want empty", scheds, err)
	}
	// And the owner sees exactly one of each, so "empty" isn't vacuous.
	if boxes, _ := r.ops.List(ctx, alice()); len(boxes) != 1 {
		t.Errorf("List(alice) = %d boxes, want 1", len(boxes))
	}
}

// TestEveryMethodIsClassified is the completeness guard: every exported method
// on *Ops is either owner-scoped and covered by the table above, or listed here
// as deliberately not resource-scoped. A new method that is neither fails this
// test rather than shipping with no ownership coverage at all.
func TestEveryMethodIsClassified(t *testing.T) {
	// Methods that take no resource name: they act on the caller's own account,
	// on a collection they filter by owner, or on the process.
	notResourceScoped := map[string]bool{
		"Capabilities": true, "Close": true, "GenerateName": true, "Touch": true,
		"Create": true, "List": true, "ListSnapshots": true, "ListSchedules": true,
		"ListKeys": true, "ListPasskeys": true, "ListJobs": true,
		"Whoami": true, "Email": true, "SetEmail": true, "AddKey": true,
		"RemoveKey": true, "ImportGitHubKeys": true, "VerifyGitHub": true,
		"RemovePasskey": true, "MintSessionToken": true, "Invite": true,
		"Go": true, "Await": true,
	}
	covered := map[string]bool{}
	for _, tc := range ownCases() {
		covered[tc.method] = true
	}

	var missing []string
	rt := reflect.TypeOf((*Ops)(nil))
	for i := 0; i < rt.NumMethod(); i++ {
		name := rt.Method(i).Name
		if covered[name] || notResourceScoped[name] {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("methods with no ownership classification: %v\n"+
			"add them to ownCases() if they take a resource name, or to notResourceScoped if they don't",
			missing)
	}
}
