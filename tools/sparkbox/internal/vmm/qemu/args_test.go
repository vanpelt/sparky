//go:build linux

package qemu

import (
	"fmt"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/guestargs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/hostnet"
	"net"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
)

// These tests run anywhere: nothing here needs KVM, a tap, a rootfs or a QEMU
// binary. That is the point of args.go being pure — the four findings in
// docs/qemu-spike.md that the hardware measured are cheap to keep measured.

// argsTestDriver builds a Driver with only the fields args.go reads. It does not go
// through New (which wants /dev/kvm and a real qemu binary), because none of
// what New validates is an input to the argv.
func argsTestDriver(t *testing.T, subnet, subnet6, guestDNS string) *Driver {
	t.Helper()
	d := &Driver{
		opts: Options{
			KernelPath:  "/assets/vmlinux",
			VMStateDir:  filepath.Join(t.TempDir(), "state"),
			MachineType: "virt-8.2",
			GuestDNS:    guestDNS,
		},
		net: hostnet.Plumbing{Net: guestnet.MustParse(subnet), TapPrefix: tapPrefix},
	}
	if subnet6 != "" {
		_, ipNet, err := net.ParseCIDR(subnet6)
		if err != nil {
			t.Fatalf("parse subnet6 %q: %v", subnet6, err)
		}
		d.net.Prefix6 = ipNet.IP.To16()
	}
	return d
}

// ---------------------------------------------------------------------------
// The guest kernel command line
// ---------------------------------------------------------------------------

