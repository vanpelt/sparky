package ctlops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// hop is the whole trip a failure takes between two machines: classify, encode,
// put it on a socket, decode, rebuild. Every assertion in this file is made
// against the far end of it, because an assertion made against ToWire's return
// value would not notice a field that JSON drops.
func hop(t *testing.T, op string, err error) *Error {
	t.Helper()
	raw, merr := json.Marshal(ToWire(op, err))
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	var w WireError
	if uerr := json.Unmarshal(raw, &w); uerr != nil {
		t.Fatalf("unmarshal %s: %v", raw, uerr)
	}
	return FromWire(&w)
}

// TestWireRoundTrip is the spike this whole design rests on: a failure raised on
// one machine has to arrive on another as the same failure, or every renderer in
// the tree grows a second code path for remote sandboxes. It enumerates
// everything AsError classifies and asserts the rendering contract — the two
// exit/status accessors and the four strings each transport prints — survives.
func TestWireRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		op   string
		err  error
		// check inspects the rebuilt cause. Nil means this failure has no
		// concrete host value to carry and must arrive with an empty cause.
		check func(t *testing.T, got *Error)
	}{
		{"limit", "start", &host.LimitError{Max: 2, Running: []string{"alicebox", "bobbox"}},
			func(t *testing.T, got *Error) {
				var e *host.LimitError
				if !errors.As(got, &e) {
					t.Fatalf("cause = %v, want *host.LimitError", got.Err)
				}
				// sshgw.failStart prints e.Running[0] with no nil guard, so an
				// empty slice here is a panicked SSH session, not a cosmetic loss.
				if e.Max != 2 || !reflect.DeepEqual(e.Running, []string{"alicebox", "bobbox"}) {
					t.Errorf("limit = %+v, want max 2 and both names", e)
				}
			}},
		{"capacity", "start", &host.CapacityError{RequestedMB: 8192, UsedMB: 40000, BudgetMB: 45000},
			func(t *testing.T, got *Error) {
				var e *host.CapacityError
				if !errors.As(got, &e) {
					t.Fatalf("cause = %v, want *host.CapacityError", got.Err)
				}
				if e.UsedMB != 40000 || e.BudgetMB != 45000 || e.RequestedMB != 8192 {
					t.Errorf("capacity = %+v", e)
				}
			}},
		{"quota", "create", &host.DiskQuotaError{Owner: "alice", RequestedMB: 25000, UsedMB: 90000, PoolMB: 100000},
			func(t *testing.T, got *Error) {
				var e *host.DiskQuotaError
				if !errors.As(got, &e) {
					t.Fatalf("cause = %v, want *host.DiskQuotaError", got.Err)
				}
				if e.Owner != "alice" || e.UsedMB != 90000 || e.PoolMB != 100000 || e.RequestedMB != 25000 {
					t.Errorf("quota = %+v", e)
				}
			}},
		{"missing", "rename", &host.MissingError{Noun: "sandbox", Name: "ghost"},
			func(t *testing.T, got *Error) {
				var e *host.MissingError
				if !errors.As(got, &e) {
					t.Fatalf("cause = %v, want *host.MissingError", got.Err)
				}
				if e.Noun != "sandbox" || e.Name != "ghost" {
					t.Errorf("missing = %+v", e)
				}
			}},
		{"state", "rename", &host.StateError{Code: "sandbox_archived", Msg: `sandbox "x" is archived`},
			func(t *testing.T, got *Error) {
				var e *host.StateError
				if !errors.As(got, &e) {
					t.Fatalf("cause = %v, want *host.StateError", got.Err)
				}
				if e.Code != "sandbox_archived" || e.Msg != `sandbox "x" is archived` {
					t.Errorf("state = %+v", e)
				}
			}},
		{"host disabled", "rename", &host.DisabledError{Code: "rename_disabled", Msg: "rename is not enabled on this host"},
			func(t *testing.T, got *Error) {
				var e *host.DisabledError
				if !errors.As(got, &e) {
					t.Fatalf("cause = %v, want *host.DisabledError", got.Err)
				}
				if e.Code != "rename_disabled" {
					t.Errorf("disabled = %+v", e)
				}
			}},
		{"name invalid", "create", &host.NameError{Problem: host.NameInvalid, Noun: "sandbox", Name: "Nope!"},
			nameCheck(host.NameInvalid, "sandbox", "Nope!")},
		{"name reserved", "create", &host.NameError{Problem: host.NameReserved, Noun: "sandbox", Name: "console"},
			nameCheck(host.NameReserved, "sandbox", "console")},
		{"name taken", "snapshot.create", &host.NameError{Problem: host.NameTaken, Noun: "snapshot", Name: "snap"},
			nameCheck(host.NameTaken, "snapshot", "snap")},

		// The sentinels carry no structure: Kind and Code are the whole answer,
		// and nothing downstream switches on their Go identity.
		{"key linked", "keys.add", users.ErrKeyLinked, nil},
		{"last key", "keys.rm", users.ErrLastKey, nil},
		{"no passkey", "passkey.rm", users.ErrNoSuchPasskey, nil},
		{"ambiguous passkey", "passkey.rm", users.ErrAmbiguousPasskey, nil},
		{"no schedule", "schedule.rm", schedule.ErrNotFound, nil},

		// A canceled or timed-out hop keeps its 499/504 override, which is the
		// only thing that tells a hung-up caller apart from a real fault.
		{"canceled", "pause", context.Canceled, nil},
		{"deadline", "pause", context.DeadlineExceeded, nil},

		{"not found", "pause", NotFound("pause", "sandbox", "ghost"), nil},
		{"invalid", "resize", Invalid("resize", "bad_size", "size must be between 1 and %d MB", MaxDiskMB), nil},
		{"feature disabled", "archive", Disabled("archive", "archiving isn't enabled on this host."), nil},
		{"denied", "invite", Denied("invite", "not_operator", "only operators can mint invites."), nil},

		{"stray", "pause", errors.New("boom"), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Wrapped, because that is how these reach a transport in practice.
			want := AsError(tc.op, fmt.Errorf("%s: %w", tc.op, tc.err))
			got := hop(t, tc.op, fmt.Errorf("%s: %w", tc.op, tc.err))

			if got.Kind != want.Kind {
				t.Errorf("kind = %v, want %v", got.Kind, want.Kind)
			}
			if got.Op != want.Op {
				t.Errorf("op = %q, want %q", got.Op, want.Op)
			}
			if got.Code != want.Code {
				t.Errorf("code = %q, want %q", got.Code, want.Code)
			}
			if got.Msg != want.Msg {
				t.Errorf("msg = %q, want %q", got.Msg, want.Msg)
			}
			if got.Hint != want.Hint {
				t.Errorf("hint = %q, want %q", got.Hint, want.Hint)
			}
			if got.Verbatim != want.Verbatim {
				t.Errorf("verbatim = %v, want %v", got.Verbatim, want.Verbatim)
			}
			if got.ExitCode() != want.ExitCode() {
				t.Errorf("exit = %d, want %d", got.ExitCode(), want.ExitCode())
			}
			if got.HTTPStatus() != want.HTTPStatus() {
				t.Errorf("status = %d, want %d", got.HTTPStatus(), want.HTTPStatus())
			}
			if got.Error() != want.Error() {
				t.Errorf("Error() = %q, want %q", got.Error(), want.Error())
			}

			if tc.check == nil {
				if got.Err != nil {
					t.Errorf("cause = %v, want none: only the host shapes travel", got.Err)
				}
				return
			}
			tc.check(t, got)
		})
	}
}

