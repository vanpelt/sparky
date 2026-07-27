// Package routedguest provides the direct, routed guest data plane. It treats
// the approved node prefix as a security boundary: a sandbox record names a
// guest, but never grants authority to dial an arbitrary address.
package routedguest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

var (
	ErrInvalidPrefix    = errors.New("invalid routed guest prefix")
	ErrMalformedHostIP  = errors.New("malformed guest HostIP")
	ErrOutOfPrefix      = errors.New("guest HostIP is outside approved prefix")
	ErrInvalidPort      = errors.New("invalid guest port")
	ErrUnsupportedKind  = errors.New("unsupported guest stream kind")
	ErrRouteUnavailable = errors.New("routed guest is unavailable")
)

// Resolver returns the current authoritative sandbox record. The returned
// record is consumed only during this call and should be a defensive copy.
type Resolver func(name string) (*host.Sandbox, bool)

// ContextDialer is satisfied by *net.Dialer and permits deterministic tests.
type ContextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// Outcome is a bounded route-health signal. It intentionally excludes sandbox
// names, IP addresses, ports, and raw errors so metrics cannot acquire
// unbounded labels.
type Outcome string

const (
	OutcomeConnected    Outcome = "connected"
	OutcomeRejected     Outcome = "rejected"
	OutcomeRouteFailure Outcome = "route_failure"
	OutcomeCanceled     Outcome = "canceled"
)

// Kind is a bounded projection of a caller's stream kind.
type Kind string

const (
	KindSSH   Kind = "ssh"
	KindTCP   Kind = "tcp"
	KindOther Kind = "other"
)

// Observation reports one routed attempt using bounded dimensions only.
type Observation struct {
	Kind    Kind
	Outcome Outcome
}

type Observer func(Observation)

// Options customizes socket dialing and route-health observation.
type Options struct {
	Dialer   ContextDialer
	Observer Observer
}

// Dialer opens ordinary TCP connections to guests inside one approved prefix.
type Dialer struct {
	prefix  netip.Prefix
	resolve Resolver
	dialer  ContextDialer
	observe Observer
}

// New validates the approved prefix and constructs a routed guest dialer.
// Prefix host bits are rejected instead of silently masked: the roster's
// approved value must itself be canonical.
func New(prefix netip.Prefix, resolve Resolver, options Options) (*Dialer, error) {
	if !prefix.IsValid() || !prefix.Addr().Is4() || prefix.Bits() > 30 || prefix != prefix.Masked() {
		return nil, fmt.Errorf("%w: %s", ErrInvalidPrefix, prefix)
	}
	if resolve == nil {
		return nil, errors.New("routedguest: sandbox resolver is required")
	}
	if options.Dialer == nil {
		options.Dialer = &net.Dialer{}
	}
	return &Dialer{
		prefix: prefix, resolve: resolve, dialer: options.Dialer, observe: options.Observer,
	}, nil
}

// Prefix is the canonical approved prefix this dialer confines connections to.
func (d *Dialer) Prefix() netip.Prefix { return d.prefix }

// ValidationError reports a refusal made before socket dialing.
type ValidationError struct {
	Sandbox string
	Field   string
	Value   string
	Prefix  netip.Prefix
	Err     error
}

func (e *ValidationError) Error() string {
	switch {
	case e == nil:
		return ""
	case errors.Is(e.Err, ErrOutOfPrefix):
		return fmt.Sprintf("sandbox %q HostIP %q is outside approved prefix %s", e.Sandbox, e.Value, e.Prefix)
	case e.Field != "":
		return fmt.Sprintf("sandbox %q has invalid %s %q: %v", e.Sandbox, e.Field, e.Value, e.Err)
	default:
		return fmt.Sprintf("sandbox %q routed target is invalid: %v", e.Sandbox, e.Err)
	}
}

func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// RouteError wraps only failures from the actual routed socket attempt.
// AutoDialer uses ErrRouteUnavailable to decide whether SSH fallback is safe;
// the underlying net error remains available to errors.Is/errors.As.
type RouteError struct {
	Kind Kind
	Err  error
}

