// Package vmm defines the driver interface sparkbox uses to manage sandbox
// VMs. Two implementations exist: mock (in-process fake VMs, no KVM needed,
// used for dev and tests) and firecracker (real microVMs via
// firecracker-go-sdk, requires /dev/kvm).
package vmm

import "context"

type State string

const (
	StateRunning State = "running"
	StatePaused  State = "paused"
)

// Config describes the sandbox VM to create.
type Config struct {
	// Name is the unique sandbox name; drivers use it to key all resources.
	Name string
	// Image is the rootfs template name (e.g. "ubuntu"). The firecracker
	// driver resolves it to <imageDir>/<Image>.ext4; the mock driver ignores it.
	Image string
	VCPUs int64
	MemMB int64
	// GatewayPublicKey is the SSH public key (authorized_keys line) that the
	// gateway will authenticate with when dialing into the VM.
	GatewayPublicKey string
}

// Instance is a running or paused sandbox VM.
type Instance struct {
	Name    string
	State   State
	SSHAddr string // host:port the gateway dials to reach the VM's sshd
	SSHUser string // user the gateway logs in as
	// HostIP is the address (no port) reachable from the host for the VM's own
	// services — the HTTP proxy dials HostIP:<forwarded-port>. Empty when paused.
	HostIP string
	// GuestV6 is the sandbox's globally-routable IPv6 address (from the host's
	// delegated /64), used for no-NAT egress and direct v6 addressing. Empty
	// when paused or when the driver has no IPv6 prefix configured.
	GuestV6 string
}

// Driver manages sandbox VM lifecycles on a single host.
//
// Pause must persist enough state that Resume brings the sandbox back with
// its filesystem intact (the firecracker driver snapshots memory too; the
// mock driver only preserves the workdir).
type Driver interface {
	Create(ctx context.Context, cfg Config) (*Instance, error)
	Pause(ctx context.Context, name string) error
	Resume(ctx context.Context, name string) (*Instance, error)
	Destroy(ctx context.Context, name string) error
	// Close releases driver resources (running fake VMs, VMM processes).
	Close() error
}

// BalloonStats reports a ballooned guest's memory picture, in MiB. All fields
// are best-effort — a driver without balloon stats returns zeros.
type BalloonStats struct {
	TargetMiB    int64 // how much RAM we've asked the balloon to reclaim to the host
	ActualMiB    int64 // how much the balloon currently holds
	FreeMiB      int64 // guest-reported free memory
	AvailableMiB int64 // guest estimate of memory available for new work
}

// Ballooner is an optional Driver capability: reclaiming a *running* guest's
// unused RAM to the host through a virtio-balloon, without pausing it. This is
// the live-overcommit lever — an idle-but-warm sandbox hands most of its RAM
// back while its in-guest cron and daemons keep running. Drivers that can't do
// this simply don't implement it; the manager checks with a type assertion, so
// adding the capability never breaks a driver that lacks it.
type Ballooner interface {
	// SetBalloonTarget inflates the balloon to reclaim targetMiB of guest RAM to
	// the host (0 fully deflates, giving the guest all its RAM back). It is a
	// no-op error if the named VM isn't running.
	SetBalloonTarget(ctx context.Context, name string, targetMiB int64) error
	// BalloonStats reports the guest's current memory use, when available.
	BalloonStats(ctx context.Context, name string) (BalloonStats, error)
}
