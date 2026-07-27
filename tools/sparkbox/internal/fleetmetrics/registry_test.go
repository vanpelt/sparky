package fleetmetrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryExposesBoundedFleetLabels(t *testing.T) {
	r := New()
	r.ObserveControlRPC("node-b", "ssh", "sandbox.ensure_running", "ok", 25*time.Millisecond)
	r.AddPending("node-b", "ssh", 1)
	r.AddPending("node-b", "ssh", -1)
	r.AddInFlight("node-b", "ssh", "sandbox.ensure_running", 1)
	r.AddInFlight("node-b", "ssh", "sandbox.ensure_running", -1)
	r.SetWriteQueueDepth("node-b", "ssh", 3)
	r.IncDropped("node-b", "ssh", "event")
	r.IncReconnect("node-b", "ssh")
	r.IncLivenessFailure("node-b", "ssh", "gateway")
	r.IncDisconnect("node-b", "ssh", "liveness")
	r.ObserveStreamOpen("node-b", "ssh", "tcp", "ok", 5*time.Millisecond)
	r.AddLiveStreams("node-b", "ssh", "tcp", 1)
	r.AddStreamBytes("node-b", "ssh", "tcp", "to_guest", 42)
	r.IncEnsureReady("node-b", "resume")
	r.ObserveProxyTTFB("node-b", "cold", 40*time.Millisecond)
	r.ObserveManagerSave("node-b", "ok", 2*time.Millisecond)
	r.ObserveActivityFlush("node-b", "ok", 7)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{
		`sparkbox_fleet_control_rpc_duration_seconds_count{node="node-b",operation="sandbox.ensure_running",outcome="ok",transport="ssh"} 1`,
		`sparkbox_fleet_control_write_queue_depth{node="node-b",transport="ssh"} 3`,
		`sparkbox_fleet_control_dropped_total{kind="event",node="node-b",transport="ssh"} 1`,
		`sparkbox_fleet_guest_stream_bytes_total{direction="to_guest",kind="tcp",node="node-b",transport="ssh"} 42`,
		`sparkbox_fleet_ensure_ready_total{classification="resume",node="node-b"} 1`,
		`sparkbox_proxy_time_to_first_byte_seconds_count{node="node-b",temperature="cold"} 1`,
		`sparkbox_manager_save_duration_seconds_count{node="node-b",outcome="ok"} 1`,
		`sparkbox_manager_activity_flushed_marks_total{node="node-b"} 7`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape missing %q\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"sandbox=", "owner=", "request_id=", "operation_id="} {
		if strings.Contains(got, forbidden) {
			t.Errorf("scrape contains forbidden unbounded label %q", forbidden)
		}
	}
}

func TestRegistriesAreIsolated(t *testing.T) {
	first, second := New(), New()
	first.IncDropped("node-a", "ssh", "reply")

	rec := httptest.NewRecorder()
	second.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(rec.Body.String(), "sparkbox_fleet_control_dropped_total") {
		t.Fatal("a fresh registry exposed another registry's samples")
	}
}
