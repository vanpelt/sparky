package nodelink

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
)

func scrapeMetrics(t *testing.T, r *fleetmetrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestControlMetricsFollowARequestLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })

	metrics := fleetmetrics.New()
	gateway := NewConn(left, left, "g", nil)
	node := NewConn(right, right, "n", nil)
	gateway.SetMetrics(metrics, "node-b", "ssh")
	node.SetMetrics(metrics, "node-b", "ssh")
	pingHandler(node)
	go gateway.Serve(ctx) //nolint:errcheck
	go node.Serve(ctx)    //nolint:errcheck

	reqCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := gateway.Request(reqCtx, TypePing, PingReq{Nonce: "metric"}, nil); err != nil {
		t.Fatal(err)
	}

	got := scrapeMetrics(t, metrics)
	for _, want := range []string{
		`sparkbox_fleet_control_rpc_duration_seconds_count{node="node-b",operation="ping",outcome="ok",transport="ssh"} 1`,
		`sparkbox_fleet_control_pending_requests{node="node-b",transport="ssh"} 0`,
		`sparkbox_fleet_control_in_flight_requests{node="node-b",operation="ping",transport="ssh"} 0`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape missing %q\n%s", want, got)
		}
	}
}

func TestBackloggedWriterCountsDroppedEvents(t *testing.T) {
	w := newStalledWriter()
	t.Cleanup(func() { close(w.release) })
	metrics := fleetmetrics.New()
	c := NewConn(strings.NewReader(""), w, "g", nil)
	c.SetMetrics(metrics, "node-b", "ssh")

	if err := c.Event(TypeHeartbeat, Heartbeat{}); err != nil {
		t.Fatal(err)
	}
	<-w.entered
	for i := 0; i < writeQueueDepth; i++ {
		if err := c.Event(TypeHeartbeat, Heartbeat{}); err != nil {
			t.Fatalf("fill event %d: %v", i, err)
		}
	}
	if err := c.Event(TypeHeartbeat, Heartbeat{}); err != ErrLinkBacklogged {
		t.Fatalf("overflow = %v, want %v", err, ErrLinkBacklogged)
	}

	got := scrapeMetrics(t, metrics)
	if want := `sparkbox_fleet_control_dropped_total{kind="event",node="node-b",transport="ssh"} 1`; !strings.Contains(got, want) {
		t.Fatalf("scrape missing %q\n%s", want, got)
	}
}

func TestGuestStreamMetricsHaveNoSandboxLabel(t *testing.T) {
	dl := newDataLink(t)
	dl.running(t, "web")
	metrics := fleetmetrics.New()
	dl.client.metrics = metrics

	addr := echoServer(t)
	_, rawPort, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dl.client.DialSandbox(context.Background(), "web", StreamTCP, port)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	got := scrapeMetrics(t, metrics)
	for _, want := range []string{
		`sparkbox_fleet_guest_stream_open_duration_seconds_count{kind="tcp",node="node-b",outcome="ok",transport="ssh"} 1`,
		`sparkbox_fleet_guest_stream_bytes_total{direction="to_guest",kind="tcp",node="node-b",transport="ssh"} 5`,
		`sparkbox_fleet_guest_stream_bytes_total{direction="from_guest",kind="tcp",node="node-b",transport="ssh"} 5`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "sandbox=") || strings.Contains(got, `web`) {
		t.Fatal("guest stream metrics leaked a sandbox label or value")
	}
}
