package fleet

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGRPCControlMetricsCoverWatchRecoveryAndLiveness(t *testing.T) {
	metrics := fleetmetrics.New()
	client := newFakeDurable()
	client.setInventory(inventoryProto(1, sandboxProto("alpha", vmm.StateRunning)))
	control, err := NewGRPCControl(context.Background(), GRPCControlOptions{
		Node: "node-b", Client: client, Metrics: metrics,
		Retry: 5 * time.Millisecond, HealthEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := control.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	first := client.nextWatch(t)

	client.setHealthError(status.Error(codes.Unavailable, "probe failed"))
	first.errs <- status.Error(codes.Unavailable, "watch failed")
	waitForGRPC(t, func() bool {
		return strings.Contains(scrapeMetrics(t, metrics),
			`sparkbox_fleet_liveness_failures_total{node="node-b",side="gateway",transport="grpc"}`)
	})
	client.setHealthError(nil)
	_ = client.nextWatch(t)

	body := scrapeMetrics(t, metrics)
	for _, want := range []string{
		`sparkbox_fleet_control_pending_requests{node="node-b",transport="grpc"} 1`,
		`sparkbox_fleet_control_in_flight_requests{node="node-b",operation="watch_events",transport="grpc"} 1`,
		`sparkbox_fleet_reconnects_total{node="node-b",transport="grpc"} 1`,
		`sparkbox_fleet_disconnects_total{node="node-b",reason="transport",transport="grpc"} 1`,
		`sparkbox_fleet_liveness_failures_total{node="node-b",side="gateway",transport="grpc"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gRPC metrics missing %q", want)
		}
	}
	if strings.Contains(body, "alpha") {
		t.Fatal("gRPC transport metrics leaked a sandbox label or value")
	}
}

func TestControlSelectorRollsOutByOperationClass(t *testing.T) {
	client := newFakeDurable()
	client.setInventory(inventoryProto(1, sandboxProto("alpha", vmm.StateRunning)))
	grpc := newReadyGRPCControl(t, client)
	ssh := &selectorStub{name: "node-b", online: true}
	selector, err := NewControlSelector(ControlTransportSSH, grpc, ssh)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.ConfigureRollout(ControlRollout{
		ReadOnly: ControlTransportGRPC, Idempotent: ControlTransportSSH,
		Destructive: ControlTransportSSH,
	}); err != nil {
		t.Fatal(err)
	}
	if box, ok := selector.Box("alpha"); !ok || box.State != vmm.StateRunning {
		t.Fatalf("read-only rollout did not use gRPC: %+v, %v", box, ok)
	}
	if _, err := selector.EnsureReady(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := selector.Pause(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if ssh.ensureCalls != 1 || ssh.pauseCalls != 1 {
		t.Fatalf("SSH rollout calls ensure=%d pause=%d, want 1/1", ssh.ensureCalls, ssh.pauseCalls)
	}

	if err := selector.ConfigureRollout(ControlRollout{
		ReadOnly: ControlTransportSSH, Idempotent: ControlTransportGRPC,
		Destructive: ControlTransportGRPC,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := selector.EnsureReady(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := selector.Pause(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if ssh.ensureCalls != 1 || ssh.pauseCalls != 1 {
		t.Fatal("gRPC operation classes unexpectedly invoked SSH")
	}

	withoutGRPC, err := NewControlSelector(ControlTransportSSH, nil, ssh)
	if err != nil {
		t.Fatal(err)
	}
	if err := withoutGRPC.ConfigureRollout(ControlRollout{ReadOnly: ControlTransportGRPC}); err == nil {
		t.Fatal("explicit gRPC operation rollout accepted no adapter")
	}
}

func TestShadowInventoryComparisonIsSideEffectFreeAndBounded(t *testing.T) {
	now := time.Unix(1234, 0).UTC()
	box := &host.Sandbox{
		Name: "alpha", Owner: "alice", Image: "ubuntu", VCPUs: 2, MemMB: 2048,
		State: vmm.StateRunning, CreatedAt: now, LastActive: now, Node: "node-b",
	}
	facts := Facts{
		Node: "node-b", Arch: "amd64", OS: "linux", Release: "test",
		Version: "v1", Driver: "mock", StartedAt: now,
	}
	capacity := host.NodeCapacity{
		Node: "node-b", TotalVCPUs: 8, TotalMemMB: 16384, BudgetMemMB: 12288,
	}
	grpc := &GRPCControl{
		node: "node-b", healthy: true, boxes: map[string]*host.Sandbox{"alpha": cloneSandbox(box)},
		snaps: map[string]*host.Snapshot{}, facts: facts, capacity: capacity,
	}
	ssh := &selectorStub{
		name: "node-b", online: true, boxes: []*host.Sandbox{cloneSandbox(box)},
		facts: facts, capacity: capacity,
	}
	metrics := fleetmetrics.New()
	selector, err := NewControlSelector(ControlTransportSSH, grpc, ssh)
	if err != nil {
		t.Fatal(err)
	}
	selector.setMetrics("node-b", metrics)
	var observed []ShadowInventoryReport
	selector.ConfigureShadowInventory(true, func(report ShadowInventoryReport) {
		observed = append(observed, report)
	})
	report := selector.CompareShadowInventory()
	if !report.Available || !report.Match || len(observed) != 1 {
		t.Fatalf("matching shadow report = %+v, observations=%d", report, len(observed))
	}
	if selector.choiceFor(ControlClassReadOnly) != ssh {
		t.Fatal("shadow comparison changed the authoritative transport")
	}

	ssh.boxes[0].State = vmm.StatePaused
	report = selector.CompareShadowInventory()
	if report.Match || report.SandboxDiffs != 1 {
		t.Fatalf("mismatching shadow report = %+v", report)
	}
	body := scrapeMetrics(t, metrics)
	for _, want := range []string{
		`sparkbox_fleet_control_shadow_inventory_total{node="node-b",outcome="match"} 1`,
		`sparkbox_fleet_control_shadow_inventory_total{node="node-b",outcome="mismatch"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shadow metrics missing %q", want)
		}
	}
	if strings.Contains(body, "alpha") {
		t.Fatal("shadow metrics leaked a sandbox label or value")
	}
}

func TestRoutedCanaryIsStableAndOnlyAffectsAuto(t *testing.T) {
	for _, percent := range []int{0, 5, 25, 100} {
		for i := 0; i < 100; i++ {
			name := "sandbox-" + strconv.Itoa(i)
			first := RoutedCanarySelected("node-b", name, percent)
			if first != RoutedCanarySelected("node-b", name, percent) {
				t.Fatalf("canary assignment changed for percent %d", percent)
			}
		}
	}
	if RoutedCanarySelected("node-b", "alpha", 0) {
		t.Fatal("zero-percent canary selected routed")
	}
	if !RoutedCanarySelected("node-b", "alpha", 100) {
		t.Fatal("hundred-percent canary did not select routed")
	}

	health := &healthStub{
		healthy: true, capabilities: []string{nodelink.CapabilityRoutedGuestV1},
	}
	routed, ssh := &guestRecorder{}, &guestRecorder{}
	selector, err := NewGuestSelector(GuestTransportAuto, health, routed, ssh)
	if err != nil {
		t.Fatal(err)
	}
	selector.setMetrics("node-b", nil)
	if err := selector.ConfigureCanary(0); err != nil {
		t.Fatal(err)
	}
	if _, err := selector.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 80); err != nil {
		t.Fatal(err)
	}
	if routed.calls != 0 || ssh.calls != 1 {
		t.Fatalf("zero-percent auto calls routed=%d ssh=%d", routed.calls, ssh.calls)
	}
	if err := selector.Configure(GuestTransportRouted, health, routed); err != nil {
		t.Fatal(err)
	}
	if _, err := selector.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 80); err != nil {
		t.Fatal(err)
	}
	if routed.calls != 1 || ssh.calls != 1 {
		t.Fatal("explicit routed mode was incorrectly canary-gated")
	}
	if err := selector.ConfigureCanary(101); err == nil {
		t.Fatal("invalid routed canary percentage was accepted")
	}
}

func scrapeMetrics(t *testing.T, metrics *fleetmetrics.Registry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	return recorder.Body.String()
}
