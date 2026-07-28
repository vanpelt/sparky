package grpccontrol

import (
	"context"
	"crypto/tls"
	"errors"
	"io"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// Client is the gateway-side durable NodeControl wrapper.
type Client struct {
	connection *grpc.ClientConn
	control    nodev1.NodeControlClient
}

// DialTLS dials a NodeControl endpoint with caller-supplied TLS credentials.
// tlsConfig should normally come from nodecert.ClientTLSConfig. There is
// intentionally no insecure dial path in this package.
func DialTLS(ctx context.Context, target string, tlsConfig *tls.Config, opts ...grpc.DialOption) (*Client, error) {
	if target == "" {
		return nil, errors.New("grpccontrol: target is required")
	}
	if tlsConfig == nil {
		return nil, errors.New("grpccontrol: TLS configuration is required")
	}
	if len(tlsConfig.Certificates) == 0 && tlsConfig.GetClientCertificate == nil {
		return nil, errors.New("grpccontrol: gateway client certificate is required")
	}
	if tlsConfig.MinVersion < tls.VersionTLS13 {
		return nil, errors.New("grpccontrol: client TLS must require TLS 1.3")
	}
	if tlsConfig.VerifyConnection == nil {
		return nil, errors.New("grpccontrol: client TLS must verify the node SPIFFE identity")
	}
	opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig.Clone())))
	connection, err := grpc.DialContext(ctx, target, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{connection: connection, control: nodev1.NewNodeControlClient(connection)}, nil
}

// WrapClient builds the durable gateway behavior over a generated client. It
// is intended for tests and for callers that already own an authenticated gRPC
// connection.
func WrapClient(control nodev1.NodeControlClient) (*Client, error) {
	if control == nil {
		return nil, errors.New("grpccontrol: NodeControl client is required")
	}
	return &Client{control: control}, nil
}

func (c *Client) Close() error {
	if c == nil || c.connection == nil {
		return nil
	}
	return c.connection.Close()
}

// Raw exposes the generated client for rollout probes that have not yet moved
// to the durable wrapper.
func (c *Client) Raw() nodev1.NodeControlClient { return c.control }

func (c *Client) Inventory(ctx context.Context) (*nodev1.Inventory, error) {
	return c.control.GetInventory(ctx, &nodev1.GetInventoryRequest{})
}

func (c *Client) Capacity(ctx context.Context) (*nodev1.Capacity, error) {
	return c.control.GetCapacity(ctx, &nodev1.GetCapacityRequest{})
}

func (c *Client) Health(ctx context.Context) (*nodev1.HealthResponse, error) {
	return c.control.Health(ctx, &nodev1.HealthRequest{})
}

func (c *Client) Vitals(ctx context.Context, sandbox string) (*nodev1.Vitals, error) {
	return c.control.GetVitals(ctx, &nodev1.GetVitalsRequest{Sandbox: sandbox})
}

