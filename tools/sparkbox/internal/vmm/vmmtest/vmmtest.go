// Package vmmtest is the driver-parity harness: one lifecycle suite, run
// against any vmm.Driver, on real guests wherever the driver boots them.
//
// It exists because internal/vmm exposes a five-method Driver plus ten optional
// capability interfaces and has had exactly one implementation, whose ~1,300
// lines of unit tests never boot a guest. Every lifecycle end-to-end in this
// repository runs against the mock. That makes the abstraction unproven: we
// cannot say a second backend is correct, because we have never written down
// what correct means in a form a backend can be run against.
//
// This package is that form. The assertions come from the contracts documented
// on the interfaces in driver.go — "Snapshot operates on a stopped VM", "a disk
// under a name the ledger has never issued must be refused", "NetBytes counts
// from the guest's point of view" — not from either implementation's behaviour.
// Where a contract is only meaningful on a machine that really boots (memory
// survives a resume; a reboot does not), the assertion is gated on a Trait
// rather than dropped, so the mock run still exercises the suite's own code.
//
// # Running it
//
// Against the mock: ordinary `go test ./internal/vmm/mock/`. No gate, no KVM.
// This is what keeps the suite compiling and honest between hardware runs.
//
// Against firecracker: a real KVM host, real fixtures, and the env gate
// SPARKBOX_VMM_PARITY=1. See internal/vmm/firecracker/parity_linux_test.go and
// hack/parity/. The gate is an environment variable rather than a build tag on
// purpose — a build tag would keep the suite out of `go test ./...` by never
// compiling it, which is exactly how a harness rots.
package vmmtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// GateEnv is the environment variable that opts a machine into the real-guest
// half of the harness. Anything that needs /dev/kvm checks it.
const GateEnv = "SPARKBOX_VMM_PARITY"

// Gated reports whether this machine has opted into real-guest parity runs.
func Gated() bool { return os.Getenv(GateEnv) == "1" }

// RequireGate skips unless the machine opted in. Drivers that need /dev/kvm
// call it from their fixture factory; the mock does not.
func RequireGate(t *testing.T) {
	t.Helper()
	if !Gated() {
		t.Skipf("set %s=1 on a host with /dev/kvm and parity fixtures to run this", GateEnv)
	}
}

// Traits say what a driver under test can be held to beyond the interface
// contracts. Every one of them is a claim some driver cannot make: the mock
// runs no kernel, so it has no boot id to change and no tmpfs to lose.
//
// A false trait never weakens an assertion that does not depend on it — it
// removes only the part of a case that is meaningless without a real machine.
type Traits struct {
	// RealGuest: Create boots an actual kernel, so an SSH session runs real
	// commands against a real filesystem and /proc is the guest's own.
	RealGuest bool
	// PreservesMemory: Resume restores guest RAM, not just the disk. The mock
	// documents that it preserves only the workdir.
	PreservesMemory bool
	// SanitizesForks: Snapshot strips per-guest identity, so a fork presents a
	// different SSH host key from the sandbox it was taken from.
	SanitizesForks bool
	// LiveDiskUsage: DiskUsageMB tracks a RUNNING guest — write a file inside
	// it and the number moves. Measured false for firecracker; see
	// docs/vmm-parity-harness.md. The case reports the guest-vs-host gap either
	// way, so a driver setting this false still has to say how wrong it is.
	LiveDiskUsage bool
	// DistinctHostIPs: each sandbox gets its own HostIP. A driver that puts
	// every sandbox on 127.0.0.1 and separates them by port cannot forward the
	// same guest port for two sandboxes at once — real for the mock, and a
	// property the proxy would be wrong to assume of every backend.
	DistinctHostIPs bool
	// BaseImageIsTemplate: Fixture.BaseImage names something
	// TemplateReporter can measure. Not every driver's base image is a
	// template artifact — the mock only has templates once Snapshot mints one.
	BaseImageIsTemplate bool
}

