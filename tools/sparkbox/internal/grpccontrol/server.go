package grpccontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/eventjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/operationjournal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	eventSandboxChanged  = "sandbox.changed"
	eventSandboxGone     = "sandbox.gone"
	eventSnapshotChanged = "snapshot.changed"
	eventSnapshotGone    = "snapshot.gone"
)

// Server is the node-side NodeControl adapter.
type Server struct {
	nodev1.UnimplementedNodeControlServer

	config ServerConfig
}

// NewServer validates and constructs a NodeControl service. Use NewRPCServer
// to expose it over a TLS-authenticated listener.
func NewServer(config ServerConfig) (*Server, error) {
	if err := config.normalize(); err != nil {
		return nil, err
	}
	return &Server{config: config}, nil
}

func (s *Server) GetInventory(ctx context.Context, _ *nodev1.GetInventoryRequest) (*nodev1.Inventory, error) {
	revision, err := s.config.Events.Current(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read inventory revision: %v", err)
	}
	boxes := s.config.Backend.List()
	snapshots := s.config.Backend.AllSnapshots()
	out := &nodev1.Inventory{
		Revision:  revision,
		Sandboxes: make([]*nodev1.Sandbox, 0, len(boxes)),
		Snapshots: make([]*nodev1.Snapshot, 0, len(snapshots)),
	}
	for _, box := range boxes {
		out.Sandboxes = append(out.Sandboxes, sandboxToProto(box))
	}
	for _, snapshot := range snapshots {
		out.Snapshots = append(out.Snapshots, snapshotToProto(snapshot))
	}
	return out, nil
}

func (s *Server) WatchEvents(request *nodev1.WatchEventsRequest, stream grpc.ServerStreamingServer[nodev1.InventoryEvent]) error {
	events, errs := s.config.Events.Watch(stream.Context(), request.GetAfterRevision())
	for events != nil || errs != nil {
		select {
		case <-stream.Context().Done():
			return status.FromContextError(stream.Context().Err()).Err()
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			mapped := new(nodev1.InventoryEvent)
			if err := proto.Unmarshal(event.Payload, mapped); err != nil {
				return status.Errorf(codes.Internal, "decode inventory event %d: %v", event.Revision, err)
			}
			if mapped.GetEvent() == nil {
				return status.Errorf(codes.Internal, "inventory event %d has no payload", event.Revision)
			}
			mapped.Revision = event.Revision
			if err := stream.Send(mapped); err != nil {
				return err
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			var gap *eventjournal.GapError
			if errors.As(err, &gap) {
				return stream.Send(&nodev1.InventoryEvent{
					Revision: gap.Current,
					Event: &nodev1.InventoryEvent_Gap{Gap: &nodev1.EventGap{
						OldestAvailableRevision: gap.Oldest,
						CurrentRevision:         gap.Current,
					}},
				})
			}
			return status.Errorf(codes.Internal, "watch inventory events: %v", err)
		}
	}
	return nil
}

func (s *Server) GetCapacity(context.Context, *nodev1.GetCapacityRequest) (*nodev1.Capacity, error) {
	capacity := s.config.Backend.Capacity()
	architecture := s.config.Architecture
	if capacity.Arch != "" {
		architecture = capacity.Arch
	}
	release := s.config.Release
	if capacity.Release != "" {
		release = capacity.Release
	}
	var archiving, snapshots bool
	if capabilities, ok := s.config.Backend.(interface {
		ArchivingEnabled() bool
		Snapshotter() bool
	}); ok {
		archiving = capabilities.ArchivingEnabled()
		snapshots = capabilities.Snapshotter()
	}
	return &nodev1.Capacity{
		Architecture:       architecture,
		OperatingSystem:    s.config.OS,
		Release:            release,
		Driver:             s.config.Driver,
		HostVcpus:          capacity.TotalVCPUs,
		TotalMemoryMb:      capacity.TotalMemMB,
		BudgetMemoryMb:     capacity.BudgetMemMB,
		UsedVcpus:          capacity.UsedVCPUs,
		UsedMemoryMb:       capacity.UsedMemMB,
		EffectiveMemoryMb:  capacity.EffectiveMemMB,
		ReserveMemoryMb:    capacity.ReserveMemMB,
		DiskPoolMbPerOwner: capacity.DiskPoolMBPerOwner,
		UsedDiskMb:         capacity.UsedDiskMB,
		Running:            int32(capacity.Running),
		Sandboxes:          int32(capacity.Sandboxes),
		Archiving:          archiving,
		Snapshots:          snapshots,
		NetworkAccounting:  s.config.Network != nil,
	}, nil
}

func (s *Server) Health(ctx context.Context, _ *nodev1.HealthRequest) (*nodev1.HealthResponse, error) {
	revision, err := s.config.Events.Current(ctx)
	health := nodev1.HealthStatus_HEALTH_STATUS_SERVING
	if err != nil {
		health = nodev1.HealthStatus_HEALTH_STATUS_DEGRADED
	}
	select {
	case <-s.config.Context.Done():
		health = nodev1.HealthStatus_HEALTH_STATUS_STOPPING
	default:
	}
	return &nodev1.HealthResponse{
		Status:            health,
		Node:              s.config.Node,
		Version:           s.config.Version,
		StartedAt:         timestamppb.New(s.config.StartedAt),
		InventoryRevision: revision,
		Capabilities:      append([]string(nil), s.config.Capabilities...),
	}, nil
}

func (s *Server) GetVitals(ctx context.Context, request *nodev1.GetVitalsRequest) (*nodev1.Vitals, error) {
	if request.GetSandbox() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox is required")
	}
	vitals, err := s.config.Backend.Vitals(ctx, request.GetSandbox())
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read sandbox vitals: %v", err)
	}
	out := new(nodev1.Vitals)
	if vitals.CPUSeconds != nil {
		out.CpuSeconds = *vitals.CPUSeconds
	}
	if vitals.MemUsedMB != nil {
		out.MemoryUsedMb = *vitals.MemUsedMB
	}
	if vitals.NetRxBytes != nil {
		out.NetworkRxBytes = *vitals.NetRxBytes
	}
	if vitals.NetTxBytes != nil {
		out.NetworkTxBytes = *vitals.NetTxBytes
	}
	return out, nil
}

