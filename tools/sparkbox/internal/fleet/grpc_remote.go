package fleet

// The gateway-side gRPC control plane.
//
// Guest streams deliberately do not appear in this file. A GRPCControl is
// composed with the existing SSH GuestDialer by ControlSelector, so moving
// lifecycle and inventory traffic cannot accidentally move or weaken the data
// path at the same time.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/grpccontrol"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/opidentity"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/reserved"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultGRPCHealthEvery = 15 * time.Second
	defaultGRPCRetry       = time.Second
	maxGRPCSnapshots       = 4096
	routedSSHPort          = "22"
)

var (
	errGRPCEventGap   = errors.New("fleet: gRPC inventory event gap")
	errGRPCWatchEnd   = errors.New("fleet: gRPC inventory watch ended")
	errGRPCNotServing = errors.New("fleet: gRPC health is not serving")
)

// DurableControlClient is the grpccontrol.Client surface GRPCControl consumes.
// Naming the seam here keeps the adapter independently testable while the
// concrete authenticated client remains the production implementation.
type DurableControlClient interface {
	Close() error
	Inventory(context.Context) (*nodev1.Inventory, error)
	Capacity(context.Context) (*nodev1.Capacity, error)
	Health(context.Context) (*nodev1.HealthResponse, error)
	Vitals(context.Context, string) (*nodev1.Vitals, error)
	WatchEvents(context.Context, uint64) (<-chan *nodev1.InventoryEvent, <-chan error)

	EnsureRunning(context.Context, *nodev1.EnsureRunningRequest) (*nodev1.Operation, error)
	Create(context.Context, *nodev1.CreateRequest) (*nodev1.Operation, error)
	Pause(context.Context, *nodev1.PauseRequest) (*nodev1.Operation, error)
	Archive(context.Context, *nodev1.ArchiveRequest) (*nodev1.Operation, error)
	Resize(context.Context, *nodev1.ResizeRequest) (*nodev1.Operation, error)
	Reboot(context.Context, *nodev1.RebootRequest) (*nodev1.Operation, error)
	SetTurbo(context.Context, *nodev1.SetTurboRequest) (*nodev1.Operation, error)
	Rename(context.Context, *nodev1.RenameRequest) (*nodev1.Operation, error)
	Destroy(context.Context, *nodev1.DestroyRequest) (*nodev1.Operation, error)
	SetPinned(context.Context, *nodev1.SetPinnedRequest) (*nodev1.Operation, error)
	ResyncEnvironment(context.Context, *nodev1.ResyncEnvironmentRequest) (*nodev1.Operation, error)
	Snapshot(context.Context, *nodev1.SnapshotRequest) (*nodev1.Operation, error)
	DeleteSnapshot(context.Context, *nodev1.DeleteSnapshotRequest) (*nodev1.Operation, error)
	Fork(context.Context, *nodev1.ForkRequest) (*nodev1.Operation, error)
	ApplyNetworkPolicy(context.Context, *nodev1.ApplyNetworkPolicyRequest) (*nodev1.Operation, error)
	MarkActive(context.Context, *nodev1.MarkActiveRequest) error
	RecordKey(context.Context, *nodev1.RecordKeyRequest) error
	NetworkUsage(context.Context) (*nodev1.GetNetworkUsageResponse, error)
}

var _ DurableControlClient = (*grpccontrol.Client)(nil)

// GRPCControlOptions configures one authenticated remote control plane.
type GRPCControlOptions struct {
	Node    string
	Client  DurableControlClient
	Facts   Facts
	Metrics *fleetmetrics.Registry
	Log     *slog.Logger

	Initiator   string
	HealthEvery time.Duration
	Retry       time.Duration
	Now         func() time.Time

	// Fleet integration callbacks. They are optional so the adapter remains
	// useful in isolation; Fleet.InstallGRPCControl supplies all four.
	OnInventory func(nodelink.InventoryMsg)
	OnChanged   func(nodelink.ChangedMsg)
	OnGone      func(nodelink.GoneMsg)
	OnPaused    func(nodelink.PausedMsg)
}

// MutationIdentity lets an API layer preserve a durable identity across a
// gateway restart. Calls without one receive a fresh cryptographically random
// identity.
type MutationIdentity = opidentity.Identity

func WithMutationIdentity(ctx context.Context, identity MutationIdentity) context.Context {
	return opidentity.WithContext(ctx, identity)
}

// GRPCControl is an authoritative, revisioned cache plus the full typed
// lifecycle client for one node.
type GRPCControl struct {
	node        string
	client      DurableControlClient
	metrics     *fleetmetrics.Registry
	log         *slog.Logger
	initiator   string
	now         func() time.Time
	healthEvery time.Duration
	retry       time.Duration

	onInventory func(nodelink.InventoryMsg)
	onChanged   func(nodelink.ChangedMsg)
	onGone      func(nodelink.GoneMsg)
	onPaused    func(nodelink.PausedMsg)

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	callbackMu sync.Mutex
	mu         sync.RWMutex
	facts      Facts
	capacity   host.NodeCapacity
	boxes      map[string]*host.Sandbox
	// routedBoxes is the only cache that retains a node-reported HostIP. It is
	// consumed by a prefix-confined routedguest.Dialer and is never returned
	// from Box or Boxes.
	routedBoxes map[string]*host.Sandbox
	snaps       map[string]*host.Snapshot
	revision    uint64
	lastSeen    time.Time
	healthy     bool
	dead        error

	ready     chan struct{}
	readyOnce sync.Once
}

var _ ControlPlane = (*GRPCControl)(nil)

