//go:build linux

// Package firecracker implements vmm.Driver on real Firecracker microVMs via
// the official firecracker-go-sdk. It requires /dev/kvm, root (for tap device
// setup), a vmlinux kernel, and per-image ext4 rootfs templates produced by
// hack/build-rootfs.sh. Before each guest boots, the driver replaces the
// template's baked fleet key with the gateway public key in vmm.Config.
//
// This driver has been exercised end to end on both a Linux KVM host and the
// nested ARM64 macOS gateway proof of concept. Known gaps vs production: no
// jailer, no balloon management, no UFFD lazy restore, no I/O rate limits.
package firecracker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
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
	// Subnet is an IPv4 CIDR carved into per-VM /30 slots. Empty uses the
	// standalone compatibility default 172.30.0.0/16.
	Subnet string
	// Subnet6 is a routable IPv6 /64 delegated to the host (e.g.
	// "2001:db8:1c7::/64"). When set, each VM gets a globally-routable /128 from
	// it (dual-stack, no NAT). Empty keeps VMs IPv4-only.
	Subnet6 string
	// LoginUser is the guest account the gateway SSHes in as — set to match the
	// template's baked authorized_keys (our images declare it via the
	// sparkbox.login-user label; see hack/build-rootfs.sh). Empty defaults root.
	LoginUser string
	// GuestDNS points guests at a specific resolver via the sparkbox_dns kernel
	// arg, honoured by the guest sparkbox-netcfg hook. The literal "gateway"
	// expands per-VM to the guest's own host-side address, where the
	// sluice allowlist resolver listens; any other value is used verbatim as the
	// nameserver address. Empty leaves guests on public resolvers (no
	// allowlisting).
	GuestDNS string
}

type vmState struct {
	idx     int // /30 network slot; host is +1 and guest is +2
	machine *sdk.Machine
	cancel  context.CancelFunc
	paused  bool
}

type Driver struct {
	mu   sync.Mutex
	opts Options
	vms  map[string]*vmState
	// creating holds the names of Creates that have released d.mu to do their
	// rootfs disk work. A name is in exactly one of creating and vms.
	creating map[string]bool
	guestNet guestnet.Network
	// reservedSlots holds /30s occupied by a host service address, such as a
	// dedicated in-prefix sluice DNS listener. They count toward prefix
	// capacity but are never handed to a VM.
	reservedSlots map[int]bool
	prefix6       net.IP // parsed /64 network address; nil disables IPv6
	uplink6       string // iface backing the v6 default route, for per-guest proxy NDP
}

// reserveName claims cfg.Name for an in-flight Create so no second Create
// prepares the same rootfs concurrently, and so the name is still refused
// while d.mu is released.
func (d *Driver) reserveName(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creating == nil {
		d.creating = map[string]bool{}
	}
	if _, ok := d.vms[name]; ok {
		return fmt.Errorf("vm %q already exists", name)
	}
	if d.creating[name] {
		return fmt.Errorf("vm %q is already being created", name)
	}
	d.creating[name] = true
	return nil
}

func (d *Driver) releaseName(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.creating, name)
}

