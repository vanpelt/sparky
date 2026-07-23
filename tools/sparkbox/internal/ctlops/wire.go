package ctlops

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// This file is the link projection of the failure taxonomy, and it lives next to
// the taxonomy for the same reason Budgets does: a second copy of the mapping,
// in whichever package happens to own a transport, is a copy that drifts.
// restapi's apiError is the HTTP projection of the same *Error; this is the one
// for a transport that has to reconstitute Go types on the far side, so it
// carries strictly more than the HTTP shape does.

// WireError is *Error projected for a transport that cannot carry Go types.
// Kind rides as its String() because the Kind constants are iota-ordered, and a
// future insertion between two of them would silently renumber a wire format
// already in the field.
//
// Err is deliberately absent. The wrapped cause may name an internal address or
// a driver path, so it is logged where it happened and never rendered to a
// client — the proxy's rule. What a caller genuinely needs back is the
// *classified* cause, and that rides in Host as typed fields.
type WireError struct {
	Kind     string         `json:"kind"`
	Op       string         `json:"op,omitempty"`
	Code     string         `json:"code,omitempty"`
	Msg      string         `json:"msg"`
	Hint     string         `json:"hint,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
	Verbatim bool           `json:"verbatim,omitempty"`
	Exit     int            `json:"exit,omitempty"`
	Status   int            `json:"status,omitempty"`
	Host     *WireHostError `json:"host,omitempty"`
}

// WireHostError carries the concrete internal/host error a *Error wraps, with
// typed fields rather than a Details map. This is not tidiness: json.Unmarshal
// turns every number in a map[string]any into a float64, and sshgw's failStart
// dereferences limit.Running[0] with no nil guard — so a LimitError rebuilt out
// of Details would either panic on a type assertion or arrive with an empty
// Running slice and panic the session that was only trying to explain itself.
//
// Type names which of the seven host shapes to rebuild; the fields are the union
// of all seven, since only one is ever populated.
type WireHostError struct {
	Type        string   `json:"type"` // limit|capacity|quota|missing|state|disabled|name
	Max         int      `json:"max,omitempty"`
	Running     []string `json:"running,omitempty"`
	RequestedMB int64    `json:"requested_mb,omitempty"`
	UsedMB      int64    `json:"used_mb,omitempty"`
	BudgetMB    int64    `json:"budget_mb,omitempty"`
	PoolMB      int64    `json:"pool_mb,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	Noun        string   `json:"noun,omitempty"`
	Name        string   `json:"name,omitempty"`
	Problem     int      `json:"problem,omitempty"` // host.NameProblem
	Code        string   `json:"code,omitempty"`
	Msg         string   `json:"msg,omitempty"`
}

// The seven host shapes, as they travel.
const (
	hostLimit    = "limit"
	hostCapacity = "capacity"
	hostQuota    = "quota"
	hostMissing  = "missing"
	hostState    = "state"
	hostDisabled = "disabled"
	hostName     = "name"
)

// ToWire classifies err the way every other transport does and projects the
// result. Sending an unclassified error is impossible by construction, so the
// far side never has to guess.
func ToWire(op string, err error) *WireError {
	e := AsError(op, err)
	if e == nil {
		return nil
	}
	w := &WireError{
		Kind:     e.Kind.String(),
		Op:       e.Op,
		Code:     e.Code,
		Msg:      e.Msg,
		Hint:     e.Hint,
		Details:  e.Details,
		Verbatim: e.Verbatim,
		Exit:     e.Exit,
		Status:   e.Status,
		Host:     hostToWire(e),
	}
	return w
}

