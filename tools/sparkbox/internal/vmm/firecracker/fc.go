//go:build linux

// Package firecracker implements vmm.Driver on real Firecracker microVMs via
// the official firecracker-go-sdk. It requires /dev/kvm, root (for tap device
// setup), a vmlinux kernel, and per-image ext4 rootfs templates produced by
// hack/build-rootfs.sh (which bakes in the gateway's SSH public key).
//
// MVP status: written against firecracker-go-sdk v1.0.0 and compiles, but
// this container has no KVM — it has NOT been exercised end to end yet. See
// docs/deploy-hetzner.md for bring-up on a real host. Known gaps vs
// production: no jailer, no balloon management, no UFFD lazy restore, no I/O
// rate limits.
package firecracker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

type Options struct {
	// KernelPath is an uncompressed vmlinux built with the microVM config.
	KernelPath string
	// ImageDir holds <image>.ext4 rootfs templates.
	ImageDir string
	// StateDir holds per-VM dirs (disk copy, socket, snapshots).
	StateDir string
	// FirecrackerBin is the firecracker binary path (default: $PATH lookup).
	FirecrackerBin string
	// Subnet is the /16 carved into per-VM /30-style pairs, default 172.30.0.0.
	Subnet string
	// Subnet6 is a routable IPv6 /64 delegated to the host (e.g.
	// "2001:db8:1c7::/64"). When set, each VM gets a globally-routable /128 from
	// it (dual-stack, no NAT). Empty keeps VMs IPv4-only.
	Subnet6 string
	// LoginUser is the guest account the gateway SSHes in as — set to match the
	// template's baked authorized_keys (our images declare it via the
	// sparkbox.login-user label; see hack/build-rootfs.sh). Empty defaults root.
	LoginUser string
}

type vmState struct {
	idx     int // network slot: host 172.30.<idx>.1, guest 172.30.<idx>.2
	machine *sdk.Machine
	cancel  context.CancelFunc
	paused  bool
}

type Driver struct {
	mu      sync.Mutex
	opts    Options
	vms     map[string]*vmState
	prefix6 net.IP // parsed /64 network address; nil disables IPv6
	uplink6 string // iface backing the v6 default route, for per-guest proxy NDP
}

func New(opts Options) (*Driver, error) {
	if opts.FirecrackerBin == "" {
		opts.FirecrackerBin = "firecracker"
	}
	if opts.Subnet == "" {
		opts.Subnet = "172.30.0.0"
	}
	d := &Driver{opts: opts, vms: map[string]*vmState{}}
	if opts.Subnet6 != "" {
		_, ipNet, err := net.ParseCIDR(opts.Subnet6)
		if err != nil {
			return nil, fmt.Errorf("subnet6 %q: %w", opts.Subnet6, err)
		}
		if ones, _ := ipNet.Mask.Size(); ones > 112 {
			return nil, fmt.Errorf("subnet6 %q: need /112 or larger for per-VM addressing", opts.Subnet6)
		}
		d.prefix6 = ipNet.IP.To16()
		// Scaleway (and most providers) deliver the routed /64 on-link: the
		// upstream router NDP-resolves each guest's /128 on the segment, and the
		// host only auto-answers for its own addresses. Per-VM addresses live on
		// the taps, so without proxy NDP on the uplink their return traffic is
		// dropped. Record the uplink now; createTap adds a proxy entry per guest.
		d.uplink6 = defaultRoute6Dev()
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return nil, fmt.Errorf("firecracker driver requires /dev/kvm: %w", err)
	}
	if _, err := os.Stat(opts.KernelPath); err != nil {
		return nil, fmt.Errorf("kernel image: %w", err)
	}
	// A previous process (e.g. before a service restart) leaves its sbtap*
	// devices behind; the first Create would then fail with "Device or resource
	// busy". Nothing is running in a fresh process, so sweep them now.
	sweepStaleTaps()
	return d, nil
}

