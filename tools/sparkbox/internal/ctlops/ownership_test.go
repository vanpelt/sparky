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
		{"Checkpoint", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Checkpoint(ctx, c, t)
			return err
		}},
		{"RestoreCheckpoint", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.RestoreCheckpoint(ctx, c, t)
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
		{"Sessions", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Sessions(ctx, c, t, 0)
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
			_, _, err := r.ops.SetTags(ctx, c, t, []string{"pwned"})
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
		{"SetPortVisibility", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.SetPortVisibility(ctx, c, t, 5173, "public")
			return err
		}},
		{"ForgetPort", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.ForgetPort(ctx, c, t, 5173)
			return err
		}},
		{"DeleteSnapshot", "snapshot", func(r *rig, c Caller, t string) error {
			return r.ops.DeleteSnapshot(ctx, c, t)
		}},
		{"Fork", "snapshot", func(r *rig, c Caller, t string) error {
			_, err := r.ops.Fork(ctx, c, ForkArgs{Snapshot: t, Name: "stolen-fork"})
			return err
		}},
		// The SNAPSHOT is the resource: a bind names one, so the ownership gate
		// has to mask it exactly as fork and rm do — and must reach no store.
		{"BindTemplate", "snapshot", func(r *rig, c Caller, t string) error {
			_, err := r.ops.BindTemplate(ctx, c, t, "cuda")
			return err
		}},
		// The two guest-initiated verbs. The SANDBOX is the resource on both,
		// and they matter here more than most: they are the only methods whose
		// Caller is synthesized from a sandbox record rather than proved by a
		// key, so a name that resolved to somebody else's box would elevate a
		// guest into another account.
		{"PlanSelfSnapshot", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.PlanSelfSnapshot(ctx, c, t, "cuda", "")
			return err
		}},
		{"SnapshotToTag", "sandbox", func(r *rig, c Caller, t string) error {
			_, err := r.ops.SnapshotToTag(ctx, c, SnapshotToTagArgs{
				Sandbox: t, Name: "stolen-snap", Tag: "cuda",
			})
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
	if _, err := r.ops.Checkpoint(ctx, alice(), "alicebox"); err != nil {
		t.Fatalf("Checkpoint own sandbox: %v", err)
	}
	if _, err := r.ops.RestoreCheckpoint(ctx, alice(), "alicebox"); err != nil {
		t.Fatalf("RestoreCheckpoint own sandbox: %v", err)
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
		"Capabilities": true, "Close": true, "GenerateName": true, "MarkActive": true,
		"Create": true, "List": true, "ListSnapshots": true, "ListSchedules": true,
		"ListKeys": true, "ListPasskeys": true, "ListJobs": true,
		"Whoami": true, "Email": true, "SetEmail": true, "AddKey": true,
		"RemoveKey": true, "ImportGitHubKeys": true, "VerifyGitHub": true,
		// The device flow names no resource at all: it acts on the caller's own
		// account, and the login it links is GitHub's answer rather than
		// anything the caller could pass in.
		"StartGitHubLink": true, "FinishGitHubLink": true,
		"RemovePasskey": true, "MintSessionToken": true, "Invite": true,
		"Go": true, "GoFrom": true, "Await": true,
		// AwaitEnv, like MarkActive, takes no Caller to check against: both are
		// reached only after a transport has resolved and authorized the box,
		// and both are pass-throughs that reveal nothing a resolved name does
		// not already imply.
		"AwaitEnv": true,
		// The node commands take a machine's name, not a resource anybody owns:
		// a node belongs to the fleet, so they are operator-gated instead of
		// owner-gated — see TestNodeCommandsAreOperatorGated.
		"ListNodes": true, "ApproveNode": true, "RemoveNode": true,
		// Secrets are keyed by (owner, env_name), so the owner scoping is
		// structural in every store query rather than a name check here: the
		// caller cannot name another owner's secret because the handle is not
		// an argument. The env name they DO pass selects only among their own.
		"ListSecrets": true, "PutSecret": true, "DeleteSecret": true,
		// Repos are keyed by (owner, host, slug) for exactly the same reason,
		// and the blast radius is bigger: a query that lost its owner term
		// would put another account's private repository in this caller's
		// manifest. The slug they pass selects only among their own rows.
		"ListRepos": true, "AttachRepo": true, "DetachRepo": true, "CheckRepos": true,
		// An unbind names a tag, and a binding is keyed (owner, tag), so the
		// owner scoping is structural in the store query for the same reason
		// ListSecrets' and ListRepos' are: the handle is not an argument, and
		// the tag the caller passes selects only among their own rows.
		"UnbindTemplate": true,
		// GitHubInstallURL names nothing at all: it is this host's App, and the
		// same URL for everyone who asks.
		"GitHubInstallURL": true,
		// Account provisioning names GitHub logins and an org, none of which is
		// a resource anybody here owns. Operator-gated instead — see
		// TestProvisioningIsOperatorGated.
		"ProvisionGitHubUsers": true, "ProvisionGitHubOrg": true, "ListAccounts": true,
		// AdmitGitHubLogin names a GitHub login and takes no Caller at all: the
		// thing that was proved is a redeemed federated handoff, not somebody
		// asking. It is the only method here with nobody to check the target
		// against, which is exactly why federated.go's doc comment carries the
		// contract instead — see TestAdmitIsNotOperatorReachable for the half
		// that is testable.
		"AdmitGitHubLogin": true,
		// Environments are keyed (owner, name) for the same reason secrets and
		// repos are, and the store's every query carries the owner on both
		// sides of the tag join — so the handle is not an argument and the name
		// the caller passes selects only among their own. The masking is
		// asserted directly instead, in env_test.go, because the answer for
		// another owner's environment must be byte-identical to the answer for
		// one nobody has.
		"ListEnvironments": true, "GetEnvironment": true, "PutEnvironment": true,
		"DeleteEnvironment": true, "SetEnvVar": true, "UnsetEnvVar": true,
		// The setup-script pair names an environment and nothing else, so it is
		// scoped exactly as the six above are: envs.Store.Get and SetScript
		// both carry the owner in their WHERE clause, and a name belonging to
		// somebody else comes back as ErrNoSuchEnvironment.
		"EnvScript": true, "SetEnvScript": true,
		// The build pair names an environment and is scoped exactly as those
		// eight are, through the same owner-carrying Get. The one extra thing
		// they name — the builder sandbox — is DERIVED from the environment's
		// own name and its row, never passed in, and each one still goes
		// through o.owned before touching it. TestBuildRefusesBeforeTheFirstWrite
		// and TestCaptureEnvironmentRefusals assert the masking directly, for
		// the reason the environment verbs above do.
		"BuildEnvironment": true, "CaptureEnvironment": true,
		// The guest door takes a sandbox RECORD the host resolved from a tap,
		// not a name and not a Caller — there is nobody here to check a target
		// against. What stands in for the owner gate is the comparison of the
		// environment row's owner against that record's, which is asserted
		// directly in TestSetupForAnswersOnlyItsOwnBuilder and
		// TestSetupDoneRefusesACrossOwnerBuilder because it is a security
		// boundary rather than a query filter.
		"SetupFor": true, "SetupDone": true,
		// The reconciler acts for no person at all: it walks every owner's
		// in-flight builds through the one store query with no owner term, and
		// takes a context and nothing else. Its owner discipline is the same
		// comparison the guest door makes, asserted in
		// TestReconcileEnvironmentBuilds.
		"ReconcileEnvironmentBuilds": true,
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

// TestNodeCommandsAreOperatorGated is the node half of what
// TestInviteIsTheOnlyOperatorGate pins for invites: operator status is resolved
// from the account store inside each method, so a transport cannot assert it —
// Caller has no operator field to assert with. Listing is gated too, because a
// node name is fleet topology.
func TestNodeCommandsAreOperatorGated(t *testing.T) {
	ctx := context.Background()
	opsy := Caller{Handle: "opsy"}

	for _, tc := range []struct {
		method string
		run    func(r *rig, c Caller) error
	}{
		{"ListNodes", func(r *rig, c Caller) error { _, err := r.ops.ListNodes(ctx, c); return err }},
		{"ApproveNode", func(r *rig, c Caller) error {
			_, err := r.ops.ApproveNode(ctx, c, fpNewcomer)
			return err
		}},
		{"RemoveNode", func(r *rig, c Caller) error { return r.ops.RemoveNode(ctx, c, "newcomer") }},
	} {
		t.Run(tc.method, func(t *testing.T) {
			r := newRig(t)
			r.withNodes()

			err := tc.run(r, alice())
			if !IsKind(err, KindDenied) {
				t.Fatalf("%s as a non-operator = %v, want KindDenied", tc.method, err)
			}
			var e *Error
			errors.As(err, &e)
			if e.Code != "not_operator" || e.HTTPStatus() != 403 || e.ExitCode() != 1 {
				t.Errorf("got %s/%d/exit %d, want not_operator/403/exit 1",
					e.Code, e.HTTPStatus(), e.ExitCode())
			}
			// The refusal is a complete sentence the ctl channel prints as-is.
			if !e.Verbatim || !strings.HasSuffix(e.Msg, ".") {
				t.Errorf("refusal %q is not a curated sentence (verbatim=%v)", e.Msg, e.Verbatim)
			}
			// A refused caller must not have reached the roster at all.
			for _, c := range r.calls.mutating() {
				t.Errorf("a non-operator's %s reached the roster: %s", tc.method, c)
			}

			r.calls.reset()
			if err := tc.run(r, opsy); err != nil {
				t.Fatalf("%s as an operator: %v", tc.method, err)
			}
		})
	}
}

// TestNodeCommandsWithoutAFleet: a host with no roster answers KindDisabled to
// everyone, operator or not, and never consults the account store to decide it.
func TestNodeCommandsWithoutAFleet(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	if r.ops.Capabilities().Fleet {
		t.Fatal("a host with no roster reported itself a fleet gateway")
	}
	_, err := r.ops.ListNodes(ctx, Caller{Handle: "opsy"})
	if !IsKind(err, KindDisabled) {
		t.Fatalf("ListNodes with no roster = %v, want KindDisabled", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Msg != "this host is not a fleet gateway." || e.Code != "nodes_disabled" {
		t.Errorf("got %q/%s, want the fleet-gateway sentence and nodes_disabled", e.Msg, e.Code)
	}
	if _, err := r.ops.ApproveNode(ctx, Caller{Handle: "opsy"}, fpNodeB); !IsKind(err, KindDisabled) {
		t.Errorf("ApproveNode with no roster = %v, want KindDisabled", err)
	}
	if err := r.ops.RemoveNode(ctx, Caller{Handle: "opsy"}, "node-b"); !IsKind(err, KindDisabled) {
		t.Errorf("RemoveNode with no roster = %v, want KindDisabled", err)
	}
}

// TestRemoveNodeRefusesWhileItHoldsSandboxes: the row is a name, the sandboxes
// are on that machine's disk. Dropping the row would not delete them, it would
// only strand them under a name nothing in the fleet claims.
func TestRemoveNodeRefusesWhileItHoldsSandboxes(t *testing.T) {
	ctx := context.Background()
	opsy := Caller{Handle: "opsy"}
	r := newRig(t)
	roster := r.withNodes()

	err := r.ops.RemoveNode(ctx, opsy, "node-b")
	if !IsKind(err, KindConflict) {
		t.Fatalf("RemoveNode with placements = %v, want KindConflict", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Code != "node_has_sandboxes" || e.HTTPStatus() != 409 {
		t.Errorf("got %s/%d, want node_has_sandboxes/409", e.Code, e.HTTPStatus())
	}
	if !strings.Contains(e.Msg, "2 sandboxes") {
		t.Errorf("refusal %q does not name the count", e.Msg)
	}
	if e.Details["sandboxes"] != 2 {
		t.Errorf("details = %v, want the count a client can render", e.Details)
	}
	if len(roster.list) != 3 {
		t.Fatal("a refused removal still dropped the row")
	}

	// This gateway is in its own listing and cannot remove itself, whatever it
	// is holding.
	err = r.ops.RemoveNode(ctx, opsy, "here")
	if !IsKind(err, KindConflict) {
		t.Fatalf("RemoveNode of the local machine = %v, want KindConflict", err)
	}
	errors.As(err, &e)
	if e.Code != "node_is_local" {
		t.Errorf("code = %s, want node_is_local", e.Code)
	}

	// An idle machine goes.
	if err := r.ops.RemoveNode(ctx, opsy, "newcomer"); err != nil {
		t.Fatalf("RemoveNode of an idle machine: %v", err)
	}
	if len(roster.list) != 2 {
		t.Fatalf("roster still holds %d rows", len(roster.list))
	}
}

// TestNodeCommandsMaskAnUnknownName: an operator naming a machine that is not
// there gets the same `no node named %q` sentence every other missing object
// gets, from the same constructor, and the roster is never written.
func TestNodeCommandsMaskAnUnknownName(t *testing.T) {
	ctx := context.Background()
	opsy := Caller{Handle: "opsy"}
	r := newRig(t)
	r.withNodes()

	// The two commands are keyed on different things, so they mask different
	// things: `rm` names a machine an operator has already decided about, while
	// `approve` names the key it is about to trust and never a name.
	for _, tc := range []struct {
		name    string
		wantMsg string
		run     func() error
	}{
		{"approve", "no node in this fleet holds the key " + fpNobody,
			func() error { _, err := r.ops.ApproveNode(ctx, opsy, fpNobody); return err }},
		{"rm", `no node named "ghost"`,
			func() error { return r.ops.RemoveNode(ctx, opsy, "ghost") }},
	} {
		r.calls.reset()
		err := tc.run()
		if !IsKind(err, KindNotFound) {
			t.Fatalf("node %s of something absent = %v, want KindNotFound", tc.name, err)
		}
		var e *Error
		errors.As(err, &e)
		if e.Msg != tc.wantMsg || !e.Verbatim {
			t.Errorf("node %s said %q, want %q", tc.name, e.Msg, tc.wantMsg)
		}
		for _, c := range r.calls.mutating() {
			t.Errorf("node %s of an unknown machine reached the roster: %s", tc.name, c)
		}
	}

	// A missing argument is a malformed invocation, not a missing machine: exit 2.
	if err := r.ops.RemoveNode(ctx, opsy, ""); !IsKind(err, KindInvalid) {
		t.Errorf("RemoveNode with no name = %v, want KindInvalid", err)
	}
	if _, err := r.ops.ApproveNode(ctx, opsy, ""); !IsKind(err, KindInvalid) {
		t.Errorf("ApproveNode with no fingerprint = %v, want KindInvalid", err)
	}
}

// TestApproveNodeRefusesAName is the point of the whole thing: a node picks its
// own name at enrolment, so a name cannot carry an operator's approval. Whoever
// enrols `gpu-01` first holds it, and the machine the operator was told to
// expect is refused as a duplicate — so an approval keyed on the name blesses
// whichever machine got there first.
//
// The refusal must not resolve the name to the fingerprint that currently holds
// it. Printing it would turn the ceremony into a paste the operator never
// compared against the machine, which is the thing being prevented.
func TestApproveNodeRefusesAName(t *testing.T) {
	ctx := context.Background()
	opsy := Caller{Handle: "opsy"}
	r := newRig(t)
	r.withNodes()

	for _, ref := range []string{
		"newcomer",              // a real, pending row — by name
		"node-b",                // a real, approved row — by name
		fpNewcomer[:20],         // a prefix of a real fingerprint
		"SHA256:",               // the label alone
		"sha256:not-base64-!!!", // right label, wrong body
	} {
		r.calls.reset()
		_, err := r.ops.ApproveNode(ctx, opsy, ref)
		if !IsKind(err, KindInvalid) {
			t.Errorf("ApproveNode(%q) = %v, want KindInvalid", ref, err)
			continue
		}
		var e *Error
		errors.As(err, &e)
		if e.Code != "bad_fingerprint" {
			t.Errorf("ApproveNode(%q) code = %q, want bad_fingerprint", ref, e.Code)
		}
		if strings.Contains(e.Msg, fpNewcomer) || strings.Contains(e.Msg, fpNodeB) {
			t.Errorf("ApproveNode(%q) leaked the fingerprint that holds the name: %q", ref, e.Msg)
		}
		for _, c := range r.calls.mutating() {
			t.Errorf("ApproveNode(%q) reached the roster: %s", ref, c)
		}
	}
}

// TestApproveNodeAcceptsTheFingerprint covers the forms an operator actually
// types: the fingerprint as `node ls` prints it, and the body alone for someone
// who copied only the interesting half.
func TestApproveNodeAcceptsTheFingerprint(t *testing.T) {
	ctx := context.Background()
	opsy := Caller{Handle: "opsy"}

	for _, ref := range []string{
		fpNewcomer,
		strings.TrimPrefix(fpNewcomer, "SHA256:"),
		"  " + fpNewcomer + "  ",
		"sha256:" + strings.TrimPrefix(fpNewcomer, "SHA256:"),
	} {
		r := newRig(t)
		nodes := r.withNodes()
		got, err := r.ops.ApproveNode(ctx, opsy, ref)
		if err != nil {
			t.Errorf("ApproveNode(%q) = %v", ref, err)
			continue
		}
		if got.Name != "newcomer" || got.Status != "approved" {
			t.Errorf("ApproveNode(%q) = %+v, want newcomer approved", ref, got)
		}
		// The roster is always handed the canonical form, whatever was typed:
		// it is a database key, and two spellings of it would be two rows.
		if nodes.list[2].FP != fpNewcomer {
			t.Errorf("roster fingerprint = %q, want %q", nodes.list[2].FP, fpNewcomer)
		}
	}
}

// The gateway's own entry carries no fingerprint. Nothing an operator can type
// may match it — an empty-string match would make a malformed call approve the
// local machine.
func TestApproveNodeNeverMatchesTheLocalRow(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.withNodes()
	if _, err := r.ops.ApproveNode(ctx, Caller{Handle: "opsy"}, `""`); !IsKind(err, KindInvalid) {
		t.Errorf("ApproveNode of an empty-ish fingerprint = %v, want KindInvalid", err)
	}
	for _, c := range r.calls.mutating() {
		t.Errorf("it reached the roster: %s", c)
	}
}
