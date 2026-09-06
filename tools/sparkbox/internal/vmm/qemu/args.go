//go:build linux

package qemu

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"runtime"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/qemuargs"
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

// The argv itself lives in internal/vmm/qemuargs, because the privileged helper
// builds it too and QEMU's migration stream is matched positionally against the
// command line — see that package's doc comment for what a second copy would
// cost. These aliases keep the driver (and its tests) reading as before.
type qemuSpec = qemuargs.Spec

func buildQemuArgs(spec qemuSpec) ([]string, error) { return qemuargs.Build(spec) }

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
		TapName:     d.tapName(st.idx),
		MAC:         macFor(st.idx),
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
		guestConsole(), d.guestIP(idx), d.hostIP(idx), name, machineIDFor(name))
	if d.prefix6 != nil {
		kernelArgs += fmt.Sprintf(" sparkbox_ip6=%s/127 sparkbox_gw6=%s",
			d.guestIP6(idx), d.hostIP6(idx))
	}
	dnsArg, err := guestDNSArg(d.opts.GuestDNS, d.hostIP(idx))
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

// machineIDFor derives this sandbox's /etc/machine-id from its name.
//
// It exists because of forks and old templates. Current base images and
// captures carry an empty machine-id, but older templates can be byte-for-byte
// copies of somebody's populated rootfs, and PID 1 reads that file before any
// unit runs. systemd reads systemd.machine_id= off the kernel command line when
// the file is uninitialised; the host writes that argument per boot and no guest
// can forge it, so every clean fork differs from its parent from PID 1 onward.
//
// Derived rather than random so it is STABLE across the sandbox's own boots: a
// machine id that changed every time would give journald a new machine
// directory on every resume. It changes on a rename, which is the same
// tradeoff the hostname already makes.
//
// The guest-side pre-capture clear and sparkbox-identity-reset stay regardless:
// they cover dbus, SSH host keys, old templates, and images with no systemd to
// read this at all.
//
// Byte-identical to fc.go's, deliberately: the same sandbox name must produce
// the same machine id whichever driver boots it, or moving a rootfs between
// backends would give journald a new machine directory.
func machineIDFor(name string) string {
	sum := sha256.Sum256([]byte("sparkbox-machine-id\x00" + name))
	return hex.EncodeToString(sum[:16])
}

// macFor derives a stable locally-administered MAC from the network slot so
// snapshots restore onto an identically-configured interface.
//
// The third octet is 01 where the firecracker driver's is 00. Every other
// per-slot namespace in this package (the tap name, the /30, the /127) is
// already distinct from that driver's, and the MAC has to be too: two drivers
// sharing a VMStateDir and a host would otherwise hand the same address to two
// live guests on the same L2 segment, which presents as intermittent
// unreachability rather than as a collision.
// macFor delegates to guestnet so the privileged helper and this driver cannot
// drift. The helper builds the QEMU argv on its own path and therefore picks
// the MAC itself; two copies of this formula would eventually disagree, and the
// symptom would be a sandbox that loses its network on RESUME rather than at
// boot, because the guest kernel sees an interface its netcfg hook has never
// seen before.
func macFor(idx int) string { return guestnet.MACFor(idx) }

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
