package routedguest

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

type guestDialFunc func(context.Context, string, string, int) (net.Conn, error)

func (f guestDialFunc) DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error) {
	return f(ctx, sandbox, kind, port)
}

func TestAutoDialerFallsBackOnlyForRouteFailures(t *testing.T) {
	var fallbackCalls int
	var outcome FallbackOutcome
	primary := guestDialFunc(func(context.Context, string, string, int) (net.Conn, error) {
		return nil, &RouteError{Kind: KindTCP, Err: syscall.EHOSTUNREACH}
	})
	fallback := guestDialFunc(func(_ context.Context, sandbox, kind string, port int) (net.Conn, error) {
		fallbackCalls++
		if sandbox != "demo" || kind != nodelink.StreamTCP || port != 8080 {
			t.Fatalf("fallback args = %q %q %d", sandbox, kind, port)
		}
		return successfulSocket(t)
	})
	auto, err := NewAuto(primary, fallback, func(value FallbackOutcome) { outcome = value })
	if err != nil {
		t.Fatal(err)
	}
	connection, err := auto.DialGuest(context.Background(), "demo", nodelink.StreamTCP, 8080)
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if fallbackCalls != 1 || outcome != FallbackConnected {
		t.Fatalf("fallback calls = %d, outcome = %q", fallbackCalls, outcome)
	}

	fallbackCalls = 0
	outcome = ""
	validation := guestDialFunc(func(context.Context, string, string, int) (net.Conn, error) {
		return nil, &ValidationError{Sandbox: "demo", Err: ErrOutOfPrefix}
	})
	auto, err = NewAuto(validation, fallback, func(value FallbackOutcome) { outcome = value })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auto.DialGuest(context.Background(), "demo", nodelink.StreamTCP, 8080); !errors.Is(err, ErrOutOfPrefix) {
		t.Fatalf("validation error = %v", err)
	}
	if fallbackCalls != 0 || outcome != "" {
		t.Fatalf("validation used fallback: calls=%d outcome=%q", fallbackCalls, outcome)
	}
}

func TestAutoDialerPreservesBothFailures(t *testing.T) {
	primary := guestDialFunc(func(context.Context, string, string, int) (net.Conn, error) {
		return nil, &RouteError{Kind: KindSSH, Err: syscall.ENETUNREACH}
	})
	fallbackCause := errors.New("SSH pool offline")
	fallback := guestDialFunc(func(context.Context, string, string, int) (net.Conn, error) {
		return nil, fallbackCause
	})
	var outcome FallbackOutcome
	auto, err := NewAuto(primary, fallback, func(value FallbackOutcome) { outcome = value })
	if err != nil {
		t.Fatal(err)
	}
	_, err = auto.DialGuest(context.Background(), "demo", nodelink.StreamSSH, 0)
	if !errors.Is(err, ErrRouteUnavailable) || !errors.Is(err, syscall.ENETUNREACH) || !errors.Is(err, fallbackCause) {
		t.Fatalf("fallback error = %v", err)
	}
	var combined *FallbackError
	if !errors.As(err, &combined) {
		t.Fatalf("fallback error type = %T", err)
	}
	if outcome != FallbackFailed {
		t.Fatalf("outcome = %q", outcome)
	}
}

func TestAutoDialerDoesNotFallbackAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var fallbackCalls int
	primary := guestDialFunc(func(context.Context, string, string, int) (net.Conn, error) {
		return nil, context.Canceled
	})
	fallback := guestDialFunc(func(context.Context, string, string, int) (net.Conn, error) {
		fallbackCalls++
		return nil, nil
	})
	auto, err := NewAuto(primary, fallback, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auto.DialGuest(ctx, "demo", nodelink.StreamTCP, 8080); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if fallbackCalls != 0 {
		t.Fatalf("fallback calls = %d, want 0", fallbackCalls)
	}
}

func TestNewAutoRequiresBothTransports(t *testing.T) {
	dialer := guestDialFunc(func(context.Context, string, string, int) (net.Conn, error) {
		return nil, nil
	})
	if _, err := NewAuto(nil, dialer, nil); err == nil {
		t.Fatal("NewAuto accepted nil primary")
	}
	if _, err := NewAuto(dialer, nil, nil); err == nil {
		t.Fatal("NewAuto accepted nil fallback")
	}
}
