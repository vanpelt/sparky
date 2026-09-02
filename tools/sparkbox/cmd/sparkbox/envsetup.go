package main

// The guest door of `env build`, and the one type conversion that lets it exist.

import (
	"context"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
)

// envSetupOps is what the metadata service asks when a guest knocks on
// /self/setup: does this sandbox have a setup script to run, and — later —
// here is what happened when it ran one.
//
// It exists as an adapter in main rather than as a method set on *ctlops.Ops
// for the reason selfLifecycleOps does, and it is the same reason:
// internal/metadata imports internal/ctlops (selflifecycle.go, for
// ctlops.SelfSnapshotPlan), so ctlops cannot name metadata.SetupResult. The two
// structs are declared field for field alike, which makes the conversion below
// legal and makes any future divergence a compile error in this file — which is
// the failure mode to want, since the alternative is a field that silently
// stops being carried.
//
// There is no elevation here of the kind selfLifecycleOps documents: neither
// method takes a Caller at all. The sandbox record the fleet resolved from the
// tap is the whole of the authority, and ctlops does the rest — it matches the
// box against the environment row that names it as its builder AND compares the
// row's owner against the record's, because sandbox names are global and
// `web-build` is a name anybody may take.
type envSetupOps struct{ ops *ctlops.Ops }

func (e envSetupOps) SetupFor(ctx context.Context, box *host.Sandbox) (script, env string, ok bool, err error) {
	return e.ops.SetupFor(ctx, box)
}

func (e envSetupOps) SetupDone(ctx context.Context, box *host.Sandbox, r metadata.SetupResult) error {
	return e.ops.SetupDone(ctx, box, ctlops.SetupReport(r))
}

// Asserted here rather than left to the wiring: a signature drift on either
// side should fail the build of this package, not the read of a 501 in
// production.
var _ metadata.EnvSetup = envSetupOps{}

// And the fleet's half, which takes ctlops.SetupReport and so needs no adapter
// at all: *Ops satisfies it directly. Asserted for the same reason — the fleet
// is what answers a builder on a NODE, which on a control-plane-only gateway is
// every builder there is.
var _ fleet.EnvSetup = (*ctlops.Ops)(nil)