type mutationResult struct {
	result *nodev1.OperationResult
	events []*nodev1.InventoryEvent
}

type mutationAction func(context.Context) (mutationResult, error)

func (s *Server) begin(
	ctx context.Context,
	identity *nodev1.OperationIdentity,
	kind string,
	target string,
	request proto.Message,
	action mutationAction,
) (*nodev1.Operation, error) {
	spec, err := operationSpec(identity, kind, target, request)
	if err != nil {
		return nil, err
	}
	op, existing, err := s.config.Operations.Claim(ctx, spec)
	if err != nil {
		return nil, journalStatus(err)
	}
	if !existing {
		go s.execute(spec.ID, action)
	}
	return operationToProto(op)
}

func operationSpec(identity *nodev1.OperationIdentity, kind, target string, request proto.Message) (operationjournal.Spec, error) {
	if identity == nil {
		return operationjournal.Spec{}, status.Error(codes.InvalidArgument, "operation identity is required")
	}
	expected, err := RequestHash(request)
	if err != nil {
		return operationjournal.Spec{}, status.Errorf(codes.InvalidArgument, "hash mutation request: %v", err)
	}
	if len(identity.GetRequestHash()) != sha256Size || !bytes.Equal(identity.GetRequestHash(), expected) {
		return operationjournal.Spec{}, status.Error(codes.InvalidArgument, "request_hash does not match immutable request fields")
	}
	if identity.GetCreatedAt() == nil {
		return operationjournal.Spec{}, status.Error(codes.InvalidArgument, "operation created_at is required")
	}
	if err := identity.GetCreatedAt().CheckValid(); err != nil {
		return operationjournal.Spec{}, status.Errorf(codes.InvalidArgument, "invalid operation created_at: %v", err)
	}
	return operationjournal.Spec{
		ID:             identity.GetOperationId(),
		IdempotencyKey: identity.GetIdempotencyKey(),
		RequestHash:    identity.GetRequestHash(),
		Kind:           kind,
		Target:         target,
		Initiator:      identity.GetInitiator(),
		CreatedAt:      identity.GetCreatedAt().AsTime(),
	}, nil
}