func New(opts Options) (*Driver, error) {
	if opts.FirecrackerBin == "" {
		opts.FirecrackerBin = "firecracker"
	}
	guestNetwork, err := guestnet.Parse(opts.Subnet)
	if err != nil {
		return nil, err
	}
	// macFor encodes the slot in two bytes. The supported defaults are much
	// smaller (/16 and /20), but fail explicitly rather than silently reusing
	// MAC addresses if an unusually broad prefix is configured.
	if guestNetwork.Capacity() > 1<<16 {
		return nil, fmt.Errorf("guest subnet %s has %d slots; firecracker supports at most 65536",
			guestNetwork, guestNetwork.Capacity())
	}
	opts.Subnet = guestNetwork.String()
	if err := validateGuestDNS(opts.GuestDNS); err != nil {
		return nil, err
	}
	d := &Driver{
		opts: opts, vms: map[string]*vmState{}, creating: map[string]bool{},
		guestNet: guestNetwork, reservedSlots: map[int]bool{},
	}
	if dnsAddr, err := netip.ParseAddr(opts.GuestDNS); err == nil {
		if index, ok := guestNetwork.SlotContaining(dnsAddr.Unmap()); ok {
			d.reservedSlots[index] = true
		}
	}
	if opts.Subnet6 != "" {
		_, ipNet, err := net.ParseCIDR(opts.Subnet6)
		if err != nil {
			return nil, fmt.Errorf("subnet6 %q: %w", opts.Subnet6, err)
		}
		ones, _ := ipNet.Mask.Size()
		if ones > 112 {
			return nil, fmt.Errorf("subnet6 %q: need /112 or larger for per-VM addressing", opts.Subnet6)
		}
		// IPv6 reserves ::1 and assigns one /127 per IPv4 guest slot. A broad
		// IPv4 prefix can therefore outgrow an otherwise valid /112; reject the
		// pair now instead of wrapping addresses into a different IPv6 prefix
		// after enough VMs have been created.
		hostBits := 128 - ones
		if hostBits < 32 {
			available := uint64(1) << hostBits
			required := uint64(guestNetwork.Capacity())*2 + 2
			if required > available {
				return nil, fmt.Errorf(
					"subnet6 %q has %d addresses; guest subnet %s needs at least %d",
					opts.Subnet6, available, guestNetwork, required,
				)
			}
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

func (d *Driver) hostIP(idx int) string  { return d.guestSlot(idx).Host.String() }
func (d *Driver) guestIP(idx int) string { return d.guestSlot(idx).Guest.String() }
func tapName(idx int) string             { return fmt.Sprintf("sbtap%d", idx) }

func (d *Driver) guestSlot(idx int) guestnet.Slot {
	slot, err := d.guestNet.Slot(idx)
	if err != nil {
		// Slots are allocated by freeSlot and stored in vmState, so reaching
		// this means an internal invariant was violated rather than bad input.
		panic(err)
	}
	return slot
}

// validateGuestDNS accepts only the empty string (feature off), the "gateway"
// sentinel, or a bare IP literal. Anything else — a hostname, or a value with
// whitespace that would inject extra kernel args — is rejected, so a typo in
// --guest-dns fails loudly instead of producing a malformed cmdline or an
// unusable /etc/resolv.conf inside the guest.
func validateGuestDNS(guestDNS string) error {
	switch guestDNS {
	case "", "gateway":
		return nil
	}
	if _, err := netip.ParseAddr(guestDNS); err != nil {
		return fmt.Errorf("guest-dns %q: must be \"gateway\" or an IP address", guestDNS)
	}
	return nil
}

// guestDNSArg builds the sparkbox_dns kernel-arg fragment (with a leading space)
// for the guest netcfg hook. The sentinel "gateway" expands to this VM's gateway
// address, where the sluice allowlist resolver listens; an IP literal is used
// verbatim. An empty setting yields no arg, leaving the guest on public DNS.
func guestDNSArg(guestDNS, gatewayIP string) (string, error) {
	if err := validateGuestDNS(guestDNS); err != nil {
		return "", err
	}
	switch guestDNS {
	case "":
		return "", nil
	case "gateway":
		return " sparkbox_dns=" + gatewayIP, nil
	default:
		return " sparkbox_dns=" + guestDNS, nil
	}
}

// IPv6 addressing: each slot gets a point-to-point /127 carved from the /64,
// host on the even address and guest on the odd one. IPv4 slot indexes begin
// at zero, so add one before deriving the IPv6 offset: slot idx=0 becomes
// ::2 (host) / ::3 (guest), leaving ::1 free for the host's own edge address
// (the AAAA target). Globally routable, so egress needs no NAT — just host
// forwarding.
func (d *Driver) hostIP6(idx int) string  { return d.addr6((idx + 1) * 2) }
func (d *Driver) guestIP6(idx int) string { return d.addr6((idx+1)*2 + 1) }

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
	// Preparing the rootfs means a template copy and a loop mount that may
	// replay an ext4 journal on a 25 GiB image — far too slow to hold the
	// driver-wide lock through, especially on a restart recreating every
	// sandbox in turn. Reserve the name instead, do the disk work unlocked,
	// then take d.mu for the slot bookkeeping and boot.
	if err := d.reserveName(cfg.Name); err != nil {
		return nil, err
	}
	defer d.releaseName(cfg.Name)

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
	if err := installAuthorizedKey(ctx, rootfs, d.opts.LoginUser, cfg.GatewayPublicKey); err != nil {
		return nil, fmt.Errorf("install gateway key: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// Re-check under the lock: reserveName only excludes a second Create, and
	// Restore could have registered this name while the disk work ran.
	if _, ok := d.vms[cfg.Name]; ok {
		return nil, fmt.Errorf("vm %q already exists", cfg.Name)
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
// record only goes away after the tap does. The configured prefix determines
// the bound: every index maps to exactly one /30 inside it.
func (d *Driver) freeSlot() (int, error) {
	used := make(map[int]bool, len(d.vms))
	for _, s := range d.vms {
		used[s.idx] = true
	}
	for idx := 0; idx < d.guestNet.Capacity(); idx++ {
		if !used[idx] && !d.reservedSlots[idx] {
			return idx, nil
		}
	}
	return 0, fmt.Errorf("no free network slots in %s (max %d concurrent VMs)",
		d.guestNet, d.guestNet.Capacity())
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
	dnsArg, err := guestDNSArg(d.opts.GuestDNS, d.hostIP(st.idx))
	if err != nil {
		return err
	}
	kernelArgs += dnsArg

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

// DiskUsageMB implements vmm.DiskReporter: blocks used *inside* the sandbox's
// ext4 filesystem. A missing image (archived / destroyed) is zero, not an
// error.
//
// Host-side `du` is not this measurement. It counts shared reflink extents once
// for every clone, and a decompressor that materializes the template's zeroes
// makes an almost-empty 25 GiB filesystem look 25 GiB full. Reading the ext4
// superblock gives the value users expect beside the filesystem's hard ceiling
// and makes pooled accounting independent of sparse/reflink representation.
//
// Deliberately ignore mem.snap: it is a transient host implementation detail,
// not durable sandbox storage, and is discarded on the next cold boot.
func (d *Driver) DiskUsageMB(_ context.Context, name string) (int64, error) {
	rootfs := d.rootfsPath(name)
	if _, err := os.Stat(rootfs); err != nil {
		return 0, nil
	}
	used, _, err := ext4DiskMB(rootfs)
	return used, err
}

// ext4DiskMB reads the primary ext4 superblock directly and follows statfs(2):
// capacity = total blocks - filesystem metadata overhead, used = capacity -
// free. Both come from the same superblock read so the console's meter has a
// numerator and denominator on the same basis — measuring used against the raw
// image size instead would leave a genuinely full guest short of 100% by
// however much metadata the filesystem holds. Firecracker owns the image and
// may have it mounted in a guest, so invoking e2fsck/debugfs here would be
// unsafe; a fixed-size read is passive and gives a sufficiently fresh
// best-effort counter for the periodic console measurement.
func ext4DiskMB(path string) (usedMB, capacityMB int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	const (
		superOffset      = 1024
		superSize        = 1024
		ext4Magic        = 0xef53
		incompat64Bit    = 0x80
		roCompatBigalloc = 0x200
		maxLogBlockSize  = 6 // 1024 << 6 == ext4's 64 KiB maximum
		bytesPerMiB      = 1024 * 1024
		maxSignedInt64   = uint64(1<<63 - 1)
	)
	sb := make([]byte, superSize)
	if _, err := f.ReadAt(sb, superOffset); err != nil {
		return 0, 0, fmt.Errorf("read ext4 superblock: %w", err)
	}
	if magic := binary.LittleEndian.Uint16(sb[0x38:0x3a]); magic != ext4Magic {
		return 0, 0, fmt.Errorf("rootfs has invalid ext4 magic %#x", magic)
	}
	logBlockSize := binary.LittleEndian.Uint32(sb[0x18:0x1c])
	if logBlockSize > maxLogBlockSize {
		return 0, 0, fmt.Errorf("rootfs has invalid ext4 block-size shift %d", logBlockSize)
	}
	blocks := uint64(binary.LittleEndian.Uint32(sb[0x04:0x08]))
	free := uint64(binary.LittleEndian.Uint32(sb[0x0c:0x10]))
	incompat := binary.LittleEndian.Uint32(sb[0x60:0x64])
	roCompat := binary.LittleEndian.Uint32(sb[0x64:0x68])
	if roCompat&roCompatBigalloc != 0 {
		return 0, 0, fmt.Errorf("rootfs ext4 bigalloc is not supported for disk accounting")
	}
	if incompat&incompat64Bit != 0 {
		blocks |= uint64(binary.LittleEndian.Uint32(sb[0x150:0x154])) << 32
		free |= uint64(binary.LittleEndian.Uint32(sb[0x158:0x15c])) << 32
	}
	if free > blocks {
		return 0, 0, fmt.Errorf("rootfs ext4 free blocks %d exceed total blocks %d", free, blocks)
	}
	blockSize := uint64(1024) << logBlockSize
	usedBlocks := blocks - free
	// s_overhead_last is the number of filesystem-metadata blocks excluded
	// from statfs.f_blocks, and therefore from both `df`'s used figure and its
	// total.
	overhead := uint64(binary.LittleEndian.Uint32(sb[0x248:0x24c]))
	if overhead > usedBlocks {
		return 0, 0, fmt.Errorf("rootfs ext4 overhead blocks %d exceed occupied blocks %d",
			overhead, usedBlocks)
	}
	usedBlocks -= overhead
	capacityBlocks := blocks - overhead
	if capacityBlocks > maxSignedInt64/blockSize {
		return 0, 0, fmt.Errorf("rootfs ext4 block count overflows int64")
	}
	return int64(usedBlocks * blockSize / bytesPerMiB),
		int64(capacityBlocks * blockSize / bytesPerMiB), nil
}

// ResizeDisk implements vmm.DiskResizer: grow a stopped sandbox's rootfs to
// sizeMB. The image is a bare ext4 (no partition table), so this is the same
// three steps we run on a template — fsck, extend the file, extend the
// filesystem into it — in the order that keeps the disk consistent if we die
// partway: a truncate that lands without the resize2fs just leaves unused tail
// bytes, whereas the reverse would leave a filesystem larger than its device.
//
// stoppedRootfs is the guard against resizing under a live guest. It is the
// second half of the safety pairing; the first (dropping the memory snapshot)
// belongs to the manager, which is the only layer that knows about snapshots.
func (d *Driver) ResizeDisk(ctx context.Context, name string, sizeMB int64) error {
	rootfs, err := d.stoppedRootfs(name)
	if err != nil {
		return err
	}
	fi, err := os.Stat(rootfs)
	if err != nil {
		return err
	}
	want := sizeMB * 1024 * 1024
	if want <= fi.Size() {
		return fmt.Errorf("disk is already %d MB; resize only grows (asked for %d MB)",
			fi.Size()/(1024*1024), sizeMB)
	}
	// -f because the image is unmounted but not necessarily marked clean, and
	// resize2fs refuses a dirty filesystem outright.
	if out, err := exec.CommandContext(ctx, "e2fsck", "-fy", rootfs).CombinedOutput(); err != nil {
		// e2fsck exits 1 when it fixed something, which is success for us; only
		// 4+ (uncorrected errors) is fatal.
		if code := exitCode(err); code > 2 {
			return fmt.Errorf("e2fsck: %v: %s", err, out)
		}
	}
	if err := os.Truncate(rootfs, want); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "resize2fs", rootfs).CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs: %v: %s", err, out)
	}
	return nil
}

// exitCode extracts a process exit status, or -1 if err isn't an exit error.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// DiskCapacityMB implements vmm.DiskReporter: the guest's hard ceiling, read
// from the same superblock as DiskUsageMB so the two agree — the space `df`
// inside the guest calls the filesystem total, which is the image minus the
// metadata overhead the guest can never spend. Discovered per sandbox rather
// than configured, so boxes created from differently-sized templates each
// report their own ceiling. 0 when the image is missing; an image that is
// present but unreadable is an error, as it is for DiskUsageMB.
func (d *Driver) DiskCapacityMB(_ context.Context, name string) (int64, error) {
	rootfs := d.rootfsPath(name)
	if _, err := os.Stat(rootfs); err != nil {
		return 0, nil
	}
	_, capacity, err := ext4DiskMB(rootfs)
	return capacity, err
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

// NetBytes implements vmm.NetStatser from the host tap's byte counters.
// Directions are swapped on the way out: the tap's rx is traffic the *guest*
// transmitted, its tx traffic the guest received. The counters are owned by
// the tap device, which createTap/deleteTap cycle on every pause/resume, so
// they restart at zero far more often than the CPU counter — callers must
// treat a decrease as a reset.
func (d *Driver) NetBytes(_ context.Context, name string) (rx, tx uint64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok || st.machine == nil {
		return 0, 0, fmt.Errorf("vm %q not running", name)
	}
	tap := tapName(st.idx)
	// Guest rx is the tap's tx and vice versa.
	if rx, err = readTapCounter(tap, "tx_bytes"); err != nil {
		return 0, 0, err
	}
	if tx, err = readTapCounter(tap, "rx_bytes"); err != nil {
		return 0, 0, err
	}
	return rx, tx, nil
}

// readTapCounter reads one of a tap device's sysfs byte counters.
func readTapCounter(tap, stat string) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/statistics/%s", tap, stat))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s/%s: %w", tap, stat, err)
	}
	return n, nil
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

type loginIdentity struct {
	home     string
	uid, gid int
}

func rootfsLoginIdentity(passwd []byte, user string) (loginIdentity, error) {
	if user == "" {
		user = "root"
	}
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 7 || fields[0] != user {
			continue
		}
		uid, uerr := strconv.Atoi(fields[2])
		gid, gerr := strconv.Atoi(fields[3])
		home := filepath.Clean(fields[5])
		if uerr != nil || gerr != nil || !filepath.IsAbs(home) || home == "/" {
			return loginIdentity{}, fmt.Errorf("invalid passwd entry for %q", user)
		}
		return loginIdentity{home: home, uid: uid, gid: gid}, nil
	}
	return loginIdentity{}, fmt.Errorf("login user %q not found in guest /etc/passwd", user)
}

func installAuthorizedKey(ctx context.Context, rootfs, loginUser, key string) (retErr error) {
	if key == "" {
		return nil
	}
	publicKey, _, _, rest, err := xssh.ParseAuthorizedKey([]byte(key))
	if err != nil {
		return fmt.Errorf("gateway upstream public key is invalid: %w", err)
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return errors.New("gateway upstream public key is invalid: trailing data")
	}
	key = strings.TrimSpace(string(xssh.MarshalAuthorizedKey(publicKey)))
	mnt, err := os.MkdirTemp("", "sparkbox-key-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt) //nolint:errcheck
	if out, err := exec.CommandContext(ctx, "mount", "-o", "loop", rootfs, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s: %v: %s", rootfs, err, out)
	}
	defer func() {
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil && retErr == nil {
			retErr = fmt.Errorf("umount %s: %v: %s", mnt, err, out)
		}
	}()
	return writeAuthorizedKey(mnt, loginUser, key)
}

// writeAuthorizedKey puts key in the login user's authorized_keys inside an
// already-mounted guest rootfs.
//
// Everything here touches a filesystem the guest owns and can have rewritten
// arbitrarily, so it goes through os.Root: every path resolves beneath mnt,
// and a symlink that would escape it (any absolute one, or a relative one
// climbing past the root) is refused rather than followed onto the host.
// Without that a guest could point ~/.ssh at /etc/ssh and have the root-owned
// gateway chown and write *host* files on its next cold boot.
func writeAuthorizedKey(mnt, loginUser, key string) error {
	root, err := os.OpenRoot(mnt)
	if err != nil {
		return fmt.Errorf("open rootfs %s: %w", mnt, err)
	}
	defer root.Close() //nolint:errcheck

	passwd, err := root.ReadFile("etc/passwd")
	if err != nil {
		return err
	}
	identity, err := rootfsLoginIdentity(passwd, loginUser)
	if err != nil {
		return err
	}
	home := strings.TrimPrefix(identity.home, "/")
	if err := ensureGuestDir(root, home, 0o755, identity); err != nil {
		return err
	}
	// ~/.ssh is the exception to ensureGuestDir's leave-it-alone rule: sshd's
	// StrictModes ignores authorized_keys in a directory the login user does
	// not own or that anyone else can write, so these two are enforced even on
	// a directory the guest already had.
	sshDir := path.Join(home, ".ssh")
	if err := ensureGuestDir(root, sshDir, 0o700, identity); err != nil {
		return err
	}
	if err := root.Chmod(sshDir, 0o700); err != nil {
		return err
	}
	if err := root.Lchown(sshDir, identity.uid, identity.gid); err != nil {
		return err
	}

	// A read failure is not fatal: absent is the common case, and a dangling
	// symlink or a directory sitting in authorized_keys' place is the guest's
	// own mess — either way the gateway key still has to land. Replace whatever
	// is there rather than writing through it.
	authorizedKeys := path.Join(sshDir, "authorized_keys")
	existing, _ := root.ReadFile(authorizedKeys) //nolint:errcheck
	if err := root.RemoveAll(authorizedKeys); err != nil {
		return err
	}
	if err := root.WriteFile(authorizedKeys, mergeAuthorizedKeys(existing, key), 0o600); err != nil {
		return err
	}
	return root.Lchown(authorizedKeys, identity.uid, identity.gid)
}

// ensureGuestDir makes name a real directory inside the guest rootfs.
//
// A directory that is already there is left exactly as it is — mode and
// ownership included, since a guest may legitimately run a 0750 home and it is
// not our place to widen it. Anything else is replaced: a regular file, or the
// symlink a guest plants to aim our writes somewhere it prefers. A directory we
// create gets perm and the login user, because a root-owned home is precisely
// what makes sshd's StrictModes refuse the account later.
func ensureGuestDir(root *os.Root, name string, perm os.FileMode, identity loginIdentity) error {
	switch fi, err := root.Lstat(name); {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		if err := root.Remove(name); err != nil {
			return err
		}
	case !os.IsNotExist(err):
		return err
	}
	if err := root.MkdirAll(name, perm); err != nil {
		return err
	}
	if err := root.Chmod(name, perm); err != nil {
		return err
	}
	return root.Lchown(name, identity.uid, identity.gid)
}

// mergeAuthorizedKeys adds the gateway key to a guest's existing
// authorized_keys instead of replacing the file. Create is not only the
// first-boot path — the manager re-runs it whenever a resume fails — so
// overwriting would silently drop keys the user added inside their own
// sandbox. Duplicates of the gateway key itself are collapsed, comparing the
// parsed key so a differing comment or option prefix is not mistaken for a
// second key.
func mergeAuthorizedKeys(existing []byte, key string) []byte {
	var out []string
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if sameAuthorizedKey(line, key) {
			continue
		}
		out = append(out, line)
	}
	out = append(out, key)
	return []byte(strings.Join(out, "\n") + "\n")
}

// sameAuthorizedKey reports whether two authorized_keys lines carry the same
// public key. Both sides are reduced to the bare type-and-blob form so a
// differing comment or option prefix is not read as a second key — the gateway
// key arrives with a comment on it, and a line that already holds it will have
// whatever comment the last write left behind.
func sameAuthorizedKey(a, b string) bool {
	normalize := func(line string) (string, bool) {
		pub, _, _, _, err := xssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(xssh.MarshalAuthorizedKey(pub))), true
	}
	na, aok := normalize(a)
	nb, bok := normalize(b)
	return aok && bok && na == nb
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