// WatchEvents streams replayed and live inventory events. An EventGap is a
// normal event, not a transport error; consumers must reconcile with Inventory
// before resuming from the returned current revision.
func (c *Client) WatchEvents(ctx context.Context, after uint64) (<-chan *nodev1.InventoryEvent, <-chan error) {
	out := make(chan *nodev1.InventoryEvent, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		stream, err := c.control.WatchEvents(ctx, &nodev1.WatchEventsRequest{AfterRevision: after})
		if err != nil {
			select {
			case errs <- err:
			case <-ctx.Done():
			}
			return
		}
		for {
			event, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				select {
				case errs <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
			if event.GetGap() != nil {
				return
			}
		}
	}()
	return out, errs
}

// GetOperation reattaches to a durable operation without starting work.
func (c *Client) GetOperation(ctx context.Context, operationID string) (*nodev1.Operation, error) {
	return c.control.GetOperation(ctx, &nodev1.GetOperationRequest{OperationId: operationID})
}

// WaitOperation follows an operation from its current sequence to a terminal
// state. If a stream is interrupted it re-reads the journal and attaches a new
// watch, so a gateway connection restart does not lose the result.
func (c *Client) WaitOperation(ctx context.Context, current *nodev1.Operation) (*nodev1.Operation, error) {
	if current == nil || current.GetOperationId() == "" {
		return nil, errors.New("grpccontrol: operation is required")
	}
	for {
		if operationTerminal(current.GetState()) {
			return current, operationError(current)
		}
		stream, err := c.control.WatchOperation(ctx, &nodev1.WatchOperationRequest{
			OperationId:   current.GetOperationId(),
			AfterSequence: current.GetSequence(),
		})
		if err == nil {
			for {
				next, recvErr := stream.Recv()
				if recvErr == nil {
					current = next
					if operationTerminal(current.GetState()) {
						return current, operationError(current)
					}
					continue
				}
				err = recvErr
				break
			}
		}
		if ctx.Err() != nil {
			return current, ctx.Err()
		}
		reattached, getErr := c.GetOperation(ctx, current.GetOperationId())
		if getErr != nil {
			if errors.Is(err, io.EOF) {
				return current, getErr
			}
			return current, err
		}
		current = reattached
	}
}

// ReattachOperation fetches a durable operation and waits for its terminal
// result. It is the gateway restart path.
func (c *Client) ReattachOperation(ctx context.Context, operationID string) (*nodev1.Operation, error) {
	op, err := c.GetOperation(ctx, operationID)
	if err != nil {
		return nil, err
	}
	return c.WaitOperation(ctx, op)
}

type beginCall func(context.Context) (*nodev1.Operation, error)

func (c *Client) run(ctx context.Context, operationID string, begin beginCall) (*nodev1.Operation, error) {
	op, err := begin(ctx)
	if err != nil {
		if operationID == "" || !uncertainMutationReply(err) || ctx.Err() != nil {
			return nil, err
		}
		reattached, getErr := c.GetOperation(ctx, operationID)
		if getErr != nil {
			return nil, err
		}
		op = reattached
	}
	return c.WaitOperation(ctx, op)
}

func uncertainMutationReply(err error) bool {
	switch status.Code(err) {
	case codes.Unknown, codes.Internal, codes.Unavailable, codes.DeadlineExceeded:
		return true
	default:
		return false
	}
}

func operationTerminal(state nodev1.OperationState) bool {
	switch state {
	case nodev1.OperationState_OPERATION_STATE_SUCCEEDED,
		nodev1.OperationState_OPERATION_STATE_FAILED,
		nodev1.OperationState_OPERATION_STATE_CANCELLED:
		return true
	default:
		return false
	}
}

func operationID(identity *nodev1.OperationIdentity) string {
	if identity == nil {
		return ""
	}
	return identity.GetOperationId()
}

func (c *Client) EnsureRunning(ctx context.Context, request *nodev1.EnsureRunningRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.EnsureRunning(ctx, request)
	})
}

func (c *Client) Create(ctx context.Context, request *nodev1.CreateRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginCreate(ctx, request)
	})
}

func (c *Client) Pause(ctx context.Context, request *nodev1.PauseRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginPause(ctx, request)
	})
}

func (c *Client) Archive(ctx context.Context, request *nodev1.ArchiveRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginArchive(ctx, request)
	})
}

func (c *Client) Resize(ctx context.Context, request *nodev1.ResizeRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginResize(ctx, request)
	})
}

func (c *Client) Reboot(ctx context.Context, request *nodev1.RebootRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginReboot(ctx, request)
	})
}

func (c *Client) SetTurbo(ctx context.Context, request *nodev1.SetTurboRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginSetTurbo(ctx, request)
	})
}

func (c *Client) Rename(ctx context.Context, request *nodev1.RenameRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginRename(ctx, request)
	})
}

func (c *Client) Destroy(ctx context.Context, request *nodev1.DestroyRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginDestroy(ctx, request)
	})
}

func (c *Client) SetPinned(ctx context.Context, request *nodev1.SetPinnedRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginSetPinned(ctx, request)
	})
}

func (c *Client) ResyncEnvironment(ctx context.Context, request *nodev1.ResyncEnvironmentRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginResyncEnvironment(ctx, request)
	})
}

func (c *Client) Snapshot(ctx context.Context, request *nodev1.SnapshotRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginSnapshot(ctx, request)
	})
}

func (c *Client) DeleteSnapshot(ctx context.Context, request *nodev1.DeleteSnapshotRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginDeleteSnapshot(ctx, request)
	})
}

func (c *Client) Fork(ctx context.Context, request *nodev1.ForkRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.BeginFork(ctx, request)
	})
}

func (c *Client) ApplyNetworkPolicy(ctx context.Context, request *nodev1.ApplyNetworkPolicyRequest) (*nodev1.Operation, error) {
	return c.run(ctx, operationID(request.GetOperation()), func(ctx context.Context) (*nodev1.Operation, error) {
		return c.control.ApplyNetworkPolicy(ctx, request)
	})
}

func (c *Client) MarkActive(ctx context.Context, request *nodev1.MarkActiveRequest) error {
	_, err := c.control.MarkActive(ctx, request)
	return err
}

func (c *Client) RecordKey(ctx context.Context, request *nodev1.RecordKeyRequest) error {
	_, err := c.control.RecordKey(ctx, request)
	return err
}

func (c *Client) NetworkUsage(ctx context.Context) (*nodev1.GetNetworkUsageResponse, error) {
	return c.control.GetNetworkUsage(ctx, &nodev1.GetNetworkUsageRequest{})
}