// Fixture is one driver under test, with everything the suite needs to drive
// it. A fresh one is built per subtest so a failure cannot poison the next.
type Fixture struct {
	Driver vmm.Driver
	// BaseImage is the template Create resolves for a fresh sandbox.
	BaseImage string
	// TemplatePrefix namespaces the templates the suite mints, so a run that
	// dies mid-test leaves droppings that are obviously ours.
	TemplatePrefix string
	VCPUs, MemMB   int64
	// AuthorizedKey is the authorized_keys line the driver installs, and Signer
	// its private half. The suite logs in as Instance.SSHUser with Signer.
	AuthorizedKey string
	Signer        xssh.Signer
	// BootTimeout bounds how long the suite waits for sshd after a Create or
	// Resume. Real guests need tens of seconds cold; the mock needs none.
	BootTimeout time.Duration
	Traits      Traits

	// DialSSH overrides how the suite reaches a guest. Empty dials
	// Instance.SSHAddr over tcp, which is right for every driver so far.
	DialSSH func(ctx context.Context, inst *vmm.Instance, cfg *xssh.ClientConfig) (*xssh.Client, error)
}

// NewFixture builds a Fixture for one subtest. It must register its own
// teardown with t.Cleanup; the suite tears down the VMs it created but does not
// know what the factory allocated around them.
type NewFixture func(t *testing.T) *Fixture

func (f *Fixture) bootTimeout() time.Duration {
	if f.BootTimeout > 0 {
		return f.BootTimeout
	}
	return 90 * time.Second
}

func (f *Fixture) template(name string) string {
	p := f.TemplatePrefix
	if p == "" {
		p = "parity"
	}
	return p + "-" + name
}

// --- the box: one sandbox, with teardown that actually tears down -----------

// box is a sandbox under test. Creating one registers a Destroy, because a
// leaked firecracker VM holds a network slot and a tap device that the next
// subtest's driver will try to create again and fail on — a cascade that hides
// the failure that started it.
type box struct {
	t    *testing.T
	f    *Fixture
	name string
	inst *vmm.Instance

	mu       sync.Mutex
	hostKeys []string // SSH host keys seen, newest last
}

func newBox(t *testing.T, f *Fixture, name string) *box {
	b := &box{t: t, f: f, name: name}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := f.Driver.Destroy(ctx, name); err != nil {
			t.Errorf("teardown: destroy %s: %v", name, err)
		}
	})
	return b
}

// create cold-boots the box. fresh marks it a name that has never existed,
// which drivers must refuse to satisfy from residue (see vmm.Config.NewSandbox).
func (b *box) create(fresh bool) *vmm.Instance {
	b.t.Helper()
	inst, err := b.tryCreate(fresh)
	if err != nil {
		b.t.Fatalf("create %s (NewSandbox=%v): %v", b.name, fresh, err)
	}
	return inst
}

func (b *box) tryCreate(fresh bool) (*vmm.Instance, error) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	inst, err := b.f.Driver.Create(ctx, vmm.Config{
		Name:             b.name,
		Image:            b.f.BaseImage,
		VCPUs:            b.f.VCPUs,
		MemMB:            b.f.MemMB,
		GatewayPublicKey: b.f.AuthorizedKey,
		NewSandbox:       fresh,
	})
	if err != nil {
		return nil, err
	}
	b.inst = inst
	return inst, nil
}

// createFrom cold-boots the box from a named template instead of the base
// image. This is the fork path.
func (b *box) createFrom(image string) *vmm.Instance {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	inst, err := b.f.Driver.Create(ctx, vmm.Config{
		Name:             b.name,
		Image:            image,
		VCPUs:            b.f.VCPUs,
		MemMB:            b.f.MemMB,
		GatewayPublicKey: b.f.AuthorizedKey,
		NewSandbox:       true,
	})
	if err != nil {
		b.t.Fatalf("create %s from template %s: %v", b.name, image, err)
	}
	b.inst = inst
	return inst
}

func (b *box) pause() {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := b.f.Driver.Pause(ctx, b.name); err != nil {
		b.t.Fatalf("pause %s: %v", b.name, err)
	}
}

func (b *box) resume() *vmm.Instance {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	inst, err := b.f.Driver.Resume(ctx, b.name)
	if err != nil {
		b.t.Fatalf("resume %s: %v", b.name, err)
	}
	b.inst = inst
	return inst
}

func (b *box) destroy() {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.f.Driver.Destroy(ctx, b.name); err != nil {
		b.t.Fatalf("destroy %s: %v", b.name, err)
	}
	b.inst = nil
}

// --- reaching the guest -----------------------------------------------------