func nameCheck(problem host.NameProblem, noun, name string) func(*testing.T, *Error) {
	return func(t *testing.T, got *Error) {
		t.Helper()
		var e *host.NameError
		if !errors.As(got, &e) {
			t.Fatalf("cause = %v, want *host.NameError", got.Err)
		}
		if e.Problem != problem || e.Noun != noun || e.Name != name {
			t.Errorf("name = %+v, want %v/%s/%s", e, problem, noun, name)
		}
	}
}

// TestWireFreshError pins that FromWire hands out a value nobody else holds.
// AsError stamps Op in place on an already-classified error, so a shared one
// would let the second caller rename the first caller's operation.
func TestWireFreshError(t *testing.T) {
	w := ToWire("pause", NotFound("", "sandbox", "ghost"))
	a, b := FromWire(w), FromWire(w)
	if a == b {
		t.Fatal("FromWire returned a shared value")
	}
	AsError("outer", a)
	if b.Op != "pause" {
		t.Errorf("stamping one copy moved the other's op to %q", b.Op)
	}
	if ToWire("x", nil) != nil || FromWire(nil) != nil {
		t.Error("nil must survive as nil so no transport needs a special case")
	}
}

// TestWireDetailsAreFloats is the hazard WireHostError exists to route around,
// pinned so nobody deletes it as duplication. json.Unmarshal turns every number
// in a map[string]any into a float64; the typed cause is the only place an int
// or an []string survives the hop.
func TestWireDetailsAreFloats(t *testing.T) {
	got := hop(t, "start", &host.LimitError{Max: 2, Running: []string{"alicebox", "bobbox"}})

	if _, ok := got.Details["max"].(float64); !ok {
		t.Errorf("details[max] = %T; the round trip makes every number a float64", got.Details["max"])
	}
	if _, ok := got.Details["max"].(int); ok {
		t.Error("details[max] asserted as int — no consumer may do this")
	}
	var limit *host.LimitError
	if !errors.As(got, &limit) {
		t.Fatal("the typed cause is the only int-safe path and it did not arrive")
	}
	if limit.Max != 2 || len(limit.Running) != 2 {
		t.Errorf("limit = %+v, want the ints and the names intact", limit)
	}
}