// sweepStaleTaps deletes leftover sbtap* devices from a prior process. Safe at
// startup only — call before any VM exists.
func sweepStaleTaps() {
	out, err := exec.Command("ip", "-o", "link", "show").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		// "3: sbtap1@if4: <BROADCAST,...>" -> field 1 is the name.
		parts := strings.SplitN(line, ": ", 3)
		if len(parts) < 2 {
			continue
		}
		name := parts[1]
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[:i]
		}
		if strings.HasPrefix(name, "sbtap") {
			exec.Command("ip", "link", "del", name).Run() //nolint:errcheck
		}
	}
}

func (d *Driver) vmDir(name string) string {
	return filepath.Join(d.opts.StateDir, "fc-vms", name)
}

func (d *Driver) hostIP(idx int) string  { return fmt.Sprintf("172.30.%d.1", idx) }
func (d *Driver) guestIP(idx int) string { return fmt.Sprintf("172.30.%d.2", idx) }
func tapName(idx int) string             { return fmt.Sprintf("sbtap%d", idx) }

// IPv6 addressing: each slot gets a point-to-point /127 carved from the /64,
// host on the even address and guest on the odd one. Slot idx=1 -> ::2 (host) /
// ::3 (guest), leaving ::1 free for the host's own edge address (the AAAA
// target). Globally routable, so egress needs no NAT — just host forwarding.
func (d *Driver) hostIP6(idx int) string  { return d.addr6(idx * 2) }
func (d *Driver) guestIP6(idx int) string { return d.addr6(idx*2 + 1) }

func (d *Driver) addr6(off int) string {
	ip := make(net.IP, net.IPv6len)
	copy(ip, d.prefix6)
	// Place the offset in the low 32 bits of the /64's host portion.
	ip[12] = byte(off >> 24)
	ip[13] = byte(off >> 16)
	ip[14] = byte(off >> 8)
	ip[15] = byte(off)
	return ip.String()
}

func (d *Driver) Create(ctx context.Context, cfg vmm.Config) (*vmm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.vms[cfg.Name]; ok {
		return nil, fmt.Errorf("vm %q already exists", cfg.Name)
	}
	dir := d.vmDir(cfg.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// CoW-copy the rootfs template: instant on XFS/btrfs via reflink, falls
	// back to a full copy elsewhere.
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if _, err := os.Stat(rootfs); os.IsNotExist(err) {
		template := filepath.Join(d.opts.ImageDir, cfg.Image+".ext4")
		if out, err := exec.CommandContext(ctx, "cp", "--reflink=auto", template, rootfs).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("copy rootfs: %v: %s", err, out)
		}
	}

	idx, err := d.freeSlot()
	if err != nil {
		return nil, err
	}
	st := &vmState{idx: idx}
	if err := d.createTap(ctx, st.idx); err != nil {
		// Clean up any half-configured (or stale) device so a retry can reuse
		// this slot; only recording it in d.vms reserves it, so a failed create
		// leaks no idx.
		d.deleteTap(st.idx)
		return nil, err
	}
	if err := d.boot(ctx, cfg.Name, cfg.VCPUs, cfg.MemMB, st, rootfs, nil); err != nil {
		d.deleteTap(st.idx)
		return nil, err
	}
	d.vms[cfg.Name] = st
	return d.instance(cfg.Name, st), nil
}

// freeSlot returns the lowest network slot no vmState holds. Caller must hold
// d.mu. Every path that drops a record (Destroy, DropSnapshots, RenameVM)
// thereby releases its slot for reuse — a reused slot can't collide with a
// live tap because paused VMs keep their record (idx stays reserved) and the
// record only goes away after the tap does. The bound comes from hostIP's
// "172.30.<idx>.1" third octet; past it a Create would mint an unroutable
// address, so error instead.
func (d *Driver) freeSlot() (int, error) {
	used := make(map[int]bool, len(d.vms))
	for _, s := range d.vms {
		used[s.idx] = true
	}
	for idx := 1; idx <= 255; idx++ {
		if !used[idx] {
			return idx, nil
		}
	}
	return 0, fmt.Errorf("no free network slots (max 255 concurrent VMs)")
}

