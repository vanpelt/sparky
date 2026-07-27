package routedguest

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// GuestDialer is the transport-neutral data-plane contract shared with fleet.
type GuestDialer interface {
	DialGuest(context.Context, string, string, int) (net.Conn, error)
}

// FallbackOutcome is a bounded rollout signal.
type FallbackOutcome string

const (
	FallbackConnected  FallbackOutcome = "connected"
	FallbackFailed     FallbackOutcome = "failed"
	FallbackSuppressed FallbackOutcome = "suppressed"
)

type FallbackObserver func(FallbackOutcome)

// AutoDialer tries routed data first and uses the compatibility dialer only
// when the primary reports a typed route failure. It never falls back around a
// validation, placement, lifecycle, or cancellation refusal.
type AutoDialer struct {
	primary  GuestDialer
	fallback GuestDialer
	observe  FallbackObserver
}

func NewAuto(primary, fallback GuestDialer, observe FallbackObserver) (*AutoDialer, error) {
	if primary == nil {
		return nil, errors.New("routedguest: primary dialer is required")
	}
	if fallback == nil {
		return nil, errors.New("routedguest: fallback dialer is required")
	}
	return &AutoDialer{primary: primary, fallback: fallback, observe: observe}, nil
}

func (d *AutoDialer) DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error) {
	connection, primaryError := d.primary.DialGuest(ctx, sandbox, kind, port)
	if primaryError == nil {
		return connection, nil
	}
	if !errors.Is(primaryError, ErrRouteUnavailable) {
		return nil, primaryError
	}
	if err := ctx.Err(); err != nil {
		d.report(FallbackSuppressed)
		return nil, err
	}
	connection, fallbackError := d.fallback.DialGuest(ctx, sandbox, kind, port)
	if fallbackError == nil {
		d.report(FallbackConnected)
		return connection, nil
	}
	if ctx.Err() != nil {
		d.report(FallbackSuppressed)
		return nil, ctx.Err()
	}
	d.report(FallbackFailed)
	return nil, &FallbackError{Primary: primaryError, Fallback: fallbackError}
}

// FallbackError preserves both route and compatibility transport failures.
type FallbackError struct {
	Primary  error
	Fallback error
}

func (e *FallbackError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("routed guest failed (%v); fallback failed (%v)", e.Primary, e.Fallback)
}

func (e *FallbackError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{e.Primary, e.Fallback}
}

func (d *AutoDialer) report(outcome FallbackOutcome) {
	if d.observe != nil {
		d.observe(outcome)
	}
}

var (
	_ GuestDialer = (*Dialer)(nil)
	_ GuestDialer = (*AutoDialer)(nil)
)
