//go:build linux

package vmhelper

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
)

// qemuTestServer builds a QEMU-backed server without newServer's device and
// binary probes, so the argv contract is checkable on any Linux host. It does
// not open a state directory: everything that reads one is on ownableServer.
func qemuTestServer(t *testing.T) *server {
	t.Helper()
	return &server{
		opts: ServerOptions{
			Backend:       BackendQEMU,
			QemuBin:       "/usr/bin/qemu-system-x86_64",
			MachineType:   "pc-q35-8.2,sata=off,vmport=off",
			KernelPath:    "/srv/assets/vmlinux",
			ChrootBase:    "/srv/hot/jailer",
			JailerUIDBase: 100000,
			ControllerGID: 65532,
		},
		network: guestnet.MustParse("172.30.0.0/20"),
		active:  map[int]activeVM{},
		stateFD: -1,
	}
}

// ownableServer is the variant for the tests that actually create files.
//
// In production this helper is root and chowns its outputs to a per-slot uid
// nobody else has. A test process usually is not root, and fchown to another
// uid is EPERM — so when it is not, the uid base is chosen to make
// jailUID(slot) this process's OWN uid. The fchown still happens and is still
// checked; it is simply one the kernel permits. Under root (a container build)
// the production base is used unchanged.
func ownableServer(t *testing.T, slot int) *server {
	t.Helper()
	s := qemuTestServer(t)
	stateDir := t.TempDir()
	fd, err := unix.Open(stateDir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) }) //nolint:errcheck
	if uid := os.Getuid(); uid != 0 {
		if uid <= slot {
			t.Skipf("running as uid %d, which cannot host slot %d's jail uid", uid, slot)
		}
		s.opts.JailerUIDBase = uid - slot
		s.opts.ControllerGID = os.Getgid()
	}
	s.opts.VMStateDir = stateDir
	s.stateFD = fd
	return s
}

