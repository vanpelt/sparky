//go:build linux

package qemu

import (
	"fmt"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/guestargs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/hostnet"
	"runtime"
	"strconv"
	"strings"
)

// This file is the whole of what the driver tells QEMU at exec time: one argv
// and one guest kernel command line. Everything in it is a pure function of
// explicit inputs — it touches no filesystem, starts no process and reads no
// mutable driver state — which is what lets the four measured findings in
// docs/qemu-spike.md be regression-tested on a laptop with no KVM. fc.go makes
// the same argument for extracting its kernelArgs (fc.go:704): every value on
// the command line is something the host asserts and the guest cannot forge,
// and three of them decide what the guest does to its own disk on the way up,
// so they are worth testing without a VMM in the loop.

// ---------------------------------------------------------------------------
// The argv
// ---------------------------------------------------------------------------

// qemuSpec is every per-VM input the argv needs, gathered so buildQemuArgs can
// be called from a test without a Driver, a slot allocation or a disk.
//
// RestoreFrom is the whole difference between a cold boot and a restore: empty
// means cold boot, non-empty appends `-incoming file:<path>`. See buildQemuArgs
// for why that must remain the only difference.
type qemuSpec struct {
	MachineType string // -M, and it must be a VERSIONED name; see Options.MachineType
	KernelPath  string // -kernel, an uncompressed vmlinux
	Cmdline     string // -append, from (*Driver).kernelArgs
	VCPUs       int64  // -smp
	MemMB       int64  // -m, and the balloon's baseline (see caps_vmm.go)
	RootfsPath  string // the raw ext4 image backing /dev/vda
	TapName     string // an already-created host tap, from d.net.TapName(idx)
	MAC         string // from hostnet.MAC(qemuMACOUI, idx)
	QMPSocket   string // the monitor socket; boot unlinks it before exec
	SerialLog   string // -serial file: target
	RestoreFrom string // "" for a cold boot, else the state.migrate to load
}

