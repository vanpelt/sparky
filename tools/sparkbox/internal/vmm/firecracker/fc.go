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
	"sync"

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
	next    int
	prefix6 net.IP // parsed /64 network address; nil disables IPv6
}

func New(opts Options) (*Driver, error) {
	if opts.FirecrackerBin == "" {
		opts.FirecrackerBin = "firecracker"
	}
	if opts.Subnet == "" {
		opts.Subnet = "172.30.0.0"
	}
	d := &Driver{opts: opts, vms: map[string]*vmState{}, next: 1}
	if opts.Subnet6 != "" {
		_, ipNet, err := net.ParseCIDR(opts.Subnet6)
		if err != nil {
			return nil, fmt.Errorf("subnet6 %q: %w", opts.Subnet6, err)
		}
		if ones, _ := ipNet.Mask.Size(); ones > 112 {
			return nil, fmt.Errorf("subnet6 %q: need /112 or larger for per-VM addressing", opts.Subnet6)
		}
		d.prefix6 = ipNet.IP.To16()
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return nil, fmt.Errorf("firecracker driver requires /dev/kvm: %w", err)
	}
	if _, err := os.Stat(opts.KernelPath); err != nil {
		return nil, fmt.Errorf("kernel image: %w", err)
	}
	return d, nil
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

	st := &vmState{idx: d.next}
	d.next++
	if err := d.createTap(ctx, st.idx); err != nil {
		return nil, err
	}
	if err := d.boot(ctx, cfg.Name, cfg.VCPUs, cfg.MemMB, st, rootfs, nil); err != nil {
		d.deleteTap(st.idx)
		return nil, err
	}
	d.vms[cfg.Name] = st
	return d.instance(cfg.Name, st), nil
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
		st.machine.ResumeVM(ctx) //nolint:errcheck // best effort: leave it running rather than wedged
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
	inst := &vmm.Instance{Name: name, SSHUser: "root"}
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
	return nil
}

func (d *Driver) deleteTap(idx int) {
	exec.Command("ip", "link", "del", tapName(idx)).Run() //nolint:errcheck
}

// macFor derives a stable locally-administered MAC from the network slot so
// snapshots restore onto an identically-configured interface.
func macFor(idx int) string {
	return fmt.Sprintf("02:5b:00:00:%02x:%02x", (idx>>8)&0xff, idx&0xff)
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