// TestWireParseKind: Kind rides as a string precisely so an insertion into the
// iota block cannot renumber a released wire format, which only works if the
// mapping is total in both directions.
func TestWireParseKind(t *testing.T) {
	all := []Kind{KindInternal, KindInvalid, KindNotFound, KindDenied, KindConflict,
		KindDisabled, KindLimit, KindCapacity, KindQuota, KindUpstream}
	for _, k := range all {
		got, ok := ParseKind(k.String())
		if !ok || got != k {
			t.Errorf("ParseKind(%q) = %v/%v, want %v/true", k.String(), got, ok, k)
		}
	}

	// A peer that has learned a Kind this build has not still raised a failure
	// this build must render, and a 500 is the honest rendering of it.
	for _, s := range []string{"", "not_a_kind", "Internal", "NOT_FOUND"} {
		got, ok := ParseKind(s)
		if ok || got != KindInternal {
			t.Errorf("ParseKind(%q) = %v/%v, want KindInternal/false", s, got, ok)
		}
	}
}

// TestWireNeverCarriesTheCause: an unclassified cause may name an internal
// address or a driver path, so it is logged where it happened and never
// rendered to a client. The curated Host projection is the whole exception, and
// the way to keep it the whole exception is for no field on the wire to be able
// to hold an error at all.
func TestWireNeverCarriesTheCause(t *testing.T) {
	errType := reflect.TypeOf((*error)(nil)).Elem()
	wire := reflect.TypeOf(WireError{})
	for i := 0; i < wire.NumField(); i++ {
		f := wire.Field(i)
		if f.Type.Implements(errType) || f.Type == errType {
			t.Errorf("WireError.%s can carry an error; the cause stays in the node's log", f.Name)
		}
	}
}