// buildQemuArgs returns the arguments to exec QEMU with, NOT including argv[0]
// (the caller has the resolved binary in Options.QemuBin).
//
// COLD BOOT AND RESTORE SHARE THIS FUNCTION ON PURPOSE. There is exactly one
// list, and restore appends exactly one flag to it. Do not grow a second
// builder, and do not make any other token conditional on spec.RestoreFrom.
//
// Cmdline is an INPUT rather than something built here, and that is what keeps
// -append inside the guarantee instead of beside it: bootCmdline replays the
// line the source booted with, so a restore's argv matches token for token and
// not merely device for device.
//
// What breaks if they drift, and why you will not find out here: QEMU's
// migration stream is a sequence of device sections keyed by device name and
// qdev/PCI address ("0000:00:02.0/virtio-net"), and the incoming side matches
// them positionally against the machine the argv describes. Add, remove or
// reorder a -device and every later device relocates; change -M, -smp or -m and
// the machine itself no longer matches. QEMU then refuses the load with
// something like `Unknown savevm section or instance` or
// `Length mismatch: mach-virt.ram: 0x40000000 in != 0x20000000` and exits
// nonzero. That is loud, but it is loud on *stderr* *after* exec, on a resume,
// of a sandbox that paused perfectly well an hour earlier — never at build
// time and never on the cold boot that created the snapshot. boot captures the
// child's output into qemu.log precisely so this failure has a diagnostic.
//
// Firecracker has no equivalent hazard (it reads the machine config back out of
// state.snap, which is why fc.go:1073 can pass zeros for vcpus and memMB on the
// resume path), so this is one of the places a reflex lift from fc.go is wrong.
func buildQemuArgs(spec qemuSpec) ([]string, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}

	args := []string{
		// -cpu host is KVM-only and makes the migration stream host-CPU
		// specific, so a memory snapshot is node-local. That is already the
		// contract — PackRootfs drops the snapshot and an archive restore is a
		// cold boot — but it must not be relaxed without revisiting it.
		"-M", spec.MachineType,
		"-cpu", "host",
		"-enable-kvm",
		// -nodefaults, and the machine-type suffixes that go with it, are the
		// difference between "the three devices we configured" and "those plus
		// whatever this machine model builds by default". MEASURED with
		// `info qtree` on the production x86_64 CKS node, QEMU 8.2.2, pc-q35-8.2:
		//
		//	-nodefaults       removes VGA, ide-cd, isa-parallel, isa-serial, e1000e
		//	sata=off          removes ich9-ahci  (-nodefaults alone does NOT)
		//	vmport=off        removes vmport + vmmouse, the VMware backdoor ports
		//
		// There is no floppy on q35 in any configuration, so the FDC bug class
		// people reach for first was never reachable here. What survives all
		// three is the irreducible platform: ICH9-LPC, ICH9-SMB, fw_cfg_io,
		// hpet, i8042, i8257, ioapic, isa-i8259, isa-pcspk, isa-pit, kvmvapic,
		// mc146818rtc, mch, port92, ps2-kbd, ps2-mouse, q35-pcihost,
		// smbus-eeprom. The two suffixes ride on Options.MachineType because
		// they are machine properties, not global flags, and because arm64
		// -M virt has neither.
		//
		// This is also why the argv is frozen before the first production
		// snapshot rather than tidied later: every one of these tokens changes
		// which device sections the migration stream carries, and -nodefaults
		// additionally moves the NIC's PCI address. A guest paused under one
		// argv cannot be resumed under another.
		"-nodefaults",
		"-m", strconv.FormatInt(spec.MemMB, 10),
		"-smp", strconv.FormatInt(spec.VCPUs, 10),
		"-kernel", spec.KernelPath,
		"-append", spec.Cmdline,

		// The rootfs is a bare ext4 image with no partition table, presented as
		// /dev/vda. format=raw is explicit so QEMU never probes the guest's own
		// bytes to decide the image format.
		"-drive", "file=" + spec.RootfsPath + ",format=raw,if=none,id=rootfs",
		// romfile= here goes BEYOND the measured line: hack/qemu-spike/probe.sh
		// carried it on the net and balloon devices only, and that argv booted.
		// docs/qemu-spike.md states the finding as "every PCI device needs
		// romfile=", though, and romfile is a property of PCIDevice itself — so
		// it is accepted on any PCI device and is inert where the build ships no
		// default ROM. Matching the doc's literal reading costs nothing and
		// covers the packaged QEMU (or the never-run x86_64 path) whose
		// virtio-blk-pci does have one, where the failure would otherwise be a
		// hard exec-time `failed to find romfile ...` with only qemu.log to
		// show for it.
		"-device", "virtio-blk-pci,drive=rootfs,romfile=",

		// script=no,downscript=no: the tap already exists and is addressed by
		// createTap, and QEMU must not run /etc/qemu-ifup against it.
		"-netdev", "tap,id=net0,ifname=" + spec.TapName + ",script=no,downscript=no",
		// romfile= is MEASURED, not defensive: the Ubuntu package ships no
		// option ROMs, so virtio-net-pci without it fails outright with
		// `failed to find romfile "efi-virtio.rom"` (docs/qemu-spike.md, finding
		// 4). We boot with -kernel, so the PXE ROM is dead weight anyway.
		//
		// mac= is not in the spike's probe.sh. It pins the NIC to the network
		// slot the way fc.go's macFor does, so a restored guest lands on an
		// identically configured interface instead of a fresh random address
		// that its netcfg hook has never seen.
		"-device", "virtio-net-pci,netdev=net0,mac=" + spec.MAC + ",romfile=",

		// The balloon is on the RESTORE line too, which is the opposite of
		// Firecracker: fc.go:860 deliberately skips re-adding it on resume
		// because the snapshot restores the device. Here the device comes from
		// argv and the stream is matched against argv, so omitting it on the
		// restore line leaves an incoming device section with nowhere to land.
		//
		// deflate-on-oom=on matches the `true` fc.go passes to
		// NewCreateBalloonHandler: a guest under memory pressure gives its
		// pages back rather than OOM-killing while the balloon holds them. It
		// is the one token in this argv no spike run exercised, so a
		// "Property 'virtio-balloon-pci.deflate-on-oom' not found" from a first
		// hardware run is this line and nothing subtler.
		"-device", "virtio-balloon-pci,id=" + balloonDeviceID + ",deflate-on-oom=on,romfile=",

		// server=on,wait=off: QEMU creates the socket and boots immediately
		// rather than blocking for a monitor client. boot polls for the socket
		// and then dials.
		"-qmp", "unix:" + spec.QMPSocket + ",server=on,wait=off",

		"-nographic",
		"-serial", "file:" + spec.SerialLog,
		// -monitor none: the HMP monitor would otherwise land on stdio and
		// compete with -nographic for the console.
		"-monitor", "none",
	}

	if spec.RestoreFrom != "" {
		// APPENDED LAST, and it is the only difference between the two forms.
		args = append(args, "-incoming", "file:"+spec.RestoreFrom)
	}
	return args, nil
}