func NewGRPCControl(ctx context.Context, opts GRPCControlOptions) (*GRPCControl, error) {
	if opts.Node == "" {
		return nil, errors.New("fleet: gRPC control needs an authenticated node name")
	}
	if opts.Client == nil {
		return nil, errors.New("fleet: gRPC control needs a client")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	initiator := opts.Initiator
	if initiator == "" {
		initiator = "sparkbox-gateway"
	}
	runCtx, cancel := context.WithCancel(ctx)
	facts := opts.Facts
	facts.Node = opts.Node
	g := &GRPCControl{
		node: opts.Node, client: opts.Client, metrics: opts.Metrics,
		log:       log.With("node", opts.Node, "transport", "grpc"),
		initiator: initiator, now: now,
		healthEvery: orDuration(opts.HealthEvery, defaultGRPCHealthEvery),
		retry:       orDuration(opts.Retry, defaultGRPCRetry),
		onInventory: opts.OnInventory, onChanged: opts.OnChanged,
		onGone: opts.OnGone, onPaused: opts.OnPaused,
		ctx: runCtx, cancel: cancel, facts: facts,
		boxes: map[string]*host.Sandbox{}, routedBoxes: map[string]*host.Sandbox{},
		snaps: map[string]*host.Snapshot{},
		ready: make(chan struct{}),
	}
	go g.supervise()
	return g, nil
}

// WaitReady waits for the first complete inventory/capacity/health sync.
func (g *GRPCControl) WaitReady(ctx context.Context) error {
	select {
	case <-g.ready:
		if g.Healthy() {
			return nil
		}
		g.mu.RLock()
		err := g.dead
		g.mu.RUnlock()
		if err != nil {
			return err
		}
		return Unreachable("grpc.health", "", g.node)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *GRPCControl) Healthy() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.healthy && g.dead == nil
}

func (g *GRPCControl) Name() string { return g.node }

func (g *GRPCControl) Facts() Facts {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := g.facts
	out.Capabilities = append([]string(nil), out.Capabilities...)
	return out
}

func (g *GRPCControl) Online() bool { return g.Healthy() }

func (g *GRPCControl) LastSeen() time.Time {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.lastSeen
}

func (g *GRPCControl) Capacity() host.NodeCapacity {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := g.capacity
	out.Node = g.node
	out.Online = g.healthy && g.dead == nil
	if !g.lastSeen.IsZero() {
		seen := g.lastSeen
		out.LastSeenAt = &seen
	}
	return out
}

func (g *GRPCControl) Box(name string) (*host.Sandbox, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	box, ok := g.boxes[name]
	return cloneSandbox(box), ok
}

// RoutedBox returns the authoritative gRPC record with its node-reported
// HostIP. Callers must confine that address to the roster-approved prefix.
// Ordinary Box and Boxes deliberately never expose this record.
func (g *GRPCControl) RoutedBox(name string) (*host.Sandbox, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.healthy || g.dead != nil {
		return nil, false
	}
	capable := false
	for _, capability := range g.facts.Capabilities {
		if capability == nodelink.CapabilityRoutedGuestV1 {
			capable = true
			break
		}
	}
	if !capable {
		return nil, false
	}
	box, ok := g.routedBoxes[name]
	return cloneSandbox(box), ok
}

func (g *GRPCControl) Boxes() []*host.Sandbox {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]*host.Sandbox, 0, len(g.boxes))
	for _, box := range g.boxes {
		out = append(out, cloneSandbox(box))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (g *GRPCControl) Templates() []*host.Snapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]*host.Snapshot, 0, len(g.snaps))
	for _, snapshot := range g.snaps {
		copy := *snapshot
		out = append(out, &copy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func cloneSandbox(box *host.Sandbox) *host.Sandbox {
	if box == nil {
		return nil
	}
	out := *box
	return &out
}

func (g *GRPCControl) supervise() {
	hadGeneration := false
	for g.ctx.Err() == nil {
		if err := g.reconcile(g.ctx); err != nil {
			g.markUnhealthy(err)
			if !sleepContext(g.ctx, g.retry) {
				return
			}
			continue
		}
		if hadGeneration {
			g.metrics.IncReconnect(g.node, "grpc")
		}
		hadGeneration = true
		g.readyOnce.Do(func() { close(g.ready) })
		err := g.watch()
		g.metrics.IncDisconnect(g.node, "grpc", grpcDisconnectReason(err))
		if err != nil && g.ctx.Err() == nil {
			g.markUnhealthy(err)
			if !sleepContext(g.ctx, g.retry) {
				return
			}
		}
	}
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (g *GRPCControl) reconcile(ctx context.Context) error {
	inventory, err := observeGRPC(g, ctx, "inventory", func(ctx context.Context) (*nodev1.Inventory, error) {
		return g.client.Inventory(ctx)
	})
	if err != nil {
		return err
	}
	if inventory == nil || len(inventory.GetSandboxes()) > nodelink.MaxSandboxesPerNode ||
		len(inventory.GetSnapshots()) > maxGRPCSnapshots {
		return errors.New("fleet: gRPC inventory exceeds the gateway cache bounds")
	}
	capacity, err := observeGRPC(g, ctx, "capacity", func(ctx context.Context) (*nodev1.Capacity, error) {
		return g.client.Capacity(ctx)
	})
	if err != nil {
		return err
	}
	health, err := g.probeHealth(ctx)
	if err != nil {
		return err
	}

	boxes := make(map[string]*host.Sandbox, len(inventory.GetSandboxes()))
	routedBoxes := make(map[string]*host.Sandbox, len(inventory.GetSandboxes()))
	rows := make([]nodelink.SandboxRow, 0, len(inventory.GetSandboxes()))
	for _, wire := range inventory.GetSandboxes() {
		row := sandboxRowFromProto(wire)
		rows = append(rows, row)
		if box := grpcBox(g.node, row); box != nil {
			boxes[box.Name] = box
			routedBoxes[box.Name] = grpcRoutedBox(box, wire.GetHostIp())
		}
	}
	snaps := make(map[string]*host.Snapshot, len(inventory.GetSnapshots()))
	snapshotRows := make([]nodelink.SnapshotRow, 0, len(inventory.GetSnapshots()))
	for _, wire := range inventory.GetSnapshots() {
		row := snapshotRowFromProto(wire)
		snapshotRows = append(snapshotRows, row)
		if snapshot := grpcSnapshot(g.node, row); snapshot != nil {
			snaps[snapshot.Name] = snapshot
		}
	}
	hostCapacity := capacityFromProto(g.node, capacity)
	facts := factsFromProto(g.node, g.Facts(), health, capacity)
	now := g.now()
	g.mu.Lock()
	if err := g.stoppedLocked(); err != nil {
		g.mu.Unlock()
		return err
	}
	g.boxes, g.routedBoxes, g.snaps = boxes, routedBoxes, snaps
	g.revision = inventory.GetRevision()
	g.capacity, g.facts = hostCapacity, facts
	g.lastSeen, g.healthy = now, true
	g.mu.Unlock()
	if g.onInventory != nil {
		g.deliverCallback(func() {
			g.onInventory(nodelink.InventoryMsg{
				Node: g.node, Sandboxes: rows, Snapshots: snapshotRows,
				Capacity: hostCapacity, At: now,
			})
		})
	}
	return nil
}

func (g *GRPCControl) watch() (err error) {
	g.mu.RLock()
	after := g.revision
	g.mu.RUnlock()
	started := time.Now()
	g.metrics.AddPending(g.node, "grpc", 1)
	g.metrics.AddInFlight(g.node, "grpc", "watch_events", 1)
	watchCtx, cancelWatch := context.WithCancel(g.ctx)
	events, errs := g.client.WatchEvents(watchCtx, after)
	defer func() {
		cancelWatch()
		drainGRPCWatch(events, errs)
		g.metrics.AddInFlight(g.node, "grpc", "watch_events", -1)
		g.metrics.AddPending(g.node, "grpc", -1)
		g.metrics.ObserveControlRPC(g.node, "grpc", "watch_events", grpcMetricOutcome(err), time.Since(started))
	}()
	ticker := time.NewTicker(g.healthEvery)
	defer ticker.Stop()
	for events != nil || errs != nil {
		select {
		case <-g.ctx.Done():
			return g.ctx.Err()
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if event.GetGap() != nil {
				return errGRPCEventGap
			}
			if err := g.applyEvent(event); err != nil {
				return err
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				return err
			}
		case <-ticker.C:
			health, err := g.probeHealth(g.ctx)
			if err != nil {
				return err
			}
			capacity, err := observeGRPC(g, g.ctx, "capacity", func(ctx context.Context) (*nodev1.Capacity, error) {
				return g.client.Capacity(ctx)
			})
			if err != nil {
				return err
			}
			g.mu.Lock()
			if err := g.stoppedLocked(); err != nil {
				g.mu.Unlock()
				return err
			}
			g.capacity = capacityFromProto(g.node, capacity)
			g.facts = factsFromProto(g.node, g.facts, health, capacity)
			g.lastSeen, g.healthy = g.now(), g.dead == nil
			g.mu.Unlock()
		}
	}
	return errGRPCWatchEnd
}

// drainGRPCWatch joins the producer goroutine after its attempt-local context
// is canceled. DurableControlClient implementations must close both returned
// channels when that context is done.
func drainGRPCWatch(events <-chan *nodev1.InventoryEvent, errs <-chan error) {
	for events != nil || errs != nil {
		select {
		case _, ok := <-events:
			if !ok {
				events = nil
			}
		case _, ok := <-errs:
			if !ok {
				errs = nil
			}
		}
	}
}

func (g *GRPCControl) probeHealth(ctx context.Context) (*nodev1.HealthResponse, error) {
	health, err := observeGRPC(g, ctx, "health", func(ctx context.Context) (*nodev1.HealthResponse, error) {
		return g.client.Health(ctx)
	})
	if err == nil {
		switch {
		case health == nil:
			err = fmt.Errorf("%w: empty response", errGRPCNotServing)
		case health.GetNode() != "" && health.GetNode() != g.node:
			err = fmt.Errorf("%w: identity does not match authenticated node %q", errGRPCNotServing, g.node)
		case health.GetStatus() != nodev1.HealthStatus_HEALTH_STATUS_SERVING:
			err = fmt.Errorf("%w: node %q reported %s", errGRPCNotServing, g.node, health.GetStatus())
		}
	}
	if err != nil {
		g.metrics.IncLivenessFailure(g.node, "grpc", "gateway")
		return nil, err
	}
	return health, nil
}

func (g *GRPCControl) applyEvent(event *nodev1.InventoryEvent) error {
	if event == nil || event.GetRevision() == 0 {
		return errors.New("fleet: gRPC inventory event has no revision")
	}
	g.mu.Lock()
	if err := g.stoppedLocked(); err != nil {
		g.mu.Unlock()
		return err
	}
	if event.GetRevision() <= g.revision {
		g.mu.Unlock()
		return nil
	}
	if event.GetRevision() != g.revision+1 {
		g.mu.Unlock()
		return fmt.Errorf("fleet: gRPC inventory revision jumped from %d to %d", g.revision, event.GetRevision())
	}
	now := g.now()
	var (
		changed *nodelink.ChangedMsg
		gone    *nodelink.GoneMsg
		paused  *nodelink.PausedMsg
	)
	switch payload := event.GetEvent().(type) {
	case *nodev1.InventoryEvent_SandboxChanged:
		row := sandboxRowFromProto(payload.SandboxChanged.GetSandbox())
		box := grpcBox(g.node, row)
		if box == nil {
			g.mu.Unlock()
			return errors.New("fleet: gRPC sandbox event has an invalid name")
		}
		if _, exists := g.boxes[box.Name]; !exists && len(g.boxes) >= nodelink.MaxSandboxesPerNode {
			g.mu.Unlock()
			return errors.New("fleet: gRPC sandbox event exceeds the cache bound")
		}
		g.boxes[box.Name] = box
		g.routedBoxes[box.Name] = grpcRoutedBox(box, payload.SandboxChanged.GetSandbox().GetHostIp())
		msg := nodelink.ChangedMsg{
			Node: g.node, Sandbox: row,
			Reason: ctlops.SafeText(payload.SandboxChanged.GetReason(), nodelink.MaxReasonText),
			At:     now,
		}
		changed = &msg
		if box.State == vmm.StatePaused {
			reason := msg.Reason
			if reason == "" {
				reason = "was paused"
			}
			p := nodelink.PausedMsg{Node: g.node, Name: box.Name, Reason: reason}
			paused = &p
		}
	case *nodev1.InventoryEvent_SandboxGone:
		name := payload.SandboxGone.GetName()
		delete(g.boxes, name)
		delete(g.routedBoxes, name)
		msg := nodelink.GoneMsg{Node: g.node, Name: name, Reason: "gRPC inventory event"}
		gone = &msg
	case *nodev1.InventoryEvent_SnapshotChanged:
		row := snapshotRowFromProto(payload.SnapshotChanged.GetSnapshot())
		snapshot := grpcSnapshot(g.node, row)
		if snapshot == nil {
			g.mu.Unlock()
			return errors.New("fleet: gRPC snapshot event has an invalid name")
		}
		if _, exists := g.snaps[snapshot.Name]; !exists && len(g.snaps) >= maxGRPCSnapshots {
			g.mu.Unlock()
			return errors.New("fleet: gRPC snapshot event exceeds the cache bound")
		}
		g.snaps[snapshot.Name] = snapshot
	case *nodev1.InventoryEvent_SnapshotGone:
		delete(g.snaps, payload.SnapshotGone.GetName())
	default:
		g.mu.Unlock()
		return errors.New("fleet: gRPC inventory event has no payload")
	}
	g.revision = event.GetRevision()
	g.lastSeen, g.healthy = now, true
	g.mu.Unlock()
	if changed != nil || gone != nil || paused != nil {
		g.deliverCallback(func() {
			if changed != nil && g.onChanged != nil {
				g.onChanged(*changed)
			}
			if gone != nil && g.onGone != nil {
				g.onGone(*gone)
			}
			if paused != nil && g.onPaused != nil {
				g.onPaused(*paused)
			}
		})
	}
	return nil
}

func (g *GRPCControl) stoppedLocked() error {
	if g.dead != nil {
		return g.dead
	}
	return g.ctx.Err()
}

// deliverCallback serializes callback admission with Close and Revoke. Once
// either lifecycle method returns, every admitted callback has finished and no
// later inventory generation can enter a callback.
func (g *GRPCControl) deliverCallback(callback func()) {
	g.callbackMu.Lock()
	defer g.callbackMu.Unlock()
	g.mu.RLock()
	live := g.dead == nil && g.ctx.Err() == nil
	g.mu.RUnlock()
	if live {
		callback()
	}
}

func (g *GRPCControl) seen() {
	g.mu.Lock()
	g.lastSeen = g.now()
	g.mu.Unlock()
}

func (g *GRPCControl) markUnhealthy(err error) {
	g.mu.Lock()
	g.healthy = false
	g.mu.Unlock()
	if g.ctx.Err() == nil {
		g.log.Warn("gRPC control unavailable", "err", err)
	}
}

func observeGRPC[T any](g *GRPCControl, ctx context.Context, operation string, call func(context.Context) (T, error)) (out T, err error) {
	started := time.Now()
	g.metrics.AddPending(g.node, "grpc", 1)
	g.metrics.AddInFlight(g.node, "grpc", operation, 1)
	defer func() {
		g.metrics.AddInFlight(g.node, "grpc", operation, -1)
		g.metrics.AddPending(g.node, "grpc", -1)
		g.metrics.ObserveControlRPC(g.node, "grpc", operation, grpcMetricOutcome(err), time.Since(started))
	}()
	out, err = call(ctx)
	if err == nil {
		g.seen()
	} else if grpcTransportFailure(err) {
		g.markUnhealthy(err)
	}
	return out, err
}

func grpcDisconnectReason(err error) string {
	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, errGRPCWatchEnd):
		return "eof"
	case errors.Is(err, context.Canceled):
		return "shutdown"
	case errors.Is(err, context.DeadlineExceeded):
		return "liveness"
	case errors.Is(err, errGRPCEventGap):
		return "reconcile"
	case errors.Is(err, errGRPCNotServing):
		return "liveness"
	}
	switch status.Code(err) {
	case codes.Unavailable:
		return "transport"
	case codes.DeadlineExceeded:
		return "liveness"
	case codes.Canceled:
		return "shutdown"
	case codes.Internal, codes.Unknown, codes.DataLoss:
		return "protocol"
	default:
		return "error"
	}
}

func grpcTransportFailure(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.Unknown, codes.Internal:
		return true
	default:
		return false
	}
}

func grpcMetricOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var remote *grpccontrol.RemoteError
	if errors.As(err, &remote) {
		return "remote"
	}
	switch status.Code(err) {
	case codes.Unavailable:
		return "unavailable"
	case codes.DeadlineExceeded:
		return "timeout"
	default:
		return "error"
	}
}

func sandboxRowFromProto(wire *nodev1.Sandbox) nodelink.SandboxRow {
	if wire == nil {
		return nodelink.SandboxRow{}
	}
	return nodelink.SandboxRow{
		ID: wire.GetId(), Name: wire.GetName(), Owner: wire.GetOwner(), Image: wire.GetImage(),
		State: sandboxStateFromProto(wire.GetState()), VCPUs: wire.GetVcpus(),
		MemMB: wire.GetMemoryMb(), DiskMB: wire.GetDiskMb(),
		DiskTotalMB: wire.GetDiskTotalMb(), Pinned: wire.GetPinned(),
		Ballooned: wire.GetBallooned(), SSHUser: wire.GetSshUser(),
		KeyFP: wire.GetKeyFingerprint(), NetRxBytes: wire.GetNetworkRxBytes(),
		NetTxBytes: wire.GetNetworkTxBytes(), ArchivedAt: protoTime(wire.GetArchivedAt()),
		CreatedAt: protoTime(wire.GetCreatedAt()), LastActive: protoTime(wire.GetLastActive()),
		Turbo: wire.GetTurbo(),
	}
}

