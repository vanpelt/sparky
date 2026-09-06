// Package qemuargs builds the QEMU command line, and it is a package rather
// than a function inside the driver because TWO processes now build that
// command line.
//
// On a dev box and in the parity suite, internal/vmm/qemu execs QEMU itself.
// On a hardened node it does not: the unprivileged controller has no /dev/kvm
// and no NET_ADMIN, so internal/vmhelper — a root process behind a Unix socket
// whose protocol carries no commands and no paths — builds the argv and execs
// QEMU on its behalf. Firecracker needed no such split, because it takes an
// empty argv and is configured over its own REST socket afterwards; QEMU takes
// the whole machine on the command line and there is no afterwards.
//
// Two copies of this list would be a slow, quiet failure. QEMU's migration
// stream is matched positionally against the argv (see Build), so a divergence
// between the process that snapshots a guest and the process that restores it
// does not show up at build time, at review time, or on the boot that takes the
// snapshot. It shows up as a resume failing on stderr an hour later. Both
// callers import this, so there is one list to diverge from.
//
// It deliberately has no build tag and touches no filesystem, starts no process
// and reads no mutable state: everything here is a pure function of its inputs,
// so the whole argv contract is testable on a laptop with no KVM.
package qemuargs

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// BalloonDeviceID must appear as `id=` on the -device line: the QOM path guest
// stats are read from is derived from it, and an unnamed device lands under
// /machine/peripheral-anon/device[N] with an index that shifts whenever another
// device is added.
const BalloonDeviceID = "balloon0"

// DefaultMachineType is the -M argument for this architecture, and it lives
// here because TWO processes need the same answer: the driver, which records
// the machine a sandbox was booted with, and the privileged helper, which
// builds the argv that has to describe the identical machine when that sandbox
// is restored. A per-arch default pinned separately in each would agree until
// one of them was edited, and the disagreement would first appear as a resume
// that cannot load its stream.
//
// It MUST stay a versioned name. The migration stream is keyed on the machine
// model and bare "virt"/"q35" alias whatever the installed binary considers
// newest, so an unpinned type silently changes model on a package upgrade and
// strands every paused sandbox. Versioned types are retained across releases.
//
// arm64's virt-8.2 is the pairing docs/qemu-spike.md measured. amd64's
// pc-q35-8.2 is what the parity suite ran 19/19 against on the production CKS
// node; sata=off and vmport=off are measured hardening rather than taste — see
// the -nodefaults comment in Build for the `info qtree` device lists — and they
// ride on the machine type because they are machine properties, which is also
// why an operator who overrides this gets exactly what they asked for and
// loses them.
func DefaultMachineType() (string, error) {
	switch runtime.GOARCH {
	case "arm64":
		return "virt-8.2", nil
	case "amd64":
		return "pc-q35-8.2,sata=off,vmport=off", nil
	default:
		return "", fmt.Errorf("no known qemu machine type for GOARCH %q", runtime.GOARCH)
	}
}

// ---------------------------------------------------------------------------
// The argv
// ---------------------------------------------------------------------------

// Spec is every per-VM input the argv needs, gathered so Build can
// be called from a test without a Driver, a slot allocation or a disk.
//
// RestoreFrom is the whole difference between a cold boot and a restore: empty
// means cold boot, non-empty appends `-incoming file:<path>`. See Build
// for why that must remain the only difference.
type Spec struct {
	MachineType string // -M, and it must be a VERSIONED name; see Options.MachineType
	KernelPath  string // -kernel, an uncompressed vmlinux
	Cmdline     string // -append, from (*Driver).kernelArgs
	VCPUs       int64  // -smp
	MemMB       int64  // -m, and the balloon's baseline (see caps_vmm.go)
	RootfsPath  string // the raw ext4 image backing /dev/vda
	TapName     string // an already-created host tap, from tapName(idx)
	MAC         string // from macFor(idx)
	QMPSocket   string // the monitor socket; boot unlinks it before exec
	SerialLog   string // -serial file: target
	RestoreFrom string // "" for a cold boot, else the state.migrate to load
	// Confine is zero on the direct launcher and set by the privileged helper.
	Confine Confine
}