// hostToWire walks the cause chain for the concrete admission and name errors
// the manager raises. Everything else — a store fault, a driver fault, a
// sentinel from users or schedule — is fully described by Kind, Code and Msg,
// and nothing on the far side switches on its Go type.
func hostToWire(err error) *WireHostError {
	var limit *host.LimitError
	if errors.As(err, &limit) {
		return &WireHostError{Type: hostLimit, Max: limit.Max, Running: limit.Running}
	}
	var capacity *host.CapacityError
	if errors.As(err, &capacity) {
		return &WireHostError{Type: hostCapacity, RequestedMB: capacity.RequestedMB,
			UsedMB: capacity.UsedMB, BudgetMB: capacity.BudgetMB}
	}
	var quota *host.DiskQuotaError
	if errors.As(err, &quota) {
		return &WireHostError{Type: hostQuota, Owner: quota.Owner, RequestedMB: quota.RequestedMB,
			UsedMB: quota.UsedMB, PoolMB: quota.PoolMB}
	}
	var missing *host.MissingError
	if errors.As(err, &missing) {
		return &WireHostError{Type: hostMissing, Noun: missing.Noun, Name: missing.Name}
	}
	var state *host.StateError
	if errors.As(err, &state) {
		return &WireHostError{Type: hostState, Code: state.Code, Msg: state.Msg}
	}
	var off *host.DisabledError
	if errors.As(err, &off) {
		return &WireHostError{Type: hostDisabled, Code: off.Code, Msg: off.Msg}
	}
	var name *host.NameError
	if errors.As(err, &name) {
		return &WireHostError{Type: hostName, Problem: int(name.Problem),
			Noun: name.Noun, Name: name.Name}
	}
	return nil
}

// FromWire rebuilds the *Error, including the concrete internal/host value it
// wrapped. That last part is the whole point of this round trip: (*Error).Unwrap
// returns Err, so the shipped errors.As switches in sshgw.failStart and
// xterm.startFailure keep firing on an error that crossed a machine boundary,
// and not one line of either renderer has to learn that nodes exist.
//
// The result is always a fresh value, never a shared sentinel, because AsError
// stamps Op in place on an already-classified error — handing the same *Error to
// two callers would let one rename the other's operation.
//
// Every field of w was authored by the peer, and this is the only place in the
// tree where those bytes become an *Error. Downstream nothing re-checks them:
// Msg and Hint are printed onto an operator's terminal by sshgw.fail and
// sshgw.failCtl, Status is handed to http.ResponseWriter.WriteHeader (which
// panics outside [100, 999], and answers 200 for a failure inside it), Exit
// becomes the ctl@ process's exit status, and Details is copied verbatim into
// the JSON body of every API answer. Sanitising the cause and trusting the
// envelope around it only moved the hole, so the whole envelope is bounded
// here — one boundary, so that no consumer has to remember there is one.
func FromWire(w *WireError) *Error {
	if w == nil {
		return nil
	}
	kind, _ := ParseKind(w.Kind)
	e := &Error{
		Kind: kind,
		// Op is the <what> in "sparkbox: <what> failed" and the "op" of the
		// JSON body. Every op that can cross a link is a frame type — a dotted
		// token — and a peer that sends something else has its op dropped
		// rather than printed, which costs nothing: AsError stamps the local
		// caller's op onto an error whose own is empty, and the local name of
		// the operation we just performed is the more trustworthy one anyway.
		Op: wireToken(w.Op),
		// Code is what API clients switch on. Same rule, same reason.
		Code: wireToken(w.Code),
		Msg:  wireMsg(w.Msg),
		// A refused Hint is dropped rather than replaced, because a hint is
		// optional by construction: "no advice" is a state every renderer
		// already handles, and inventing advice we do not have would be worse
		// than staying quiet.
		Hint:     wireSentence(w.Hint, maxWireMsg),
		Details:  wireDetails(w.Details),
		Verbatim: w.Verbatim,
		Exit:     wireExit(w.Exit),
		Status:   wireStatus(w.Status),
		Err:      hostFromWire(w.Host),
	}
	return e
}

// wireToken keeps a machine token that is one and drops one that is not. An
// empty result is the honest answer for a token we will not repeat, and both
// fields that use it are already optional on the wire.
func wireToken(s string) string {
	if safeToken(s) {
		return s
	}
	return ""
}

// wireMsg bounds the sentence every transport prints. Unlike the names inside
// the cause — which are scrubbed, because a user reading about a sandbox we
// refused to name is worse than a scrubbed name — a Msg that needed scrubbing
// is not a curated sentence at all: the CR and the ESC are there to forge a
// second "sparkbox: " line or to drive the reader's emulator, and what remains
// after scrubbing is still the attacker's line with its punctuation removed,
// printed by us as if we had written it. So it is replaced outright.
//
// An empty or blank Msg gets the same replacement, because (*Error).Error() is
// Msg: a failure with no sentence renders as nothing at all, which reads as
// success on a terminal.
func wireMsg(s string) string {
	if out := wireSentence(s, maxWireMsg); out != "" {
		return out
	}
	return msgRemoteUndescribed
}

