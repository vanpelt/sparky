// Package opidentity carries a durable mutation identity across otherwise
// transport-neutral control-plane layers.
package opidentity

import (
	"context"
	"time"
)

// Identity is stable across a retry of the same logical mutation.
type Identity struct {
	OperationID    string
	IdempotencyKey string
	Initiator      string
	CreatedAt      time.Time
}

type contextKey struct{}

// WithContext attaches identity to ctx.
func WithContext(ctx context.Context, identity Identity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, contextKey{}, identity)
}

// FromContext returns the durable identity, when the caller supplied one.
func FromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	identity, ok := ctx.Value(contextKey{}).(Identity)
	return identity, ok
}