// boot starts a Firecracker process for the VM; snapshot, when non-nil, is
// the [memPath, statePath] pair to restore from instead of a cold boot.
func (d *Driver) boot(ctx context.Context, name string, vcpus, memMB int64, st *vmState, rootfs string, snapshot []string) error {
	dir := d.vmDir(name)
	sock := filepath.Join(dir, "fc.sock")
	os.Remove(sock) //nolint:errcheck

	// Static guest networking via kernel arg — no DHCP daemon needed. The ip=
	// arg is IPv4-only; IPv6 is applied inside the guest by the sparkbox-netcfg
	// hook (build-rootfs.sh), which reads sparkbox_ip6/sparkbox_gw6 here.
	kernelArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off quiet ip=%s::%s:255.255.255.252::eth0:off sparkbox_host=%s",
		d.guestIP(st.idx), d.hostIP(st.idx), name)
	if d.prefix6 != nil {
		kernelArgs += fmt.Sprintf(" sparkbox_ip6=%s/127 sparkbox_gw6=%s",
			d.guestIP6(st.idx), d.hostIP6(st.idx))
	}

	fcCfg := sdk.Config{
		SocketPath:      sock,
		KernelImagePath: d.opts.KernelPath,
		KernelArgs:      kernelArgs,
		Drives: []models.Drive{{
			DriveID:      strPtr("rootfs"),
			PathOnHost:   strPtr(rootfs),
			IsRootDevice: boolPtr(true),
			IsReadOnly:   boolPtr(false),
		}},
		NetworkInterfaces: sdk.NetworkInterfaces{{
			StaticConfiguration: &sdk.StaticNetworkConfiguration{
				HostDevName: tapName(st.idx),
				MacAddress:  macFor(st.idx),
			},
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  int64Ptr(vcpus),
			MemSizeMib: int64Ptr(memMB),
			Smt:        boolPtr(false),
		},
		VMID: name,
	}

	vmCtx, cancel := context.WithCancel(context.Background())
	cmd := sdk.VMCommandBuilder{}.
		WithBin(d.opts.FirecrackerBin).
		WithSocketPath(sock).
		Build(vmCtx)

	opts := []sdk.Opt{sdk.WithProcessRunner(cmd)}
	if snapshot != nil {
		opts = append(opts, sdk.WithSnapshot(snapshot[0], snapshot[1]))
	}
	m, err := sdk.NewMachine(vmCtx, fcCfg, opts...)
	if err != nil {
		cancel()
		return err
	}
	// Attach a deflated memory balloon on fresh boot so the platform can later
	// reclaim this guest's idle RAM to the host without pausing it (the
	// live-overcommit lever). deflate_on_oom lets the guest take its own pages
	// back under pressure, so an aggressive balloon can't OOM-kill it; stats
	// polling exposes the guest's real working set. The balloon is a pre-boot
	// device, so it's injected into the init handler chain here. On resume the
	// balloon is restored from the snapshot, so we skip it (re-adding would
	// fight the snapshot-load handlers).
	if snapshot == nil {
		m.Handlers.FcInit = m.Handlers.FcInit.Append(
			sdk.NewCreateBalloonHandler(0, true, balloonStatsIntervalSecs))
	}
	// Start/ResumeVM must use vmCtx, not the caller's ctx: the SDK stops the
	// VMM (SIGTERM) when the context passed here is cancelled. The caller's ctx
	// is request-scoped (the create-on-connect SSH session), so binding to it
	// kills the microVM the instant that first connection closes. The VM's
	// lifetime is owned by vmCtx, cancelled only by Pause/Destroy via st.cancel.
	if err := m.Start(vmCtx); err != nil {
		cancel()
		return fmt.Errorf("start vm: %w", err)
	}
	if snapshot != nil {
		if err := m.ResumeVM(vmCtx); err != nil {
			m.StopVMM() //nolint:errcheck
			cancel()
			return fmt.Errorf("resume from snapshot: %w", err)
		}
	}
	st.machine = m
	st.cancel = cancel
	st.paused = false
	return nil
}

