package fleet

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/grpccontrol"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routedguest"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestGRPCControlReconcilesRevisionGap(t *testing.T) {
	client := newFakeDurable()
	client.setInventory(inventoryProto(1, sandboxProto("alpha", vmm.StateRunning)))
	control := newReadyGRPCControl(t, client)

	if box, ok := control.Box("alpha"); !ok || box.Node != "node-b" {
		t.Fatalf("initial authoritative cache = %+v, found=%v", box, ok)
	}
	watch := client.nextWatch(t)
	watch.events <- &nodev1.InventoryEvent{
		Revision: 2,
		Event: &nodev1.InventoryEvent_SandboxChanged{SandboxChanged: &nodev1.SandboxChanged{
			Sandbox: sandboxProto("alpha", vmm.StatePaused), Reason: "idle",
		}},
	}
	waitForGRPC(t, func() bool {
		box, ok := control.Box("alpha")
		return ok && box.State == vmm.StatePaused
	})

	// Retention gap: the cache must be replaced from GetInventory before a new
	// watch begins at the returned revision.
	client.setInventory(inventoryProto(9, sandboxProto("beta", vmm.StateRunning)))
	watch.events <- &nodev1.InventoryEvent{
		Revision: 8,
		Event: &nodev1.InventoryEvent_Gap{Gap: &nodev1.EventGap{
			OldestAvailableRevision: 7, CurrentRevision: 8,
		}},
	}
	second := client.nextWatch(t)
	if second.after != 9 {
		t.Fatalf("watch after gap resumed at %d, want reconciled revision 9", second.after)
	}
	if _, ok := control.Box("alpha"); ok {
		t.Fatal("full reconcile merged the old cache instead of replacing it")
	}
	if box, ok := control.Box("beta"); !ok || box.State != vmm.StateRunning {
		t.Fatalf("post-gap cache = %+v, found=%v", box, ok)
	}
}

