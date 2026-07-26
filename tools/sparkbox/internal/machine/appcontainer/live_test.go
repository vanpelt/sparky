package appcontainer

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
)

// Read-only conformance against a REAL Apple Container install.
//
// Skipped unless SPARKBOX_TEST_CONTAINER=1, so CI and every developer without a
// Mac are unaffected. It exists because testdata/ is a snapshot: these fixtures
// were captured from container 1.1.0 on macOS 26.5.2, and the day the CLI
// changes its JSON the unit tests above will still pass happily against the old
// shapes. This is the thing that notices.
//
// Deliberately NOTHING here mutates: no create, no start, no stop, no delete,
// no build, no exec. The machine on a developer's Mac is their real work.
func liveDriver(t *testing.T) machine.Driver {
	t.Helper()
	if os.Getenv("SPARKBOX_TEST_CONTAINER") != "1" {
		t.Skip("set SPARKBOX_TEST_CONTAINER=1 to run read-only conformance against the real `container` CLI")
	}
	return New(NewCommander("container"))
}

func TestLiveRuntime(t *testing.T) {
	d := liveDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rt, err := d.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !machine.VersionAtLeast(rt.CLIVersion, "1.0.0") {
		t.Errorf("CLIVersion = %q — did the parser pick the apiserver's prose version?", rt.CLIVersion)
	}
	t.Logf("container %s, service running = %v", rt.CLIVersion, rt.ServiceRunning)
}

func TestLiveInspectMissingMachine(t *testing.T) {
	d := liveDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A name that cannot exist, so this is safe on any Mac. The point is the
	// error MAPPING: "does not exist" must be ErrNotFound and nothing else,
	// because the machine step branches on it.
	_, err := d.Inspect(ctx, "sparkbox-no-such-machine-xyzzy")
	if !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLiveImageExistsIsHonest(t *testing.T) {
	d := liveDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ok, err := d.ImageExists(ctx, "sparkbox/definitely-not-an-image:xyzzy")
	if err != nil {
		t.Fatalf("an absent image must be (false, nil), got err %v", err)
	}
	if ok {
		t.Error("reported an image that cannot exist")
	}
}

// TestLiveInspectNamedMachine inspects a machine named by the environment, for
// a developer who wants to check the parsers against their own box. Skipped
// when unset, so it never depends on a particular machine existing.
func TestLiveInspectNamedMachine(t *testing.T) {
	d := liveDriver(t)
	name := os.Getenv("SPARKBOX_TEST_MACHINE")
	if name == "" {
		t.Skip("set SPARKBOX_TEST_MACHINE=<name> to inspect a real machine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := d.Inspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != name || info.ContainerID == "" {
		t.Errorf("info = %+v", info)
	}
	t.Logf("machine %+v", info)
	// The container view is the only place nested virtualization is readable.
	ci, err := d.InspectContainer(ctx, info.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("container view: virtualization=%v state=%q", ci.Virtualization, ci.State)
}
