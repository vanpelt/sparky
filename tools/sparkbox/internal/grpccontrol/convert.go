package grpccontrol

import (
	"errors"
	"fmt"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/operationjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func sandboxToProto(box *host.Sandbox) *nodev1.Sandbox {
	if box == nil {
		return nil
	}
	state := nodev1.SandboxState_SANDBOX_STATE_UNSPECIFIED
	switch box.State {
	case vmm.StateRunning:
		state = nodev1.SandboxState_SANDBOX_STATE_RUNNING
	case vmm.StatePaused:
		state = nodev1.SandboxState_SANDBOX_STATE_PAUSED
	case vmm.StateArchived:
		state = nodev1.SandboxState_SANDBOX_STATE_ARCHIVED
	}
	out := &nodev1.Sandbox{
		Name:           box.Name,
		Owner:          box.Owner,
		Image:          box.Image,
		Vcpus:          box.VCPUs,
		MemoryMb:       box.MemMB,
		State:          state,
		SshUser:        box.SSHUser,
		CreatedAt:      timestamp(box.CreatedAt),
		LastActive:     timestamp(box.LastActive),
		Pinned:         box.Pinned,
		Ballooned:      box.Ballooned,
		KeyFingerprint: box.KeyFP,
		NetworkRxBytes: box.NetRxBytes,
		NetworkTxBytes: box.NetTxBytes,
		ArchivedAt:     timestamp(box.ArchivedAt),
		DiskMb:         box.DiskMB,
		DiskTotalMb:    box.DiskTotalMB,
		HostIp:         box.HostIP,
	}
	return out
}

func snapshotToProto(snapshot *host.Snapshot) *nodev1.Snapshot {
	if snapshot == nil {
		return nil
	}
	return &nodev1.Snapshot{
		Name:        snapshot.Name,
		Owner:       snapshot.Owner,
		Image:       snapshot.Image,
		FromSandbox: snapshot.FromBox,
		CreatedAt:   timestamp(snapshot.CreatedAt),
	}
}

func timestamp(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() {
		return nil
	}
	return timestamppb.New(value.UTC())
}

func operationToProto(op operationjournal.Operation) (*nodev1.Operation, error) {
	out := &nodev1.Operation{
		OperationId:    op.ID,
		IdempotencyKey: op.IdempotencyKey,
		RequestHash:    append([]byte(nil), op.RequestHash...),
		Initiator:      op.Initiator,
		CreatedAt:      timestamppb.New(op.CreatedAt),
		UpdatedAt:      timestamppb.New(op.UpdatedAt),
		State:          operationStateToProto(op.State),
		Sequence:       op.Sequence,
		Kind:           op.Kind,
		Target:         op.Target,
	}
	if len(op.Result) > 0 {
		out.Result = new(nodev1.OperationResult)
		if err := proto.Unmarshal(op.Result, out.Result); err != nil {
			return nil, fmt.Errorf("decode operation %s result: %w", op.ID, err)
		}
	}
	if op.Failure != nil {
		out.Error = &nodev1.OperationError{
			Code:      op.Failure.Code,
			Message:   op.Failure.Message,
			Retryable: op.Failure.Retryable,
		}
	}
	return out, nil
}

func operationStateToProto(state operationjournal.State) nodev1.OperationState {
	switch state {
	case operationjournal.StatePending:
		return nodev1.OperationState_OPERATION_STATE_PENDING
	case operationjournal.StateRunning:
		return nodev1.OperationState_OPERATION_STATE_RUNNING
	case operationjournal.StateSucceeded:
		return nodev1.OperationState_OPERATION_STATE_SUCCEEDED
	case operationjournal.StateFailed:
		return nodev1.OperationState_OPERATION_STATE_FAILED
	case operationjournal.StateCancelled:
		return nodev1.OperationState_OPERATION_STATE_CANCELLED
	default:
		return nodev1.OperationState_OPERATION_STATE_UNSPECIFIED
	}
}

func emptyResult() *nodev1.OperationResult {
	return &nodev1.OperationResult{
		Result: &nodev1.OperationResult_Empty{Empty: &emptypb.Empty{}},
	}
}

func sandboxResult(box *host.Sandbox) *nodev1.OperationResult {
	return &nodev1.OperationResult{
		Result: &nodev1.OperationResult_Sandbox{Sandbox: sandboxToProto(box)},
	}
}

func snapshotResult(snapshot *host.Snapshot) *nodev1.OperationResult {
	return &nodev1.OperationResult{
		Result: &nodev1.OperationResult_Snapshot{Snapshot: snapshotToProto(snapshot)},
	}
}

func failureFromError(err error) operationjournal.Failure {
	failure := operationjournal.Failure{Code: "internal", Message: err.Error(), Retryable: true}
	var (
		nameError     *host.NameError
		missingError  *host.MissingError
		stateError    *host.StateError
		disabledError *host.DisabledError
		limitError    *host.LimitError
		capacityError *host.CapacityError
		quotaError    *host.DiskQuotaError
	)
	switch {
	case errors.As(err, &nameError):
		failure.Retryable = false
		switch nameError.Problem {
		case host.NameTaken:
			failure.Code = "name_taken"
		case host.NameReserved:
			failure.Code = "name_reserved"
		default:
			failure.Code = "invalid_name"
		}
	case errors.As(err, &missingError):
		failure.Code, failure.Retryable = "not_found", false
	case errors.As(err, &stateError):
		failure.Code, failure.Retryable = stateError.Code, false
	case errors.As(err, &disabledError):
		failure.Code, failure.Retryable = disabledError.Code, false
	case errors.As(err, &limitError):
		failure.Code, failure.Retryable = "running_limit", false
	case errors.As(err, &capacityError):
		failure.Code = "capacity"
	case errors.As(err, &quotaError):
		failure.Code, failure.Retryable = "disk_quota", false
	}
	return failure
}
