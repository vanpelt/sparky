package vmmtest

import (
	"context"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Run is the whole harness. Every case builds its own Fixture, so a driver that
// wedges in one case still gets a clean driver for the next.
func Run(t *testing.T, newFixture NewFixture) {
	t.Run("Capabilities", func(t *testing.T) { testCapabilityInventory(t, newFixture) })

	// Core Driver: the five methods every backend must have.
	t.Run("BootAndSSH", func(t *testing.T) { testBootAndSSH(t, newFixture) })
	t.Run("PauseResume", func(t *testing.T) { testPauseResume(t, newFixture) })
	t.Run("PauseResumeUnknown", func(t *testing.T) { testPauseResumeUnknown(t, newFixture) })
	t.Run("Destroy", func(t *testing.T) { testDestroy(t, newFixture) })
	t.Run("DestroyThenReuseName", func(t *testing.T) { testDestroyThenReuseName(t, newFixture) })
	t.Run("CreateRefusesResidue", func(t *testing.T) { testCreateRefusesResidue(t, newFixture) })
	t.Run("TwoAtOnce", func(t *testing.T) { testTwoAtOnce(t, newFixture) })

	// The ten optional capabilities.
	t.Run("Archive", func(t *testing.T) { testCheckpointRestore(t, newFixture) })
	t.Run("ForkFromTemplate", func(t *testing.T) { testForkFromTemplate(t, newFixture) })
	t.Run("RootfsPresence", func(t *testing.T) { testRootfsPresence(t, newFixture) })
	t.Run("Reboot", func(t *testing.T) { testReboot(t, newFixture) })
	t.Run("Rename", func(t *testing.T) { testRename(t, newFixture) })
	t.Run("DiskReport", func(t *testing.T) { testDiskReport(t, newFixture) })
	t.Run("TemplateUsage", func(t *testing.T) { testTemplateUsage(t, newFixture) })
	t.Run("DiskResize", func(t *testing.T) { testDiskResize(t, newFixture) })
	t.Run("Balloon", func(t *testing.T) { testBalloon(t, newFixture) })
	t.Run("CPUStats", func(t *testing.T) { testCPUStats(t, newFixture) })
	t.Run("NetStats", func(t *testing.T) { testNetStats(t, newFixture) })
}

// capability is one optional interface, named so a skip says which one is
// missing rather than "not supported".
type capability struct {
	name string
	has  func(vmm.Driver) bool
}

var capabilities = []capability{
	{"Archivable", func(d vmm.Driver) bool { _, ok := d.(vmm.Archivable); return ok }},
	{"DiskReporter", func(d vmm.Driver) bool { _, ok := d.(vmm.DiskReporter); return ok }},
	{"TemplateReporter", func(d vmm.Driver) bool { _, ok := d.(vmm.TemplateReporter); return ok }},
	{"RootfsPresencer", func(d vmm.Driver) bool { _, ok := d.(vmm.RootfsPresencer); return ok }},
	{"Renamer", func(d vmm.Driver) bool { _, ok := d.(vmm.Renamer); return ok }},
	{"Rebooter", func(d vmm.Driver) bool { _, ok := d.(vmm.Rebooter); return ok }},
	{"CPUStatser", func(d vmm.Driver) bool { _, ok := d.(vmm.CPUStatser); return ok }},
	{"NetStatser", func(d vmm.Driver) bool { _, ok := d.(vmm.NetStatser); return ok }},
	{"DiskResizer", func(d vmm.Driver) bool { _, ok := d.(vmm.DiskResizer); return ok }},
	{"Ballooner", func(d vmm.Driver) bool { _, ok := d.(vmm.Ballooner); return ok }},
}

// testCapabilityInventory prints what this driver claims, so a `-v` run answers
// "which capabilities did the harness exercise" without reading skip lines.
//
// It is deliberately not a failure to be missing one: a driver is allowed to
// lack a capability, and the manager degrades by design. What is not allowed is
// nobody noticing, which is what this fixes.
func testCapabilityInventory(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	missing := 0
	for _, c := range capabilities {
		if c.has(f.Driver) {
			t.Logf("capability %-17s present", c.name)
			continue
		}
		missing++
		t.Logf("capability %-17s ABSENT  (its cases will skip)", c.name)
	}
	t.Logf("traits: RealGuest=%v PreservesMemory=%v SanitizesForks=%v LiveDiskUsage=%v "+
		"DistinctHostIPs=%v BaseImageIsTemplate=%v",
		f.Traits.RealGuest, f.Traits.PreservesMemory, f.Traits.SanitizesForks, f.Traits.LiveDiskUsage,
		f.Traits.DistinctHostIPs, f.Traits.BaseImageIsTemplate)
	t.Logf("%d of %d optional capabilities present", len(capabilities)-missing, len(capabilities))
}

// requireCap fetches an optional interface or skips with its name.
func requireCap[T any](t *testing.T, d vmm.Driver, name string) T {
	t.Helper()
	v, ok := d.(T)
	if !ok {
		t.Skipf("driver does not implement vmm.%s", name)
	}
	return v
}

// testBootAndSSH is the case the whole harness exists for: a driver that boots
// nothing has always passed every test in this repository.
func testBootAndSSH(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	b := newBox(t, f, uniq(t, "boot"))
	inst := b.create(true)
	wantRunning(t, inst, b.name)

	c := b.dial()
	if got := run(t, c, "echo sparkbox-parity"); got != "sparkbox-parity" {
		t.Fatalf("guest echo = %q", got)
	}
	if !f.Traits.RealGuest {
		return
	}
	// A real guest must be the machine the driver said it was: the hostname
	// comes from the sparkbox_host kernel arg, which the host asserts and the
	// guest cannot forge, and getting it wrong means every log line and every
	// metadata lookup is attributed to the wrong sandbox.
	if got := run(t, c, "cat /proc/sys/kernel/hostname"); got != b.name {
		t.Errorf("guest hostname = %q, want %q", got, b.name)
	}
	if got := run(t, c, "id -un"); got != inst.SSHUser {
		t.Errorf("logged in as %q, but Instance.SSHUser says %q", got, inst.SSHUser)
	}
	// The guest's own address must be the one the control plane hands the
	// proxy, or every forwarded port lands somewhere else.
	if got := run(t, c, "hostname -I 2>/dev/null || ip -4 -o addr show dev eth0"); got != "" {
		if inst.HostIP != "" && !containsWord(got, inst.HostIP) {
			t.Errorf("guest addresses %q do not include Instance.HostIP %q", got, inst.HostIP)
		}
	}
}

func containsWord(haystack, word string) bool {
	for _, f := range splitFields(haystack) {
		if f == word {
			return true
		}
	}
	return false
}

func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '/':
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		default:
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// testPauseResume covers the idle model. Two separate claims, because a driver
// can honour one and not the other and the second is the expensive one:
//
//  1. the disk survives (every driver),
//  2. the memory survives — the guest resumes rather than reboots
//     (Traits.PreservesMemory).
func testPauseResume(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	b := newBox(t, f, uniq(t, "pr"))
	b.create(true)

	c := b.dial()
	run(t, c, "echo on-disk > $HOME/parity-disk && sync")
	var bootID, ramMarker string
	if f.Traits.RealGuest {
		bootID = run(t, c, "cat /proc/sys/kernel/random/boot_id")
		// /dev/shm is tmpfs: it exists only in guest RAM, so it is the cleanest
		// available answer to "was this resumed or rebooted".
		ramMarker = "parity-" + bootID
		run(t, c, "printf %s "+ramMarker+" > /dev/shm/parity-ram")
	}

	b.pause()
	// Pause must be idempotent — the reaper pauses unattended and can race a
	// user-initiated pause.
	b.pause()

	inst := b.resume()
	wantRunning(t, inst, b.name)
	c2 := b.dial()
	if got := run(t, c2, "cat $HOME/parity-disk"); got != "on-disk" {
		t.Errorf("after resume, disk marker = %q, want %q", got, "on-disk")
	}
	if !f.Traits.RealGuest {
		return
	}
	if !f.Traits.PreservesMemory {
		t.Logf("driver does not claim PreservesMemory; skipping the resumed-not-rebooted assertion")
		return
	}
	if got := run(t, c2, "cat /proc/sys/kernel/random/boot_id"); got != bootID {
		t.Errorf("boot id changed across resume (%q -> %q): the guest rebooted, it did not resume", bootID, got)
	}
	if got, err := tryRun(c2, "cat /dev/shm/parity-ram"); err != nil || got != ramMarker {
		t.Errorf("tmpfs marker after resume = %q (err %v), want %q: guest RAM was not restored", got, err, ramMarker)
	}
}

// testPauseResumeUnknown pins the one thing every caller in the manager relies
// on: a name the driver has no record of is an error, not a silent success.
// Pause returning nil for an unknown sandbox would have the reaper believe it
// had freed memory it never touched.
func testPauseResumeUnknown(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	ctx := context.Background()
	wantErr(t, f.Driver.Pause(ctx, "parity-no-such-vm"), "Pause of an unknown sandbox")
	_, err := f.Driver.Resume(ctx, "parity-no-such-vm")
	wantErr(t, err, "Resume of an unknown sandbox")
}

// testDestroy checks that Destroy actually reclaims, not just forgets.
func testDestroy(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	b := newBox(t, f, uniq(t, "destroy"))
	b.create(true)
	b.dial()
	b.destroy()

	// Destroy is idempotent: the manager retries it, and a restart re-issues it
	// for sandboxes it can no longer see.
	ctx := context.Background()
	if err := f.Driver.Destroy(ctx, b.name); err != nil {
		t.Errorf("second Destroy: %v", err)
	}
	wantErr(t, f.Driver.Pause(ctx, b.name), "Pause after Destroy")
	if p, ok := f.Driver.(vmm.RootfsPresencer); ok {
		present, err := p.RootfsPresent(b.name)
		if err != nil {
			t.Fatalf("RootfsPresent after destroy: %v", err)
		}
		if present {
			t.Errorf("RootfsPresent = true after Destroy: the disk was leaked")
		}
	}
}

// testDestroyThenReuseName is the regression this repository has already
// shipped a bug for: `ctl rm` returned success without deleting the rootfs
// whenever the driver had no in-memory record of the VM, so the next sandbox to
// claim that name booted the previous tenant's filesystem.
//
// The shape that matters is the one with NO record — a controller that
// restarted since the VM last booted — which is why the case drops the record
// first (via Rebooter, the only interface that does so without touching the
// disk) and only then destroys.
func testDestroyThenReuseName(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	name := uniq(t, "reuse")
	ctx := context.Background()

	first := newBox(t, f, name)
	first.create(true)
	c := first.dial()
	run(t, c, "echo tenant-one > $HOME/parity-tenant && sync")
	first.pause()

	if rb, ok := f.Driver.(vmm.Rebooter); ok {
		// Forget the record but keep the disk: exactly the state a controller
		// restart leaves behind.
		if err := rb.DropSnapshots(name); err != nil {
			t.Fatalf("DropSnapshots: %v", err)
		}
	} else {
		t.Logf("no vmm.Rebooter: testing the with-a-record path only, which is not the one that broke")
	}
	if err := f.Driver.Destroy(ctx, name); err != nil {
		t.Fatalf("destroy %s: %v", name, err)
	}

	second := newBox(t, f, name)
	second.create(true)
	c2 := second.dial()
	if got, err := tryRun(c2, "cat $HOME/parity-tenant"); err == nil && got == "tenant-one" {
		t.Fatalf("the re-used name booted the previous tenant's filesystem: %q", got)
	}
}

// testCreateRefusesResidue is the other half of the same contract, and it
// points the opposite way: a disk under a name the ledger has never issued must
// be REFUSED, not adopted. See vmm.Config.NewSandbox — a ledger can be restored
// from a backup, and 25 GiB of somebody's work is not recoverable from a wrong
// guess.
func testCreateRefusesResidue(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	rb := requireCap[vmm.Rebooter](t, f.Driver, "Rebooter")

	b := newBox(t, f, uniq(t, "residue"))
	b.create(true)
	c := b.dial()
	run(t, c, "echo residue > $HOME/parity-residue && sync")
	b.pause()
	if err := rb.DropSnapshots(b.name); err != nil {
		t.Fatalf("DropSnapshots: %v", err)
	}

	// The disk is still there and the driver has no record: a new sandbox
	// claiming this name must be refused.
	if _, err := b.tryCreate(true); err == nil {
		t.Errorf("Create{NewSandbox: true} adopted a rootfs left by a previous sandbox of the same name")
	}
	// The same call for a sandbox the ledger DOES know must cold-boot it.
	b.create(false)
	c2 := b.dial()
	if got := run(t, c2, "cat $HOME/parity-residue"); got != "residue" {
		t.Errorf("cold boot of an existing sandbox lost the disk: marker = %q", got)
	}
}

// testTwoAtOnce catches per-slot resource collisions — tap names, addresses,
// jail uids — that a suite creating one VM at a time never sees.
func testTwoAtOnce(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	a := newBox(t, f, uniq(t, "twoa"))
	bb := newBox(t, f, uniq(t, "twob"))
	ia := a.create(true)
	ib := bb.create(true)

	if ia.SSHAddr == ib.SSHAddr {
		t.Fatalf("both sandboxes got the same SSH address %q", ia.SSHAddr)
	}
	if f.Traits.DistinctHostIPs && ia.HostIP == ib.HostIP {
		t.Fatalf("both sandboxes got the same HostIP %q", ia.HostIP)
	}
	ca, cb := a.dial(), bb.dial()
	run(t, ca, "echo a > $HOME/parity-which")
	run(t, cb, "echo b > $HOME/parity-which")
	if got := run(t, ca, "cat $HOME/parity-which"); got != "a" {
		t.Errorf("first sandbox sees %q: the two share a disk", got)
	}
	if got := run(t, cb, "cat $HOME/parity-which"); got != "b" {
		t.Errorf("second sandbox sees %q: the two share a disk", got)
	}
	if f.Traits.RealGuest {
		if run(t, ca, "cat /proc/sys/kernel/hostname") == run(t, cb, "cat /proc/sys/kernel/hostname") {
			t.Errorf("both guests believe they are the same sandbox")
		}
	}
}
