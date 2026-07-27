package fleet

import (
	"errors"
	"net/netip"
	"sync"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routedguest"
)

type routedResolver struct {
	mu      sync.RWMutex
	control *GRPCControl
}

func (r *routedResolver) set(control *GRPCControl) {
	r.mu.Lock()
	r.control = control
	r.mu.Unlock()
}

func (r *routedResolver) get() *GRPCControl {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.control
}

func (r *routedResolver) box(name string) (*host.Sandbox, bool) {
	control := r.get()
	if control == nil {
		return nil, false
	}
	return control.RoutedBox(name)
}

type routedGuestBinding struct {
	mode          GuestTransport
	resolver      *routedResolver
	dialer        GuestDialer
	canaryPercent int
}

// InstallRoutedGuest persists one node's guest data-plane selection across SSH
// reconnects. prefix must be the canonical, roster-approved IPv4 prefix. Auto
// requires a live gRPC adapter as its authoritative address source but falls
// back to the node's independently supervised SSH data pool when gRPC is
// unhealthy or a routed socket attempt returns a typed route failure.
func (f *Fleet) InstallRoutedGuest(node string, mode GuestTransport, prefix string) error {
	if node == "" {
		return errors.New("fleet: routed guest transport needs a node name")
	}
	if !mode.valid() {
		return errors.New("fleet: guest transport must be auto, routed, or ssh")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if mode == GuestTransportSSH {
		delete(f.routedGuests, node)
		if linked, ok := f.nodes[node].(*linkedRemote); ok {
			return linked.guestSelector.Configure(GuestTransportSSH, nil, nil)
		}
		return nil
	}

	controlBinding := f.grpcControls[node]
	if controlBinding == nil || controlBinding.control == nil {
		return errors.New("fleet: routed guest transport requires a configured gRPC control adapter")
	}
	approved, err := netip.ParsePrefix(prefix)
	if err != nil {
		return errors.Join(routedguest.ErrInvalidPrefix, err)
	}
	resolver := &routedResolver{control: controlBinding.control}
	dialer, err := routedguest.New(approved, resolver.box, routedguest.Options{})
	if err != nil {
		return err
	}
	instrumented := &routedMetricsDialer{node: node, next: dialer, metrics: f.metrics}
	binding := &routedGuestBinding{
		mode: mode, resolver: resolver, dialer: instrumented, canaryPercent: 100,
	}
	if linked, ok := f.nodes[node].(*linkedRemote); ok {
		if err := linked.guestSelector.Configure(mode, controlBinding.control, instrumented); err != nil {
			return err
		}
		if err := linked.guestSelector.ConfigureCanary(binding.canaryPercent); err != nil {
			return err
		}
	}
	f.routedGuests[node] = binding
	return nil
}

// ConfigureRoutedCanary persists a deterministic per-node percentage for auto
// mode. Zero keeps every new connection on SSH; one hundred preserves auto's
// original preference for healthy routed data.
func (f *Fleet) ConfigureRoutedCanary(node string, percent int) error {
	if percent < 0 || percent > 100 {
		return errors.New("fleet: routed guest canary percent must be between 0 and 100")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	binding := f.routedGuests[node]
	if binding == nil {
		return errors.New("fleet: routed guest canary requires an installed routed guest transport")
	}
	if linked, ok := f.nodes[node].(*linkedRemote); ok {
		if err := linked.guestSelector.ConfigureCanary(percent); err != nil {
			return err
		}
	}
	binding.canaryPercent = percent
	return nil
}
