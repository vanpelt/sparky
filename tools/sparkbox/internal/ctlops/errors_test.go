package ctlops

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// TestKindContracts is the taxonomy table from the design doc, asserted rather
// than documented: each Kind's exit code and HTTP status is a contract two
// transports and an OpenAPI document all quote.
func TestKindContracts(t *testing.T) {
	cases := []struct {
		kind   Kind
		str    string
		exit   int
		status int
	}{
		{KindInternal, "internal", 1, 500},
		{KindInvalid, "invalid", 2, 400},
		{KindNotFound, "not_found", 1, 404},
		{KindDenied, "denied", 1, 403},
		{KindConflict, "conflict", 1, 409},
		{KindDisabled, "disabled", 1, 501},
		{KindLimit, "limit", 1, 429},
		{KindCapacity, "capacity", 1, 503},
		{KindQuota, "quota", 1, 507},
		{KindUpstream, "upstream", 1, 502},
	}
	for _, tc := range cases {
		t.Run(tc.str, func(t *testing.T) {
			if got := tc.kind.String(); got != tc.str {
				t.Errorf("String() = %q, want %q", got, tc.str)
			}
			e := &Error{Kind: tc.kind, Op: "x", Msg: "m"}
			if got := e.ExitCode(); got != tc.exit {
				t.Errorf("ExitCode() = %d, want %d", got, tc.exit)
			}
			if got := e.HTTPStatus(); got != tc.status {
				t.Errorf("HTTPStatus() = %d, want %d", got, tc.status)
			}
			if !IsKind(e, tc.kind) {
				t.Error("IsKind should match its own kind")
			}
		})
	}
}

// TestOverridesWin covers the two documented escape hatches: `keys add` answers
// a malformed key line with exit 1 rather than the 2 every other bad invocation
// gets, and a canceled request is a 499 rather than a 500.
func TestOverridesWin(t *testing.T) {
	e := &Error{Kind: KindInvalid, Exit: 1, Status: 400}
	if e.ExitCode() != 1 || e.HTTPStatus() != 400 {
		t.Errorf("overrides ignored: %d/%d", e.ExitCode(), e.HTTPStatus())
	}
	c := AsError("get", fmt.Errorf("wrapped: %w", context.Canceled))
	if c.HTTPStatus() != 499 || c.Kind != KindInternal {
		t.Errorf("canceled = %v/%d, want KindInternal/499", c.Kind, c.HTTPStatus())
	}
}

// TestAsErrorClassifies is the whole reason transports no longer grep error
// strings. Note *host.DiskQuotaError in particular: the SSH path renders it as a
// generic failure today, and this is where that is fixed.
func TestAsErrorClassifies(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		kind    Kind
		code    string
		details []string // keys that must be present
	}{
		{"limit", &host.LimitError{Max: 2, Running: []string{"a", "b"}},
			KindLimit, "running_limit", []string{"running", "max"}},
		{"capacity", &host.CapacityError{RequestedMB: 8192, UsedMB: 40000, BudgetMB: 45000},
			KindCapacity, "host_at_capacity", []string{"used_mb", "requested_mb", "budget_mb"}},
		{"quota", &host.DiskQuotaError{Owner: "alice", RequestedMB: 25000, UsedMB: 90000, PoolMB: 100000},
			KindQuota, "disk_pool_full", []string{"used_mb", "requested_mb", "pool_mb"}},
		{"key linked", users.ErrKeyLinked, KindConflict, "key_linked_elsewhere", nil},
		{"last key", users.ErrLastKey, KindConflict, "last_key", nil},
		{"no passkey", users.ErrNoSuchPasskey, KindNotFound, "passkey_not_found", nil},
		{"ambiguous passkey", users.ErrAmbiguousPasskey, KindConflict, "passkey_ambiguous", nil},
		{"stray", errors.New("boom"), KindInternal, "internal", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Wrapped, because that is how these reach a transport in practice.
			e := AsError("start", fmt.Errorf("start sandbox: %w", tc.err))
			if e.Kind != tc.kind {
				t.Errorf("kind = %v, want %v", e.Kind, tc.kind)
			}
			if e.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Code, tc.code)
			}
			if e.Op != "start" {
				t.Errorf("op = %q, want start", e.Op)
			}
			for _, k := range tc.details {
				if _, ok := e.Details[k]; !ok {
					t.Errorf("details missing %q: %v", k, e.Details)
				}
			}
			if !errors.Is(e, tc.err) {
				t.Error("the cause must stay unwrappable for the logs")
			}
		})
	}
}