// dial waits for the guest's sshd and returns a client. A cold boot is the
// slowest thing in this suite, so the wait is a poll rather than a sleep, and
// the last dial error is reported when it runs out — "connection refused" and
// "handshake failed: unable to authenticate" are very different failures and
// the difference is the whole diagnosis.
func (b *box) dial() *xssh.Client {
	b.t.Helper()
	if b.inst == nil {
		b.t.Fatalf("dial %s: no instance (create or resume first)", b.name)
	}
	if b.inst.SSHAddr == "" {
		b.t.Fatalf("dial %s: instance has no SSHAddr in state %q", b.name, b.inst.State)
	}
	cfg := &xssh.ClientConfig{
		User: b.inst.SSHUser,
		Auth: []xssh.AuthMethod{xssh.PublicKeys(b.f.Signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key xssh.PublicKey) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.hostKeys = append(b.hostKeys, xssh.FingerprintSHA256(key))
			return nil
		},
		Timeout: 10 * time.Second,
	}
	deadline := time.Now().Add(b.f.bootTimeout())
	var lastErr error
	for time.Now().Before(deadline) {
		client, err := b.dialOnce(cfg)
		if err == nil {
			b.t.Cleanup(func() { client.Close() }) //nolint:errcheck
			return client
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	b.t.Fatalf("dial %s at %s as %s: gave up after %s: %v",
		b.name, b.inst.SSHAddr, b.inst.SSHUser, b.f.bootTimeout(), lastErr)
	return nil
}

func (b *box) dialOnce(cfg *xssh.ClientConfig) (*xssh.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if b.f.DialSSH != nil {
		return b.f.DialSSH(ctx, b.inst, cfg)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", b.inst.SSHAddr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := xssh.NewClientConn(conn, b.inst.SSHAddr, cfg)
	if err != nil {
		conn.Close() //nolint:errcheck
		return nil, err
	}
	return xssh.NewClient(c, chans, reqs), nil
}

// lastHostKey is the fingerprint the most recent dial saw, which is how the
// suite tells a fork apart from the sandbox it was forked from.
func (b *box) lastHostKey() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.hostKeys) == 0 {
		return ""
	}
	return b.hostKeys[len(b.hostKeys)-1]
}

// run executes a command in the guest and returns its trimmed stdout.
func run(t *testing.T, c *xssh.Client, cmd string) string {
	t.Helper()
	out, err := tryRun(c, cmd)
	if err != nil {
		t.Fatalf("guest %q: %v (output %q)", cmd, err, out)
	}
	return out
}

func tryRun(c *xssh.Client, cmd string) (string, error) {
	sess, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close() //nolint:errcheck
	out, err := sess.CombinedOutput(cmd)
	return strings.TrimSpace(string(out)), err
}

// --- assertions used across cases -------------------------------------------

func wantRunning(t *testing.T, inst *vmm.Instance, name string) {
	t.Helper()
	if inst == nil {
		t.Fatalf("%s: nil instance", name)
	}
	if inst.Name != name {
		t.Errorf("%s: Instance.Name = %q", name, inst.Name)
	}
	if inst.State != vmm.StateRunning {
		t.Errorf("%s: State = %q, want %q", name, inst.State, vmm.StateRunning)
	}
	if inst.SSHAddr == "" {
		t.Errorf("%s: running instance has no SSHAddr", name)
	}
	if inst.SSHUser == "" {
		t.Errorf("%s: running instance has no SSHUser", name)
	}
	// HostIP is what the HTTP proxy dials for a forwarded port. A running
	// sandbox with none is unreachable for everything except SSH, and nothing
	// in the control plane would notice.
	if inst.HostIP == "" {
		t.Errorf("%s: running instance has no HostIP", name)
	}
}

// wantErr fails when err is nil, and says what was expected. Used for the
// contracts that are refusals — resize a running VM, adopt residue, rename over
// a live snapshot — because a driver that quietly succeeds at those is the
// failure mode with no error anywhere.
func wantErr(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected an error, got nil", what)
	}
}

// uniq builds a sandbox name that is unique within a run and legal everywhere:
// lowercase, digits and dashes, short enough for a tap device's slot and a
// jailer chroot path.
func uniq(t *testing.T, stem string) string {
	t.Helper()
	return fmt.Sprintf("p%s%d", stem, time.Now().UnixNano()%1e7)
}
