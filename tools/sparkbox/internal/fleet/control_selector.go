package fleet

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
)

// ControlTransport is the operator's control-plane rollout choice.
type ControlTransport string

const (
	ControlTransportAuto ControlTransport = "auto"
	ControlTransportGRPC ControlTransport = "grpc"
	ControlTransportSSH  ControlTransport = "ssh"
)

func (mode ControlTransport) valid() bool {
	return mode == ControlTransportAuto || mode == ControlTransportGRPC || mode == ControlTransportSSH
}

// ControlOperationClass is the rollout boundary used to move progressively
// riskier operations from SSH to gRPC.
type ControlOperationClass string

const (
	ControlClassReadOnly    ControlOperationClass = "read_only"
	ControlClassIdempotent  ControlOperationClass = "idempotent"
	ControlClassDestructive ControlOperationClass = "destructive"
)

// ControlRollout optionally overrides the selector's base mode per operation
// class. An empty field inherits the base mode, preserving Configure's
// historical all-operations behavior.
type ControlRollout struct {
	ReadOnly    ControlTransport
	Idempotent  ControlTransport
	Destructive ControlTransport
}

// ControlSelector switches only the ControlPlane. The GuestDialer is composed
// separately and is therefore always the independently supervised SSH data
// pool during this rollout.
type ControlSelector struct {
	mu      sync.RWMutex
	mode    ControlTransport
	grpc    *GRPCControl
	ssh     ControlPlane
	rollout ControlRollout
	shadow  shadowInventoryConfig
	node    string
	metrics *fleetmetrics.Registry
}

var _ ControlPlane = (*ControlSelector)(nil)

func NewControlSelector(mode ControlTransport, grpc *GRPCControl, ssh ControlPlane) (*ControlSelector, error) {
	if ssh == nil {
		return nil, errors.New("fleet: SSH fallback control is required")
	}
	selector := &ControlSelector{ssh: ssh}
	if err := selector.Configure(mode, grpc); err != nil {
		return nil, err
	}
	return selector, nil
}

// Configure atomically changes the preferred control transport. Explicit gRPC
// with no adapter is rejected rather than silently becoming SSH.
func (s *ControlSelector) Configure(mode ControlTransport, grpc *GRPCControl) error {
	if !mode.valid() {
		return errors.New("fleet: control transport must be auto, grpc, or ssh")
	}
	if mode == ControlTransportGRPC && grpc == nil {
		return errors.New("fleet: explicit gRPC control has no gRPC adapter")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if grpc == nil && rolloutNeedsGRPC(s.rollout) {
		return errors.New("fleet: explicit gRPC rollout has no gRPC adapter")
	}
	s.mode, s.grpc = mode, grpc
	return nil
}

// ConfigureRollout atomically installs per-operation overrides. Explicit gRPC
// for any class requires an installed adapter and therefore fails closed.
func (s *ControlSelector) ConfigureRollout(rollout ControlRollout) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, mode := range []ControlTransport{rollout.ReadOnly, rollout.Idempotent, rollout.Destructive} {
		if mode != "" && !mode.valid() {
			return errors.New("fleet: rollout transport must be empty, auto, grpc, or ssh")
		}
		if mode == ControlTransportGRPC && s.grpc == nil {
			return errors.New("fleet: explicit gRPC rollout has no gRPC adapter")
		}
	}
	s.rollout = rollout
	return nil
}

func rolloutNeedsGRPC(rollout ControlRollout) bool {
	return rollout.ReadOnly == ControlTransportGRPC ||
		rollout.Idempotent == ControlTransportGRPC ||
		rollout.Destructive == ControlTransportGRPC
}

func (s *ControlSelector) effectiveMode(class ControlOperationClass) ControlTransport {
	mode := s.mode
	switch class {
	case ControlClassReadOnly:
		if s.rollout.ReadOnly != "" {
			mode = s.rollout.ReadOnly
		}
	case ControlClassIdempotent:
		if s.rollout.Idempotent != "" {
			mode = s.rollout.Idempotent
		}
	case ControlClassDestructive:
		if s.rollout.Destructive != "" {
			mode = s.rollout.Destructive
		}
	}
	return mode
}

func (s *ControlSelector) choiceFor(class ControlOperationClass) ControlPlane {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch s.effectiveMode(class) {
	case ControlTransportGRPC:
		return s.grpc
	case ControlTransportAuto:
		if s.grpc != nil && s.grpc.Healthy() {
			return s.grpc
		}
	}
	return s.ssh
}

func (s *ControlSelector) choice() ControlPlane {
	return s.choiceFor(ControlClassReadOnly)
}

func (s *ControlSelector) pairFor(class ControlOperationClass) (*GRPCControl, ControlPlane, ControlTransport) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.grpc, s.ssh, s.effectiveMode(class)
}

func (s *ControlSelector) pair() (*GRPCControl, ControlPlane, ControlTransport) {
	return s.pairFor(ControlClassReadOnly)
}

func (s *ControlSelector) Name() string        { return s.ssh.Name() }
func (s *ControlSelector) Facts() Facts        { return s.choice().Facts() }
func (s *ControlSelector) Online() bool        { return s.choice().Online() }
func (s *ControlSelector) LastSeen() time.Time { return s.choice().LastSeen() }
func (s *ControlSelector) Box(name string) (*host.Sandbox, bool) {
	return s.choice().Box(name)
}
func (s *ControlSelector) Boxes() []*host.Sandbox      { return s.choice().Boxes() }
func (s *ControlSelector) Templates() []*host.Snapshot { return s.choice().Templates() }
func (s *ControlSelector) Capacity() host.NodeCapacity { return s.choice().Capacity() }