const sha256Size = 32

func (s *Server) execute(operationID string, action mutationAction) {
	ctx := s.config.Context
	durableContext := context.WithoutCancel(ctx)
	if _, err := s.config.Operations.Start(durableContext, operationID); err != nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			_, _ = s.config.Operations.Fail(durableContext, operationID, operationjournal.Failure{
				Code: "panic", Message: fmt.Sprint(recovered), Retryable: true,
			})
		}
	}()
	if err := ctx.Err(); err != nil {
		_, _ = s.config.Operations.Cancel(durableContext, operationID, operationjournal.Failure{
			Code: "server_stopping", Message: err.Error(), Retryable: true,
		})
		return
	}
	outcome, err := action(ctx)
	if err != nil {
		if ctx.Err() != nil {
			_, _ = s.config.Operations.Cancel(durableContext, operationID, operationjournal.Failure{
				Code: "server_stopping", Message: ctx.Err().Error(), Retryable: true,
			})
		} else {
			_, _ = s.config.Operations.Fail(durableContext, operationID, failureFromError(err))
		}
		return
	}
	if outcome.result == nil {
		outcome.result = emptyResult()
	}
	for _, event := range outcome.events {
		if s.config.SandboxEventsFromObserver && isSandboxEvent(event) {
			continue
		}
		payload, err := proto.Marshal(event)
		if err != nil {
			_, _ = s.config.Operations.Fail(durableContext, operationID, operationjournal.Failure{
				Code: "event_encode", Message: err.Error(), Retryable: true,
			})
			return
		}
		if _, err := s.config.Events.Append(ctx, inventoryEventKind(event), payload); err != nil {
			_, _ = s.config.Operations.Fail(durableContext, operationID, operationjournal.Failure{
				Code: "event_journal", Message: err.Error(), Retryable: true,
			})
			return
		}
	}
	result, err := proto.Marshal(outcome.result)
	if err != nil {
		_, _ = s.config.Operations.Fail(durableContext, operationID, operationjournal.Failure{
			Code: "result_encode", Message: err.Error(), Retryable: true,
		})
		return
	}
	_, _ = s.config.Operations.Succeed(durableContext, operationID, result)
}

func isSandboxEvent(event *nodev1.InventoryEvent) bool {
	switch event.GetEvent().(type) {
	case *nodev1.InventoryEvent_SandboxChanged, *nodev1.InventoryEvent_SandboxGone:
		return true
	default:
		return false
	}
}

func inventoryEventKind(event *nodev1.InventoryEvent) string {
	switch event.GetEvent().(type) {
	case *nodev1.InventoryEvent_SandboxChanged:
		return eventSandboxChanged
	case *nodev1.InventoryEvent_SandboxGone:
		return eventSandboxGone
	case *nodev1.InventoryEvent_SnapshotChanged:
		return eventSnapshotChanged
	case *nodev1.InventoryEvent_SnapshotGone:
		return eventSnapshotGone
	default:
		return "inventory.unknown"
	}
}

func sandboxChanged(box *host.Sandbox, reason string) *nodev1.InventoryEvent {
	return &nodev1.InventoryEvent{
		Event: &nodev1.InventoryEvent_SandboxChanged{SandboxChanged: &nodev1.SandboxChanged{
			Sandbox: sandboxToProto(box),
			Reason:  reason,
		}},
	}
}

func sandboxGone(name string) *nodev1.InventoryEvent {
	return &nodev1.InventoryEvent{
		Event: &nodev1.InventoryEvent_SandboxGone{SandboxGone: &nodev1.SandboxGone{Name: name}},
	}
}

func snapshotChanged(snapshot *host.Snapshot, reason string) *nodev1.InventoryEvent {
	return &nodev1.InventoryEvent{
		Event: &nodev1.InventoryEvent_SnapshotChanged{SnapshotChanged: &nodev1.SnapshotChanged{
			Snapshot: snapshotToProto(snapshot),
			Reason:   reason,
		}},
	}
}