func (d *Driver) Pause(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok {
		return fmt.Errorf("vm %q not found", name)
	}
	if st.paused {
		return nil
	}
	dir := d.vmDir(name)
	if err := st.machine.PauseVM(ctx); err != nil {
		return err
	}
	if err := st.machine.CreateSnapshot(ctx,
		filepath.Join(dir, "mem.snap"), filepath.Join(dir, "state.snap")); err != nil {
		// Recover on a FRESH context: when the failure IS the caller's ctx
		// expiring (snapshot outran a deadline), resuming on that same dead
		// ctx fails instantly and leaves the vCPUs paused — an unreachable,
		// unresumable sandbox that needs a manual firecracker-socket poke.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer rcancel()
		st.machine.ResumeVM(rctx) //nolint:errcheck // best effort: leave it running rather than wedged
		return err
	}
	st.machine.StopVMM() //nolint:errcheck
	st.cancel()
	// The VM is gone, so its host-side tap is orphaned. Tear it down here;
	// Resume recreates it via createTap. Leaving it wedges Resume with
	// "tuntap add: Device or resource busy" on the still-present device.
	d.deleteTap(st.idx)
	st.machine = nil
	st.paused = true
	return nil
}

func (d *Driver) Resume(ctx context.Context, name string) (*vmm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok {
		return nil, fmt.Errorf("vm %q not found", name)
	}
	if !st.paused {
		return d.instance(name, st), nil
	}
	dir := d.vmDir(name)
	mem, state := filepath.Join(dir, "mem.snap"), filepath.Join(dir, "state.snap")
	if _, err := os.Stat(state); err != nil {
		return nil, fmt.Errorf("no snapshot for %q: %w", name, err)
	}
	if err := d.createTap(ctx, st.idx); err != nil {
		return nil, err
	}
	// Snapshots don't carry vcpu/mem config into NewMachine; they're baked
	// into the state file. Zero values here are ignored on the restore path.
	if err := d.boot(ctx, name, 0, 0, st, filepath.Join(dir, "rootfs.ext4"), []string{mem, state}); err != nil {
		return nil, err
	}
	return d.instance(name, st), nil
}

// balloonStatsIntervalSecs is how often the guest refreshes balloon stats. A
// small interval keeps the working-set signal fresh; the read itself is
// on-demand (BalloonStats) so this is cheap.
const balloonStatsIntervalSecs int64 = 1

// SetBalloonTarget inflates (or deflates, at 0) the named VM's balloon to
// reclaim targetMiB of guest RAM to the host. Implements vmm.Ballooner.
func (d *Driver) SetBalloonTarget(ctx context.Context, name string, targetMiB int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok || st.machine == nil {
		return fmt.Errorf("vm %q not running", name)
	}
	if targetMiB < 0 {
		targetMiB = 0
	}
	return st.machine.UpdateBalloon(ctx, targetMiB)
}

// BalloonStats reports the guest's current memory picture. Implements
// vmm.Ballooner. Errors if the VM isn't running or has no balloon (e.g. an old
// snapshot predating this feature).
func (d *Driver) BalloonStats(ctx context.Context, name string) (vmm.BalloonStats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok || st.machine == nil {
		return vmm.BalloonStats{}, fmt.Errorf("vm %q not running", name)
	}
	s, err := st.machine.GetBalloonStats(ctx)
	if err != nil {
		return vmm.BalloonStats{}, err
	}
	out := vmm.BalloonStats{
		FreeMiB:      s.FreeMemory / (1024 * 1024),
		AvailableMiB: s.AvailableMemory / (1024 * 1024),
	}
	if s.ActualMib != nil {
		out.ActualMiB = *s.ActualMib
		out.TargetMiB = *s.ActualMib
	}
	return out, nil
}

func (d *Driver) Destroy(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok {
		return nil
	}
	if st.machine != nil {
		st.machine.StopVMM() //nolint:errcheck
		st.cancel()
	}
	d.deleteTap(st.idx)
	delete(d.vms, name)
	return os.RemoveAll(d.vmDir(name))
}