func TestGRPCControlCancelsWatchAttemptBeforeReconnect(t *testing.T) {
	client := newFakeDurable()
	client.setInventory(inventoryProto(1, sandboxProto("alpha", vmm.StateRunning)))
	control, err := NewGRPCControl(context.Background(), GRPCControlOptions{
		Node: "node-b", Client: client, Retry: time.Millisecond,
		HealthEvery: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := control.WaitReady(ctx); err != nil {
		t.Fatalf("gRPC control did not become ready: %v", err)
	}

	first := client.nextWatch(t)
	client.setHealthError(status.Error(codes.Unavailable, "health transport down"))
	select {
	case <-first.done:
	case <-time.After(time.Second):
		t.Fatal("failed watch producer was not canceled and joined")
	}

	client.setHealthError(nil)
	second := client.nextWatch(t)
	select {
	case <-first.done:
	default:
		t.Fatal("replacement watch started before the previous producer exited")
	}
	if second == first {
		t.Fatal("reconnect reused the previous watch attempt")
	}
}

func TestGRPCControlCloseJoinsCallbacksAndSuppressesLaterEvents(t *testing.T) {
	client := newFakeDurable()
	client.setInventory(inventoryProto(1, sandboxProto("alpha", vmm.StateRunning)))
	entered := make(chan struct{})
	release := make(chan struct{})
	callbackCalls := 0
	control, err := NewGRPCControl(context.Background(), GRPCControlOptions{
		Node: "node-b", Client: client, Retry: time.Millisecond,
		HealthEvery: time.Hour,
		OnChanged: func(nodelink.ChangedMsg) {
			callbackCalls++
			close(entered)
			<-release
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := control.WaitReady(ctx); err != nil {
		t.Fatalf("gRPC control did not become ready: %v", err)
	}

	watch := client.nextWatch(t)
	watch.events <- &nodev1.InventoryEvent{
		Revision: 2,
		Event: &nodev1.InventoryEvent_SandboxChanged{SandboxChanged: &nodev1.SandboxChanged{
			Sandbox: sandboxProto("alpha", vmm.StatePaused), Reason: "idle",
		}},
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("authoritative callback did not start")
	}

	closed := make(chan error, 1)
	go func() { closed <- control.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before the admitted callback exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return after the callback exited")
	}

	err = control.applyEvent(&nodev1.InventoryEvent{
		Revision: 3,
		Event: &nodev1.InventoryEvent_SandboxChanged{SandboxChanged: &nodev1.SandboxChanged{
			Sandbox: sandboxProto("alpha", vmm.StateRunning),
		}},
	})
	if err == nil {
		t.Fatal("closed control accepted a later authoritative event")
	}
	if callbackCalls != 1 {
		t.Fatalf("callbacks after Close = %d, want one admitted callback total", callbackCalls)
	}
}

func TestGRPCControlKeepsRoutedAddressOutOfOrdinaryInventory(t *testing.T) {
	client := newFakeDurable()
	wire := sandboxProto("alpha", vmm.StateRunning)
	wire.HostIp = "10.44.0.2"
	client.setInventory(inventoryProto(1, wire))
	control := newReadyGRPCControl(t, client)

	ordinary, ok := control.Box("alpha")
	if !ok {
		t.Fatal("ordinary cache has no alpha")
	}
	if ordinary.HostIP == wire.HostIp {
		t.Fatal("ordinary Box exposed the node-reported routed address")
	}
	routed, ok := control.RoutedBox("alpha")
	if !ok || routed.HostIP != "10.44.0.2" || routed.SSHAddr != "10.44.0.2:22" {
		t.Fatalf("routed cache = %+v, found=%v", routed, ok)
	}

	watch := client.nextWatch(t)
	changed := sandboxProto("alpha", vmm.StateRunning)
	changed.HostIp = "10.44.0.6"
	watch.events <- &nodev1.InventoryEvent{
		Revision: 2,
		Event: &nodev1.InventoryEvent_SandboxChanged{SandboxChanged: &nodev1.SandboxChanged{
			Sandbox: changed,
		}},
	}
	waitForGRPC(t, func() bool {
		box, found := control.RoutedBox("alpha")
		return found && box.HostIP == "10.44.0.6"
	})
	if ordinary, _ := control.Box("alpha"); ordinary.HostIP == "10.44.0.6" {
		t.Fatal("routed event address leaked into ordinary Box")
	}
}

func TestGRPCControlLifecycleUsesDurableIdentitiesAndResults(t *testing.T) {
	client := newFakeDurable()
	client.setInventory(inventoryProto(1, sandboxProto("alpha", vmm.StateRunning)))
	control := newReadyGRPCControl(t, client)
	ctx := WithMutationIdentity(context.Background(), MutationIdentity{
		OperationID: "operation-stable", IdempotencyKey: "key-stable",
		Initiator: "test-gateway", CreatedAt: time.Unix(100, 0),
	})

	created, err := control.Create(ctx, "demo", "alice", "ubuntu", 2, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "demo" || created.Owner != "alice" || created.Node != "node-b" {
		t.Fatalf("create result = %+v", created)
	}
	if _, err := control.Create(ctx, "demo", "alice", "ubuntu", 2, 2048); err != nil {
		t.Fatalf("restart retry with the same durable identity: %v", err)
	}
	if _, err := control.EnsureReady(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := control.Pause(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := control.Archive(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := control.Resize(context.Background(), "demo", 4096); err != nil {
		t.Fatal(err)
	}
	if err := control.Reboot(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := control.SetPinned(context.Background(), "demo", true); err != nil {
		t.Fatal(err)
	}
	if err := control.ResyncEnv(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := control.Rename(context.Background(), "demo", "renamed", "alice"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := control.Snapshotter(context.Background(), "renamed", "base", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Name != "base" || snapshot.FromBox != "renamed" || snapshot.Node != "node-b" {
		t.Fatalf("snapshot result = %+v", snapshot)
	}
	forked, err := control.Fork(context.Background(), "base", "forked", "alice", 1, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if forked.Name != "forked" || forked.Owner != "alice" {
		t.Fatalf("fork result = %+v", forked)
	}
	if err := control.DeleteSnapshot(context.Background(), "base", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := control.NetPolicy(context.Background(), map[string][]string{
		"renamed": {"example.com", "10.0.0.0/8"},
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := control.NetUsage(context.Background())
	if err != nil || usage["renamed"].RxBytes != 11 {
		t.Fatalf("network usage = %+v, err=%v", usage, err)
	}
	if err := control.MarkActive(context.Background(), "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := control.RecordKey(context.Background(), "renamed", "SHA256:test"); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Vitals(context.Background(), "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := control.Destroy(context.Background(), "renamed"); err != nil {
		t.Fatal(err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.identities) < 15 {
		t.Fatalf("recorded %d mutation identities, want every lifecycle call", len(client.identities))
	}
	for _, identity := range client.identities[:2] {
		if identity.GetOperationId() != "operation-stable" ||
			identity.GetIdempotencyKey() != "key-stable" ||
			identity.GetInitiator() != "test-gateway" {
			t.Fatalf("mutation identity changed across restart-safe retries: %+v", identity)
		}
	}
	seen := map[string]bool{"operation-stable": true}
	for _, identity := range client.identities[2:] {
		if identity.GetOperationId() == "" || seen[identity.GetOperationId()] {
			t.Fatalf("default operation identity was empty or reused: %+v", identity)
		}
		seen[identity.GetOperationId()] = true
	}
}

func TestControlSelectorAutoFallbackAndExplicitGRPCFailClosed(t *testing.T) {
	client := newFakeDurable()
	client.setInventory(inventoryProto(1, sandboxProto("alpha", vmm.StateRunning)))
	control := newReadyGRPCControl(t, client)
	ssh := &selectorStub{name: "node-b", online: true}
	selector, err := NewControlSelector(ControlTransportAuto, control, ssh)
	if err != nil {
		t.Fatal(err)
	}

	client.pauseErr = status.Error(codes.Unavailable, "transport down")
	if err := selector.Pause(context.Background(), "alpha"); err == nil {
		t.Fatal("uncertain gRPC mutation silently fell back and risked double execution")
	}
	if err := selector.Pause(context.Background(), "alpha"); err != nil {
		t.Fatalf("next auto operation did not fall back to SSH: %v", err)
	}
	if ssh.pauseCalls != 1 {
		t.Fatalf("SSH pause calls = %d, want exactly the post-failure fallback", ssh.pauseCalls)
	}

	if err := selector.Configure(ControlTransportGRPC, control); err != nil {
		t.Fatal(err)
	}
	if err := selector.Pause(context.Background(), "alpha"); err == nil {
		t.Fatal("explicit gRPC mode fell back to SSH")
	}
	if ssh.pauseCalls != 1 {
		t.Fatal("explicit gRPC mode invoked SSH")
	}

	guestErr := errors.New("independent SSH guest dialer")
	node := ComposeNode(selector, guestStub{err: guestErr})
	if _, err := node.DialGuest(context.Background(), "alpha", "tcp", 80); !errors.Is(err, guestErr) {
		t.Fatalf("control selection changed the guest dialer: %v", err)
	}
}

func TestGuestSelectorAutoUsesOnlyTypedRouteFallback(t *testing.T) {
	health := &healthStub{
		healthy:      false,
		capabilities: []string{nodelink.CapabilityRoutedGuestV1},
	}
	routeFailure := &guestRecorder{err: &routedguest.RouteError{
		Kind: routedguest.KindTCP, Err: syscall.EHOSTUNREACH,
	}}
	ssh := &guestRecorder{}
	selector, err := NewGuestSelector(GuestTransportAuto, health, routeFailure, ssh)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := selector.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 80); err != nil {
		t.Fatal(err)
	}
	if routeFailure.calls != 0 || ssh.calls != 1 {
		t.Fatalf("unhealthy auto calls routed=%d ssh=%d, want 0/1", routeFailure.calls, ssh.calls)
	}

	health.healthy = true
	if _, err := selector.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 80); err != nil {
		t.Fatal(err)
	}
	if routeFailure.calls != 1 || ssh.calls != 2 {
		t.Fatalf("typed route failure calls routed=%d ssh=%d, want 1/2", routeFailure.calls, ssh.calls)
	}

	validation := &guestRecorder{err: &routedguest.ValidationError{
		Sandbox: "alpha", Err: routedguest.ErrOutOfPrefix,
	}}
	if err := selector.Configure(GuestTransportAuto, health, validation); err != nil {
		t.Fatal(err)
	}
	if _, err := selector.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 80); !errors.Is(err, routedguest.ErrOutOfPrefix) {
		t.Fatalf("validation error = %v", err)
	}
	if ssh.calls != 2 {
		t.Fatal("auto fell back around a routed address validation failure")
	}

	health.healthy = false
	if err := selector.Configure(GuestTransportRouted, health, routeFailure); err != nil {
		t.Fatal(err)
	}
	if _, err := selector.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 80); err == nil {
		t.Fatal("explicit routed mode used a stale cache or fell back while gRPC was unhealthy")
	}
	if ssh.calls != 2 {
		t.Fatal("explicit routed mode fell back to SSH")
	}
	if err := selector.Configure(GuestTransportRouted, health, nil); err == nil {
		t.Fatal("explicit routed mode accepted no routed dialer")
	}
}

func TestGuestSelectorAutoFallsBackAcrossHealthTransition(t *testing.T) {
	health := &healthStub{
		healthy:      true,
		capabilities: []string{nodelink.CapabilityRoutedGuestV1},
	}
	routed := guestDialerFunc(func(context.Context, string, string, int) (net.Conn, error) {
		health.healthy = false
		return nil, nodelink.ErrUnknownSandbox
	})
	ssh := &guestRecorder{}
	selector, err := NewGuestSelector(GuestTransportAuto, health, routed, ssh)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := selector.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 80); err != nil {
		t.Fatalf("health-transition fallback failed: %v", err)
	}
	if ssh.calls != 1 {
		t.Fatalf("SSH fallback calls = %d, want 1", ssh.calls)
	}
}

func TestGuestSelectorRequiresRoutedCapability(t *testing.T) {
	health := &healthStub{healthy: true}
	routed := &guestRecorder{}
	ssh := &guestRecorder{}
	selector, err := NewGuestSelector(GuestTransportAuto, health, routed, ssh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selector.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 80); err != nil {
		t.Fatal(err)
	}
	if routed.calls != 0 || ssh.calls != 1 {
		t.Fatalf("auto without routed_guest_v1 called routed=%d ssh=%d, want 0/1", routed.calls, ssh.calls)
	}
	if err := selector.Configure(GuestTransportRouted, health, routed); err != nil {
		t.Fatal(err)
	}
	if _, err := selector.DialGuest(context.Background(), "alpha", nodelink.StreamTCP, 80); err == nil {
		t.Fatal("explicit routed mode ignored the missing routed_guest_v1 capability")
	}
	if routed.calls != 0 || ssh.calls != 1 {
		t.Fatal("explicit routed mode fell back or dialed without the capability")
	}
}

func TestSSHLinkSupersessionLeavesSharedGRPCControlRunning(t *testing.T) {
	client := newFakeDurable()
	client.setInventory(inventoryProto(1))
	control := newReadyGRPCControl(t, client)
	oldSSH := &selectorStub{name: "node-b", online: true}
	newSSH := &selectorStub{name: "node-b", online: true}
	oldSelector, err := NewControlSelector(ControlTransportAuto, control, oldSSH)
	if err != nil {
		t.Fatal(err)
	}
	newSelector, err := NewControlSelector(ControlTransportSSH, nil, newSSH)
	if err != nil {
		t.Fatal(err)
	}
	oldGuest, err := NewGuestSelector(GuestTransportSSH, nil, nil, guestStub{})
	if err != nil {
		t.Fatal(err)
	}
	newGuest, err := NewGuestSelector(GuestTransportSSH, nil, nil, guestStub{})
	if err != nil {
		t.Fatal(err)
	}
	old := &linkedRemote{
		Node: ComposeNode(oldSelector, oldGuest), selector: oldSelector,
		guestSelector: oldGuest, ssh: oldSSH, sshGuest: oldGuest.ssh,
	}
	replacement := &linkedRemote{
		Node: ComposeNode(newSelector, newGuest), selector: newSelector,
		guestSelector: newGuest, ssh: newSSH, sshGuest: newGuest.ssh,
	}
	f := &Fleet{
		log:   slog.New(slog.DiscardHandler),
		nodes: map[string]Node{"node-b": old},
		grpcControls: map[string]*grpcBinding{
			"node-b": {control: control, mode: ControlTransportAuto},
		},
		routedGuests: map[string]*routedGuestBinding{},
	}

	detach := f.linkUp(replacement)
	defer detach()
	if oldSSH.hangupCalls != 1 || oldSSH.hangupCode != nodelink.CodeSuperseded {
		t.Fatalf("old SSH hangup = %d/%q, want 1/%q",
			oldSSH.hangupCalls, oldSSH.hangupCode, nodelink.CodeSuperseded)
	}
	if !control.Healthy() {
		t.Fatal("SSH supersession closed the shared gRPC control")
	}
	client.mu.Lock()
	closedClients := client.closed
	client.mu.Unlock()
	if closedClients != 0 {
		t.Fatalf("SSH supersession closed the durable client %d time(s)", closedClients)
	}
	if replacement.selector.choice() != control {
		t.Fatal("replacement SSH link did not reuse the shared gRPC control")
	}
}

func TestEvictNodeClosesGRPCWithoutAnSSHLink(t *testing.T) {
	client := newFakeDurable()
	client.setInventory(inventoryProto(1))
	control := newReadyGRPCControl(t, client)
	f := &Fleet{
		log: slog.New(slog.DiscardHandler), nodes: map[string]Node{},
		grpcControls: map[string]*grpcBinding{
			"node-b": {control: control, mode: ControlTransportGRPC},
		},
		routedGuests: map[string]*routedGuestBinding{},
	}

	if !f.EvictNode("node-b", "approval removed") {
		t.Fatal("EvictNode reported no transport for a live gRPC watcher")
	}
	if control.Healthy() {
		t.Fatal("evicted gRPC control remained healthy")
	}
	if _, exists := f.grpcControls["node-b"]; exists {
		t.Fatal("evicted gRPC binding remained installed")
	}
	client.mu.Lock()
	closed := client.closed
	client.mu.Unlock()
	if closed == 0 {
		t.Fatal("eviction did not close the gRPC client and watcher")
	}
	if f.EvictNode("node-b", "again") {
		t.Fatal("second eviction reported a transport that was already removed")
	}
}

func newReadyGRPCControl(t *testing.T, client *fakeDurable) *GRPCControl {
	t.Helper()
	control, err := NewGRPCControl(context.Background(), GRPCControlOptions{
		Node: "node-b", Client: client, Retry: time.Millisecond,
		HealthEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := control.WaitReady(ctx); err != nil {
		t.Fatalf("gRPC control did not become ready: %v", err)
	}
	return control
}

func waitForGRPC(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for gRPC control state")
}

func inventoryProto(revision uint64, boxes ...*nodev1.Sandbox) *nodev1.Inventory {
	return &nodev1.Inventory{Revision: revision, Sandboxes: boxes}
}

func sandboxProto(name string, state vmm.State) *nodev1.Sandbox {
	wireState := nodev1.SandboxState_SANDBOX_STATE_RUNNING
	if state == vmm.StatePaused {
		wireState = nodev1.SandboxState_SANDBOX_STATE_PAUSED
	} else if state == vmm.StateArchived {
		wireState = nodev1.SandboxState_SANDBOX_STATE_ARCHIVED
	}
	return &nodev1.Sandbox{
		Name: name, Owner: "alice", Image: "ubuntu", Vcpus: 2, MemoryMb: 2048,
		State: wireState, CreatedAt: timestamppb.Now(), LastActive: timestamppb.Now(),
	}
}

type fakeWatch struct {
	after  uint64
	events chan *nodev1.InventoryEvent
	errs   chan error
	done   chan struct{}
}

type fakeDurable struct {
	DurableControlClient

	mu         sync.Mutex
	inventory  *nodev1.Inventory
	identities []*nodev1.OperationIdentity
	watches    chan *fakeWatch
	pauseErr   error
	healthErr  error
	closed     int
}

func newFakeDurable() *fakeDurable {
	return &fakeDurable{watches: make(chan *fakeWatch, 8)}
}

func (f *fakeDurable) setInventory(inventory *nodev1.Inventory) {
	f.mu.Lock()
	f.inventory = proto.Clone(inventory).(*nodev1.Inventory)
	f.mu.Unlock()
}

func (f *fakeDurable) Inventory(context.Context) (*nodev1.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return proto.Clone(f.inventory).(*nodev1.Inventory), nil
}

func (f *fakeDurable) Capacity(context.Context) (*nodev1.Capacity, error) {
	return &nodev1.Capacity{
		Architecture: "amd64", OperatingSystem: "linux", Release: "test",
		Driver: "mock", HostVcpus: 8, TotalMemoryMb: 16384, BudgetMemoryMb: 12288,
	}, nil
}

func (f *fakeDurable) Health(context.Context) (*nodev1.HealthResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return &nodev1.HealthResponse{
		Status: nodev1.HealthStatus_HEALTH_STATUS_SERVING,
		Node:   "node-b", Version: "test", StartedAt: timestamppb.Now(),
		Capabilities: []string{nodelink.CapabilityRoutedGuestV1},
	}, nil
}

func (f *fakeDurable) setHealthError(err error) {
	f.mu.Lock()
	f.healthErr = err
	f.mu.Unlock()
}

func (f *fakeDurable) WatchEvents(ctx context.Context, after uint64) (<-chan *nodev1.InventoryEvent, <-chan error) {
	watch := &fakeWatch{
		after: after, events: make(chan *nodev1.InventoryEvent, 8), errs: make(chan error, 1),
		done: make(chan struct{}),
	}
	out := make(chan *nodev1.InventoryEvent, 8)
	outErrs := make(chan error, 1)
	f.watches <- watch
	go func() {
		defer close(watch.done)
		defer close(out)
		defer close(outErrs)
		events, errs := watch.events, watch.errs
		for events != nil || errs != nil {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			case err, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				select {
				case outErrs <- err:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, outErrs
}

func (f *fakeDurable) nextWatch(t *testing.T) *fakeWatch {
	t.Helper()
	select {
	case watch := <-f.watches:
		return watch
	case <-time.After(time.Second):
		t.Fatal("gRPC control did not start an event watch")
		return nil
	}
}

func (f *fakeDurable) Close() error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}

func (f *fakeDurable) mutation(request proto.Message, identity *nodev1.OperationIdentity, result *nodev1.OperationResult) (*nodev1.Operation, error) {
	expected, err := grpccontrol.RequestHash(request)
	if err != nil {
		return nil, err
	}
	if identity == nil || !bytes.Equal(identity.GetRequestHash(), expected) {
		return nil, errors.New("request did not carry its canonical durable identity")
	}
	f.mu.Lock()
	f.identities = append(f.identities, proto.Clone(identity).(*nodev1.OperationIdentity))
	f.mu.Unlock()
	return &nodev1.Operation{
		OperationId: identity.GetOperationId(),
		State:       nodev1.OperationState_OPERATION_STATE_SUCCEEDED,
		Sequence:    2, Result: result,
	}, nil
}

func sandboxOperationResult(box *nodev1.Sandbox) *nodev1.OperationResult {
	return &nodev1.OperationResult{Result: &nodev1.OperationResult_Sandbox{Sandbox: box}}
}
func emptyOperationResult() *nodev1.OperationResult {
	return &nodev1.OperationResult{Result: &nodev1.OperationResult_Empty{Empty: &emptypb.Empty{}}}
}

func (f *fakeDurable) Create(_ context.Context, request *nodev1.CreateRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), sandboxOperationResult(sandboxProto(request.GetName(), vmm.StateRunning)))
}
func (f *fakeDurable) EnsureRunning(_ context.Context, request *nodev1.EnsureRunningRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), sandboxOperationResult(sandboxProto(request.GetSandbox(), vmm.StateRunning)))
}
func (f *fakeDurable) Pause(_ context.Context, request *nodev1.PauseRequest) (*nodev1.Operation, error) {
	if f.pauseErr != nil {
		return nil, f.pauseErr
	}
	return f.mutation(request, request.GetOperation(), sandboxOperationResult(sandboxProto(request.GetSandbox(), vmm.StatePaused)))
}
func (f *fakeDurable) Archive(_ context.Context, request *nodev1.ArchiveRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), sandboxOperationResult(sandboxProto(request.GetSandbox(), vmm.StateArchived)))
}
func (f *fakeDurable) Resize(_ context.Context, request *nodev1.ResizeRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), sandboxOperationResult(sandboxProto(request.GetSandbox(), vmm.StateRunning)))
}
func (f *fakeDurable) Reboot(_ context.Context, request *nodev1.RebootRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), sandboxOperationResult(sandboxProto(request.GetSandbox(), vmm.StateRunning)))
}
func (f *fakeDurable) Rename(_ context.Context, request *nodev1.RenameRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), sandboxOperationResult(sandboxProto(request.GetNewName(), vmm.StateRunning)))
}
func (f *fakeDurable) SetPinned(_ context.Context, request *nodev1.SetPinnedRequest) (*nodev1.Operation, error) {
	box := sandboxProto(request.GetSandbox(), vmm.StateRunning)
	box.Pinned = request.GetPinned()
	return f.mutation(request, request.GetOperation(), sandboxOperationResult(box))
}
func (f *fakeDurable) ResyncEnvironment(_ context.Context, request *nodev1.ResyncEnvironmentRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), emptyOperationResult())
}
func (f *fakeDurable) Snapshot(_ context.Context, request *nodev1.SnapshotRequest) (*nodev1.Operation, error) {
	result := &nodev1.OperationResult{Result: &nodev1.OperationResult_Snapshot{Snapshot: &nodev1.Snapshot{
		Name: request.GetSnapshot(), Owner: request.GetOwner(), FromSandbox: request.GetSandbox(),
		CreatedAt: timestamppb.Now(),
	}}}
	return f.mutation(request, request.GetOperation(), result)
}
func (f *fakeDurable) Fork(_ context.Context, request *nodev1.ForkRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), sandboxOperationResult(sandboxProto(request.GetName(), vmm.StateRunning)))
}
func (f *fakeDurable) Destroy(_ context.Context, request *nodev1.DestroyRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), emptyOperationResult())
}
func (f *fakeDurable) DeleteSnapshot(_ context.Context, request *nodev1.DeleteSnapshotRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), emptyOperationResult())
}
func (f *fakeDurable) ApplyNetworkPolicy(_ context.Context, request *nodev1.ApplyNetworkPolicyRequest) (*nodev1.Operation, error) {
	return f.mutation(request, request.GetOperation(), emptyOperationResult())
}
func (f *fakeDurable) NetworkUsage(context.Context) (*nodev1.GetNetworkUsageResponse, error) {
	return &nodev1.GetNetworkUsageResponse{Usage: []*nodev1.NetworkUsage{{
		Sandbox: "renamed", RxBytes: 11, TxBytes: 22,
	}}}, nil
}
func (f *fakeDurable) MarkActive(context.Context, *nodev1.MarkActiveRequest) error { return nil }
func (f *fakeDurable) RecordKey(context.Context, *nodev1.RecordKeyRequest) error   { return nil }
func (f *fakeDurable) Vitals(context.Context, string) (*nodev1.Vitals, error) {
	return &nodev1.Vitals{CpuSeconds: 1.5, MemoryUsedMb: 256}, nil
}