// validate rejects a spec that would produce a plausible-looking argv QEMU
// misreads. The comma check is not paranoia and has no fc.go counterpart:
// QEMU's -drive/-netdev/-device/-qmp values are comma-separated property lists
// in which a literal comma must be doubled, so a single comma anywhere in a
// path silently truncates the value and turns the rest into a garbage property
// — for -drive that means booting with a different (or no) disk.
func (s qemuSpec) validate() error {
	if s.MachineType == "" {
		// New() refuses to default this off arm64 for a reason; if it is empty
		// here, bare "virt" would alias whatever machine model the installed
		// QEMU considers newest and every existing state.migrate would stop
		// loading after a package upgrade.
		return fmt.Errorf("qemu argv: machine type is empty")
	}
	if s.KernelPath == "" {
		return fmt.Errorf("qemu argv: kernel path is empty")
	}
	if s.Cmdline == "" {
		return fmt.Errorf("qemu argv: kernel command line is empty")
	}
	if s.VCPUs <= 0 {
		return fmt.Errorf("qemu argv: vcpus is %d; the restore argv must repeat the boot argv's value", s.VCPUs)
	}
	if s.MemMB <= 0 {
		return fmt.Errorf("qemu argv: memory is %d MiB; the restore argv must repeat the boot argv's value", s.MemMB)
	}
	for _, f := range []struct {
		what     string
		value    string
		optional bool
	}{
		{what: "rootfs path", value: s.RootfsPath},
		{what: "tap name", value: s.TapName},
		{what: "mac address", value: s.MAC},
		{what: "qmp socket path", value: s.QMPSocket},
		{what: "serial log path", value: s.SerialLog},
		// Empty means "cold boot", not "missing".
		{what: "incoming snapshot path", value: s.RestoreFrom, optional: true},
	} {
		if f.value == "" {
			if f.optional {
				continue
			}
			return fmt.Errorf("qemu argv: %s is empty", f.what)
		}
		if strings.Contains(f.value, ",") {
			return fmt.Errorf("qemu argv: %s %q contains a comma, which QEMU reads as a property separator", f.what, f.value)
		}
	}
	return nil
}

// bootCmdline decides which guest command line one launch gets: a cold boot
// builds it, a restore REPLAYS the line the source booted with.
//
// Replaying is what makes buildQemuArgs's "restore appends exactly one flag"
// contract true of the whole argv rather than of everything except -append.
// Without it a sandbox that cold-booted fresh restores with an -append missing
// sparkbox_fresh=1, so the first Pause/Resume of a new sandbox exercises a form
// no run has ever taken — and if QEMU's incoming load is sensitive to the
// cmdline blob at all, it fails on stderr after exec on a resume an hour later
// while a resume of a re-created sandbox succeeds. That intermittent shape is
// exactly what the argv-identity rule exists to prevent, so close it here
// rather than waive it in a test.
//
// It is also harmless for the guest, which is why replaying a stale
// sparkbox_fresh=1 is not the marker bug all over again: -kernel/-append only
// seed guest RAM at reset, and an incoming stream overwrites that RAM. A
// restored guest reads the cmdline it FIRST booted with out of its own memory
// no matter what the host passes here.
//
// The empty-cmdline fallback covers a restore of a record that carries no
// command line. Nothing can produce one today — d.vms is per-process and only
// boot registers a cmdline, so a paused record always has the one its cold boot
// recorded — but rebuilding is the old behaviour, where an empty -append is a
// validate() failure that would strand a resumable sandbox.
func (d *Driver) bootCmdline(name string, st *vmState, restore, fresh bool) (string, error) {
	if restore && st.cmdline != "" {
		return st.cmdline, nil
	}
	return d.kernelArgs(name, st.idx, fresh)
}

// qemuArgs builds the argv for one VM. restore selects the -incoming form and
// is the only boolean: the cmdline is passed in, from bootCmdline, so that this
// function cannot be the place a restore's -append quietly diverges.
//
// Everything it reads is either an argument or immutable driver configuration,
// so it needs no lock. The per-VM paths come from lifecycle.go, which is the
// only file allowed to name them.
func (d *Driver) qemuArgs(name string, st *vmState, rootfs, cmdline string, restore bool) ([]string, error) {
	spec := qemuSpec{
		MachineType: d.opts.MachineType,
		KernelPath:  d.opts.KernelPath,
		Cmdline:     cmdline,
		VCPUs:       st.vcpus,
		MemMB:       st.memMB,
		RootfsPath:  rootfs,
		TapName:     d.net.TapName(st.idx),
		MAC:         hostnet.MAC(qemuMACOUI, st.idx),
		QMPSocket:   d.qmpSocketPath(name),
		SerialLog:   d.serialLogPath(name),
	}
	if restore {
		spec.RestoreFrom = d.snapshotPath(name)
	}
	return buildQemuArgs(spec)
}

