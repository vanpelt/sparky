package ctlops

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

func TestWhoamiProjectsTheAccount(t *testing.T) {
	r := newRig(t)
	_, fp := mustKey(t, testKey)

	got, err := r.ops.Whoami(context.Background(), Caller{Handle: "alice", KeyFP: fp})
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if got.Handle != "alice" || got.Status != "active" || got.Operator {
		t.Errorf("whoami = %+v", got)
	}
	if got.Subject != oidc.SubjectFor("alice") {
		t.Errorf("subject = %q, want the OIDC subject", got.Subject)
	}
	if got.KeyFP != fp {
		t.Errorf("key_fp = %q; it must echo the key on THIS session", got.KeyFP)
	}
	// An HTTP caller has no session key, and the field must simply be empty
	// rather than borrowing one from the account.
	if http, _ := r.ops.Whoami(context.Background(), alice()); http.KeyFP != "" {
		t.Errorf("key_fp over HTTP = %q, want empty", http.KeyFP)
	}
	if op, _ := r.ops.Whoami(context.Background(), Caller{Handle: "opsy"}); !op.Operator {
		t.Error("operator flag must come from the account store")
	}
}

func TestKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	_, fp := mustKey(t, testKey)

	keys, err := r.ops.ListKeys(ctx, Caller{Handle: "alice", KeyFP: fp})
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys = %v, %v", keys, err)
	}
	if !keys[0].Current {
		t.Error("the key authenticating this session must be marked current")
	}

	const second = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA0lTGKvNBQpJmDzsxNTLxUjbG9hpNjHZE0bZbCiTAlp desktop"
	added, err := r.ops.AddKey(ctx, alice(), second)
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if added.Label != "desktop" || added.Via != keyVia {
		t.Errorf("KeyInfo = %+v", added)
	}
	// AddKey is idempotent, so a repeat must not claim a fresh AddedAt.
	again, err := r.ops.AddKey(ctx, alice(), second)
	if err != nil || !again.AddedAt.Equal(added.AddedAt) {
		t.Errorf("re-adding a key changed AddedAt: %v vs %v (%v)", again.AddedAt, added.AddedAt, err)
	}

	if err := r.ops.RemoveKey(ctx, alice(), added.FP); err != nil {
		t.Fatalf("RemoveKey: %v", err)
	}
	// Now one key remains: removing it would lock the account out.
	err = r.ops.RemoveKey(ctx, alice(), fp)
	var e *Error
	if !errors.As(err, &e) || e.Code != "last_key" || e.HTTPStatus() != 409 {
		t.Fatalf("removing the last key = %v, want last_key/409", err)
	}
	// A fingerprint that was never on the account is the masked not-found.
	if err := r.ops.RemoveKey(ctx, alice(), "SHA256:nope"); !IsKind(err, KindNotFound) {
		t.Errorf("unknown fingerprint = %v, want KindNotFound", err)
	}
}

func TestImportGitHubKeysReportsSkips(t *testing.T) {
	r := newRig(t)
	r.accts.users["alice"] = users.User{Handle: "alice", Status: "active", GitHubLogin: "alice-gh"}
	key, fp := mustKey(t, testKey)
	r.github.keys["alice-gh"] = []xssh.PublicKey{key}
	r.accts.addErr = users.ErrKeyLinked

	got, err := r.ops.ImportGitHubKeys(context.Background(), alice())
	if err != nil {
		t.Fatalf("ImportGitHubKeys: %v", err)
	}
	if got.Listed != 1 || got.Imported != 0 {
		t.Errorf("result = %+v, want 1 listed / 0 imported", got)
	}
	if !slices.Equal(got.Skipped, []string{fp}) {
		t.Errorf("skipped = %v, want the fingerprint that belongs elsewhere", got.Skipped)
	}
	if got.Skipped == nil {
		t.Error("Skipped must never be nil — the REST edge encodes it as []")
	}
}

func TestRemovePasskeyDisambiguates(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	now := time.Unix(0, 0).UTC()
	r.accts.passkeys["alice"] = []users.Passkey{
		{ID: "abc111", Handle: "alice", CreatedAt: now},
		{ID: "abc222", Handle: "alice", CreatedAt: now},
	}

	pks, err := r.ops.ListPasskeys(ctx, alice())
	if err != nil || len(pks) != 2 {
		t.Fatalf("ListPasskeys = %v, %v", pks, err)
	}

	err = r.ops.RemovePasskey(ctx, alice(), "abc")
	var e *Error
	if !errors.As(err, &e) || e.Code != "passkey_ambiguous" || e.HTTPStatus() != 409 {
		t.Fatalf("ambiguous prefix = %v, want passkey_ambiguous/409", err)
	}
	matches, _ := e.Details["matches"].([]string)
	if len(matches) != 2 {
		t.Errorf("details.matches = %v; a client needs the candidates to disambiguate", e.Details)
	}
	if err := r.ops.RemovePasskey(ctx, alice(), "abc111"); err != nil {
		t.Errorf("unambiguous prefix: %v", err)
	}
	if err := r.ops.RemovePasskey(ctx, alice(), "zzz"); !IsKind(err, KindNotFound) {
		t.Errorf("unknown prefix = %v, want KindNotFound", err)
	}
}