func snapshotRowFromProto(wire *nodev1.Snapshot) nodelink.SnapshotRow {
	if wire == nil {
		return nodelink.SnapshotRow{}
	}
	return nodelink.SnapshotRow{
		Name: wire.GetName(), Owner: wire.GetOwner(), Image: wire.GetImage(),
		FromBox: wire.GetFromSandbox(), CreatedAt: protoTime(wire.GetCreatedAt()),
	}
}

func sandboxStateFromProto(state nodev1.SandboxState) string {
	switch state {
	case nodev1.SandboxState_SANDBOX_STATE_RUNNING:
		return string(vmm.StateRunning)
	case nodev1.SandboxState_SANDBOX_STATE_PAUSED:
		return string(vmm.StatePaused)
	case nodev1.SandboxState_SANDBOX_STATE_ARCHIVED:
		return string(vmm.StateArchived)
	default:
		return ""
	}
}

func protoTime(value *timestamppb.Timestamp) time.Time {
	if value == nil || value.CheckValid() != nil {
		return time.Time{}
	}
	return value.AsTime()
}

func grpcBox(node string, row nodelink.SandboxRow) *host.Sandbox {
	if !reserved.ValidLabel(row.Name) {
		return nil
	}
	box := &host.Sandbox{
		ID: safeSandboxID(row.ID), Name: row.Name, Owner: safeOwner(row.Owner),
		Image: ctlops.SafeText(row.Image, maxDisplayText),
		VCPUs: row.VCPUs, MemMB: row.MemMB, State: safeState(row.State),
		SSHUser:   ctlops.SafeText(row.SSHUser, maxDisplayText),
		CreatedAt: row.CreatedAt, LastActive: row.LastActive,
		Pinned: row.Pinned, Ballooned: row.Ballooned,
		KeyFP:      ctlops.SafeText(row.KeyFP, maxDisplayText),
		NetRxBytes: row.NetRxBytes, NetTxBytes: row.NetTxBytes,
		ArchivedAt: row.ArchivedAt, DiskMB: row.DiskMB,
		DiskTotalMB: row.DiskTotalMB, Node: node,
	}
	box.HostIP = Host(box.Name, node)
	box.SSHAddr = net.JoinHostPort(box.HostIP, SSHPort)
	return box
}