// imageNameRe bounds a snapshot/template basename so Snapshot can't be tricked
// into writing outside ImageDir. Mirrors the manager's sandbox-name rules but
// also allows the '.' and uppercase we use in derived template names.
var imageNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,126}$`)

// --- Archivable + DiskReporter: the disk-lifecycle capabilities ------------

func (d *Driver) rootfsPath(name string) string {
	return filepath.Join(d.vmDir(name), "rootfs.ext4")
}

// stoppedRootfs returns name's rootfs path, refusing a *running* VM: archive and
// snapshot run e2fsck/zerofree/mount against the ext4, which would corrupt a
// live guest's disk. The manager pauses before calling, so a paused (machine ==
// nil) or post-restart (no driver entry) VM is fine.
func (d *Driver) stoppedRootfs(name string) (string, error) {
	d.mu.Lock()
	st, ok := d.vms[name]
	running := ok && st.machine != nil
	d.mu.Unlock()
	if running {
		return "", fmt.Errorf("vm %q is running; pause it first", name)
	}
	rootfs := d.rootfsPath(name)
	if _, err := os.Stat(rootfs); err != nil {
		return "", fmt.Errorf("no rootfs for %q: %w", name, err)
	}
	return rootfs, nil
}

// PackRootfs implements vmm.Archivable: compact the stopped VM's rootfs, drop
// its memory snapshot, and zstd it into a sibling of the VM dir (so Destroy(dir)
// won't clobber it before the manager uploads).
func (d *Driver) PackRootfs(ctx context.Context, name string) (string, error) {
	rootfs, err := d.stoppedRootfs(name)
	if err != nil {
		return "", err
	}
	if err := compact(ctx, rootfs); err != nil {
		return "", err
	}
	// Archive is a cold restore, so the memory snapshot is dead weight — dropping
	// it is exactly the disk pausing spent that we now reclaim.
	dir := d.vmDir(name)
	os.Remove(filepath.Join(dir, "mem.snap"))   //nolint:errcheck
	os.Remove(filepath.Join(dir, "state.snap")) //nolint:errcheck
	out := dir + ".pack.ext4.zst"
	if o, err := exec.CommandContext(ctx, "zstd", "-T0", "-10", "-f", rootfs, "-o", out).CombinedOutput(); err != nil {
		return "", fmt.Errorf("compress rootfs: %v: %s", err, o)
	}
	return out, nil
}

// UnpackRootfs implements vmm.Archivable: decompress a packed artifact into
// name's VM dir so the next Create cold-boots it (Create skips its reflink copy
// when rootfs.ext4 already exists).
func (d *Driver) UnpackRootfs(ctx context.Context, name, inPath string) error {
	d.mu.Lock()
	st, ok := d.vms[name]
	running := ok && st.machine != nil
	d.mu.Unlock()
	if running {
		return fmt.Errorf("vm %q is running", name)
	}
	dir := d.vmDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if o, err := exec.CommandContext(ctx, "zstd", "-d", "-f", inPath, "-o", rootfs).CombinedOutput(); err != nil {
		return fmt.Errorf("decompress rootfs: %v: %s", err, o)
	}
	return nil
}

// Snapshot implements vmm.Archivable: promote the stopped VM's rootfs into a new
// reusable ImageDir template. Reflink-copies first (never mutates the source
// VM's disk), sanitizes per-guest identity, compacts, then renames into place.
func (d *Driver) Snapshot(ctx context.Context, name, newImage string) error {
	if !imageNameRe.MatchString(newImage) {
		return fmt.Errorf("invalid snapshot image name %q", newImage)
	}
	rootfs, err := d.stoppedRootfs(name)
	if err != nil {
		return err
	}
	if d.opts.ImageDir == "" {
		return fmt.Errorf("no image dir configured; cannot snapshot")
	}
	tmp := filepath.Join(d.opts.ImageDir, "."+newImage+".ext4.tmp")
	final := filepath.Join(d.opts.ImageDir, newImage+".ext4")
	os.Remove(tmp) //nolint:errcheck // clear any torn prior attempt
	if o, err := exec.CommandContext(ctx, "cp", "--reflink=auto", rootfs, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("copy rootfs: %v: %s", err, o)
	}
	if err := sanitizeTemplate(ctx, tmp); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	if err := compact(ctx, tmp); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	// Login-user sidecar (build-rootfs.sh writes the same file next to templates
	// it builds) so the gateway logs into forks as the right account.
	user := d.opts.LoginUser
	if user == "" {
		user = "root"
	}
	os.WriteFile(final+".login-user", []byte(user+"\n"), 0o644) //nolint:errcheck
	return nil
}

// RemoveTemplate implements vmm.Archivable: delete a snapshot template + sidecar.
func (d *Driver) RemoveTemplate(_ context.Context, image string) error {
	if !imageNameRe.MatchString(image) {
		return fmt.Errorf("invalid template name %q", image)
	}
	if d.opts.ImageDir == "" {
		return nil
	}
	final := filepath.Join(d.opts.ImageDir, image+".ext4")
	os.Remove(final + ".login-user") //nolint:errcheck
	if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DiskUsageMB implements vmm.DiskReporter: the VM dir's on-host footprint
// (rootfs write delta + any memory snapshot), in MiB. A missing dir (archived /
// destroyed) is zero, not an error.
func (d *Driver) DiskUsageMB(ctx context.Context, name string) (int64, error) {
	dir := d.vmDir(name)
	if _, err := os.Stat(dir); err != nil {
		return 0, nil
	}
	out, err := exec.CommandContext(ctx, "du", "-sk", dir).Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, nil
	}
	kb, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, err
	}
	return kb / 1024, nil
}

// --- Renamer + Rebooter + CPUStatser: the user-console capabilities --------

// DropSnapshots implements vmm.Rebooter: delete the stopped VM's memory
// snapshot pair (the same files PackRootfs drops) and forget the driver's
// record of it. Without a snapshot the paused record is a trap — Resume would
// fail and the manager's recreate path would then hit Create's already-exists
// check — so forgetting it leaves the VM in the post-restart shape that
// resumeOrRecreate cold-boots from the preserved rootfs.ext4.
func (d *Driver) DropSnapshots(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.vms[name]; ok && st.machine != nil {
		return fmt.Errorf("vm %q is running; pause it first", name)
	}
	dir := d.vmDir(name)
	for _, f := range []string{"mem.snap", "state.snap"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	delete(d.vms, name)
	return nil
}

// RenameVM implements vmm.Renamer: move the stopped VM's state dir to the new
// name. Refuses while a memory snapshot exists — state.snap embeds absolute
// paths into the old dir, so resuming after the move would break; the manager
// calls DropSnapshots first, which also drops the driver record, so the next
// start cold-boots the moved rootfs.ext4 under the new name.
func (d *Driver) RenameVM(oldName, newName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.vms[oldName]; ok && st.machine != nil {
		return fmt.Errorf("vm %q is running; pause it first", oldName)
	}
	if _, ok := d.vms[newName]; ok {
		return fmt.Errorf("vm %q already exists", newName)
	}
	oldDir, newDir := d.vmDir(oldName), d.vmDir(newName)
	for _, f := range []string{"mem.snap", "state.snap"} {
		if _, err := os.Stat(filepath.Join(oldDir, f)); err == nil {
			return fmt.Errorf("vm %q has a memory snapshot; drop snapshots before renaming", oldName)
		}
	}
	if _, err := os.Stat(newDir); err == nil {
		return fmt.Errorf("vm dir for %q already exists", newName)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}
	delete(d.vms, oldName)
	return nil
}

// userHZ is the fixed unit /proc/<pid>/stat reports utime/stime in (10ms
// ticks). It is part of the kernel's userspace ABI, constant regardless of
// the kernel's CONFIG_HZ.
const userHZ = 100

// CPUTimeNanos implements vmm.CPUStatser: cumulative utime+stime of the
// firecracker process from /proc/<pid>/stat. This measures the whole FC
// process (vCPU threads + VMM overhead), so surface it to users as "host
// CPU", not guest CPU.
func (d *Driver) CPUTimeNanos(_ context.Context, name string) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok || st.machine == nil {
		return 0, fmt.Errorf("vm %q not running", name)
	}
	pid, err := st.machine.PID()
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	ticks, err := procStatCPUTicks(string(data))
	if err != nil {
		return 0, err
	}
	return ticks * (1_000_000_000 / userHZ), nil
}

// procStatCPUTicks sums the utime and stime fields (14 and 15) of a
// /proc/<pid>/stat line. The comm field may itself contain spaces and ')',
// so fields are counted from the last ')' rather than split naively.
func procStatCPUTicks(stat string) (uint64, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed stat line %q", stat)
	}
	fields := strings.Fields(stat[i+1:])
	// fields[0] is field 3 (state), so utime/stime land at indices 11/12.
	if len(fields) < 13 {
		return 0, fmt.Errorf("stat line has %d fields after comm, want >= 13", len(fields))
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("utime: %w", err)
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("stime: %w", err)
	}
	return utime + stime, nil
}

// compact fscks then zeroes the free space of an unmounted ext4 image so a
// following zstd/reflink only carries used blocks. e2fsck -fy is mandatory
// before zerofree (which refuses a dirty fs) and repairs the unclean state a
// killed VMM leaves the disk in.
func compact(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, "e2fsck", "-fy", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		// e2fsck exits 1/2 when it *corrected* errors — success for us; only >= 4
		// (uncorrected or operational error) is fatal.
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() >= 4 {
			return fmt.Errorf("e2fsck %s: %v: %s", path, err, out)
		}
	}
	if o, err := exec.CommandContext(ctx, "zerofree", path).CombinedOutput(); err != nil {
		return fmt.Errorf("zerofree %s: %v: %s", path, err, o)
	}
	return nil
}

// sanitizeTemplate strips a rootfs of its per-guest identity so every fork gets
// a fresh one — the same end state hack/build-rootfs.sh gives a freshly built
// template (blank hostname, no SSH host keys; the sparkbox-netcfg boot hook
// regenerates them via ssh-keygen -A). Best-effort per file: a template missing
// any of these is still valid.
func sanitizeTemplate(ctx context.Context, path string) error {
	mnt, err := os.MkdirTemp("", "sparkbox-snap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt) //nolint:errcheck
	if o, err := exec.CommandContext(ctx, "mount", "-o", "loop", path, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s: %v: %s", path, err, o)
	}
	for _, rel := range []string{"etc/machine-id", "var/lib/dbus/machine-id", "etc/resolv.conf"} {
		os.Remove(filepath.Join(mnt, rel)) //nolint:errcheck
	}
	os.WriteFile(filepath.Join(mnt, "etc/hostname"), nil, 0o644) //nolint:errcheck
	if keys, _ := filepath.Glob(filepath.Join(mnt, "etc/ssh/ssh_host_*")); keys != nil {
		for _, k := range keys {
			os.Remove(k) //nolint:errcheck
		}
	}
	os.RemoveAll(filepath.Join(mnt, "var/run/secrets/hivemind")) //nolint:errcheck
	os.RemoveAll(filepath.Join(mnt, "run/secrets/hivemind"))     //nolint:errcheck
	if o, err := exec.CommandContext(ctx, "umount", mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("umount %s: %v: %s", mnt, err, o)
	}
	return nil
}

func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, st := range d.vms {
		if st.machine != nil {
			st.machine.StopVMM() //nolint:errcheck
			st.cancel()
		}
	}
	return nil
}

func (d *Driver) instance(name string, st *vmState) *vmm.Instance {
	user := d.opts.LoginUser
	if user == "" {
		user = "root"
	}
	inst := &vmm.Instance{Name: name, SSHUser: user}
	if st.paused {
		inst.State = vmm.StatePaused
	} else {
		inst.State = vmm.StateRunning
		inst.SSHAddr = net.JoinHostPort(d.guestIP(st.idx), "22")
		// The proxy reaches in-VM services over the internal v4 hop (works
		// regardless of whether the guest app binds v4 or ::); the routable v6
		// is the sandbox's public identity + no-NAT egress.
		inst.HostIP = d.guestIP(st.idx)
		if d.prefix6 != nil {
			inst.GuestV6 = d.guestIP6(st.idx)
		}
	}
	return inst
}

// createTap sets up the host side of the VM's network: a tap device owned by
// this process's user with the host-side /30 address.
func (d *Driver) createTap(ctx context.Context, idx int) error {
	tap := tapName(idx)
	cmds := [][]string{
		{"ip", "tuntap", "add", "dev", tap, "mode", "tap"},
		{"ip", "addr", "add", d.hostIP(idx) + "/30", "dev", tap},
	}
	if d.prefix6 != nil {
		// Host side of the point-to-point /127; the connected route this creates
		// is how inbound traffic to the guest's /128 reaches the tap.
		cmds = append(cmds, []string{"ip", "-6", "addr", "add", d.hostIP6(idx) + "/127", "dev", tap})
	}
	cmds = append(cmds, []string{"ip", "link", "set", "dev", tap, "up"})
	for _, c := range cmds {
		if out, err := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %v: %s", c, err, out)
		}
	}
	// Strict reverse-path filtering: drop any packet from this tap whose source
	// address doesn't route back to it, so a guest can't source-spoof a
	// neighbour's address across the host's inter-tap forwarding. Set per-tap
	// because the kernel takes the max of the "all" and per-device values, so a
	// permissive host default can't undo it.
	//
	// Best-effort, like the proxy-NDP setup below: this is defence in depth, not
	// the guarantee. The metadata service identifies callers by source address
	// (see internal/metadata), and TCP already makes that unspoofable — a forged
	// SYN is answered towards the real owner of the address, so the spoofer
	// never completes the handshake. Failing sandbox creation over this would
	// trade a whole-host outage for no real security.
	exec.CommandContext(ctx, "sysctl", "-qw", "net.ipv4.conf."+tap+".rp_filter=1").Run() //nolint:errcheck

	// Answer NDP for this guest's /128 on the uplink so the provider's on-link
	// delivery of the routed /64 reaches the VM (its address lives on the tap,
	// not the uplink). Best-effort: the VM still boots if this fails, it just
	// won't have v6 return traffic. del-then-add keeps it idempotent.
	if d.prefix6 != nil && d.uplink6 != "" {
		exec.CommandContext(ctx, "sysctl", "-qw", "net.ipv6.conf."+d.uplink6+".proxy_ndp=1").Run()             //nolint:errcheck
		exec.CommandContext(ctx, "ip", "-6", "neigh", "del", "proxy", d.guestIP6(idx), "dev", d.uplink6).Run() //nolint:errcheck
		exec.CommandContext(ctx, "ip", "-6", "neigh", "add", "proxy", d.guestIP6(idx), "dev", d.uplink6).Run() //nolint:errcheck
	}
	return nil
}

func (d *Driver) deleteTap(idx int) {
	if d.prefix6 != nil && d.uplink6 != "" {
		exec.Command("ip", "-6", "neigh", "del", "proxy", d.guestIP6(idx), "dev", d.uplink6).Run() //nolint:errcheck
	}
	exec.Command("ip", "link", "del", tapName(idx)).Run() //nolint:errcheck
}

// defaultRoute6Dev returns the interface backing the IPv6 default route (e.g.
// "enp65s0f0"), or "" if there is none. Used to place proxy-NDP entries for
// guest addresses on the correct uplink.
func defaultRoute6Dev() string {
	out, err := exec.Command("ip", "-6", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// macFor derives a stable locally-administered MAC from the network slot so
// snapshots restore onto an identically-configured interface.
func macFor(idx int) string {
	return fmt.Sprintf("02:5b:00:00:%02x:%02x", (idx>>8)&0xff, idx&0xff)
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