func TestEmailReadWriteClear(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	if got, err := r.ops.Email(ctx, alice()); err != nil || got != "" {
		t.Fatalf("Email = %q, %v; want empty", got, err)
	}
	if got, err := r.ops.SetEmail(ctx, alice(), " alice@example.com "); err != nil || got != "alice@example.com" {
		t.Fatalf("SetEmail = %q, %v", got, err)
	}
	if got, _ := r.ops.Email(ctx, alice()); got != "alice@example.com" {
		t.Errorf("Email = %q after set", got)
	}
	if got, err := r.ops.SetEmail(ctx, alice(), ""); err != nil || got != "" {
		t.Fatalf("clear = %q, %v", got, err)
	}
}

func TestVisibilityFlipsEveryRouteTogether(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.routes.rs["alicebox"] = append(r.routes.rs["alicebox"], routes.Route{
		Subdomain: "api.alicebox", Sandbox: "alicebox", Owner: "alice",
		Port: 9000, Visibility: routes.VisibilityPrivate,
	})

	before, err := r.ops.Visibility(ctx, alice(), "alicebox")
	if err != nil || len(before) != 2 {
		t.Fatalf("Visibility = %v, %v", before, err)
	}
	if before[0].URL != "https://alicebox.example.test" {
		t.Errorf("route URL = %q", before[0].URL)
	}

	got, err := r.ops.SetVisibility(ctx, alice(), "alicebox", routes.VisibilityPublic)
	if err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	if got.Changed != 2 {
		t.Errorf("changed = %d, want both routes of the sandbox", got.Changed)
	}
	for _, ri := range got.Routes {
		if ri.Visibility != routes.VisibilityPublic {
			t.Errorf("route %s stayed %s", ri.Subdomain, ri.Visibility)
		}
	}
	if got.Routes == nil {
		t.Error("Routes must never be nil")
	}
}

func TestScheduleInfoComputesNextRun(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	got, err := r.ops.ListSchedules(ctx, alice())
	if err != nil || len(got) != 1 {
		t.Fatalf("ListSchedules = %v, %v", got, err)
	}
	if got[0].NextRun == nil || !got[0].NextRun.After(r.ops.now()) {
		t.Errorf("next_run = %v, want a time after the injected now", got[0].NextRun)
	}
	if got[0].LastRun != nil {
		t.Error("a never-run schedule must report a nil LastRun, not the zero time")
	}

	added, err := r.ops.AddSchedule(ctx, alice(), ScheduleArgs{
		Sandbox: "alicebox", Spec: "@hourly", Command: "make test",
	})
	if err != nil {
		t.Fatalf("AddSchedule: %v", err)
	}
	if added.ID == "" || added.NextRun == nil {
		t.Errorf("added = %+v", added)
	}

	// A spec the cron library can no longer parse leaves NextRun nil rather than
	// reporting the zero time as a real date.
	r.sched.entries["broken"] = r.sched.entries["sch-alice"]
	broken := r.sched.entries["broken"]
	broken.ID, broken.Spec = "broken", "nonsense"
	r.sched.entries["broken"] = broken
	list, _ := r.ops.ListSchedules(ctx, alice())
	for _, s := range list {
		if s.ID == "broken" && s.NextRun != nil {
			t.Errorf("unparseable spec reported next_run = %v", s.NextRun)
		}
	}
}

func TestSnapshotAndListProjection(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	snaps, err := r.ops.ListSnapshots(ctx, alice())
	if err != nil || len(snaps) != 1 {
		t.Fatalf("ListSnapshots = %v, %v", snaps, err)
	}
	if snaps[0].FromBox != "alicebox" {
		t.Errorf("snapshot = %+v", snaps[0])
	}
	got, err := r.ops.CreateSnapshot(ctx, alice(), "alicebox", "fresh")
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if got.Name != "fresh" || got.Owner != "alice" || got.FromBox != "alicebox" {
		t.Errorf("snapshot = %+v", got)
	}
}

// TestWithBudgetRespectsATighterCaller: a 30-second HTTP handler deadline must
// win over a 15-minute archive budget, or the handler leaks work it can no
// longer answer for.
func TestWithBudgetRespectsATighterCaller(t *testing.T) {
	tight, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got, done := withBudget(tight, ArchiveTimeout)
	defer done()
	dl, ok := got.Deadline()
	if !ok || time.Until(dl) > time.Second {
		t.Fatalf("withBudget widened a tighter caller deadline to %v", time.Until(dl))
	}

	loose, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	got2, done2 := withBudget(loose, time.Second)
	defer done2()
	dl2, ok := got2.Deadline()
	if !ok || time.Until(dl2) > 2*time.Second {
		t.Fatalf("withBudget failed to apply its own budget: %v", time.Until(dl2))
	}
}