func grpcRoutedBox(box *host.Sandbox, hostIP string) *host.Sandbox {
	if box == nil {
		return nil
	}
	routed := cloneSandbox(box)
	routed.HostIP = hostIP
	routed.SSHAddr = net.JoinHostPort(hostIP, routedSSHPort)
	return routed
}

func grpcSnapshot(node string, row nodelink.SnapshotRow) *host.Snapshot {
	if !reserved.ValidLabel(row.Name) {
		return nil
	}
	from := row.FromBox
	if !reserved.ValidLabel(from) {
		from = ""
	}
	return &host.Snapshot{
		Name: row.Name, Owner: safeOwner(row.Owner),
		Image:   ctlops.SafeText(row.Image, maxDisplayText),
		FromBox: from, CreatedAt: row.CreatedAt, Node: node,
	}
}

func capacityFromProto(node string, wire *nodev1.Capacity) host.NodeCapacity {
	if wire == nil {
		return host.NodeCapacity{Node: node}
	}
	return host.NodeCapacity{
		Node: node, TotalVCPUs: wire.GetHostVcpus(),
		TotalMemMB: wire.GetTotalMemoryMb(), BudgetMemMB: wire.GetBudgetMemoryMb(),
		UsedVCPUs: wire.GetUsedVcpus(), UsedMemMB: wire.GetUsedMemoryMb(),
		EffectiveMemMB: wire.GetEffectiveMemoryMb(), ReserveMemMB: wire.GetReserveMemoryMb(),
		OwnerMemoryPoolMB: wire.GetOwnerMemoryPoolMb(), OwnerMemoryBurstMB: wire.GetOwnerMemoryBurstMb(),
		EntitledMemMB: wire.GetEntitledMemoryMb(), ActiveOwners: int(wire.GetActiveOwners()),
		ResidentMemMB:      wire.GetResidentMemoryMb(),
		DiskPoolMBPerOwner: wire.GetDiskPoolMbPerOwner(), UsedDiskMB: wire.GetUsedDiskMb(),
		Running: int(wire.GetRunning()), Sandboxes: int(wire.GetSandboxes()),
		Arch:    ctlops.SafeText(wire.GetArchitecture(), maxDisplayText),
		Release: ctlops.SafeText(wire.GetRelease(), maxDisplayText),
	}
}

func factsFromProto(node string, base Facts, health *nodev1.HealthResponse, capacity *nodev1.Capacity) Facts {
	base.Node = node
	if capacity != nil {
		base.Arch = ctlops.SafeText(capacity.GetArchitecture(), maxDisplayText)
		base.OS = ctlops.SafeText(capacity.GetOperatingSystem(), maxDisplayText)
		base.Release = ctlops.SafeText(capacity.GetRelease(), maxDisplayText)
		base.Driver = ctlops.SafeText(capacity.GetDriver(), maxDisplayText)
		base.Archiving = capacity.GetArchiving()
		base.Snapshots = capacity.GetSnapshots()
		base.Sluice = capacity.GetNetworkAccounting()
	}
	if health != nil {
		base.Version = ctlops.SafeText(health.GetVersion(), maxDisplayText)
		base.StartedAt = protoTime(health.GetStartedAt())
		base.Capabilities = make([]string, 0, len(health.GetCapabilities()))
		for _, capability := range health.GetCapabilities() {
			if safe := ctlops.SafeText(capability, maxDisplayText); safe != "" {
				base.Capabilities = append(base.Capabilities, safe)
			}
		}
	}
	return base
}

func (g *GRPCControl) identity(ctx context.Context, request proto.Message) (*nodev1.OperationIdentity, error) {
	identity, _ := opidentity.FromContext(ctx)
	if identity.OperationID == "" {
		random, err := randomOperationID()
		if err != nil {
			return nil, err
		}
		identity.OperationID = "grpc-" + random
	}
	if identity.IdempotencyKey == "" {
		identity.IdempotencyKey = identity.OperationID
	}
	if identity.Initiator == "" {
		identity.Initiator = g.initiator
	}
	if identity.CreatedAt.IsZero() {
		identity.CreatedAt = g.now().UTC()
	}
	return grpccontrol.NewOperationIdentity(
		request, identity.OperationID, identity.IdempotencyKey,
		identity.Initiator, identity.CreatedAt,
	)
}

func randomOperationID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("fleet: create gRPC operation identity: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func setOperation(ctx context.Context, g *GRPCControl, request proto.Message, assign func(*nodev1.OperationIdentity)) error {
	identity, err := g.identity(ctx, request)
	if err != nil {
		return err
	}
	assign(identity)
	return nil
}