// msgRemoteUndescribed is what a peer gets when its own sentence cannot be
// shown. It is deliberately about the peer rather than about this host: the
// operation really did fail somewhere else, and that is the one fact still
// worth telling whoever is reading.
const msgRemoteUndescribed = "the remote host reported a failure it could not describe"

// wireSentence is the "bounded line of readable text" rule: no character that
// steers a terminal, and short enough that a terminal can show it. Length is
// truncated rather than refused, because a long *printable* sentence is merely
// verbose — it is not pretending to be something it is not.
func wireSentence(s string, max int) string {
	if strings.ContainsFunc(s, unsafeRune) {
		return ""
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max])) + "..."
	}
	return s
}

// wireExit and wireStatus below bound the two numbers a peer authors that this
// host then acts on. Both are clamped a SECOND time, on the way out, by
// (*Error).ExitCode and (*Error).HTTPStatus — and those accessors are the
// enforcing layer, because every consumer goes through them whatever built the
// *Error, while this pair only covers the one path that crosses a link. Neither
// clamp is redundant and neither is the one to delete: the accessors make the
// invariant true everywhere, and these make it true of the value actually stored,
// so a log line or a test that reads e.Status raw sees the same bounded number
// the edge will answer with.
//
// wireExit bounds the ctl@ process's exit status. Zero is the field's own "use
// the Kind's" sentinel, so an out-of-range code falls back to the Kind rather
// than to a number of this function's choosing — the classification crossed the
// link intact and is the better answer. The window is [1, 255] because an SSH
// exit status rides as a uint32 (so -9 arrives as four billion) and because a
// zero would tell `ctl@ … && rm -rf` that the command it just failed at
// succeeded.
func wireExit(v int) int {
	if v < 1 || v > 255 {
		return 0
	}
	return v
}

// wireStatus bounds the HTTP status the REST edge answers with. Same fallback
// for the same reason, and the window is the whole error range rather than an
// allowlist of the codes this build mints: a newer node may classify something
// this one cannot, exactly as ParseKind allows for, and 4xx/5xx is the property
// that actually matters. Outside it a node could panic WriteHeader (anything
// past 999), answer 200 with an error envelope, or 302 a client somewhere else
// — three ways to make a failure on another machine look like something it is
// not to every API client.
func wireStatus(v int) int {
	if v < 400 || v > 599 {
		return 0
	}
	return v
}

// The bounds a node-supplied value has to fit inside to become a Go value here.
// They are not tuned to anything this host would actually produce — they are the
// point past which a number stops describing a machine and a string stops
// fitting in a sentence somebody has to read.
const (
	maxWireNames = 32      // running sandboxes worth naming in one line
	maxWireText  = 128     // one name, one noun, one owner
	maxWireMsg   = 512     // one sentence
	maxWireCount = 1 << 20 // a per-owner cap, sanely bounded
	maxWireMB    = 1 << 32 // 4 PB expressed in MB: past any host, short of a number that reads as garbage
	maxWireToken = 64      // a machine token — "sandbox_archived", "rename_disabled"
)