func TestAsErrorPassesThroughItsOwnType(t *testing.T) {
	orig := NotFound("pause", "sandbox", "foo")
	got := AsError("outer", orig)
	if got != orig {
		t.Fatal("AsError re-wrapped an already-classified error")
	}
	if got.Op != "pause" {
		t.Errorf("op = %q; the inner, more specific op must win", got.Op)
	}
	if AsError("x", nil) != nil {
		t.Error("AsError(nil) must be nil so transports need no special case")
	}
}

// TestDisabledStoresAnswerKindDisabled: a nil optional store must produce the
// host-not-configured sentence, not a panic and not a 500.
func TestDisabledStoresAnswerKindDisabled(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		strip func(o *Ops)
		run   func(o *Ops) error
	}{
		{"tags read", func(o *Ops) { o.tags = nil }, func(o *Ops) error {
			_, err := o.Tags(ctx, alice(), "alicebox")
			return err
		}},
		{"tags write", func(o *Ops) { o.tags = nil }, func(o *Ops) error {
			_, err := o.SetTags(ctx, alice(), "alicebox", []string{"a"})
			return err
		}},
		{"schedule list", func(o *Ops) { o.schedules = nil }, func(o *Ops) error {
			_, err := o.ListSchedules(ctx, alice())
			return err
		}},
		{"schedule add", func(o *Ops) { o.schedules = nil }, func(o *Ops) error {
			_, err := o.AddSchedule(ctx, alice(), ScheduleArgs{Sandbox: "alicebox", Spec: "@daily", Command: "x"})
			return err
		}},
		{"schedule rm", func(o *Ops) { o.schedules = nil }, func(o *Ops) error {
			return o.DeleteSchedule(ctx, alice(), "sch-alice")
		}},
		{"share read", func(o *Ops) { o.routes = nil }, func(o *Ops) error {
			_, err := o.Visibility(ctx, alice(), "alicebox")
			return err
		}},
		{"share write", func(o *Ops) { o.routes = nil }, func(o *Ops) error {
			_, err := o.SetVisibility(ctx, alice(), "alicebox", "public")
			return err
		}},
		{"session token", func(o *Ops) { o.sessions = nil }, func(o *Ops) error {
			_, err := o.MintSessionToken(ctx, alice(), 0)
			return err
		}},
		{"archive", func(o *Ops) { o.boxes.(*fakeSandboxes).archiving = false }, func(o *Ops) error {
			_, err := o.Archive(ctx, alice(), "alicebox")
			return err
		}},
		{"checkpoint store", func(o *Ops) { o.checkpoints = nil }, func(o *Ops) error {
			_, err := o.Checkpoint(ctx, alice(), "alicebox")
			return err
		}},
		{"checkpoint target", func(o *Ops) {
			o.checkpoints.(*fakeCheckpoints).enabled["alicebox"] = false
		}, func(o *Ops) error {
			_, err := o.RestoreCheckpoint(ctx, alice(), "alicebox")
			return err
		}},
		{"snapshot create", func(o *Ops) { o.templates.(*fakeTemplates).on = false }, func(o *Ops) error {
			_, err := o.CreateSnapshot(ctx, alice(), "alicebox", "snap")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.strip(r.ops)
			err := tc.run(r.ops)
			if !IsKind(err, KindDisabled) {
				t.Fatalf("err = %v, want KindDisabled", err)
			}
			var e *Error
			errors.As(err, &e)
			if e.HTTPStatus() != 501 || e.ExitCode() != 1 {
				t.Errorf("exit/status = %d/%d, want 1/501", e.ExitCode(), e.HTTPStatus())
			}
			if !e.Verbatim || e.Msg == "" {
				t.Errorf("a disabled-feature sentence must be complete and verbatim: %+v", e)
			}
		})
	}
}