// ---------------------------------------------------------------------------
// The guest kernel command line
// ---------------------------------------------------------------------------

// guestConsole names the serial console the guest kernel should log to.
//
// On arm64 -M virt the platform serial port is a PL011 at ttyAMA0, not the
// 8250 at ttyS0 that Firecracker's MMIO machine exposes (docs/qemu-spike.md,
// finding 3). Note the finding's sting: the arm64 guest kernel we ship has no
// PL011 driver at all, so -serial captured 0 bytes across every spike boot
// while the guest was demonstrably healthy over SSH. Naming the right device
// costs nothing and starts working the day CONFIG_SERIAL_AMBA_PL011 lands in
// the guest kernel fragment; until then a QEMU sandbox is undebuggable by
// serial and qemu.log is the only diagnostic.
//
// x86_64 keeps ttyS0 — but nothing in this package has ever been run there
// (New refuses to guess a machine type on amd64 for the same reason), so treat
// this branch as a placeholder that has not been measured.
func guestConsole() string {
	if runtime.GOARCH == "arm64" {
		return "ttyAMA0"
	}
	return "ttyS0"
}

// kernelArgs assembles the guest command line. It is Firecracker's string
// (fc.go:711) with the four differences docs/qemu-spike.md measured, and
// nothing else: everything that carries over carries over for the reasons
// fc.go records, so those reasons are repeated here rather than lost in a diff.
//
// The four measured differences from fc.go:
//
//  1. pci=off is GONE. Firecracker is MMIO-only; -M virt puts virtio on PCIe.
//     Left in, the guest comes up with no disk and no NIC, which presents as a
//     boot that hangs rather than as a command-line bug.
//  2. root=/dev/vda rw is ADDED. Firecracker synthesises the root device from
//     the drive's is_root_device flag; QEMU does not, and there is no initramfs
//     to work it out.
//  3. console= is the platform console for the architecture, ttyAMA0 on arm64
//     rather than ttyS0. See guestConsole.
//  4. (argv, not cmdline) every PCI device that would load an option ROM gets
//     romfile=. See buildQemuArgs.
//
// Everything else is fc.go's, unchanged and for fc.go's reasons.
func (d *Driver) kernelArgs(name string, idx int, fresh bool) (string, error) {
	// Static guest networking via kernel arg — no DHCP daemon needed. The ip=
	// arg is IPv4-only; IPv6 is applied inside the guest by the sparkbox-netcfg
	// hook (build-rootfs.sh), which reads sparkbox_ip6/sparkbox_gw6 below.
	//
	// quiet is kept from fc.go. The spike's throwaway probe.sh dropped it, but
	// it makes no difference on a guest kernel with no console driver, and
	// dropping it would be an unmeasured divergence from the Firecracker guest.
	// It is, however, the first thing to remove when a boot fails silently
	// after CONFIG_SERIAL_AMBA_PL011 lands.
	kernelArgs := fmt.Sprintf(
		"console=%s reboot=k panic=1 root=/dev/vda rw quiet ip=%s::%s:255.255.255.252::eth0:off sparkbox_host=%s systemd.machine_id=%s",
		guestConsole(), d.net.GuestIP(idx), d.net.HostIP(idx), name, guestargs.MachineID(name))
	if d.net.Prefix6 != nil {
		kernelArgs += fmt.Sprintf(" sparkbox_ip6=%s/127 sparkbox_gw6=%s",
			d.net.GuestIP6(idx), d.net.HostIP6(idx))
	}
	dnsArg, err := guestargs.DNSArg(d.opts.GuestDNS, d.net.HostIP(idx))
	if err != nil {
		return "", err
	}
	kernelArgs += dnsArg
	// The first boot of a disk that was just copied from a template. Written by
	// the host and unforgeable by a guest, exactly like sparkbox_host, and read
	// by sparkbox-repos to decide whether it may ADOPT an inherited checkout —
	// switch it to the branch the manifest names — rather than merely report
	// that it is on a different one. That is safe here and nowhere else: nobody
	// has logged in yet, so there is no work in flight to lose.
	//
	// Absent on a resume, on a cold boot of an existing sandbox, and on every
	// host built before this existed. All three degrade the same way: a guest
	// that keeps the branch it inherited and says so.
	if fresh {
		kernelArgs += " sparkbox_fresh=1"
	}
	return kernelArgs, nil
}