// hostFromWire rebuilds the concrete cause, but only from fields that can
// actually satisfy the rebuilt type's invariants. Nothing downstream re-checks
// them: sshgw.failStart prints limit.Running[0] with no guard, so a LimitError
// with an empty Running is a panicked SSH session; StateError.Error() *is* its
// Msg, so an empty one is a failure that renders as nothing at all; and every
// string in here is printed straight into somebody's terminal, where a bare \r
// forges a "sparkbox: " line and an ESC drives the reader's emulator. A node is
// trusted to run sandboxes, not to have stayed honest, so the invariants are
// re-established on arrival rather than assumed.
//
// When one cannot be met the cause is dropped instead of half-built, and that
// costs nothing a caller can see: Kind, Code, Msg and Hint travel beside it and
// are what every renderer actually prints — the REST projection and both
// consoles never touch the cause at all. The typed value only ever selects the
// *friendlier* branch, the one that names the sandboxes to pause, and a branch
// that cannot be rendered honestly is one no caller should be routed into.
func hostFromWire(h *WireHostError) error {
	if h == nil {
		return nil
	}
	switch h.Type {
	case hostLimit:
		// The rule: a limit refusal whose names did not survive scrubbing, or
		// whose cap is not a cap anyone could have hit, arrives with no typed
		// cause at all.
		//
		// It is no longer load-bearing against a panic — sshgw.failStart guards
		// its Running[0] and xterm.startFailure only joins the slice — and it is
		// lossy: a genuine "max 3 running" from another machine that arrives with
		// an unprintable name list is downgraded here to the generic rendering.
		// It stays because the typed cause exists only to select the friendlier
		// branch, the one that names a sandbox to pause. With no names that
		// branch has nothing to say that the Kind, Msg and Hint travelling beside
		// it do not already say, and "you already have 0 running sandboxes" is a
		// sentence no reader can act on.
		running := safeNames(h.Running)
		if len(running) == 0 || h.Max < 1 || h.Max > maxWireCount {
			return nil
		}
		return &host.LimitError{Max: h.Max, Running: running}
	case hostCapacity:
		// These three are printed as "(used/budget MB allocated)". A negative
		// or absurd figure is not a capacity report, and a zero budget is a
		// host that admits nothing — neither describes a host to wait for.
		if !saneMB(h.RequestedMB) || !saneMB(h.UsedMB) || !saneMB(h.BudgetMB) || h.BudgetMB == 0 {
			return nil
		}
		return &host.CapacityError{RequestedMB: h.RequestedMB, UsedMB: h.UsedMB, BudgetMB: h.BudgetMB}
	case hostQuota:
		owner := safeText(h.Owner, maxWireText)
		if owner == "" || !saneMB(h.RequestedMB) || !saneMB(h.UsedMB) || !saneMB(h.PoolMB) || h.PoolMB == 0 {
			return nil
		}
		return &host.DiskQuotaError{Owner: owner, RequestedMB: h.RequestedMB,
			UsedMB: h.UsedMB, PoolMB: h.PoolMB}
	case hostMissing:
		// Noun is half a machine token wherever this is reclassified
		// ("sandbox_not_found"), so it is held to a token's charset rather than
		// scrubbed; Name is whatever the caller typed and only has to be
		// printable and present, since `"" not found` names nothing.
		name := safeText(h.Name, maxWireText)
		if !safeToken(h.Noun) || name == "" {
			return nil
		}
		return &host.MissingError{Noun: h.Noun, Name: name}
	case hostState:
		msg := safeText(h.Msg, maxWireMsg)
		if !safeToken(h.Code) || msg == "" {
			return nil
		}
		return &host.StateError{Code: h.Code, Msg: msg}
	case hostDisabled:
		msg := safeText(h.Msg, maxWireMsg)
		if !safeToken(h.Code) || msg == "" {
			return nil
		}
		return &host.DisabledError{Code: h.Code, Msg: msg}
	case hostName:
		// Name is the string the caller was refused, so it is deliberately not
		// held to the name charset — "Nope!" is exactly what a NameInvalid
		// carries — only made printable. An unrecognised Problem clamps to
		// NameInvalid rather than dropping the cause, because NameError's own
		// default branch already renders anything it does not know as "invalid
		// <noun> name": the clamp only makes the classification agree with the
		// sentence that would have been printed either way.
		name := safeText(h.Name, maxWireText)
		if !safeToken(h.Noun) || name == "" {
			return nil
		}
		problem := host.NameProblem(h.Problem)
		switch problem {
		case host.NameInvalid, host.NameReserved, host.NameTaken:
		default:
			problem = host.NameInvalid
		}
		return &host.NameError{Problem: problem, Noun: h.Noun, Name: name}
	}
	return nil
}

// unsafeRune reports whether a rune must not reach a terminal or a console.
// Control characters are the obvious half — C0, C1 and DEL are what drives an
// emulator and what forges a line. The bidi overrides are the same attack
// without a control character: past a U+202E the rest of the line renders
// right-to-left, so what a human reads is not what was sent.
func unsafeRune(r rune) bool {
	if unicode.IsControl(r) {
		return true
	}
	switch {
	case r >= 0x202a && r <= 0x202e, // LRE, RLE, PDF, LRO, RLO
		r >= 0x2066 && r <= 0x2069, // the isolates
		r == 0x200e, r == 0x200f:   // LRM, RLM
		return true
	}
	return false
}