func snapshotGone(name string) *nodev1.InventoryEvent {
	return &nodev1.InventoryEvent{
		Event: &nodev1.InventoryEvent_SnapshotGone{SnapshotGone: &nodev1.SnapshotGone{Name: name}},
	}
}

func (s *Server) EnsureRunning(ctx context.Context, request *nodev1.EnsureRunningRequest) (*nodev1.Operation, error) {
	name := request.GetSandbox()
	return s.begin(ctx, request.GetOperation(), "ensure_running", name, request, func(ctx context.Context) (mutationResult, error) {
		box, err := s.config.Backend.EnsureReady(ctx, name)
		return mutationResult{result: sandboxResult(box), events: []*nodev1.InventoryEvent{sandboxChanged(box, "ensure_running")}}, err
	})
}

func (s *Server) BeginCreate(ctx context.Context, request *nodev1.CreateRequest) (*nodev1.Operation, error) {
	clone := proto.Clone(request).(*nodev1.CreateRequest)
	return s.begin(ctx, request.GetOperation(), "create", request.GetName(), request, func(ctx context.Context) (mutationResult, error) {
		box, err := s.config.Backend.Create(ctx, clone.GetName(), clone.GetOwner(), clone.GetImage(), clone.GetVcpus(), clone.GetMemoryMb())
		return mutationResult{result: sandboxResult(box), events: []*nodev1.InventoryEvent{sandboxChanged(box, "create")}}, err
	})
}

func (s *Server) BeginPause(ctx context.Context, request *nodev1.PauseRequest) (*nodev1.Operation, error) {
	name := request.GetSandbox()
	return s.begin(ctx, request.GetOperation(), "pause", name, request, func(ctx context.Context) (mutationResult, error) {
		if err := s.config.Backend.Pause(ctx, name); err != nil {
			return mutationResult{}, err
		}
		box, _ := s.config.Backend.Get(name)
		return mutationResult{result: sandboxResult(box), events: []*nodev1.InventoryEvent{sandboxChanged(box, "pause")}}, nil
	})
}

func (s *Server) BeginArchive(ctx context.Context, request *nodev1.ArchiveRequest) (*nodev1.Operation, error) {
	name := request.GetSandbox()
	return s.begin(ctx, request.GetOperation(), "archive", name, request, func(ctx context.Context) (mutationResult, error) {
		if err := s.config.Backend.Archive(ctx, name); err != nil {
			return mutationResult{}, err
		}
		box, _ := s.config.Backend.Get(name)
		return mutationResult{result: sandboxResult(box), events: []*nodev1.InventoryEvent{sandboxChanged(box, "archive")}}, nil
	})
}

func (s *Server) BeginResize(ctx context.Context, request *nodev1.ResizeRequest) (*nodev1.Operation, error) {
	name, size := request.GetSandbox(), request.GetSizeMb()
	return s.begin(ctx, request.GetOperation(), "resize", name, request, func(ctx context.Context) (mutationResult, error) {
		if err := s.config.Backend.Resize(ctx, name, size); err != nil {
			return mutationResult{}, err
		}
		box, _ := s.config.Backend.Get(name)
		return mutationResult{result: sandboxResult(box), events: []*nodev1.InventoryEvent{sandboxChanged(box, "resize")}}, nil
	})
}

func (s *Server) BeginReboot(ctx context.Context, request *nodev1.RebootRequest) (*nodev1.Operation, error) {
	name := request.GetSandbox()
	return s.begin(ctx, request.GetOperation(), "reboot", name, request, func(ctx context.Context) (mutationResult, error) {
		if err := s.config.Backend.Reboot(ctx, name); err != nil {
			return mutationResult{}, err
		}
		box, _ := s.config.Backend.Get(name)
		return mutationResult{result: sandboxResult(box), events: []*nodev1.InventoryEvent{sandboxChanged(box, "reboot")}}, nil
	})
}

