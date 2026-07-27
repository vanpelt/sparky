// Package grpccontrol adapts the node's host manager to the durable gRPC
// control API. It deliberately owns no listener or certificate material:
// callers supply an authenticated TLS configuration when constructing a gRPC
// server or dialing a node.
package grpccontrol

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"runtime"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/eventjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/operationjournal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Backend is the host-manager surface exposed by NodeControl. *host.Manager
// satisfies this interface.
type Backend interface {
	Get(name string) (*host.Sandbox, bool)
	List() []*host.Sandbox
	AllSnapshots() []*host.Snapshot
	Capacity() host.NodeCapacity
	Vitals(context.Context, string) (host.Vitals, error)

	EnsureReady(context.Context, string) (*host.Sandbox, error)
	Create(context.Context, string, string, string, int64, int64) (*host.Sandbox, error)
	Pause(context.Context, string) error
	Archive(context.Context, string) error
	Resize(context.Context, string, int64) error
	Reboot(context.Context, string) error
	Rename(context.Context, string, string, string) error
	Destroy(context.Context, string) error
	SetPinned(string, bool) error
	ResyncEnv(context.Context, string)
	Snapshot(context.Context, string, string, string) (*host.Snapshot, error)
	DeleteSnapshot(context.Context, string, string) error
	Fork(context.Context, string, string, string, int64, int64) (*host.Sandbox, error)

	MarkActive(string)
	RecordKey(string, string)
}

var _ Backend = (*host.Manager)(nil)

// NetworkHooks connects the transport-neutral control API to the node's
// network-policy implementation. A nil hook reports current usage from the
// manager's durable counters, while policy mutations fail as unsupported.
type NetworkHooks interface {
	ApplyNetworkPolicy(context.Context, []*nodev1.NetworkPolicy) error
	GetNetworkUsage(context.Context) ([]*nodev1.NetworkUsage, error)
}

// ServerConfig contains adapter metadata and dependencies. Context controls
// detached mutation work: cancelling it stops new work from completing after
// service shutdown. It defaults to context.Background.
type ServerConfig struct {
	Context    context.Context
	Backend    Backend
	Operations *operationjournal.Journal
	Events     *eventjournal.Journal
	Network    NetworkHooks

	Node         string
	Version      string
	StartedAt    time.Time
	Architecture string
	OS           string
	Release      string
	Driver       string
	Capabilities []string
	// SandboxEventsFromObserver means a host.Observer is journaling sandbox
	// changes independently. It prevents mutation handlers from recording the
	// same manager event a second time; snapshot events remain handler-owned.
	SandboxEventsFromObserver bool
}

func (c *ServerConfig) normalize() error {
	if c.Backend == nil {
		return errors.New("grpccontrol: backend is required")
	}
	if c.Operations == nil {
		return errors.New("grpccontrol: operation journal is required")
	}
	if c.Events == nil {
		return errors.New("grpccontrol: event journal is required")
	}
	if c.Context == nil {
		c.Context = context.Background()
	}
	if c.StartedAt.IsZero() {
		c.StartedAt = time.Now().UTC()
	} else {
		c.StartedAt = c.StartedAt.UTC()
	}
	if c.Architecture == "" {
		c.Architecture = runtime.GOARCH
	}
	if c.OS == "" {
		c.OS = runtime.GOOS
	}
	if c.Node == "" {
		c.Node = c.Backend.Capacity().Node
	}
	if len(c.Capabilities) == 0 {
		c.Capabilities = []string{"durable_operations_v1", "inventory_events_v1"}
	}
	c.Capabilities = append([]string(nil), c.Capabilities...)
	if c.Network != nil {
		c.Capabilities = append(c.Capabilities, "network_policy_v1")
	}
	return nil
}

// NewRPCServer constructs and registers a TLS-only NodeControl server.
// tlsConfig should normally come from nodecert.ServerTLSConfig. There is
// intentionally no insecure constructor.
func NewRPCServer(service *Server, tlsConfig *tls.Config, opts ...grpc.ServerOption) (*grpc.Server, error) {
	if service == nil {
		return nil, errors.New("grpccontrol: service is required")
	}
	if tlsConfig == nil {
		return nil, errors.New("grpccontrol: TLS configuration is required")
	}
	if len(tlsConfig.Certificates) == 0 && tlsConfig.GetCertificate == nil {
		return nil, errors.New("grpccontrol: server certificate is required")
	}
	switch tlsConfig.ClientAuth {
	case tls.RequireAnyClientCert, tls.RequireAndVerifyClientCert:
	default:
		return nil, errors.New("grpccontrol: server TLS must require a client certificate")
	}
	opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfig.Clone())))
	server := grpc.NewServer(opts...)
	nodev1.RegisterNodeControlServer(server, service)
	return server, nil
}

// RequestHash returns the canonical hash of a mutation request's immutable
// fields. The operation identity is excluded, so retries may preserve the same
// request while reusing its identity.
func RequestHash(request proto.Message) ([]byte, error) {
	if request == nil {
		return nil, errors.New("grpccontrol: nil mutation request")
	}
	cloned := proto.Clone(request)
	reflected := cloned.ProtoReflect()
	operation := reflected.Descriptor().Fields().ByName("operation")
	if operation == nil {
		return nil, errors.New("grpccontrol: request has no operation identity")
	}
	reflected.Clear(operation)
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(cloned)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return sum[:], nil
}

// NewOperationIdentity builds the identity a caller assigns to a mutation
// request after populating all immutable request fields.
func NewOperationIdentity(request proto.Message, operationID, idempotencyKey, initiator string, createdAt time.Time) (*nodev1.OperationIdentity, error) {
	hash, err := RequestHash(request)
	if err != nil {
		return nil, err
	}
	if operationID == "" || idempotencyKey == "" || initiator == "" {
		return nil, operationjournal.ErrInvalid
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return &nodev1.OperationIdentity{
		OperationId:    operationID,
		IdempotencyKey: idempotencyKey,
		RequestHash:    hash,
		Initiator:      initiator,
		CreatedAt:      timestamppb.New(createdAt.UTC()),
	}, nil
}

// RemoteError is a terminal operation failure returned by the gateway client.
type RemoteError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *RemoteError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func operationError(op *nodev1.Operation) error {
	if op == nil || op.Error == nil {
		return nil
	}
	return &RemoteError{
		Code:      op.Error.Code,
		Message:   op.Error.Message,
		Retryable: op.Error.Retryable,
	}
}