func operationSandbox(operation *nodev1.Operation) (*nodev1.Sandbox, error) {
	if operation == nil || operation.GetState() != nodev1.OperationState_OPERATION_STATE_SUCCEEDED {
		return nil, errors.New("fleet: gRPC mutation returned no successful terminal operation")
	}
	result := operation.GetResult()
	if result == nil || result.GetSandbox() == nil {
		return nil, errors.New("fleet: gRPC mutation returned no sandbox result")
	}
	return result.GetSandbox(), nil
}

func operationSnapshot(operation *nodev1.Operation) (*nodev1.Snapshot, error) {
	if operation == nil || operation.GetState() != nodev1.OperationState_OPERATION_STATE_SUCCEEDED {
		return nil, errors.New("fleet: gRPC mutation returned no successful terminal operation")
	}
	result := operation.GetResult()
	if result == nil || result.GetSnapshot() == nil {
		return nil, errors.New("fleet: gRPC mutation returned no snapshot result")
	}
	return result.GetSnapshot(), nil
}

func operationEmpty(operation *nodev1.Operation) error {
	if operation == nil || operation.GetState() != nodev1.OperationState_OPERATION_STATE_SUCCEEDED {
		return errors.New("fleet: gRPC mutation returned no successful terminal operation")
	}
	if operation.GetResult() == nil || operation.GetResult().GetEmpty() == nil {
		return errors.New("fleet: gRPC mutation returned no empty result")
	}
	return nil
}

func (g *GRPCControl) fail(op, sandbox string, err error) error {
	if err == nil {
		return nil
	}
	var typed *ctlops.Error
	if errors.As(err, &typed) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var remote *grpccontrol.RemoteError
	if errors.As(err, &remote) {
		return grpcRemoteError(op, sandbox, remote)
	}
	switch status.Code(err) {
	case codes.NotFound:
		return ctlops.NotFound(op, "sandbox", sandbox)
	case codes.InvalidArgument:
		return ctlops.Invalid(op, "invalid_remote_request", "%s",
			ctlops.SafeText(err.Error(), 512))
	case codes.FailedPrecondition, codes.Unimplemented:
		return &ctlops.Error{
			Kind: ctlops.KindDisabled, Op: op, Code: "remote_capability_disabled",
			Msg: ctlops.SafeText(err.Error(), 512), Verbatim: true,
		}
	case codes.ResourceExhausted:
		return &ctlops.Error{
			Kind: ctlops.KindCapacity, Op: op, Code: "host_at_capacity",
			Msg: ctlops.SafeText(err.Error(), 512), Verbatim: true,
		}
	}
	return Unreachable(op, sandbox, g.node)
}

func grpcRemoteError(op, subject string, remote *grpccontrol.RemoteError) error {
	message := ctlops.SafeText(remote.Message, 512)
	if message == "" {
		message = "the node refused that operation"
	}
	switch remote.Code {
	case "not_found":
		return ctlops.NotFound(op, "sandbox", subject)
	case "name_taken":
		return ctlops.Fail(op, &host.NameError{Problem: host.NameTaken, Noun: "sandbox", Name: subject})
	case "name_reserved":
		return ctlops.Fail(op, &host.NameError{Problem: host.NameReserved, Noun: "sandbox", Name: subject})
	case "invalid_name":
		return ctlops.Fail(op, &host.NameError{Problem: host.NameInvalid, Noun: "sandbox", Name: subject})
	case "running_limit":
		return &ctlops.Error{Kind: ctlops.KindLimit, Op: op, Code: remote.Code, Msg: message, Verbatim: true}
	case "capacity":
		return &ctlops.Error{Kind: ctlops.KindCapacity, Op: op, Code: "host_at_capacity", Msg: message, Verbatim: true}
	case "disk_quota":
		return &ctlops.Error{Kind: ctlops.KindQuota, Op: op, Code: "disk_pool_full", Msg: message, Verbatim: true}
	case "network_policy_unsupported", "network_accounting_unsupported":
		return &ctlops.Error{Kind: ctlops.KindDisabled, Op: op, Code: remote.Code, Msg: message, Verbatim: true}
	default:
		return &ctlops.Error{Kind: ctlops.KindConflict, Op: op, Code: safeErrorCode(remote.Code), Msg: message, Verbatim: true}
	}
}

func safeErrorCode(code string) string {
	for i := range len(code) {
		c := code[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-') {
			return "remote_failure"
		}
	}
	if code == "" {
		return "remote_failure"
	}
	return code
}

func (g *GRPCControl) putBox(box *host.Sandbox, wire *nodev1.Sandbox) {
	if box == nil {
		return
	}
	g.mu.Lock()
	g.boxes[box.Name] = cloneSandbox(box)
	if wire != nil {
		g.routedBoxes[box.Name] = grpcRoutedBox(box, wire.GetHostIp())
	}
	g.lastSeen = g.now()
	g.mu.Unlock()
}

func (g *GRPCControl) deleteBox(name string) {
	g.mu.Lock()
	delete(g.boxes, name)
	delete(g.routedBoxes, name)
	g.lastSeen = g.now()
	g.mu.Unlock()
}

func (g *GRPCControl) putSnapshot(snapshot *host.Snapshot) {
	if snapshot == nil {
		return
	}
	g.mu.Lock()
	copy := *snapshot
	g.snaps[snapshot.Name] = &copy
	g.lastSeen = g.now()
	g.mu.Unlock()
}

func (g *GRPCControl) deleteSnapshot(name string) {
	g.mu.Lock()
	delete(g.snaps, name)
	g.lastSeen = g.now()
	g.mu.Unlock()
}

func (g *GRPCControl) Vitals(ctx context.Context, name string) (host.Vitals, error) {
	wire, err := observeGRPC(g, ctx, "vitals", func(ctx context.Context) (*nodev1.Vitals, error) {
		return g.client.Vitals(ctx, name)
	})
	if err != nil {
		return host.Vitals{}, g.fail("vitals", name, err)
	}
	if box, ok := g.Box(name); !ok || box.State != vmm.StateRunning {
		return host.Vitals{}, nil
	}
	cpu, memory := wire.GetCpuSeconds(), wire.GetMemoryUsedMb()
	rx, tx := wire.GetNetworkRxBytes(), wire.GetNetworkTxBytes()
	return host.Vitals{
		CPUSeconds: &cpu, MemUsedMB: &memory, NetRxBytes: &rx, NetTxBytes: &tx,
	}, nil
}