func (e *RouteError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("routed guest %s connection failed: %v", e.Kind, e.Err)
}

func (e *RouteError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{ErrRouteUnavailable, e.Err}
}

// DialGuest satisfies fleet.GuestDialer without importing fleet.
func (d *Dialer) DialGuest(ctx context.Context, sandbox, streamKind string, port int) (net.Conn, error) {
	kind := boundedKind(streamKind)
	address, err := d.target(sandbox, streamKind, port)
	if err != nil {
		d.report(kind, OutcomeRejected)
		return nil, err
	}
	connection, err := d.dialer.DialContext(ctx, "tcp", address)
	if err == nil {
		d.report(kind, OutcomeConnected)
		return connection, nil
	}
	if canceled := contextError(ctx, err); canceled != nil {
		d.report(kind, OutcomeCanceled)
		return nil, canceled
	}
	d.report(kind, OutcomeRouteFailure)
	return nil, &RouteError{Kind: kind, Err: err}
}

func (d *Dialer) target(sandbox, streamKind string, port int) (string, error) {
	box, ok := d.resolve(sandbox)
	if !ok || box == nil {
		return "", fmt.Errorf("%w: %q", nodelink.ErrUnknownSandbox, sandbox)
	}
	// Check state before looking at addresses. A paused record can retain stale
	// endpoint data, and validating or dialing it would turn a normal lifecycle
	// race into a connection to a recycled guest slot.
	if box.State != vmm.StateRunning {
		return "", fmt.Errorf("%w: %q", nodelink.ErrNotRunning, sandbox)
	}
	address, err := netip.ParseAddr(box.HostIP)
	if err != nil || !address.Is4() || address.String() != box.HostIP {
		return "", &ValidationError{
			Sandbox: sandbox, Field: "HostIP", Value: box.HostIP,
			Prefix: d.prefix, Err: ErrMalformedHostIP,
		}
	}
	if !d.prefix.Contains(address) {
		return "", &ValidationError{
			Sandbox: sandbox, Field: "HostIP", Value: box.HostIP,
			Prefix: d.prefix, Err: ErrOutOfPrefix,
		}
	}

	switch streamKind {
	case nodelink.StreamSSH:
		rawHost, rawPort, err := net.SplitHostPort(box.SSHAddr)
		if err != nil || rawHost == "" {
			if err == nil {
				err = errors.New("missing host")
			}
			return "", &ValidationError{
				Sandbox: sandbox, Field: "SSHAddr", Value: box.SSHAddr,
				Prefix: d.prefix, Err: errors.Join(ErrInvalidPort, err),
			}
		}
		sshPort, err := numericPort(rawPort)
		if err != nil {
			return "", &ValidationError{
				Sandbox: sandbox, Field: "SSHAddr port", Value: rawPort,
				Prefix: d.prefix, Err: err,
			}
		}
		return net.JoinHostPort(address.String(), strconv.Itoa(sshPort)), nil
	case nodelink.StreamTCP:
		if port < 1 || port > 65535 {
			return "", &ValidationError{
				Sandbox: sandbox, Field: "TCP port", Value: strconv.Itoa(port),
				Prefix: d.prefix, Err: ErrInvalidPort,
			}
		}
		return net.JoinHostPort(address.String(), strconv.Itoa(port)), nil
	default:
		return "", &ValidationError{
			Sandbox: sandbox, Field: "stream kind", Value: streamKind,
			Prefix: d.prefix, Err: ErrUnsupportedKind,
		}
	}
}

func numericPort(raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != raw {
		return 0, ErrInvalidPort
	}
	return port, nil
}

func boundedKind(kind string) Kind {
	switch kind {
	case nodelink.StreamSSH:
		return KindSSH
	case nodelink.StreamTCP:
		return KindTCP
	default:
		return KindOther
	}
}

func contextError(ctx context.Context, dialError error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if errors.Is(dialError, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(dialError, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func (d *Dialer) report(kind Kind, outcome Outcome) {
	if d.observe != nil {
		d.observe(Observation{Kind: kind, Outcome: outcome})
	}
}
