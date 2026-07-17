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
	// StateArchived means the sandbox's rootfs has been compacted and parked in
	// object storage; no host RAM and (after the archive completes) no host disk.
	// Resume downloads the archive and cold-boots it — see host.Manager.Archive /
	// the Archivable driver capability. Purely a control-plane state: the driver
	// itself only knows running/paused, so this never appears in vmm.Instance.
	StateArchived State = "archived"
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

// Archivable is an optional Driver capability: turning a sandbox's on-disk
// rootfs into a portable, compacted artifact (for parking in object storage or
// promoting to a fork-able template) and rehydrating one. Object storage itself
// lives above the driver (host.ObjectStore) — the driver only ever deals in
// local files, keeping it unaware of buckets and credentials.
//
// All three operate on a *stopped* VM: the caller (host.Manager) pauses first
// so the guest has flushed and unmounted its rootfs. PackRootfs/Snapshot both
// run e2fsck + zerofree so the compressed artifact is ~the used size, not the
// full 25 GB ceiling. Drivers that can't do this simply don't implement it; the
// manager checks with a type assertion, so the capability is additive.
type Archivable interface {
	// PackRootfs compacts the named (stopped) VM's rootfs into a single
	// compressed file, drops any memory snapshot (archive is a cold restore),
	// and returns the artifact's local path. The path is outside the VM's dir so
	// a subsequent Destroy can reclaim the dir without deleting it.
	PackRootfs(ctx context.Context, name string) (path string, err error)
	// UnpackRootfs installs a previously packed artifact as name's rootfs so the
	// next Create/Resume cold-boots it. inPath is a file PackRootfs produced.
	UnpackRootfs(ctx context.Context, name, inPath string) error
	// Snapshot compacts and *sanitizes* (strips per-guest identity: SSH host
	// keys, machine-id, hostname, cached tokens) the named VM's rootfs into a new
	// reusable template the driver resolves like any Config.Image, so
	// Create{Image: newImage} forks a fresh sandbox from it.
	Snapshot(ctx context.Context, name, newImage string) error
	// RemoveTemplate deletes a template previously produced by Snapshot (and its
	// sidecar). Used to clean up a deleted snapshot. A missing template is not an
	// error. The image must be a snapshot template, never a base image — the
	// manager only ever passes names it minted via Snapshot.
	RemoveTemplate(ctx context.Context, image string) error
}

// DiskReporter is an optional Driver capability: reporting a sandbox's on-host
// disk footprint (rootfs write delta + any memory snapshot), in MiB. Used by
// the pooled per-owner disk accounting. Best-effort; drivers without it are
// simply not counted.
type DiskReporter interface {
	DiskUsageMB(ctx context.Context, name string) (int64, error)
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