// TestArgumentValidation covers the KindInvalid branches, which are the ones a
// caller can fix — and which must exit 2 over SSH except for the one shipped
// inconsistency.
func TestArgumentValidation(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		run      func(o *Ops) error
		code     string
		wantExit int
	}{
		{"resize zero", func(o *Ops) error {
			_, err := o.Resize(ctx, alice(), "alicebox", 0)
			return err
		}, "bad_size", 2},
		{"resize absurd", func(o *Ops) error {
			_, err := o.Resize(ctx, alice(), "alicebox", MaxDiskMB+1)
			return err
		}, "bad_size", 2},
		{"rename to nothing", func(o *Ops) error {
			_, err := o.Rename(ctx, alice(), "alicebox", "")
			return err
		}, "missing_name", 2},
		{"bad cron", func(o *Ops) error {
			_, err := o.AddSchedule(ctx, alice(), ScheduleArgs{
				Sandbox: "alicebox", Spec: "not a cron", Command: "make"})
			return err
		}, "bad_cron", 2},
		{"blank command", func(o *Ops) error {
			_, err := o.AddSchedule(ctx, alice(), ScheduleArgs{
				Sandbox: "alicebox", Spec: "@daily", Command: "   "})
			return err
		}, "bad_command", 2},
		{"bad visibility", func(o *Ops) error {
			_, err := o.SetVisibility(ctx, alice(), "alicebox", "semi-public")
			return err
		}, "bad_visibility", 2},
		{"bad email", func(o *Ops) error {
			_, err := o.SetEmail(ctx, alice(), "not-an-email")
			return err
		}, "bad_email", 1}, // ctl has always answered 1 here
		{"bad key line", func(o *Ops) error {
			_, err := o.AddKey(ctx, alice(), "not a key")
			return err
		}, "bad_key", 1}, // the documented CLI inconsistency, preserved
		{"too many tags", func(o *Ops) error {
			many := make([]string, MaxTagsPerSandbox+1)
			for i := range many {
				many[i] = fmt.Sprintf("t%d", i)
			}
			_, err := o.SetTags(ctx, alice(), "alicebox", many)
			return err
		}, "bad_tag", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			err := tc.run(r.ops)
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want *ctlops.Error", err)
			}
			if e.Kind != KindInvalid {
				t.Errorf("kind = %v, want KindInvalid", e.Kind)
			}
			if e.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Code, tc.code)
			}
			if e.ExitCode() != tc.wantExit {
				t.Errorf("exit = %d, want %d", e.ExitCode(), tc.wantExit)
			}
			if e.HTTPStatus() != 400 {
				t.Errorf("status = %d, want 400 whatever the exit code is", e.HTTPStatus())
			}
		})
	}
}

