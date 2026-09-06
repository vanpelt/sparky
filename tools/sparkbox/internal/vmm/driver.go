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
	// NewSandbox marks a Create that brings a name into existence, as opposed
	// to cold-booting a sandbox the ledger already knows (a restart, a resume
	// that failed, a restore). Drivers key a sandbox's disk by name, and names
	// are reusable, so the two cases disagree about what a disk already sitting
	// under this name means: on a cold boot it is the sandbox's own state, and
	// on a new sandbox it is the residue of a destroy that did not finish.
	// Adopting that residue would hand its previous owner's home directory to
	// whoever claimed the name next, so drivers must refuse it.
	NewSandbox bool
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

// DiskReporter is an optional Driver capability: reporting a sandbox's durable
// disk usage and its ceiling, in MiB. Feeds the pooled per-owner accounting and
// the consoles' disk meter. Best-effort; drivers without it are not counted.
type DiskReporter interface {
	// DiskUsageMB is the durable storage used by the sandbox's root filesystem,
	// excluding representation details such as shared/sparse host extents and
	// any regenerable memory snapshot.
	//
	// Freshness is NOT part of this contract, and the firecracker driver does
	// not deliver it: it reads the ext4 superblock's free-block count, which
	// Linux does not write back while the filesystem is mounted, so a running
	// sandbox reports the figure its template had. Measured by the parity
	// harness — see docs/vmm-parity-harness.md, and vmmtest.Traits.LiveDiskUsage
	// for the trait a driver sets when it does track a live guest.
	DiskUsageMB(ctx context.Context, name string) (int64, error)
	// DiskCapacityMB is the guest's hard disk ceiling — the size of the rootfs
	// filesystem, which it cannot grow past. 0 when unknown.
	DiskCapacityMB(ctx context.Context, name string) (int64, error)
}

// TemplateReporter is an optional Driver capability: measuring the used blocks
// of a *template* — the image a sandbox was created from — so pooled accounting
// can subtract the blocks a fork shares rather than charging for them.
//
// DiskUsageMB is deliberately representation-independent (it reads the guest's
// own filesystem counters, not host allocation), which is right for the meter a
// user sees but wrong for a pool: ten forks of one 8 GiB template each report
// ~8 GiB used while the host holds one copy plus each fork's writes. Subtracting
// this baseline charges an owner for what their sandboxes wrote.
//
// Kept off DiskReporter on purpose. Capabilities are detected by type assertion,
// so a third method there would silently disable disk accounting entirely for
// any driver that can measure a live rootfs but not a template. As a separate
// interface such a driver loses only the discount.
//
// Unlike DiskUsageMB, a missing image is an ERROR, not zero: deleting a template
// does not retroactively make its blocks the fork's fault, so callers keep the
// last baseline they measured instead of spiking every fork's charge.
type TemplateReporter interface {
	TemplateUsageMB(ctx context.Context, image string) (int64, error)
}

// RootfsPresencer is the optional guard against silently recreating a known
// sandbox from its base image after the hot tier was lost. Absence means
// "restore required" when a checkpoint exists and "unrecoverable" otherwise,
// never "fresh clone".
type RootfsPresencer interface {
	RootfsPresent(name string) (bool, error)
}

// Renamer is an optional Driver capability: moving a stopped VM's on-host
// state to a new name, so the manager can rename a sandbox without a
// pack/unpack round trip. The caller (host.Manager) pauses first and — for
// drivers whose memory snapshots embed paths (firecracker's state.snap
// records absolute paths into the old VM dir) — drops snapshots first, so the
// next start cold-boots the moved rootfs.
type Renamer interface {
	RenameVM(oldName, newName string) error
}

// Rebooter is an optional Driver capability: discarding a stopped VM's memory
// snapshot so the next start is a cold boot of the preserved rootfs instead
// of a resume. This is how Manager.Reboot restarts a guest (pause → drop →
// EnsureRunning) and how Rename avoids resuming a snapshot that points at the
// old VM dir.
type Rebooter interface {
	DropSnapshots(name string) error
}

// CPUStatser is an optional Driver capability: the cumulative CPU time a
// sandbox has consumed on the host, in nanoseconds. Callers derive a
// utilization percentage from deltas between polls; the counter resets to
// zero on a cold boot (it follows the VMM process, not the guest).
type CPUStatser interface {
	CPUTimeNanos(ctx context.Context, name string) (uint64, error)
}

// NetStatser is an optional Driver capability: the cumulative bytes a sandbox's
// virtual NIC has carried, counted from the *guest's* point of view — rx is
// what the guest received, tx what it sent. The host-side tap reports the
// mirror image of that (a packet the guest sends is an rx on the host), so
// drivers do the swap and callers never have to think about it.
//
// The counters live with the host-side device, which is torn down and
// recreated on every pause/resume and cold boot, so they reset to zero far
// more often than the CPU counter does. A reading lower than the previous one
// is a reset, not a 64-bit rollover: callers accumulating lifetime totals must
// treat it as such (see Manager.sampleVitals).
type NetStatser interface {
	NetBytes(ctx context.Context, name string) (rx, tx uint64, err error)
}

// DiskResizer is an optional Driver capability: growing a *stopped* sandbox's
// root filesystem to a new size.
//
// Grow only. Shrinking an ext4 means resizing the filesystem before truncating
// the image — the opposite order — and it fails outright if the data doesn't
// fit below the new boundary, so a half-completed shrink is a destroyed disk.
// The safe operation is the one we support.
//
// Implementations MUST refuse a running VM: the resize rewrites filesystem
// metadata the live guest has cached. Callers must additionally discard any
// memory snapshot first — see Manager.Resize for why that pairing is not
// optional.
type DiskResizer interface {
	ResizeDisk(ctx context.Context, name string, sizeMB int64) error
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

// FullDriver is every capability this package defines: the required Driver plus
// all ten optional interfaces.
//
// Nothing accepts a FullDriver as a parameter, and nothing should. host.Manager
// reaches each capability by type assertion on purpose, so a driver is free to
// implement a subset and the manager degrades around what is missing. This
// interface exists for the opposite case — a driver that means to offer all of
// them — so it can say so in one line:
//
//	var _ vmm.FullDriver = (*Driver)(nil)
//
// The point is what happens when an eleventh capability is added below. Because
// every caller reaches these by assertion, a driver that quietly stops
// satisfying one degrades the fleet with no error anywhere: no test fails, no
// log line appears, the manager just takes the other branch forever. One line
// per driver turns that into a build failure in every driver at once, which is
// the only moment anybody is looking.
type FullDriver interface {
	Driver
	Archivable
	DiskReporter
	TemplateReporter
	RootfsPresencer
	Renamer
	Rebooter
	CPUStatser
	NetStatser
	DiskResizer
	Ballooner
}