func TestKernelArgs(t *testing.T) {
	// Slot 3 of 172.31.0.0/24 is 172.31.0.12/30: host .13, guest .14.
	// Slot 0 is 172.31.0.0/30: host .1, guest .2.
	// IPv6 slot 3 is offset (3+1)*2 = 8, so host ::8 and guest ::9.
	const (
		boxID   = "096f2f469359b69057d5c4c11e4b6142" // sha256("sparkbox-machine-id\x00box")[:16]
		v6ID    = "e6313aa5a3634ea18807b93cfadf7a7e"
		freshID = "c4eaf3b22e4ea50305c122dc1c6c46fb"
	)
	// The console is taken from guestConsole() so that this table does not
	// become arch-specific; the console itself has a dedicated test below.
	base := func(guestIP, hostIP, name, machineID string) string {
		return fmt.Sprintf(
			"console=%s reboot=k panic=1 root=/dev/vda rw quiet "+
				"ip=%s::%s:255.255.255.252::eth0:off sparkbox_host=%s systemd.machine_id=%s",
			guestConsole(), guestIP, hostIP, name, machineID)
	}

	tests := []struct {
		name     string
		subnet   string
		subnet6  string
		guestDNS string
		vmName   string
		idx      int
		fresh    bool
		want     string
	}{
		{
			name:   "ipv4 only, not fresh",
			subnet: "172.31.0.0/24", vmName: "box", idx: 3,
			want: base("172.31.0.14", "172.31.0.13", "box", boxID),
		},
		{
			name:   "dual stack adds sparkbox_ip6 and sparkbox_gw6",
			subnet: "172.31.0.0/24", subnet6: "2001:db8:1:2::/64", vmName: "ipv6-box", idx: 3,
			want: base("172.31.0.14", "172.31.0.13", "ipv6-box", v6ID) +
				" sparkbox_ip6=2001:db8:1:2::9/127 sparkbox_gw6=2001:db8:1:2::8",
		},
		{
			name:   "fresh appends sparkbox_fresh=1 last",
			subnet: "172.31.0.0/24", vmName: "fresh-box", idx: 0, fresh: true,
			want: base("172.31.0.2", "172.31.0.1", "fresh-box", freshID) +
				" sparkbox_fresh=1",
		},
		{
			name:   "guest-dns gateway expands to this VM's host address",
			subnet: "172.31.0.0/24", guestDNS: "gateway", vmName: "box", idx: 3,
			want: base("172.31.0.14", "172.31.0.13", "box", boxID) +
				" sparkbox_dns=172.31.0.13",
		},
		{
			name:   "guest-dns literal is used verbatim",
			subnet: "172.31.0.0/24", guestDNS: "10.0.0.53", vmName: "box", idx: 3,
			want: base("172.31.0.14", "172.31.0.13", "box", boxID) +
				" sparkbox_dns=10.0.0.53",
		},
		{
			name:   "dual stack, fresh, and dns compose in a fixed order",
			subnet: "172.31.0.0/24", subnet6: "2001:db8:1:2::/64", guestDNS: "gateway",
			vmName: "ipv6-box", idx: 3, fresh: true,
			want: base("172.31.0.14", "172.31.0.13", "ipv6-box", v6ID) +
				" sparkbox_ip6=2001:db8:1:2::9/127 sparkbox_gw6=2001:db8:1:2::8" +
				" sparkbox_dns=172.31.0.13 sparkbox_fresh=1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := argsTestDriver(t, tc.subnet, tc.subnet6, tc.guestDNS)
			got, err := d.kernelArgs(tc.vmName, tc.idx, tc.fresh)
			if err != nil {
				t.Fatalf("kernelArgs: %v", err)
			}
			if got != tc.want {
				t.Errorf("kernelArgs mismatch\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestKernelArgsAbsentWhenNotFresh(t *testing.T) {
	d := argsTestDriver(t, "172.31.0.0/24", "", "")
	got, err := d.kernelArgs("box", 1, false)
	if err != nil {
		t.Fatalf("kernelArgs: %v", err)
	}
	if strings.Contains(got, "sparkbox_fresh") {
		t.Errorf("sparkbox_fresh must be absent on a reused disk; got %s", got)
	}
	// A guest reads sparkbox_fresh=1 as permission to move an inherited git
	// checkout onto the manifest's branch, so getting this wrong destroys work
	// rather than merely misconfiguring a boot.
}

func TestKernelArgsIPv6AbsentWithoutSubnet6(t *testing.T) {
	d := argsTestDriver(t, "172.31.0.0/24", "", "")
	got, err := d.kernelArgs("box", 1, false)
	if err != nil {
		t.Fatalf("kernelArgs: %v", err)
	}
	for _, tok := range []string{"sparkbox_ip6", "sparkbox_gw6"} {
		if strings.Contains(got, tok) {
			t.Errorf("%s must be absent on an IPv4-only host; got %s", tok, got)
		}
	}
}

func TestKernelArgsRejectsBadGuestDNS(t *testing.T) {
	d := argsTestDriver(t, "172.31.0.0/24", "", "resolver.example.com")
	if _, err := d.kernelArgs("box", 1, false); err == nil {
		t.Fatal("a hostname guest-dns must be rejected: it would inject nothing useful into the guest " +
			"and a value with whitespace would inject extra kernel args")
	}
}

// ---------------------------------------------------------------------------
// The four measured differences from Firecracker.
//
// docs/qemu-spike.md, "Measured: four things on the kernel command line do not
// port". These four ran on the arm64 dev box against the guest kernel and
// rootfs template we ship. They are regressions waiting to happen, not
// speculation, and each one fails as a hang or a silent misconfiguration
// rather than as an error — which is why they get their own named tests.
// ---------------------------------------------------------------------------

// Finding 1: Firecracker is MMIO-only and passes pci=off. -M virt puts virtio
// on PCIe, so a lifted pci=off gives the guest no disk and no NIC — presenting
// as a boot that never reaches SSH.
func TestMeasuredSpikeDelta1_NoPCIOffOnTheCommandLine(t *testing.T) {
	d := argsTestDriver(t, "172.31.0.0/24", "2001:db8:1:2::/64", "gateway")
	got, err := d.kernelArgs("box", 2, true)
	if err != nil {
		t.Fatalf("kernelArgs: %v", err)
	}
	if strings.Contains(got, "pci=off") {
		t.Errorf("pci=off must not appear on a QEMU guest command line; got %s", got)
	}
}

// Finding 2: Firecracker synthesises the root device from the drive's
// is_root_device flag. QEMU does not, and there is no initramfs to work it out,
// so root=/dev/vda rw must be explicit.
func TestMeasuredSpikeDelta2_RootDeviceIsExplicitlyVda(t *testing.T) {
	d := argsTestDriver(t, "172.31.0.0/24", "", "")
	got, err := d.kernelArgs("box", 2, false)
	if err != nil {
		t.Fatalf("kernelArgs: %v", err)
	}
	if !strings.Contains(got, "root=/dev/vda rw") {
		t.Errorf("root=/dev/vda rw must be explicit under QEMU; got %s", got)
	}
	// /dev/vda and not /dev/vdb: the rootfs is the only -drive, attached to the
	// single virtio-blk-pci device buildQemuArgs emits.
	args, err := buildQemuArgs(argsValidSpec())
	if err != nil {
		t.Fatalf("buildQemuArgs: %v", err)
	}
	if n := argsCountDevices(args, "virtio-blk-pci"); n != 1 {
		t.Errorf("root=/dev/vda assumes exactly one virtio-blk device; argv has %d", n)
	}
}

// Finding 3: -M virt on arm64 gives a PL011 at ttyAMA0, not the 8250 at ttyS0
// Firecracker's machine exposes. (The same finding also recorded that our arm64
// guest kernel has no PL011 driver, so -serial captures 0 bytes until
// CONFIG_SERIAL_AMBA_PL011 lands — naming the right device is necessary, not
// sufficient.)
func TestMeasuredSpikeDelta3_ConsoleIsThePlatformSerialPort(t *testing.T) {
	d := argsTestDriver(t, "172.31.0.0/24", "", "")
	got, err := d.kernelArgs("box", 2, false)
	if err != nil {
		t.Fatalf("kernelArgs: %v", err)
	}
	if runtime.GOARCH == "arm64" {
		if !strings.HasPrefix(got, "console=ttyAMA0 ") {
			t.Errorf("arm64 -M virt exposes a PL011 at ttyAMA0; got %s", got)
		}
		if strings.Contains(got, "ttyS0") {
			t.Errorf("ttyS0 is the Firecracker console and is wrong on arm64 -M virt; got %s", got)
		}
		return
	}
	// Not measured anywhere: no run of this package has happened off arm64.
	if !strings.HasPrefix(got, "console=ttyS0 ") {
		t.Errorf("non-arm64 falls back to ttyS0; got %s", got)
	}
}

// Finding 4: the Ubuntu package ships no option ROMs, so virtio-net-pci without
// romfile= fails outright with `failed to find romfile "efi-virtio.rom"`. We
// boot with -kernel, so a PXE ROM is dead weight regardless. This is a hard
// startup failure, and it is packaging-specific — a QEMU built from source will
// not reproduce it, which is exactly why it needs a test rather than a memory.
func TestMeasuredSpikeDelta4_PCIDevicesWithOptionROMsCarryRomfileOverride(t *testing.T) {
	args, err := buildQemuArgs(argsValidSpec())
	if err != nil {
		t.Fatalf("buildQemuArgs: %v", err)
	}
	for _, dev := range argsDevices(args) {
		if !strings.Contains(dev, ",romfile=") {
			t.Errorf("-device %s must carry romfile= (no option ROMs in the packaged QEMU)", dev)
		}
	}
	// Named explicitly so removing a device does not quietly empty the loop.
	// virtio-blk-pci is in the list even though hack/qemu-spike/probe.sh booted
	// without the token on it: romfile is a PCIDevice property, inert where the
	// build ships no default ROM for the device, so the doc's generalisation
	// costs nothing and the spike's narrower line is not evidence that a
	// different package behaves the same way.
	for _, want := range []string{"virtio-blk-pci", "virtio-net-pci", "virtio-balloon-pci"} {
		if argsCountDevices(args, want) != 1 {
			t.Fatalf("expected exactly one %s device in %v", want, args)
		}
	}
}

// ---------------------------------------------------------------------------
// The argv
// ---------------------------------------------------------------------------

func argsValidSpec() qemuSpec {
	return qemuSpec{
		MachineType: "virt-8.2",
		KernelPath:  "/assets/vmlinux",
		Cmdline:     "console=ttyAMA0 root=/dev/vda rw",
		VCPUs:       2,
		MemMB:       1024,
		RootfsPath:  "/state/qemu-vms/box/rootfs.ext4",
		TapName:     "sbqtap3",
		MAC:         "02:5b:01:00:00:03",
		QMPSocket:   "/state/qemu-vms/box/qmp.sock",
		SerialLog:   "/state/qemu-vms/box/serial.log",
	}
}

func TestBuildQemuArgsColdBoot(t *testing.T) {
	got, err := buildQemuArgs(argsValidSpec())
	if err != nil {
		t.Fatalf("buildQemuArgs: %v", err)
	}
	want := []string{
		"-M", "virt-8.2",
		"-cpu", "host",
		"-enable-kvm",
		// Measured hardening, not decoration: see the -nodefaults comment in
		// args.go for the `info qtree` device lists it is derived from. Its
		// position in this list is part of the assertion, because the argv is
		// what the migration stream is matched against.
		"-nodefaults",
		"-m", "1024",
		"-smp", "2",
		"-kernel", "/assets/vmlinux",
		"-append", "console=ttyAMA0 root=/dev/vda rw",
		"-drive", "file=/state/qemu-vms/box/rootfs.ext4,format=raw,if=none,id=rootfs",
		"-device", "virtio-blk-pci,drive=rootfs,romfile=",
		"-netdev", "tap,id=net0,ifname=sbqtap3,script=no,downscript=no",
		"-device", "virtio-net-pci,netdev=net0,mac=02:5b:01:00:00:03,romfile=",
		"-device", "virtio-balloon-pci,id=balloon0,deflate-on-oom=on,romfile=",
		"-qmp", "unix:/state/qemu-vms/box/qmp.sock,server=on,wait=off",
		"-nographic",
		"-serial", "file:/state/qemu-vms/box/serial.log",
		"-monitor", "none",
	}
	argsAssertEqual(t, got, want)
	if idx := slices.Index(got, "-incoming"); idx >= 0 {
		t.Errorf("a cold boot must not carry -incoming; got %v", got)
	}
}

// The restore argv must be the cold-boot argv plus one appended flag. QEMU
// matches the migration stream's device sections against the machine the argv
// describes, so any other difference — a device added, removed or reordered, a
// changed -m/-smp/-M — makes the load fail on stderr after exec, on a resume,
// long after the boot that produced the snapshot.
func TestBuildQemuArgsRestoreDiffersOnlyByIncoming(t *testing.T) {
	cold, err := buildQemuArgs(argsValidSpec())
	if err != nil {
		t.Fatalf("cold: %v", err)
	}
	spec := argsValidSpec()
	spec.RestoreFrom = "/state/qemu-vms/box/state.migrate"
	restore, err := buildQemuArgs(spec)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(restore) != len(cold)+2 {
		t.Fatalf("restore argv should be cold + 2 tokens\ncold:    %v\nrestore: %v", cold, restore)
	}
	argsAssertEqual(t, restore[:len(cold)], cold)
	if restore[len(cold)] != "-incoming" || restore[len(cold)+1] != "file:/state/qemu-vms/box/state.migrate" {
		t.Errorf("restore must append -incoming file:<snapshot> last; got %v", restore[len(cold):])
	}
	// The balloon in particular must survive onto the restore line. Firecracker
	// skips re-adding it on resume because the snapshot restores the device;
	// here the device comes from argv, so dropping it leaves an incoming device
	// section with nowhere to land.
	if argsCountDevices(restore, "virtio-balloon-pci") != 1 {
		t.Errorf("the restore argv must still declare the balloon; got %v", restore)
	}
}

func TestBuildQemuArgsRejectsUnusableSpecs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*qemuSpec)
	}{
		{"no machine type", func(s *qemuSpec) { s.MachineType = "" }},
		{"no kernel", func(s *qemuSpec) { s.KernelPath = "" }},
		{"no cmdline", func(s *qemuSpec) { s.Cmdline = "" }},
		// Zero vcpus/memory is the shape of fc.go:1073's resume call, which
		// passes literal zeros because Firecracker reads the machine config out
		// of state.snap. Lifted here it would produce `-smp 0 -m 0`.
		{"zero vcpus", func(s *qemuSpec) { s.VCPUs = 0 }},
		{"zero memory", func(s *qemuSpec) { s.MemMB = 0 }},
		{"no rootfs", func(s *qemuSpec) { s.RootfsPath = "" }},
		{"no tap", func(s *qemuSpec) { s.TapName = "" }},
		// A comma is a property separator in every one of these values, so a
		// path containing one silently truncates the property and turns the
		// remainder into garbage — for -drive, a different disk or none.
		{"comma in rootfs path", func(s *qemuSpec) { s.RootfsPath = "/state/a,b/rootfs.ext4" }},
		{"comma in qmp socket path", func(s *qemuSpec) { s.QMPSocket = "/state/a,b/qmp.sock" }},
		{"comma in serial log path", func(s *qemuSpec) { s.SerialLog = "/state/a,b/serial.log" }},
		{"comma in incoming path", func(s *qemuSpec) { s.RestoreFrom = "/state/a,b/state.migrate" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := argsValidSpec()
			tc.mutate(&spec)
			if _, err := buildQemuArgs(spec); err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
		})
	}
}

// qemuArgs is the thin adapter: it must feed the VM's own paths and the machine
// size recorded in vmState — never zeros — into the shared builder.
func TestQemuArgsWiresDriverStateIntoTheSpec(t *testing.T) {
	d := argsTestDriver(t, "172.31.0.0/24", "", "")
	st := &vmState{idx: 3, vcpus: 4, memMB: 2048}
	rootfs := d.rootfsPath("box")

	// What boot does: build the line on the cold boot, record it, replay it.
	cmdline, err := d.bootCmdline("box", st, false, true)
	if err != nil {
		t.Fatalf("bootCmdline cold: %v", err)
	}
	cold, err := d.qemuArgs("box", st, rootfs, cmdline, false)
	if err != nil {
		t.Fatalf("qemuArgs cold: %v", err)
	}
	argsAssertPair(t, cold, "-m", "2048")
	argsAssertPair(t, cold, "-smp", "4")
	argsAssertPair(t, cold, "-M", "virt-8.2")
	argsAssertPair(t, cold, "-kernel", "/assets/vmlinux")
	argsAssertPair(t, cold, "-qmp", "unix:"+d.qmpSocketPath("box")+",server=on,wait=off")
	argsAssertPair(t, cold, "-serial", "file:"+d.serialLogPath("box"))
	if !strings.Contains(argsValueAfter(cold, "-drive"), "file="+rootfs+",") {
		t.Errorf("-drive must name the VM's own rootfs; got %s", argsValueAfter(cold, "-drive"))
	}
	if !strings.Contains(argsValueAfter(cold, "-netdev"), "ifname="+d.net.TapName(3)+",") {
		t.Errorf("-netdev must name this slot's tap; got %s", argsValueAfter(cold, "-netdev"))
	}
	if !strings.Contains(argsValueAfter(cold, "-append"), " sparkbox_fresh=1") {
		t.Errorf("fresh must reach the guest command line; got %s", argsValueAfter(cold, "-append"))
	}

	st.cmdline = cmdline
	restoreCmdline, err := d.bootCmdline("box", st, true, false)
	if err != nil {
		t.Fatalf("bootCmdline restore: %v", err)
	}
	restore, err := d.qemuArgs("box", st, rootfs, restoreCmdline, true)
	if err != nil {
		t.Fatalf("qemuArgs restore: %v", err)
	}
	argsAssertPair(t, restore, "-incoming", "file:"+d.snapshotPath("box"))
	// Byte for byte, -append included: only the trailing flag differs. The
	// -append half of that is what bootCmdline's replay buys — rebuilt with
	// fresh=false it would come back without sparkbox_fresh=1, and the restore
	// argv of a sandbox created fresh would be a form nothing has ever booted.
	argsAssertEqual(t, restore[:len(restore)-2], cold)
}

// bootCmdline is the whole of the build-or-replay rule, and it is worth its own
// test because the alternative — noticing it on hardware — is a resume that
// fails on stderr an hour after the boot that produced the snapshot.
func TestBootCmdlineReplaysTheBootLineOnARestore(t *testing.T) {
	d := argsTestDriver(t, "172.31.0.0/24", "", "")
	st := &vmState{idx: 3, vcpus: 4, memMB: 2048}

	cold, err := d.bootCmdline("box", st, false, true)
	if err != nil {
		t.Fatalf("bootCmdline cold: %v", err)
	}
	if !strings.Contains(cold, " sparkbox_fresh=1") {
		t.Fatalf("a fresh cold boot must carry sparkbox_fresh=1; got %s", cold)
	}
	st.cmdline = cold

	got, err := d.bootCmdline("box", st, true, false)
	if err != nil {
		t.Fatalf("bootCmdline restore: %v", err)
	}
	if got != cold {
		t.Errorf("restore must replay the recorded line\n got %s\nwant %s", got, cold)
	}

	// A cold boot never replays: the record's line belongs to a guest that is
	// gone, and sparkbox_fresh=1 on a disk somebody has worked in is the guest's
	// permission to move their git checkout.
	got, err = d.bootCmdline("box", st, false, false)
	if err != nil {
		t.Fatalf("bootCmdline second cold boot: %v", err)
	}
	if strings.Contains(got, "sparkbox_fresh") {
		t.Errorf("a cold boot of an existing disk must rebuild without sparkbox_fresh; got %s", got)
	}

	// No recorded line (nothing in-process produces this today) falls back to
	// rebuilding rather than handing buildQemuArgs an empty -append.
	st.cmdline = ""
	got, err = d.bootCmdline("box", st, true, false)
	if err != nil {
		t.Fatalf("bootCmdline restore with no recorded line: %v", err)
	}
	if got == "" || strings.Contains(got, "sparkbox_fresh") {
		t.Errorf("fallback must rebuild a non-fresh line; got %s", got)
	}
}

func TestMacForIsSlotStableAndDistinctFromFirecracker(t *testing.T) {
	if got, want := hostnet.MAC(qemuMACOUI, 0), "02:5b:01:00:00:00"; got != want {
		t.Errorf("hostnet.MAC(qemuMACOUI, 0) = %s, want %s", got, want)
	}
	if got, want := hostnet.MAC(qemuMACOUI, 258), "02:5b:01:00:01:02"; got != want {
		t.Errorf("hostnet.MAC(qemuMACOUI, 258) = %s, want %s", got, want)
	}
	// The firecracker driver's third octet is 00. Two drivers on one host share
	// an L2 segment, and a duplicated MAC there presents as intermittent
	// unreachability rather than as a collision anybody can see.
	if strings.HasPrefix(hostnet.MAC(qemuMACOUI, 1), "02:5b:00:") {
		t.Errorf("macFor must not collide with the firecracker driver's 02:5b:00: range; got %s", hostnet.MAC(qemuMACOUI, 1))
	}
	if hostnet.MAC(qemuMACOUI, 1) == hostnet.MAC(qemuMACOUI, 2) {
		t.Error("macFor must be injective over slots")
	}
}

func TestMachineIDForIsStableAndPerName(t *testing.T) {
	// Golden, because the value is what makes a fork differ from its parent
	// from PID 1 onward, and because it must match the firecracker driver's
	// derivation exactly: the same sandbox name has to keep the same machine id
	// whichever driver boots it, or journald starts a new machine directory.
	if got, want := guestargs.MachineID("box"), "096f2f469359b69057d5c4c11e4b6142"; got != want {
		t.Errorf("guestargs.MachineID(box) = %s, want %s", got, want)
	}
	if got, want := guestargs.MachineID("other"), "23d3a7397ce32deee3ab53e95e3de5e9"; got != want {
		t.Errorf("guestargs.MachineID(other) = %s, want %s", got, want)
	}
	if len(guestargs.MachineID("box")) != 32 {
		t.Errorf("systemd.machine_id must be 32 hex characters; got %q", guestargs.MachineID("box"))
	}
	if guestargs.MachineID("box") != guestargs.MachineID("box") {
		t.Error("guestargs.MachineID must be stable across the sandbox's own boots")
	}
}

func TestGuestDNSArg(t *testing.T) {
	tests := []struct {
		guestDNS string
		want     string
		wantErr  bool
	}{
		{guestDNS: "", want: ""},
		{guestDNS: "gateway", want: " sparkbox_dns=172.31.0.1"},
		{guestDNS: "10.0.0.53", want: " sparkbox_dns=10.0.0.53"},
		{guestDNS: "2001:db8::53", want: " sparkbox_dns=2001:db8::53"},
		{guestDNS: "resolver.example.com", wantErr: true},
		{guestDNS: "10.0.0.53 init=/bin/sh", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.guestDNS, func(t *testing.T) {
			got, err := guestargs.DNSArg(tc.guestDNS, "172.31.0.1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("guestargs.DNSArg(%q) should have failed", tc.guestDNS)
				}
				return
			}
			if err != nil {
				t.Fatalf("guestargs.DNSArg(%q): %v", tc.guestDNS, err)
			}
			if got != tc.want {
				t.Errorf("guestargs.DNSArg(%q) = %q, want %q", tc.guestDNS, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func argsAssertEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv length %d, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func argsAssertPair(t *testing.T, args []string, flag, want string) {
	t.Helper()
	if got := argsValueAfter(args, flag); got != want {
		t.Errorf("%s = %q, want %q", flag, got, want)
	}
}

func argsValueAfter(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// argsDevices returns the value of every -device flag.
func argsDevices(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-device" {
			out = append(out, args[i+1])
		}
	}
	return out
}

func argsCountDevices(args []string, kind string) int {
	n := 0
	for _, dev := range argsDevices(args) {
		if k, _, _ := strings.Cut(dev, ","); k == kind {
			n++
		}
	}
	return n
}