// TestInviteIsTheOnlyOperatorGate: operator status is resolved from the account
// store inside Invite, never taken from the caller, so a transport cannot assert
// it.
func TestInviteIsTheOnlyOperatorGate(t *testing.T) {
	ctx := context.Background()

	r := newRig(t)
	_, err := r.ops.Invite(ctx, alice())
	if !IsKind(err, KindDenied) {
		t.Fatalf("non-operator invite = %v, want KindDenied", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Code != "not_operator" || e.HTTPStatus() != 403 {
		t.Errorf("got %s/%d, want not_operator/403", e.Code, e.HTTPStatus())
	}

	if _, err := r.ops.Invite(ctx, Caller{Handle: "opsy"}); err != nil {
		t.Fatalf("operator invite: %v", err)
	}

	// With a quota, a non-operator gets exactly that many and then a distinct
	// code, so a client can tell "you're not allowed" from "you're out".
	r2 := newRig(t)
	r2.ops.invitesPerUser = 1
	if _, err := r2.ops.Invite(ctx, alice()); err != nil {
		t.Fatalf("first quota invite: %v", err)
	}
	err = func() error { _, err := r2.ops.Invite(ctx, alice()); return err }()
	errors.As(err, &e)
	if e == nil || e.Code != "invite_quota_exhausted" {
		t.Fatalf("second invite = %v, want invite_quota_exhausted", err)
	}
	if e.Details["max"] != 1 {
		t.Errorf("details = %v, want the quota a client can render", e.Details)
	}
}

// TestVerifyGitHubRejectsAForeignFingerprint is the authorization step of the
// GitHub link: naming a key you do not own must not let you claim the login it
// proves.
func TestVerifyGitHubRejectsAForeignFingerprint(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.github.listed = true

	_, err := r.ops.VerifyGitHub(ctx, mallory(), "someone", "SHA256:not-mine")
	if !IsKind(err, KindNotFound) {
		t.Fatalf("err = %v, want the masked KindNotFound", err)
	}
	for _, c := range r.calls.all() {
		if c == "LinkGitHub mallory someone" {
			t.Fatal("a foreign fingerprint linked the account")
		}
	}

	// Alice's own key, listed on github: the link lands.
	_, fp := mustKey(t, testKey)
	who, err := r.ops.VerifyGitHub(ctx, Caller{Handle: "alice", KeyFP: fp}, "alice-gh", "")
	if err != nil {
		t.Fatalf("VerifyGitHub: %v", err)
	}
	if who.GitHubLogin != "alice-gh" {
		t.Errorf("whoami github = %q", who.GitHubLogin)
	}

	// Not listed: denied, and denied is not the same as not-found.
	r.github.listed = false
	_, err = r.ops.VerifyGitHub(ctx, Caller{Handle: "alice", KeyFP: fp}, "alice-gh", "")
	if !IsKind(err, KindDenied) {
		t.Fatalf("unlisted key = %v, want KindDenied", err)
	}
}

// TestGitHubFailureIsUpstream keeps a github.com outage out of the 500 bucket.
func TestGitHubFailureIsUpstream(t *testing.T) {
	r := newRig(t)
	r.accts.users["alice"] = users.User{Handle: "alice", Status: "active", GitHubLogin: "alice-gh",
		GitHubVia: users.GitHubViaKeys}
	r.github.err = errors.New("dial tcp: i/o timeout")

	_, err := r.ops.ImportGitHubKeys(context.Background(), alice())
	if !IsKind(err, KindUpstream) {
		t.Fatalf("err = %v, want KindUpstream", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.HTTPStatus() != 502 {
		t.Errorf("status = %d, want 502", e.HTTPStatus())
	}
}

// TestImportWithoutLinkIsConflict: nothing is wrong with the request, the
// account just has no login to import from yet.
func TestImportWithoutLinkIsConflict(t *testing.T) {
	r := newRig(t)
	_, err := r.ops.ImportGitHubKeys(context.Background(), alice())
	var e *Error
	if !errors.As(err, &e) || e.Code != "github_not_linked" || e.HTTPStatus() != 409 {
		t.Fatalf("err = %v, want github_not_linked/409", err)
	}
}

// TestMintSessionTokenRefusesDisabledAccount is the security regression. This
// is the only operation whose own credential is sufficient to mint its
// successor, so without an account-state check a 7-day token renews itself
// forever — and the MAC key is fleet-wide, so the only other revocation is
// rotating the OIDC key for every user on the host. `status = 'disabled'` is
// the schema's sole deprovisioning mechanism, and the SSH-key and passkey paths
// both honour it.
func TestMintSessionTokenRefusesDisabledAccount(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	u := r.accts.users["alice"]
	u.Status = "disabled"
	r.accts.users["alice"] = u
	r.calls.reset()

	_, err := r.ops.MintSessionToken(ctx, alice(), time.Hour)
	if !IsKind(err, KindDenied) {
		t.Fatalf("err = %v, want KindDenied", err)
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Errorf("a disabled account still reached the minter: %v", got)
	}
}

// TestMintSessionTokenClampsTTL: a week-and-a-day request is a rounding error,
// not a user error — ctl has always clamped it silently.
func TestMintSessionTokenClampsTTL(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	for _, tc := range []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, DefaultSessionTokenTTL},
		{-time.Hour, DefaultSessionTokenTTL},
		{time.Hour, time.Hour},
		{365 * 24 * time.Hour, SessionTokenMaxTTL},
	} {
		r.calls.reset()
		if _, err := r.ops.MintSessionToken(ctx, alice(), tc.in); err != nil {
			t.Fatalf("mint %s: %v", tc.in, err)
		}
		want := "Mint alice ttl=" + tc.want.String()
		got := r.calls.all()
		if len(got) == 0 || got[len(got)-1] != want {
			t.Errorf("mint %s recorded %v, want last call %q", tc.in, got, want)
		}
	}
}
