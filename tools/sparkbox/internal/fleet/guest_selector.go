package fleet

import (
	"context"
	"errors"
	"hash/fnv"
	"net"
	"sync"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routedguest"
)

// GuestTransport is the operator's guest data-plane rollout choice.
type GuestTransport string

const (
	GuestTransportAuto   GuestTransport = "auto"
	GuestTransportRouted GuestTransport = "routed"
	GuestTransportSSH    GuestTransport = "ssh"
)

func (mode GuestTransport) valid() bool {
	return mode == GuestTransportAuto || mode == GuestTransportRouted || mode == GuestTransportSSH
}

type grpcGuestState interface {
	Healthy() bool
	Facts() Facts
}

// GuestSelector switches one linked node's data plane without changing its
// control selection. Auto enters the routed path only while its authoritative
// gRPC inventory is healthy and advertises routed_guest_v1;
// routedguest.AutoDialer then permits SSH fallback only for a typed
// socket-route failure.
type GuestSelector struct {
	mu            sync.RWMutex
	mode          GuestTransport
	health        grpcGuestState
	routed        GuestDialer
	ssh           GuestDialer
	node          string
	metrics       *fleetmetrics.Registry
	canaryPercent uint8
}

var _ GuestDialer = (*GuestSelector)(nil)

func NewGuestSelector(mode GuestTransport, health grpcGuestState, routed, ssh GuestDialer) (*GuestSelector, error) {
	if ssh == nil {
		return nil, errors.New("fleet: SSH fallback guest dialer is required")
	}
	selector := &GuestSelector{ssh: ssh, canaryPercent: 100}
	if err := selector.Configure(mode, health, routed); err != nil {
		return nil, err
	}
	return selector, nil
}

// ConfigureCanary bounds the share of auto-mode sandboxes selected for routed
// dialing on this node. Explicit ssh/routed modes ignore the canary.
func (s *GuestSelector) ConfigureCanary(percent int) error {
	if percent < 0 || percent > 100 {
		return errors.New("fleet: routed guest canary percent must be between 0 and 100")
	}
	s.mu.Lock()
	s.canaryPercent = uint8(percent)
	s.mu.Unlock()
	return nil
}

// Configure atomically changes the preferred data transport. Explicit routed
// mode is rejected without a prefix-confined dialer rather than silently
// becoming SSH.
func (s *GuestSelector) Configure(mode GuestTransport, health grpcGuestState, routed GuestDialer) error {
	if !mode.valid() {
		return errors.New("fleet: guest transport must be auto, routed, or ssh")
	}
	if mode != GuestTransportSSH && routed == nil {
		return errors.New("fleet: routed guest transport has no routed dialer")
	}
	if routed != nil {
		if _, err := routedguest.NewAuto(routed, s.ssh, nil); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.mode, s.health, s.routed = mode, health, routed
	s.mu.Unlock()
	return nil
}

func (s *GuestSelector) setMetrics(node string, metrics *fleetmetrics.Registry) {
	s.mu.Lock()
	s.node, s.metrics = node, metrics
	s.mu.Unlock()
}

func (s *GuestSelector) DialGuest(ctx context.Context, sandbox, kind string, port int) (net.Conn, error) {
	s.mu.RLock()
	mode, health, routed, ssh := s.mode, s.health, s.routed, s.ssh
	node, metrics := s.node, s.metrics
	canaryPercent := s.canaryPercent
	s.mu.RUnlock()

	switch mode {
	case GuestTransportRouted:
		if health == nil || !health.Healthy() || !routedGuestCapable(health) {
			return nil, Unreachable("dial", sandbox, node)
		}
		return routed.DialGuest(ctx, sandbox, kind, port)
	case GuestTransportAuto:
		if health != nil && health.Healthy() && routedGuestCapable(health) &&
			RoutedCanarySelected(node, sandbox, int(canaryPercent)) {
			auto, err := routedguest.NewAuto(routed, ssh, func(outcome routedguest.FallbackOutcome) {
				metrics.IncRouteFallback(node, string(outcome))
			})
			if err != nil {
				return nil, err
			}
			connection, primaryErr := auto.DialGuest(ctx, sandbox, kind, port)
			if primaryErr == nil || !errors.Is(primaryErr, nodelink.ErrUnknownSandbox) ||
				health.Healthy() {
				return connection, primaryErr
			}

			// RoutedBox deliberately becomes unavailable as soon as its
			// authoritative gRPC control turns unhealthy. If that transition
			// happens after auto mode selected the routed path but before its
			// resolver runs, the resulting "unknown sandbox" is a transport
			// transition rather than an authoritative placement refusal. This
			// is the one additional case where SSH fallback is safe.
			if err := ctx.Err(); err != nil {
				metrics.IncRouteFallback(node, string(routedguest.FallbackSuppressed))
				return nil, err
			}
			connection, fallbackErr := ssh.DialGuest(ctx, sandbox, kind, port)
			if fallbackErr == nil {
				metrics.IncRouteFallback(node, string(routedguest.FallbackConnected))
				return connection, nil
			}
			if err := ctx.Err(); err != nil {
				metrics.IncRouteFallback(node, string(routedguest.FallbackSuppressed))
				return nil, err
			}
			metrics.IncRouteFallback(node, string(routedguest.FallbackFailed))
			return nil, &routedguest.FallbackError{Primary: primaryErr, Fallback: fallbackErr}
		}
	}
	return ssh.DialGuest(ctx, sandbox, kind, port)
}

// RoutedCanarySelected deterministically assigns a sandbox on a node to the
// configured routed percentage. Stable assignment avoids connection-by-
// connection flapping while pooled connections drain during rollout.
func RoutedCanarySelected(node, sandbox string, percent int) bool {
	switch {
	case percent <= 0:
		return false
	case percent >= 100:
		return true
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(node))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(sandbox))
	return int(hash.Sum32()%100) < percent
}

func routedGuestCapable(state grpcGuestState) bool {
	if state == nil {
		return false
	}
	for _, capability := range state.Facts().Capabilities {
		if capability == nodelink.CapabilityRoutedGuestV1 {
			return true
		}
	}
	return false
}