func (s *Server) BeginRename(ctx context.Context, request *nodev1.RenameRequest) (*nodev1.Operation, error) {
	oldName, newName, owner := request.GetSandbox(), request.GetNewName(), request.GetOwner()
	return s.begin(ctx, request.GetOperation(), "rename", oldName, request, func(ctx context.Context) (mutationResult, error) {
		if err := s.config.Backend.Rename(ctx, oldName, newName, owner); err != nil {
			return mutationResult{}, err
		}
		box, _ := s.config.Backend.Get(newName)
		return mutationResult{
			result: sandboxResult(box),
			events: []*nodev1.InventoryEvent{sandboxGone(oldName), sandboxChanged(box, "rename")},
		}, nil
	})
}

func (s *Server) BeginDestroy(ctx context.Context, request *nodev1.DestroyRequest) (*nodev1.Operation, error) {
	name := request.GetSandbox()
	return s.begin(ctx, request.GetOperation(), "destroy", name, request, func(ctx context.Context) (mutationResult, error) {
		if err := s.config.Backend.Destroy(ctx, name); err != nil {
			return mutationResult{}, err
		}
		return mutationResult{result: emptyResult(), events: []*nodev1.InventoryEvent{sandboxGone(name)}}, nil
	})
}

func (s *Server) BeginSetPinned(ctx context.Context, request *nodev1.SetPinnedRequest) (*nodev1.Operation, error) {
	name, pinned := request.GetSandbox(), request.GetPinned()
	return s.begin(ctx, request.GetOperation(), "set_pinned", name, request, func(context.Context) (mutationResult, error) {
		if err := s.config.Backend.SetPinned(name, pinned); err != nil {
			return mutationResult{}, err
		}
		box, _ := s.config.Backend.Get(name)
		return mutationResult{result: sandboxResult(box), events: []*nodev1.InventoryEvent{sandboxChanged(box, "set_pinned")}}, nil
	})
}

func (s *Server) BeginResyncEnvironment(ctx context.Context, request *nodev1.ResyncEnvironmentRequest) (*nodev1.Operation, error) {
	name := request.GetSandbox()
	return s.begin(ctx, request.GetOperation(), "resync_environment", name, request, func(ctx context.Context) (mutationResult, error) {
		if _, ok := s.config.Backend.Get(name); !ok {
			return mutationResult{}, &host.MissingError{Noun: "sandbox", Name: name}
		}
		s.config.Backend.ResyncEnv(ctx, name)
		return mutationResult{result: emptyResult()}, nil
	})
}

func (s *Server) BeginSnapshot(ctx context.Context, request *nodev1.SnapshotRequest) (*nodev1.Operation, error) {
	box, snapshot, owner := request.GetSandbox(), request.GetSnapshot(), request.GetOwner()
	return s.begin(ctx, request.GetOperation(), "snapshot", box, request, func(ctx context.Context) (mutationResult, error) {
		result, err := s.config.Backend.Snapshot(ctx, box, snapshot, owner)
		return mutationResult{
			result: snapshotResult(result),
			events: []*nodev1.InventoryEvent{snapshotChanged(result, "snapshot")},
		}, err
	})
}

func (s *Server) BeginDeleteSnapshot(ctx context.Context, request *nodev1.DeleteSnapshotRequest) (*nodev1.Operation, error) {
	name, owner := request.GetSnapshot(), request.GetOwner()
	return s.begin(ctx, request.GetOperation(), "delete_snapshot", name, request, func(ctx context.Context) (mutationResult, error) {
		if err := s.config.Backend.DeleteSnapshot(ctx, name, owner); err != nil {
			return mutationResult{}, err
		}
		return mutationResult{result: emptyResult(), events: []*nodev1.InventoryEvent{snapshotGone(name)}}, nil
	})
}

func (s *Server) BeginFork(ctx context.Context, request *nodev1.ForkRequest) (*nodev1.Operation, error) {
	clone := proto.Clone(request).(*nodev1.ForkRequest)
	return s.begin(ctx, request.GetOperation(), "fork", request.GetName(), request, func(ctx context.Context) (mutationResult, error) {
		box, err := s.config.Backend.Fork(ctx, clone.GetSnapshot(), clone.GetName(), clone.GetOwner(), clone.GetVcpus(), clone.GetMemoryMb())
		return mutationResult{result: sandboxResult(box), events: []*nodev1.InventoryEvent{sandboxChanged(box, "fork")}}, err
	})
}