func (g *GRPCControl) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	request := &nodev1.CreateRequest{Name: name, Owner: owner, Image: image, Vcpus: vcpus, MemoryMb: memMB}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return nil, err
	}
	operation, err := observeGRPC(g, ctx, "create", func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.Create(ctx, request)
	})
	if err != nil {
		return nil, g.fail("create", name, err)
	}
	wire, err := operationSandbox(operation)
	if err != nil {
		return nil, g.fail("create", name, err)
	}
	row := sandboxRowFromProto(wire)
	row.Name, row.Owner = name, owner
	box := grpcBox(g.node, row)
	if box == nil {
		return nil, g.fail("create", name, errors.New("invalid sandbox result"))
	}
	g.putBox(box, wire)
	return cloneSandbox(box), nil
}

func (g *GRPCControl) EnsureReady(ctx context.Context, name string) (*host.Sandbox, error) {
	request := &nodev1.EnsureRunningRequest{Sandbox: name}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return nil, err
	}
	operation, err := observeGRPC(g, ctx, "ensure_running", func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.EnsureRunning(ctx, request)
	})
	if err != nil {
		return nil, g.fail("restore", name, err)
	}
	wire, err := operationSandbox(operation)
	if err != nil {
		return nil, g.fail("restore", name, err)
	}
	row := sandboxRowFromProto(wire)
	row.Name = name
	box := grpcBox(g.node, row)
	if box == nil {
		return nil, g.fail("restore", name, errors.New("invalid sandbox result"))
	}
	g.putBox(box, wire)
	return cloneSandbox(box), nil
}

func (g *GRPCControl) sandboxMutation(
	ctx context.Context,
	operationName, displayOp, name string,
	request proto.Message,
	call func(context.Context) (*nodev1.Operation, error),
) error {
	operation, err := observeGRPC(g, ctx, operationName, call)
	if err != nil {
		return g.fail(displayOp, name, err)
	}
	wire, err := operationSandbox(operation)
	if err != nil {
		return g.fail(displayOp, name, err)
	}
	row := sandboxRowFromProto(wire)
	row.Name = name
	if box := grpcBox(g.node, row); box != nil {
		g.putBox(box, wire)
	}
	return nil
}

func (g *GRPCControl) Pause(ctx context.Context, name string) error {
	request := &nodev1.PauseRequest{Sandbox: name}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	return g.sandboxMutation(ctx, "pause", "pause", name, request, func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.Pause(ctx, request)
	})
}

func (g *GRPCControl) Archive(ctx context.Context, name string) error {
	request := &nodev1.ArchiveRequest{Sandbox: name}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	return g.sandboxMutation(ctx, "archive", "archive", name, request, func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.Archive(ctx, request)
	})
}

func (g *GRPCControl) Resize(ctx context.Context, name string, sizeMB int64) error {
	request := &nodev1.ResizeRequest{Sandbox: name, SizeMb: sizeMB}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	return g.sandboxMutation(ctx, "resize", "resize", name, request, func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.Resize(ctx, request)
	})
}

func (g *GRPCControl) Reboot(ctx context.Context, name string) error {
	request := &nodev1.RebootRequest{Sandbox: name}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	return g.sandboxMutation(ctx, "reboot", "reboot", name, request, func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.Reboot(ctx, request)
	})
}

func (g *GRPCControl) SetTurbo(ctx context.Context, name string, on bool) error {
	request := &nodev1.SetTurboRequest{Sandbox: name, On: on}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	return g.sandboxMutation(ctx, "turbo", "turbo", name, request, func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.SetTurbo(ctx, request)
	})
}

func (g *GRPCControl) Rename(ctx context.Context, oldName, newName, owner string) error {
	request := &nodev1.RenameRequest{Sandbox: oldName, NewName: newName, Owner: owner}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	operation, err := observeGRPC(g, ctx, "rename", func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.Rename(ctx, request)
	})
	if err != nil {
		return g.fail("rename", oldName, err)
	}
	wire, err := operationSandbox(operation)
	if err != nil {
		return g.fail("rename", oldName, err)
	}
	row := sandboxRowFromProto(wire)
	row.Name, row.Owner = newName, owner
	box := grpcBox(g.node, row)
	if box == nil {
		return g.fail("rename", oldName, errors.New("invalid sandbox result"))
	}
	g.mu.Lock()
	delete(g.boxes, oldName)
	delete(g.routedBoxes, oldName)
	g.boxes[newName] = box
	g.routedBoxes[newName] = grpcRoutedBox(box, wire.GetHostIp())
	g.lastSeen = g.now()
	g.mu.Unlock()
	return nil
}

func (g *GRPCControl) Destroy(ctx context.Context, name string) error {
	request := &nodev1.DestroyRequest{Sandbox: name}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	operation, err := observeGRPC(g, ctx, "destroy", func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.Destroy(ctx, request)
	})
	if err != nil {
		return g.fail("rm", name, err)
	}
	if err := operationEmpty(operation); err != nil {
		return g.fail("rm", name, err)
	}
	g.deleteBox(name)
	return nil
}

func (g *GRPCControl) SetPinned(ctx context.Context, name string, pinned bool) error {
	request := &nodev1.SetPinnedRequest{Sandbox: name, Pinned: pinned}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	display := "unpin"
	if pinned {
		display = "pin"
	}
	return g.sandboxMutation(ctx, "set_pinned", display, name, request, func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.SetPinned(ctx, request)
	})
}

func (g *GRPCControl) ResyncEnv(ctx context.Context, name string) error {
	request := &nodev1.ResyncEnvironmentRequest{Sandbox: name}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	operation, err := observeGRPC(g, ctx, "resync_environment", func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.ResyncEnvironment(ctx, request)
	})
	if err != nil {
		return g.fail("secrets.sync", name, err)
	}
	return g.fail("secrets.sync", name, operationEmpty(operation))
}

