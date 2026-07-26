package machine

import (
	"context"
	"errors"
	"io"
	"testing"
)

// recordingDriver is a minimal Driver that records whether it was reached, so
// the dry-run test can assert the wrapper REFUSED rather than merely returned.
type recordingDriver struct{ reached []string }

func (d *recordingDriver) Runtime(context.Context) (Runtime, error) {
	d.reached = append(d.reached, "runtime")
	return Runtime{CLIVersion: "1.1.0", ServiceRunning: true}, nil
}
func (d *recordingDriver) Inspect(_ context.Context, name string) (Info, error) {
	d.reached = append(d.reached, "inspect")
	return Info{Name: name}, nil
}
func (d *recordingDriver) InspectContainer(context.Context, string) (ContainerInfo, error) {
	d.reached = append(d.reached, "inspect-container")
	return ContainerInfo{}, nil
}
func (d *recordingDriver) ImageExists(context.Context, string) (bool, error) {
	d.reached = append(d.reached, "image-inspect")
	return true, nil
}
func (d *recordingDriver) BuildImage(context.Context, BuildSpec, io.Writer) error {
	d.reached = append(d.reached, "build")
	return nil
}
func (d *recordingDriver) Create(context.Context, Spec) error {
	d.reached = append(d.reached, "create")
	return nil
}
func (d *recordingDriver) Start(context.Context, string) error {
	d.reached = append(d.reached, "start")
	return nil
}
func (d *recordingDriver) Exec(context.Context, ExecSpec) (ExecResult, error) {
	d.reached = append(d.reached, "exec")
	return ExecResult{}, nil
}

func TestDryRunRefusesEveryMutation(t *testing.T) {
	inner := &recordingDriver{}
	d := DryRun(inner)
	ctx := context.Background()

	mutations := map[string]func() error{
		"BuildImage": func() error { return d.BuildImage(ctx, BuildSpec{Tag: "x"}, nil) },
		"Create":     func() error { return d.Create(ctx, Spec{Name: "sparkbox"}) },
		"Start":      func() error { return d.Start(ctx, "sparkbox") },
		// Exec is refused even when ReadOnly: a read-only guest probe changes
		// nothing but costs seconds and can BOOT a stopped machine, and a dry
		// run must be instant and inert.
		"Exec(read-only)": func() error {
			_, err := d.Exec(ctx, ExecSpec{Machine: "sparkbox", Op: "probe", ReadOnly: true})
			return err
		},
	}
	for name, call := range mutations {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrDryRun) {
				t.Fatalf("%s: err = %v, want ErrDryRun", name, err)
			}
		})
	}
	if len(inner.reached) != 0 {
		t.Fatalf("dry-run wrapper reached the real driver: %v", inner.reached)
	}

	// Read-only calls still pass through — a dry run's job is to describe the
	// host that is actually there.
	if _, err := d.Runtime(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Inspect(ctx, "sparkbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ImageExists(ctx, "ref"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InspectContainer(ctx, "cid"); err != nil {
		t.Fatal(err)
	}
	if len(inner.reached) != 4 {
		t.Fatalf("read-only calls should reach the driver, got %v", inner.reached)
	}
}
