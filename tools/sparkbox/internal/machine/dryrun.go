package machine

import (
	"context"
	"fmt"
	"io"
)

// DryRun wraps a Driver so every mutating call is refused with ErrDryRun.
//
// This is enforcement rather than convention. Provision installs it whenever
// --dry-run is set, so a darwin step that forgets its own guard fails loudly in
// the dry-run test instead of quietly building an image, creating a VM or
// running a script on the operator's live machine. The alternative — an
// `if cfg.DryRun` at the top of every Apply — is exactly the discipline that
// only has to be forgotten once.
//
// Exec is refused too, ReadOnly included. A read-only guest probe changes
// nothing, but it costs seconds per call and can BOOT a stopped machine
// (`container machine run` boots if necessary), and a dry run must be instant
// and inert. Steps get ErrDryRun back and report "not probed in --dry-run",
// which is honest, rather than implying a check they did not make.
func DryRun(d Driver) Driver { return dryRunDriver{d} }

type dryRunDriver struct{ inner Driver }

// Read-only calls pass through: a dry run's whole job is to describe the host
// in front of the operator, which means actually looking at it.
func (d dryRunDriver) Runtime(ctx context.Context) (Runtime, error) { return d.inner.Runtime(ctx) }

func (d dryRunDriver) Inspect(ctx context.Context, name string) (Info, error) {
	return d.inner.Inspect(ctx, name)
}

func (d dryRunDriver) InspectContainer(ctx context.Context, cid string) (ContainerInfo, error) {
	return d.inner.InspectContainer(ctx, cid)
}

func (d dryRunDriver) ImageExists(ctx context.Context, ref string) (bool, error) {
	return d.inner.ImageExists(ctx, ref)
}

func (d dryRunDriver) BuildImage(_ context.Context, s BuildSpec, _ io.Writer) error {
	return fmt.Errorf("build image %s: %w", s.Tag, ErrDryRun)
}

func (d dryRunDriver) Create(_ context.Context, s Spec) error {
	return fmt.Errorf("create machine %s: %w", s.Name, ErrDryRun)
}

func (d dryRunDriver) Start(_ context.Context, name string) error {
	return fmt.Errorf("start machine %s: %w", name, ErrDryRun)
}

func (d dryRunDriver) Exec(_ context.Context, s ExecSpec) (ExecResult, error) {
	return ExecResult{}, fmt.Errorf("%s: %w", s.Op, ErrDryRun)
}