func mustMkVMDir(t *testing.T, s *server, name string) string {
	t.Helper()
	dir := filepath.Join(s.opts.VMStateDir, s.vmRel(BackendQEMU, name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func qemuTestRequest() Request {
	return Request{
		Version: ProtocolVersion, Op: OpLaunch, Name: "box", Slot: 3,
		VCPUs: 2, MemMB: 1024,
		Cmdline: "console=ttyS0 root=/dev/vda rw sparkbox_host=172.30.0.13",
	}
}

// A QEMU node keeps its sandboxes somewhere the firecracker driver does not
// look, and vice versa. If this drifts from internal/vmm/qemu's vmsSubdir the
// helper links a rootfs that does not exist, or worse, links the wrong one.
// ONE server, both layouts. This used to be two tests over two servers, and
// the change is the point: the layout follows the backend the REQUEST names,
// not a field frozen when the helper started. A server that answered its
// startup default here would put a QEMU sandbox in fc-vms and link a rootfs
// that does not exist — or, worse, one that does and belongs to the other VMM.
func TestTheLayoutFollowsTheBackendNotTheStartupFlag(t *testing.T) {
	s := qemuTestServer(t)
	s.opts.FirecrackerBin = "/srv/assets/firecracker"

	for _, tc := range []struct {
		backend  Backend
		vmRel    string
		socket   string
		outputs  []string
		jailRoot string
	}{
		{BackendQEMU, filepath.Join("qemu-vms", "box"), "qmp.sock",
			[]string{"state.migrate.next"}, "/srv/hot/jailer/qemu/sparkbox-3/root"},
		{BackendFirecracker, filepath.Join("fc-vms", "box"), "fc.sock",
			[]string{"mem.snap.next", "state.snap.next"}, "/srv/hot/jailer/firecracker/sparkbox-3/root"},
	} {
		t.Run(string(tc.backend), func(t *testing.T) {
			if got := s.vmRel(tc.backend, "box"); got != tc.vmRel {
				t.Errorf("vmRel = %q, want %q", got, tc.vmRel)
			}
			if got := s.launchSocketName(tc.backend); got != tc.socket {
				t.Errorf("launchSocketName = %q, want %q", got, tc.socket)
			}
			if got := s.snapshotOutputNames(tc.backend); !slices.Equal(got, tc.outputs) {
				t.Errorf("snapshotOutputNames = %v, want %v", got, tc.outputs)
			}
			// The jail path is what the controller derives on its own side to
			// dial the monitor, so these components are a contract with each
			// driver's jailRoot, not an implementation detail. That the two
			// differ is also what keeps the trees disjoint on a helper serving
			// both, which is why slot 3 appears in each.
			if got := s.jailRoot(tc.backend, 3); got != tc.jailRoot {
				t.Errorf("jailRoot = %q, want %q", got, tc.jailRoot)
			}
		})
	}
}

// The confinement flags are the whole security argument for launching QEMU from
// a root process, so they are asserted as a set rather than left to review. A
// half-configured Confine is refused by qemuargs; this checks the helper asks
// for the whole thing.
func TestQemuCommandConfinesTheChild(t *testing.T) {
	s := qemuTestServer(t)
	req := qemuTestRequest()
	args, err := s.qemuArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][2]string{
		{"-run-with", "chroot=/srv/hot/jailer/qemu/sparkbox-3/root"},
		{"-runas", "100003:100003"},
		{"-sandbox", "on"},
	} {
		i := slices.Index(args, want[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != want[1] {
			t.Errorf("argv is missing %s %s: %v", want[0], want[1], args)
		}
	}
}

// The process attributes matter as much as the argv. Not SysProcAttr: chroot(2)
// there would take effect before QEMU has opened /dev/kvm or the tap, and a
// Credential would take away the privilege it needs to open them at all. And
// the working directory is what makes the relative paths resolve to the same
// files before the chroot as after it.
func TestQemuCommandRunsInTheJailWithoutProcessAttributes(t *testing.T) {
	s := ownableServer(t, 3)
	req := qemuTestRequest()
	mustMkVMDir(t, s, req.Name)
	cmd, logFile, err := s.qemuCommand(req)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close() //nolint:errcheck
	if cmd.SysProcAttr != nil {
		t.Errorf("the QEMU child was given process attributes: %+v", cmd.SysProcAttr)
	}
	if got, want := cmd.Dir, s.jailRoot(BackendQEMU, req.Slot); got != want {
		t.Errorf("cmd.Dir = %q, want %q", got, want)
	}
	if len(cmd.Env) != 0 {
		t.Errorf("the QEMU child inherited environment: %v", cmd.Env)
	}
	if cmd.Stdout != logFile || cmd.Stderr != logFile {
		t.Error("the QEMU child's output does not go to the per-VM log")
	}
}

// Every per-VM path on the command line must be relative, because they are not
// all resolved at the same moment: -kernel, -drive, -qmp, -serial and -incoming
// are opened during startup BEFORE the chroot, while the runtime
// `migrate uri=file:` is resolved from the monitor long after it. An absolute
// host path would work for the first group and fail for the second — a pause
// that breaks on a sandbox that booted perfectly.
func TestQemuCommandNamesEveryPerVMPathRelatively(t *testing.T) {
	s := qemuTestServer(t)
	req := qemuTestRequest()
	req.Resume = true
	args, err := s.qemuArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	// The chroot destination is the one absolute path, and it must be: it is
	// resolved by chroot(2) itself, from outside the jail.
	for i, arg := range args {
		if strings.HasPrefix(arg, "chroot=") {
			continue
		}
		for _, prefix := range []string{"file=", "file:", "unix:"} {
			value := strings.TrimPrefix(arg, prefix)
			if value == arg {
				continue
			}
			if strings.HasPrefix(value, "/") {
				t.Errorf("argv[%d] %q names an absolute path; it will not resolve inside the chroot", i, arg)
			}
		}
	}
	for _, want := range []string{"vmlinux", "file=rootfs.ext4,format=raw,if=none,id=rootfs"} {
		if !slices.Contains(args, want) {
			t.Errorf("argv is missing %q: %v", want, args)
		}
	}
}

// The restore argv must be the cold-boot argv plus exactly one flag. QEMU
// matches an incoming migration stream positionally against the machine the
// command line describes, and this is the process that builds both.
func TestQemuCommandRestoreAddsOnlyIncoming(t *testing.T) {
	s := qemuTestServer(t)
	req := qemuTestRequest()
	cold, err := s.qemuArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	req.Resume = true
	restore, err := s.qemuArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(restore), len(cold)+2; got != want {
		t.Fatalf("restore argv has %d elements, want %d: %v", got, want, restore)
	}
	if !slices.Equal(restore[:len(cold)], cold) {
		t.Errorf("restore argv diverges from the boot argv:\n cold %v\n warm %v", cold, restore)
	}
	if got := restore[len(cold):]; !slices.Equal(got, []string{"-incoming", "file:state.migrate"}) {
		t.Errorf("restore appends %v", got)
	}
}

// The MAC and the tap name are chosen HERE on this path and by the driver on
// the direct one. QEMU takes both from the argv on a restore as well as a cold
// boot, so a drift shows up as a sandbox losing its network on resume.
func TestQemuCommandUsesTheSharedSlotDerivations(t *testing.T) {
	s := qemuTestServer(t)
	req := qemuTestRequest()
	args, err := s.qemuArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ifname="+tapName(req.Slot)+",") {
		t.Errorf("argv does not use the helper's tap name %q: %v", tapName(req.Slot), args)
	}
	if !strings.Contains(joined, "mac="+guestnet.MACFor(req.Slot)+",") {
		t.Errorf("argv does not use guestnet.MACFor(%d) = %q: %v", req.Slot, guestnet.MACFor(req.Slot), args)
	}
}

// The guest command line is the one caller-supplied value on this argv. It must
// arrive as a single -append token however many spaces it contains.
func TestQemuCommandPassesTheCmdlineAsOneToken(t *testing.T) {
	s := qemuTestServer(t)
	req := qemuTestRequest()
	args, err := s.qemuArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "-append")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no -append in %v", args)
	}
	if args[i+1] != req.Cmdline {
		t.Errorf("-append = %q, want %q", args[i+1], req.Cmdline)
	}
}

// The child's log is opened by descriptor and lives in the controller-visible
// VM directory, NOT in the jail: cleanupSlot removes the jail at every VMM
// exit, which is exactly when a failed boot's only diagnostic is wanted.
func TestQemuVMMLogSurvivesTheJail(t *testing.T) {
	s := ownableServer(t, 3)
	req := qemuTestRequest()
	vmDir := mustMkVMDir(t, s, req.Name)
	logFile, err := s.openVMMLog(req)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close() //nolint:errcheck
	if _, err := os.Stat(filepath.Join(vmDir, "qemu.log")); err != nil {
		t.Fatalf("qemu.log is not in the VM directory: %v", err)
	}
	if _, err := logFile.WriteString("qemu: could not load kernel\n"); err != nil {
		t.Fatal(err)
	}
	// Truncated per launch, so a failed boot's log is that boot's.
	again, err := s.openVMMLog(req)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close() //nolint:errcheck
	body, err := os.ReadFile(filepath.Join(vmDir, "qemu.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Errorf("a second launch inherited %d bytes of the previous log", len(body))
	}
}

// A QEMU helper must not come up on a machine type nobody chose, and must not
// invent a second default beside the driver's.
func TestQemuOptionsRequireAnAbsoluteBinary(t *testing.T) {
	base := ServerOptions{Backend: BackendQEMU, QemuBin: "qemu-system-x86_64", MachineType: "pc-q35-8.2"}
	if err := validateQemuOptions(base); err == nil {
		t.Error("a relative qemu binary was accepted")
	}
	missing := base
	missing.QemuBin = "/nonexistent/qemu-system-x86_64"
	if err := validateQemuOptions(missing); err == nil {
		t.Error("a qemu binary that does not exist was accepted")
	}
}
