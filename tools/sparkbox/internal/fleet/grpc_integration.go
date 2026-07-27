package fleet

import (
	"context"
	"errors"
	"sync"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

type grpcBinding struct {
	control *GRPCControl
	mode    ControlTransport
	rollout ControlRollout
	shadow  shadowInventoryConfig
}

// InstallGRPCControl starts and persists one node's gRPC control adapter.
// Existing and future SSH links for that node retain their GuestDialer; only
// their ControlPlane is switched. The returned function removes this exact
// adapter and restores SSH-only control.
func (f *Fleet) InstallGRPCControl(
	ctx context.Context,
	mode ControlTransport,
	opts GRPCControlOptions,
) (*GRPCControl, func(), error) {
	if !mode.valid() {
		return nil, nil, errors.New("fleet: control transport must be auto, grpc, or ssh")
	}
	if opts.Node == "" {
		return nil, nil, errors.New("fleet: gRPC control needs a node name")
	}
	if opts.Log == nil {
		opts.Log = f.log
	}
	if opts.Metrics == nil {
		opts.Metrics = f.metrics
	}

	userInventory := opts.OnInventory
	opts.OnInventory = func(inventory nodelink.InventoryMsg) {
		f.Reconcile(opts.Node, inventory)
		f.compareShadowInventory(opts.Node)
		if userInventory != nil {
			userInventory(inventory)
		}
	}
	userChanged := opts.OnChanged
	opts.OnChanged = func(changed nodelink.ChangedMsg) {
		f.ApplyChanged(opts.Node, changed)
		if userChanged != nil {
			userChanged(changed)
		}
	}
	userGone := opts.OnGone
	opts.OnGone = func(gone nodelink.GoneMsg) {
		f.ApplyGone(opts.Node, gone)
		if userGone != nil {
			userGone(gone)
		}
	}
	userPaused := opts.OnPaused
	opts.OnPaused = func(paused nodelink.PausedMsg) {
		f.ApplyPaused(opts.Node, paused)
		if userPaused != nil {
			userPaused(paused)
		}
	}

	f.mu.RLock()
	current := f.nodes[opts.Node]
	f.mu.RUnlock()
	if current != nil && opts.Facts.Node == "" {
		opts.Facts = current.Facts()
	}
	control, err := NewGRPCControl(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	binding := &grpcBinding{control: control, mode: mode}

	f.mu.Lock()
	old := f.grpcControls[opts.Node]
	if old != nil {
		binding.rollout = old.rollout
		binding.shadow = old.shadow
	}
	f.grpcControls[opts.Node] = binding
	if linked, ok := f.nodes[opts.Node].(*linkedRemote); ok {
		err = linked.selector.Configure(mode, control)
		if err == nil {
			err = linked.selector.ConfigureRollout(binding.rollout)
		}
		if err == nil {
			linked.selector.ConfigureShadowInventory(binding.shadow.enabled, binding.shadow.observer)
		}
	}
	guestBinding := f.routedGuests[opts.Node]
	if err == nil && guestBinding != nil {
		guestBinding.resolver.set(control)
		if linked, ok := f.nodes[opts.Node].(*linkedRemote); ok {
			err = linked.guestSelector.Configure(guestBinding.mode, control, guestBinding.dialer)
		}
	}
	if err != nil {
		if old == nil {
			delete(f.grpcControls, opts.Node)
		} else {
			f.grpcControls[opts.Node] = old
		}
		if guestBinding != nil {
			var prior *GRPCControl
			if old != nil {
				prior = old.control
			}
			guestBinding.resolver.set(prior)
			if linked, ok := f.nodes[opts.Node].(*linkedRemote); ok {
				_ = linked.guestSelector.Configure(guestBinding.mode, prior, guestBinding.dialer)
			}
		}
		if linked, ok := f.nodes[opts.Node].(*linkedRemote); ok {
			if old == nil {
				_ = linked.selector.ConfigureRollout(ControlRollout{})
				_ = linked.selector.Configure(ControlTransportSSH, nil)
				linked.selector.ConfigureShadowInventory(false, nil)
			} else {
				_ = linked.selector.Configure(old.mode, old.control)
				_ = linked.selector.ConfigureRollout(old.rollout)
				linked.selector.ConfigureShadowInventory(old.shadow.enabled, old.shadow.observer)
			}
		}
	}
	f.mu.Unlock()
	if err != nil {
		_ = control.Close()
		return nil, nil, err
	}
	if old != nil {
		_ = old.control.Close()
	}

	var once sync.Once
	remove := func() {
		once.Do(func() {
			f.mu.Lock()
			if f.grpcControls[opts.Node] == binding {
				delete(f.grpcControls, opts.Node)
				if guestBinding := f.routedGuests[opts.Node]; guestBinding != nil {
					guestBinding.resolver.set(nil)
				}
				if linked, ok := f.nodes[opts.Node].(*linkedRemote); ok {
					_ = linked.selector.ConfigureRollout(ControlRollout{})
					_ = linked.selector.Configure(ControlTransportSSH, nil)
					linked.selector.ConfigureShadowInventory(false, nil)
					if guestBinding := f.routedGuests[opts.Node]; guestBinding != nil {
						_ = linked.guestSelector.Configure(guestBinding.mode, nil, guestBinding.dialer)
					}
				}
			}
			f.mu.Unlock()
			_ = control.Close()
		})
	}
	return control, remove, nil
}
