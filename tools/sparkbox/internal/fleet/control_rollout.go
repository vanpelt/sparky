package fleet

import "errors"

// ConfigureControlRollout persists per-operation transport overrides for an
// installed gRPC node and applies them to its current SSH generation.
func (f *Fleet) ConfigureControlRollout(node string, rollout ControlRollout) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	binding := f.grpcControls[node]
	if binding == nil {
		return errors.New("fleet: control rollout requires a configured gRPC control adapter")
	}
	if linked, ok := f.nodes[node].(*linkedRemote); ok {
		if err := linked.selector.ConfigureRollout(rollout); err != nil {
			return err
		}
	} else {
		selector := &ControlSelector{mode: binding.mode, grpc: binding.control}
		if err := selector.ConfigureRollout(rollout); err != nil {
			return err
		}
	}
	binding.rollout = rollout
	return nil
}

// ConfigureShadowInventory enables or disables SSH/gRPC cache comparison for
// a node. Enabling performs one comparison immediately when an SSH generation
// is present; later full gRPC inventory reconciliations compare automatically.
func (f *Fleet) ConfigureShadowInventory(
	node string,
	enabled bool,
	observer ShadowInventoryObserver,
) (ShadowInventoryReport, error) {
	f.mu.Lock()
	binding := f.grpcControls[node]
	if binding == nil {
		f.mu.Unlock()
		return ShadowInventoryReport{}, errors.New(
			"fleet: shadow inventory requires a configured gRPC control adapter",
		)
	}
	binding.shadow = shadowInventoryConfig{enabled: enabled, observer: observer}
	linked, _ := f.nodes[node].(*linkedRemote)
	if linked != nil {
		linked.selector.ConfigureShadowInventory(enabled, observer)
	}
	f.mu.Unlock()
	if enabled && linked != nil {
		return linked.selector.CompareShadowInventory(), nil
	}
	return ShadowInventoryReport{}, nil
}

func (f *Fleet) compareShadowInventory(node string) {
	f.mu.RLock()
	linked, _ := f.nodes[node].(*linkedRemote)
	f.mu.RUnlock()
	if linked != nil {
		linked.selector.observeShadowInventory()
	}
}
