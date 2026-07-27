package fleet

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routedguest"
)

type guestDialFunc func(context.Context, string, string, int) (net.Conn, error)

func (f guestDialFunc) DialGuest(
	ctx context.Context,
	sandbox, kind string,
	port int,
) (net.Conn, error) {
	return f(ctx, sandbox, kind, port)
}

type halfCloseConn struct {
	net.Conn
	closedWrite bool
}

func (c *halfCloseConn) CloseWrite() error {
	c.closedWrite = true
	return nil
}

func TestRoutedMetricsPreservesHalfClose(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() {
		left.Close()
		right.Close()
	})
	inner := &halfCloseConn{Conn: left}
	connection := &routedMetricsConn{Conn: inner}
	if err := connection.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if !inner.closedWrite {
		t.Fatal("routed metrics wrapper swallowed CloseWrite")
	}
}

func TestRoutedGuestMetricsCoverOpenLiveBytesAndFallback(t *testing.T) {
	metrics := fleetmetrics.New()
	left, right := net.Pipe()
	t.Cleanup(func() {
		left.Close()
		right.Close()
	})
	routed := &routedMetricsDialer{
		node: "node-b", metrics: metrics,
		next: guestDialFunc(func(context.Context, string, string, int) (net.Conn, error) {
			return left, nil
		}),
	}
	connection, err := routed.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 8080)
	if err != nil {
		t.Fatal(err)
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := right.Write([]byte("from"))
		readDone <- err
	}()
	buffer := make([]byte, 4)
	if _, err := io.ReadFull(connection, buffer); err != nil || string(buffer) != "from" {
		t.Fatalf("routed read = %q, %v", buffer, err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}

	writeDone := make(chan error, 1)
	go func() {
		got := make([]byte, 2)
		_, err := io.ReadFull(right, got)
		if err == nil && string(got) != "to" {
			err = errors.New("unexpected routed write")
		}
		writeDone <- err
	}()
	if _, err := connection.Write([]byte("to")); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}

	health := &healthStub{
		healthy:      true,
		capabilities: []string{nodelink.CapabilityRoutedGuestV1},
	}
	failingRoute := &routedMetricsDialer{
		node: "node-b", metrics: metrics,
		next: guestDialFunc(func(context.Context, string, string, int) (net.Conn, error) {
			return nil, &routedguest.RouteError{Kind: routedguest.KindTCP, Err: syscall.EHOSTUNREACH}
		}),
	}
	ssh := guestDialFunc(func(context.Context, string, string, int) (net.Conn, error) {
		return nil, nil
	})
	selector, err := NewGuestSelector(GuestTransportAuto, health, failingRoute, ssh)
	if err != nil {
		t.Fatal(err)
	}
	selector.setMetrics("node-b", metrics)
	if _, err := selector.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 8080); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		`sparkbox_fleet_guest_stream_open_duration_seconds_count{kind="tcp",node="node-b",outcome="ok",transport="routed"} 1`,
		`sparkbox_fleet_guest_stream_open_duration_seconds_count{kind="tcp",node="node-b",outcome="route_failure",transport="routed"} 1`,
		`sparkbox_fleet_guest_stream_bytes_total{direction="from_guest",kind="tcp",node="node-b",transport="routed"} 4`,
		`sparkbox_fleet_guest_stream_bytes_total{direction="to_guest",kind="tcp",node="node-b",transport="routed"} 2`,
		`sparkbox_fleet_guest_streams{kind="tcp",node="node-b",transport="routed"} 0`,
		`sparkbox_fleet_guest_route_fallback_total{node="node-b",outcome="connected"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(body, "alpha") {
		t.Fatal("routed guest metrics leaked a sandbox label or value")
	}
}