type selectorStub struct {
	ControlPlane
	name        string
	online      bool
	hangupCalls int
	hangupCode  string
	pauseCalls  int
	ensureCalls int
	boxes       []*host.Sandbox
	snapshots   []*host.Snapshot
	facts       Facts
	capacity    host.NodeCapacity
}

type guestStub struct{ err error }

func (g guestStub) DialGuest(context.Context, string, string, int) (net.Conn, error) {
	return nil, g.err
}

type guestDialerFunc func(context.Context, string, string, int) (net.Conn, error)

func (f guestDialerFunc) DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error) {
	return f(ctx, sandbox, kind, port)
}

type healthStub struct {
	healthy      bool
	capabilities []string
}

func (h *healthStub) Healthy() bool { return h.healthy }
func (h *healthStub) Facts() Facts {
	return Facts{Capabilities: append([]string(nil), h.capabilities...)}
}

type guestRecorder struct {
	calls int
	err   error
}

func (g *guestRecorder) DialGuest(context.Context, string, string, int) (net.Conn, error) {
	g.calls++
	return nil, g.err
}

func (s *selectorStub) Name() string { return s.name }
func (s *selectorStub) Online() bool { return s.online }
func (s *selectorStub) Hangup(code, _ string) {
	s.hangupCalls++
	s.hangupCode = code
}
func (s *selectorStub) Pause(context.Context, string) error {
	s.pauseCalls++
	return nil
}
func (s *selectorStub) Facts() Facts {
	if s.facts.Node == "" {
		s.facts.Node = s.name
	}
	return s.facts
}
func (s *selectorStub) LastSeen() time.Time { return time.Now() }
func (s *selectorStub) Box(name string) (*host.Sandbox, bool) {
	for _, box := range s.boxes {
		if box != nil && box.Name == name {
			return cloneSandbox(box), true
		}
	}
	return nil, false
}
func (s *selectorStub) Boxes() []*host.Sandbox {
	out := make([]*host.Sandbox, 0, len(s.boxes))
	for _, box := range s.boxes {
		out = append(out, cloneSandbox(box))
	}
	return out
}
func (s *selectorStub) Templates() []*host.Snapshot {
	out := make([]*host.Snapshot, 0, len(s.snapshots))
	for _, snapshot := range s.snapshots {
		if snapshot != nil {
			copy := *snapshot
			out = append(out, &copy)
		}
	}
	return out
}
func (s *selectorStub) Capacity() host.NodeCapacity {
	if s.capacity.Node != "" || s.capacity.TotalVCPUs != 0 {
		return s.capacity
	}
	return host.NodeCapacity{Node: s.name}
}
func (s *selectorStub) EnsureReady(context.Context, string) (*host.Sandbox, error) {
	s.ensureCalls++
	return &host.Sandbox{Name: "alpha", Node: s.name}, nil
}
func (s *selectorStub) NetUsage(context.Context) (map[string]netpush.VMUsage, error) {
	return nil, nil
}