// Confine is the self-confinement QEMU applies to ITSELF, late in startup.
//
// It exists because the alternative does not work here. The Firecracker jail is
// built by the parent: internal/vmhelper copies the binary into a chroot,
// mknod's the device nodes, and hands the child a SysProcAttr with Chroot and
// Credential set, so the VMM is already unprivileged when it starts. QEMU
// cannot be launched that way — it must open /dev/kvm, the tap, the rootfs and
// the kernel with the caller's privileges first, and only then drop. QEMU knows
// this and does it itself: os_setup_post() runs chroot(2) and setuid(2) after
// the machine is built and before the main loop.
//
// MEASURED on the production x86_64 CKS node, QEMU 8.2.2, inside a byte-for-byte
// copy of the vmm-helper securityContext: all four slots came up with Uid and
// Gid 100000, CapEff and CapPrm both zero, NoNewPrivs 1 and Seccomp 2, and a
// path outside the jail resolved to ENOENT. That is the same posture the
// homegrown Firecracker launcher reaches, from the child's own argv.
//
// These flags are NOT part of the machine and do not appear in the migration
// stream, so they do not compromise the argv-identity rule Build documents.
// They are a property of the launcher, and one node has exactly one launcher.
type Confine struct {
	// ChrootDir is passed as `-run-with chroot=`. QEMU chroots into it and then
	// chdir("/"), which is why every per-VM path in the argv must be RELATIVE:
	// a relative name resolves against the jail before the chroot (the parent
	// sets cmd.Dir to this same directory) and against the jail after it. That
	// matters because the two are not resolved at the same time — -kernel,
	// -drive, -qmp, -serial and -incoming are all opened during startup, while
	// the runtime `migrate uri=file:` that writes a snapshot is resolved from
	// the QMP monitor long after the chroot. Relative paths make the question
	// moot instead of making it a thing to get right twice.
	ChrootDir string
	// UID is passed as `-runas <uid>:<uid>`, the per-slot uid the helper also
	// owns the jail's files with.
	//
	// -runas is deprecated in favour of `-run-with user=`, which QEMU 8.2 does
	// not have. It is what was measured; move it the day the floor rises.
	UID int
	// Sandbox adds `-sandbox on`, QEMU's seccomp filter.
	//
	// Its default option set is DEFAULT|OBSOLETE, which denies obsolete syscalls
	// and nothing else — in particular it does NOT imply elevateprivileges=deny,
	// so it cannot break the setuid that -runas performs afterwards. The CKS run
	// above had both on at once and reached uid 100000 with Seccomp 2, so this
	// is measured rather than reasoned.
	Sandbox bool
}

// Build returns the arguments to exec QEMU with, NOT including argv[0]
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
func Build(spec Spec) ([]string, error) {
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
		"-device", "virtio-balloon-pci,id=" + BalloonDeviceID + ",deflate-on-oom=on,romfile=",

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

	// Appended AFTER the machine and BEFORE -incoming, so -incoming stays the
	// last token and the only difference between the two forms.
	args = append(args, spec.Confine.args()...)

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
func (s Spec) validate() error {
	if err := s.Confine.Validate(); err != nil {
		return err
	}
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
	for _, f := range []struct{ what, value string }{
		{"rootfs path", s.RootfsPath},
		{"tap name", s.TapName},
		{"mac address", s.MAC},
		{"qmp socket path", s.QMPSocket},
		{"serial log path", s.SerialLog},
		{"incoming snapshot path", s.RestoreFrom},
	} {
		if f.value == "" {
			if f.what == "incoming snapshot path" {
				continue // empty means "cold boot", not "missing"
			}
			return fmt.Errorf("qemu argv: %s is empty", f.what)
		}
		if strings.Contains(f.value, ",") {
			return fmt.Errorf("qemu argv: %s %q contains a comma, which QEMU reads as a property separator", f.what, f.value)
		}
	}
	return nil
}

// args renders the confinement flags, or nothing at all on the direct path.
func (c Confine) args() []string {
	var args []string
	if c.ChrootDir != "" {
		args = append(args, "-run-with", "chroot="+c.ChrootDir)
	}
	if c.UID > 0 {
		uid := strconv.Itoa(c.UID)
		args = append(args, "-runas", uid+":"+uid)
	}
	if c.Sandbox {
		args = append(args, "-sandbox", "on")
	}
	return args
}

// Validate reports whether a Confine is internally consistent. A half-set one
// is the dangerous shape: a chroot with no uid drop is a process that is still
// root inside the jail, and a uid drop with no chroot is a process that can
// still see the whole host filesystem. The helper asks for all three or none.
func (c Confine) Validate() error {
	set := 0
	for _, on := range []bool{c.ChrootDir != "", c.UID > 0, c.Sandbox} {
		if on {
			set++
		}
	}
	if set != 0 && set != 3 {
		return fmt.Errorf("qemu argv: confinement is half-configured (chroot=%q uid=%d sandbox=%v); "+
			"a chroot without a uid drop still runs as root inside the jail, and a uid drop without a "+
			"chroot still sees the whole host filesystem", c.ChrootDir, c.UID, c.Sandbox)
	}
	if strings.Contains(c.ChrootDir, ",") {
		return fmt.Errorf("qemu argv: chroot dir %q contains a comma, which QEMU reads as a property separator", c.ChrootDir)
	}
	return nil
}