func (g *GRPCControl) MarkActive(ctx context.Context, name string) error {
	_, err := observeGRPC(g, ctx, "mark_active", func(ctx context.Context) (struct{}, error) {
		err := g.client.MarkActive(ctx, &nodev1.MarkActiveRequest{
			Sandbox: name, ObservedAt: timestamppb.New(g.now().UTC()),
		})
		return struct{}{}, err
	})
	return g.fail("touch", name, err)
}

func (g *GRPCControl) RecordKey(ctx context.Context, name, fingerprint string) error {
	_, err := observeGRPC(g, ctx, "record_key", func(ctx context.Context) (struct{}, error) {
		err := g.client.RecordKey(ctx, &nodev1.RecordKeyRequest{
			Sandbox: name, KeyFingerprint: fingerprint,
		})
		return struct{}{}, err
	})
	return g.fail("key.record", name, err)
}

func (g *GRPCControl) Snapshotter(ctx context.Context, boxName, snapshotName, owner string) (*host.Snapshot, error) {
	request := &nodev1.SnapshotRequest{Sandbox: boxName, Snapshot: snapshotName, Owner: owner}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return nil, err
	}
	operation, err := observeGRPC(g, ctx, "snapshot", func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.Snapshot(ctx, request)
	})
	if err != nil {
		return nil, g.fail("snapshot.create", boxName, err)
	}
	wire, err := operationSnapshot(operation)
	if err != nil {
		return nil, g.fail("snapshot.create", boxName, err)
	}
	row := snapshotRowFromProto(wire)
	row.Name, row.Owner, row.FromBox = snapshotName, owner, boxName
	snapshot := grpcSnapshot(g.node, row)
	if snapshot == nil {
		return nil, g.fail("snapshot.create", boxName, errors.New("invalid snapshot result"))
	}
	g.putSnapshot(snapshot)
	copy := *snapshot
	return &copy, nil
}

func (g *GRPCControl) DeleteSnapshot(ctx context.Context, snapshotName, owner string) error {
	request := &nodev1.DeleteSnapshotRequest{Snapshot: snapshotName, Owner: owner}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	operation, err := observeGRPC(g, ctx, "delete_snapshot", func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.DeleteSnapshot(ctx, request)
	})
	if err != nil {
		return g.fail("snapshot.rm", snapshotName, err)
	}
	if err := operationEmpty(operation); err != nil {
		return g.fail("snapshot.rm", snapshotName, err)
	}
	g.deleteSnapshot(snapshotName)
	return nil
}

func (g *GRPCControl) Fork(ctx context.Context, snapshotName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error) {
	request := &nodev1.ForkRequest{
		Snapshot: snapshotName, Name: newName, Owner: owner,
		Vcpus: vcpus, MemoryMb: memMB,
	}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return nil, err
	}
	operation, err := observeGRPC(g, ctx, "fork", func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.Fork(ctx, request)
	})
	if err != nil {
		return nil, g.fail("fork", newName, err)
	}
	wire, err := operationSandbox(operation)
	if err != nil {
		return nil, g.fail("fork", newName, err)
	}
	row := sandboxRowFromProto(wire)
	row.Name, row.Owner = newName, owner
	box := grpcBox(g.node, row)
	if box == nil {
		return nil, g.fail("fork", newName, errors.New("invalid sandbox result"))
	}
	g.putBox(box, wire)
	return cloneSandbox(box), nil
}

func (g *GRPCControl) NetPolicy(ctx context.Context, allow map[string][]string) error {
	names := make([]string, 0, len(allow))
	for name := range allow {
		names = append(names, name)
	}
	sort.Strings(names)
	policies := make([]*nodev1.NetworkPolicy, 0, len(names))
	for _, name := range names {
		destinations := append([]string(nil), allow[name]...)
		sort.Strings(destinations)
		policies = append(policies, &nodev1.NetworkPolicy{
			Sandbox: name, AllowedDestinations: destinations,
		})
	}
	request := &nodev1.ApplyNetworkPolicyRequest{Policies: policies}
	if err := setOperation(ctx, g, request, func(identity *nodev1.OperationIdentity) { request.Operation = identity }); err != nil {
		return err
	}
	operation, err := observeGRPC(g, ctx, "apply_network_policy", func(ctx context.Context) (*nodev1.Operation, error) {
		return g.client.ApplyNetworkPolicy(ctx, request)
	})
	if err != nil {
		return g.fail("net.policy", "", err)
	}
	return g.fail("net.policy", "", operationEmpty(operation))
}

func (g *GRPCControl) NetUsage(ctx context.Context) (map[string]netpush.VMUsage, error) {
	response, err := observeGRPC(g, ctx, "network_usage", func(ctx context.Context) (*nodev1.GetNetworkUsageResponse, error) {
		return g.client.NetworkUsage(ctx)
	})
	if err != nil {
		return nil, g.fail("net.usage", "", err)
	}
	out := make(map[string]netpush.VMUsage, len(response.GetUsage()))
	for _, usage := range response.GetUsage() {
		if usage.GetSandbox() == "" {
			continue
		}
		out[usage.GetSandbox()] = netpush.VMUsage{
			Name: usage.GetSandbox(), RxBytes: usage.GetRxBytes(), TxBytes: usage.GetTxBytes(),
		}
	}
	return out, nil
}

func (g *GRPCControl) Hangup(code, message string) {
	g.log.Info("closing gRPC control", "code", code, "message", message)
	_ = g.Close()
}

func (g *GRPCControl) Revoke(_ string, reason error) {
	g.stop(reason)
	_ = g.Close()
}

func (g *GRPCControl) stop(reason error) {
	g.callbackMu.Lock()
	defer g.callbackMu.Unlock()
	g.cancel()
	g.mu.Lock()
	if g.dead == nil && reason != nil {
		g.dead = reason
	}
	g.healthy = false
	g.mu.Unlock()
}

func (g *GRPCControl) Close() error {
	var err error
	g.once.Do(func() {
		g.stop(errors.New("fleet: gRPC control closed"))
		g.readyOnce.Do(func() { close(g.ready) })
		err = g.client.Close()
	})
	return err
}