func (s *Server) MarkActive(_ context.Context, request *nodev1.MarkActiveRequest) (*emptypb.Empty, error) {
	name := request.GetSandbox()
	if _, ok := s.config.Backend.Get(name); !ok {
		return nil, status.Errorf(codes.NotFound, "sandbox %q not found", name)
	}
	s.config.Backend.MarkActive(name)
	return &emptypb.Empty{}, nil
}

func (s *Server) RecordKey(ctx context.Context, request *nodev1.RecordKeyRequest) (*emptypb.Empty, error) {
	name := request.GetSandbox()
	if _, ok := s.config.Backend.Get(name); !ok {
		return nil, status.Errorf(codes.NotFound, "sandbox %q not found", name)
	}
	s.config.Backend.RecordKey(name, request.GetKeyFingerprint())
	if s.config.SandboxEventsFromObserver {
		return &emptypb.Empty{}, nil
	}
	box, _ := s.config.Backend.Get(name)
	event := sandboxChanged(box, "record_key")
	payload, err := proto.Marshal(event)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode record-key event: %v", err)
	}
	if _, err := s.config.Events.Append(ctx, eventSandboxChanged, payload); err != nil {
		return nil, status.Errorf(codes.Internal, "record key event: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) ApplyNetworkPolicy(ctx context.Context, request *nodev1.ApplyNetworkPolicyRequest) (*nodev1.Operation, error) {
	clone := proto.Clone(request).(*nodev1.ApplyNetworkPolicyRequest)
	return s.begin(ctx, request.GetOperation(), "apply_network_policy", "network", request, func(ctx context.Context) (mutationResult, error) {
		if s.config.Network == nil {
			return mutationResult{}, &host.DisabledError{
				Code: "network_policy_unsupported",
				Msg:  "network policy is not supported by this node",
			}
		}
		if err := s.config.Network.ApplyNetworkPolicy(ctx, clone.GetPolicies()); err != nil {
			return mutationResult{}, err
		}
		return mutationResult{result: emptyResult()}, nil
	})
}

func (s *Server) GetNetworkUsage(ctx context.Context, _ *nodev1.GetNetworkUsageRequest) (*nodev1.GetNetworkUsageResponse, error) {
	if s.config.Network == nil {
		return nil, status.Error(codes.FailedPrecondition, "network accounting is not supported by this node")
	}
	usage, err := s.config.Network.GetNetworkUsage(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read network usage: %v", err)
	}
	return &nodev1.GetNetworkUsageResponse{Usage: usage}, nil
}

func (s *Server) GetOperation(ctx context.Context, request *nodev1.GetOperationRequest) (*nodev1.Operation, error) {
	if request.GetOperationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id is required")
	}
	op, err := s.config.Operations.Get(ctx, request.GetOperationId())
	if err != nil {
		return nil, journalStatus(err)
	}
	return operationToProto(op)
}

func (s *Server) WatchOperation(request *nodev1.WatchOperationRequest, stream grpc.ServerStreamingServer[nodev1.Operation]) error {
	if request.GetOperationId() == "" {
		return status.Error(codes.InvalidArgument, "operation_id is required")
	}
	if _, err := s.config.Operations.Get(stream.Context(), request.GetOperationId()); err != nil {
		return journalStatus(err)
	}
	for op := range s.config.Operations.Watch(stream.Context(), request.GetOperationId(), request.GetAfterSequence()) {
		mapped, err := operationToProto(op)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if err := stream.Send(mapped); err != nil {
			return err
		}
	}
	if err := stream.Context().Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return nil
}

func journalStatus(err error) error {
	switch {
	case errors.Is(err, operationjournal.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, operationjournal.ErrIdentityConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, operationjournal.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

var _ nodev1.NodeControlServer = (*Server)(nil)