// safeText makes a node-supplied string printable: everything unsafeRune names
// goes, and the result is trimmed and capped. Scrubbed rather than rejected
// because these are prose and names a human typed, and blanking one would leave
// a user reading about a sandbox we refused to name.
func safeText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unsafeRune(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max])) + "..."
	}
	return s
}

// safeNames scrubs the running set and bounds it. The cap is on the loop and not
// only on the result because len(in) is whatever a peer put in the JSON array,
// and a line naming a thousand sandboxes helps nobody read it anyway.
func safeNames(in []string) []string {
	n := len(in)
	if n > maxWireNames {
		n = maxWireNames
	}
	out := make([]string, 0, n)
	for _, s := range in[:n] {
		if name := safeText(s, maxWireText); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// safeToken reports whether s can serve as a machine token — a Code a client
// switches on, a Noun that becomes half of one. These are ours rather than a
// user's, so an unexpected shape means a broken or hostile peer and is refused
// instead of scrubbed. The charset is the union of every token this tree mints,
// deliberately loose enough that a newer node may add one.
func safeToken(s string) bool {
	if s == "" || len(s) > maxWireToken {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// saneMB bounds a megabyte figure. Negative is impossible for every one of
// them, and the ceiling is where a number stops describing storage a host has.
func saneMB(v int64) bool { return v >= 0 && v <= maxWireMB }

// What a Details map may be. Every one this tree mints is three flat facts or
// fewer — running[] and max, the three capacity figures, matches[] — so these
// are generous by an order of magnitude and still nowhere near the megabyte a
// peer may put in one frame.
const (
	maxWireDetailKeys  = 16   // keys in one map
	maxWireDetailItems = 32   // elements in one array
	maxWireDetailRunes = 4096 // text across the WHOLE map, not per value
)

// wireDetails bounds the one field with no shape of its own. It is not printed
// on a terminal, which is why it is not held to the sentence rule, but it is
// copied straight into the JSON body restapi answers with and rendered into a
// page by both consoles — so it is bounded in size and scrubbed of anything
// that steers whatever renders it.
//
// The rune budget is spent across the whole map rather than per value because
// sixteen fields of four thousand characters is the same megabyte as one field
// of sixty-five thousand. Keys are sorted first so that which facts survive a
// hostile map is deterministic rather than a property of Go's map iteration.
func wireDetails(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	budget := maxWireDetailRunes
	out := make(map[string]any, len(in))
	for _, k := range keys {
		if len(out) >= maxWireDetailKeys {
			break
		}
		// A key is a field name a client reads by ("used_mb", "running"), so it
		// is held to the same token rule as Code.
		if !safeToken(k) {
			continue
		}
		if v, ok := wireDetailValue(in[k], &budget); ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// wireDetailValue keeps the shapes a fact can honestly take: a scalar, or a
// flat array of them. Nesting is refused rather than walked — nothing in this
// tree puts a tree in Details, so refusing one costs nothing here and bounds
// the recursion by construction rather than by a depth counter somebody has to
// keep correct.
//
// Numbers arrive as float64 (json.Unmarshal gives a map[string]any nothing
// else); json.Number is accepted too so that a future decoder with UseNumber
// does not silently start dropping every figure in the map.
func wireDetailValue(v any, budget *int) (any, bool) {
	switch t := v.(type) {
	case nil, bool, float64, json.Number:
		return v, true
	case string:
		// The budget is also the truncation point, so a first value cannot both
		// overrun the map's allowance and leave a negative one for the next.
		if *budget <= 0 {
			return nil, false
		}
		max := maxWireMsg
		if max > *budget {
			max = *budget
		}
		s := safeText(t, max)
		if s == "" {
			return nil, false
		}
		*budget -= len([]rune(s))
		return s, true
	case []any:
		if len(t) > maxWireDetailItems {
			t = t[:maxWireDetailItems]
		}
		out := make([]any, 0, len(t))
		for _, item := range t {
			switch item.(type) {
			case []any, map[string]any:
				continue // flat facts only; see above
			}
			if s, ok := wireDetailValue(item, budget); ok {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	}
	return nil, false
}