func (s *ControlSelector) Vitals(ctx context.Context, name string) (host.Vitals, error) {
	chosen := s.choiceFor(ControlClassReadOnly)
	vitals, err := chosen.Vitals(ctx, name)
	if err == nil {
		return vitals, nil
	}
	grpc, ssh, mode := s.pairFor(ControlClassReadOnly)
	if mode == ControlTransportAuto && chosen == grpc && !grpc.Healthy() {
		return ssh.Vitals(ctx, name)
	}
	return host.Vitals{}, err
}

func (s *ControlSelector) Hangup(code, message string) {
	grpc, ssh, _ := s.pair()
	if grpc != nil {
		grpc.Hangup(code, message)
	}
	ssh.Hangup(code, message)
}

func (s *ControlSelector) Revoke(code string, reason error) {
	grpc, ssh, _ := s.pair()
	if grpc != nil {
		grpc.Revoke(code, reason)
	}
	ssh.Revoke(code, reason)
}

func (s *ControlSelector) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	return s.choiceFor(ControlClassDestructive).Create(ctx, name, owner, image, vcpus, memMB)
}
func (s *ControlSelector) EnsureReady(ctx context.Context, name string) (*host.Sandbox, error) {
	return s.choiceFor(ControlClassIdempotent).EnsureReady(ctx, name)
}
func (s *ControlSelector) Pause(ctx context.Context, name string) error {
	return s.choiceFor(ControlClassDestructive).Pause(ctx, name)
}
func (s *ControlSelector) Archive(ctx context.Context, name string) error {
	return s.choiceFor(ControlClassDestructive).Archive(ctx, name)
}
func (s *ControlSelector) Resize(ctx context.Context, name string, sizeMB int64) error {
	return s.choiceFor(ControlClassDestructive).Resize(ctx, name, sizeMB)
}
func (s *ControlSelector) Reboot(ctx context.Context, name string) error {
	return s.choiceFor(ControlClassDestructive).Reboot(ctx, name)
}
func (s *ControlSelector) Rename(ctx context.Context, oldName, newName, owner string) error {
	return s.choiceFor(ControlClassDestructive).Rename(ctx, oldName, newName, owner)
}
func (s *ControlSelector) Destroy(ctx context.Context, name string) error {
	return s.choiceFor(ControlClassDestructive).Destroy(ctx, name)
}
func (s *ControlSelector) SetPinned(ctx context.Context, name string, pinned bool) error {
	return s.choiceFor(ControlClassIdempotent).SetPinned(ctx, name, pinned)
}
func (s *ControlSelector) ResyncEnv(ctx context.Context, name string) error {
	return s.choiceFor(ControlClassIdempotent).ResyncEnv(ctx, name)
}
func (s *ControlSelector) MarkActive(ctx context.Context, name string) error {
	chosen := s.choiceFor(ControlClassIdempotent)
	err := chosen.MarkActive(ctx, name)
	grpc, ssh, mode := s.pairFor(ControlClassIdempotent)
	if err != nil && mode == ControlTransportAuto && chosen == grpc && !grpc.Healthy() {
		return ssh.MarkActive(ctx, name)
	}
	return err
}
func (s *ControlSelector) RecordKey(ctx context.Context, name, fingerprint string) error {
	chosen := s.choiceFor(ControlClassIdempotent)
	err := chosen.RecordKey(ctx, name, fingerprint)
	grpc, ssh, mode := s.pairFor(ControlClassIdempotent)
	if err != nil && mode == ControlTransportAuto && chosen == grpc && !grpc.Healthy() {
		return ssh.RecordKey(ctx, name, fingerprint)
	}
	return err
}
func (s *ControlSelector) Snapshotter(ctx context.Context, box, snapshot, owner string) (*host.Snapshot, error) {
	return s.choiceFor(ControlClassDestructive).Snapshotter(ctx, box, snapshot, owner)
}
func (s *ControlSelector) DeleteSnapshot(ctx context.Context, snapshot, owner string) error {
	return s.choiceFor(ControlClassDestructive).DeleteSnapshot(ctx, snapshot, owner)
}
func (s *ControlSelector) Fork(ctx context.Context, snapshot, name, owner string, vcpus, memMB int64) (*host.Sandbox, error) {
	return s.choiceFor(ControlClassDestructive).Fork(ctx, snapshot, name, owner, vcpus, memMB)
}
func (s *ControlSelector) NetPolicy(ctx context.Context, allow map[string][]string) error {
	return s.choiceFor(ControlClassIdempotent).NetPolicy(ctx, allow)
}
func (s *ControlSelector) NetUsage(ctx context.Context) (map[string]netpush.VMUsage, error) {
	chosen := s.choiceFor(ControlClassReadOnly)
	usage, err := chosen.NetUsage(ctx)
	if err == nil {
		return usage, nil
	}
	grpc, ssh, mode := s.pairFor(ControlClassReadOnly)
	if mode == ControlTransportAuto && chosen == grpc && !grpc.Healthy() {
		return ssh.NetUsage(ctx)
	}
	return nil, err
}
