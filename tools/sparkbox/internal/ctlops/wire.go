package ctlops

import (
	"errors"

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
func FromWire(w *WireError) *Error {
	if w == nil {
		return nil
	}
	kind, _ := ParseKind(w.Kind)
	e := &Error{
		Kind:     kind,
		Op:       w.Op,
		Code:     w.Code,
		Msg:      w.Msg,
		Hint:     w.Hint,
		Details:  w.Details,
		Verbatim: w.Verbatim,
		Exit:     w.Exit,
		Status:   w.Status,
		Err:      hostFromWire(w.Host),
	}
	return e
}

func hostFromWire(h *WireHostError) error {
	if h == nil {
		return nil
	}
	switch h.Type {
	case hostLimit:
		return &host.LimitError{Max: h.Max, Running: h.Running}
	case hostCapacity:
		return &host.CapacityError{RequestedMB: h.RequestedMB, UsedMB: h.UsedMB, BudgetMB: h.BudgetMB}
	case hostQuota:
		return &host.DiskQuotaError{Owner: h.Owner, RequestedMB: h.RequestedMB,
			UsedMB: h.UsedMB, PoolMB: h.PoolMB}
	case hostMissing:
		return &host.MissingError{Noun: h.Noun, Name: h.Name}
	case hostState:
		return &host.StateError{Code: h.Code, Msg: h.Msg}
	case hostDisabled:
		return &host.DisabledError{Code: h.Code, Msg: h.Msg}
	case hostName:
		return &host.NameError{Problem: host.NameProblem(h.Problem), Noun: h.Noun, Name: h.Name}
	}
	return nil
}
