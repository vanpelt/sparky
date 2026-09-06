// Package mock implements vmm.Driver with in-process fake VMs so the full
// sparkbox stack (API, manager, SSH gateway) can run and be tested on any
// machine — no KVM required.
//
// Each fake VM is a gliderlabs/ssh server on 127.0.0.1 that executes commands
// with /bin/sh inside a per-sandbox workdir. Pause stops the listener but
// keeps the workdir (the "disk"); Resume starts a fresh listener on a new
// port — the same observable semantics as firecracker snapshot/restore from
// the gateway's point of view, minus preserved memory.
package mock

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/creack/pty"
	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Every optional capability in vmm, asserted at compile time. The mock must
// offer all of them or the mock-driven manager tests silently skip the paths
// behind the ones it lost — the same failure as the firecracker driver losing
// one, arriving as a green test run instead of a degraded fleet.
var _ vmm.FullDriver = (*Driver)(nil)

type fakeVM struct {
	name          string
	workdir       string
	listener      net.Listener
	server        *gssh.Server
	paused        bool
	memMB         int64
	balloonTarget int64  // MiB the balloon is holding (0 = deflated)
	inUseMiB      int64  // MiB the guest is genuinely touching (see SetInUseMiB)
	cpuNanos      uint64 // synthetic cumulative CPU time (see CPUTimeNanos)
	netRx, netTx  uint64 // synthetic cumulative network bytes (see NetBytes)

	// procMu guards procs: what is running "inside" this VM right now. A real
	// microVM takes its processes with it when it stops; this one runs them on
	// the host, so without this they outlive the machine that was supposed to
	// be holding them — a shell still sitting in the workdir after the VM was
	// paused or destroyed, writing into a directory the driver has already
	// deleted or a test's temp dir is already sweeping.
	procMu sync.Mutex
	procs  map[*exec.Cmd]struct{}
}

// track registers a command as running inside the VM and returns its
// deregistration.
func (vm *fakeVM) track(cmd *exec.Cmd) func() {
	vm.procMu.Lock()
	if vm.procs == nil {
		vm.procs = map[*exec.Cmd]struct{}{}
	}
	vm.procs[cmd] = struct{}{}
	vm.procMu.Unlock()
	return func() {
		vm.procMu.Lock()
		delete(vm.procs, cmd)
		vm.procMu.Unlock()
	}
}

// stopProcs kills everything running inside the VM and waits for it to be gone,
// which is what "the machine stopped" means. Bounded, because a driver call
// must not be able to hang on a wedged child, and best-effort: a process that
// will not die is not worth failing a Pause over.
func (vm *fakeVM) stopProcs() {
	vm.procMu.Lock()
	for cmd := range vm.procs {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
		}
	}
	vm.procMu.Unlock()
	for range 200 {
		vm.procMu.Lock()
		n := len(vm.procs)
		vm.procMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type Driver struct {
	mu       sync.Mutex
	stateDir string
	hostKey  xssh.Signer
	vms      map[string]*fakeVM
	// diskCap overrides the default per-VM disk ceiling, set by ResizeDisk.
	diskCap map[string]int64
	// LoginUser mirrors the firecracker driver's login user so a mock host
	// reports the same SSHUser the real fleet would. Empty defaults root.
	LoginUser string
	// PauseDelay makes Pause take at least this long, for the tests that need an
	// operation to still be running when something else looks at it.
	//
	// A real pause takes a moment; this driver's takes microseconds, which is
	// what lets a test that means "while this is in flight" pass by coincidence
	// on a quiet machine and fail on a loaded one. Set it and the window is a
	// fact rather than a race. Zero is the default and costs nothing.
	PauseDelay time.Duration
}

func New(stateDir string, hostKey xssh.Signer) *Driver {
	return &Driver{stateDir: stateDir, hostKey: hostKey, vms: map[string]*fakeVM{}}
}

func (d *Driver) Create(_ context.Context, cfg vmm.Config) (*vmm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.vms[cfg.Name]; ok {
		return nil, fmt.Errorf("vm %q already exists", cfg.Name)
	}
	workdir := d.vmDir(cfg.Name)
	seed := isEmptyDir(workdir) // fresh dir? then a snapshot template may seed it
	// Same rule the firecracker driver enforces: a disk under a name the ledger
	// has never issued is the residue of an unfinished destroy, and adopting it
	// would hand its previous owner's home directory to whoever claimed the name
	// next. See vmm.Config.NewSandbox.
	if cfg.NewSandbox && !seed {
		return nil, fmt.Errorf("workdir for %q is not empty (%s); "+
			"a previous sandbox of this name did not finish being destroyed", cfg.Name, workdir)
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, err
	}
	// Fork support (mirrors the firecracker reflink-from-template path): if a
	// snapshot template exists for this image and the workdir is fresh, seed it.
	// A restore leaves the workdir already populated (UnpackRootfs), so skip.
	if seed {
		if tpl := filepath.Join(d.stateDir, "mock-templates", cfg.Image); dirExists(tpl) {
			copyTree(tpl, workdir) //nolint:errcheck // best effort, like reflink fallback
		}
	}
	vm := &fakeVM{name: cfg.Name, workdir: workdir, memMB: cfg.MemMB}
	if err := d.start(vm, cfg.GatewayPublicKey); err != nil {
		return nil, err
	}
	d.vms[cfg.Name] = vm
	return d.instance(vm), nil
}

// start boots the fake VM's SSH server on a fresh localhost port.
func (d *Driver) start(vm *fakeVM, gatewayKey string) error {
	authorized, _, _, _, err := xssh.ParseAuthorizedKey([]byte(gatewayKey))
	if err != nil {
		return fmt.Errorf("parse gateway key: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	// ptys carries each session's PTY details from the request loop to the
	// handler. See snapPty for why they cannot simply be read in the handler.
	var ptys sync.Map
	srv := &gssh.Server{
		Handler:                func(s gssh.Session) { handleSession(s, &ptys, vm) },
		SessionRequestCallback: func(s gssh.Session, _ string) bool { return snapPty(s, &ptys) },
		PublicKeyHandler: func(_ gssh.Context, key gssh.PublicKey) bool {
			return gssh.KeysEqual(key, authorized)
		},
	}
	srv.AddHostKey(d.hostKey)
	go srv.Serve(ln) //nolint:errcheck // returns on Close
	vm.listener = ln
	vm.server = srv
	vm.paused = false
	// Remember the key so Resume can restart with it.
	keyPath := filepath.Join(vm.workdir, ".gateway_key")
	return os.WriteFile(keyPath, []byte(gatewayKey), 0o600)
}

// ptyInfo is one session's PTY, snapshotted off the goroutine that owns it.
type ptyInfo struct {
	req   gssh.Pty
	winCh <-chan gssh.Window
	isPty bool
}

// snapPty records a session's PTY details from inside the SSH request loop, and
// exists only to keep the race detector honest about a genuine race in the
// library.
//
// gliderlabs stores the current window inside the same struct Session.Pty()
// returns by value, and writes to it from the request-loop goroutine every time
// a "window-change" arrives (session.go:351) with no lock and no ordering
// against the handler goroutine it spawned. So a guest that calls s.Pty() in
// its handler — the documented way to use the API, and what this file used to
// do — races every resize the client sends. It is upstream's race, but we are
// the ones calling into it, and it turns up as a hard failure the moment
// anything drives a browser terminal against a mock guest under -race.
//
// SessionRequestCallback is the fix because of WHERE it runs: the request loop
// invokes it for "shell"/"exec" on its own goroutine, after "pty-req" has
// already populated the fields and before it spawns the handler. Reading them
// there is same-goroutine, and the `go` that starts the handler is the
// happens-before edge that publishes the snapshot. Nothing is copied twice and
// nothing is guessed.
//
// Always returns true: this is a snapshot, not a policy.
func snapPty(s gssh.Session, ptys *sync.Map) bool {
	req, winCh, isPty := s.Pty()
	ptys.Store(s, ptyInfo{req: req, winCh: winCh, isPty: isPty})
	return true
}

// handleSession runs the requested command (or an interactive shell) in the
// sandbox workdir. This is intentionally NOT an isolation boundary — the mock
// driver exists to exercise the control plane and gateway, not to sandbox.
func handleSession(s gssh.Session, ptys *sync.Map, vm *fakeVM) {
	workdir := vm.workdir
	shell := "/bin/sh"
	var cmd *exec.Cmd
	if raw := s.RawCommand(); raw != "" {
		cmd = exec.Command(shell, "-c", raw)
	} else {
		cmd = exec.Command(shell)
	}
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "SPARKBOX=mock", "HOME="+workdir)
	cmd.Env = append(cmd.Env, s.Environ()...)

	var info ptyInfo
	if v, ok := ptys.LoadAndDelete(s); ok {
		info = v.(ptyInfo)
	}
	ptyReq, winCh, isPty := info.req, info.winCh, info.isPty
	if isPty {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
		f, err := pty.Start(cmd)
		if err != nil {
			fmt.Fprintf(s.Stderr(), "pty: %v\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		defer vm.track(cmd)()
		// The pty master outlives this function only as far as the winsize
		// goroutine, and the two must not touch it at the same time: Setsize
		// reads f.Fd() while Close is destroying the descriptor underneath it,
		// which is a real data race and, worse, a chance to resize whatever
		// file the recycled fd now belongs to. gliderlabs pushes the INITIAL
		// window onto winCh as soon as it accepts the pty request, so EVERY
		// session runs at least one Setsize and every teardown can race it —
		// which is why this showed up on a test that never resizes anything.
		//
		// One mutex, and a flag so a resize arriving after the shell exited is
		// dropped rather than applied to a dead descriptor. The goroutine is
		// left to drain winCh until gliderlabs closes it, because a blocked
		// send on that channel stalls the session's whole request loop.
		var ptyMu sync.Mutex
		ptyClosed := false
		defer func() {
			ptyMu.Lock()
			ptyClosed = true
			f.Close() //nolint:errcheck
			ptyMu.Unlock()
		}()
		go func() {
			for win := range winCh {
				ptyMu.Lock()
				if !ptyClosed {
					pty.Setsize(f, &pty.Winsize{Rows: uint16(win.Height), Cols: uint16(win.Width)}) //nolint:errcheck
				}
				ptyMu.Unlock()
			}
		}()
		go io.Copy(f, s) //nolint:errcheck
		io.Copy(s, f)    //nolint:errcheck
	} else {
		cmd.Stdin = s
		cmd.Stdout = s
		cmd.Stderr = s.Stderr()
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(s.Stderr(), "start: %v\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		defer vm.track(cmd)()
	}
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			s.Exit(exitErr.ExitCode()) //nolint:errcheck
			return
		}
		s.Exit(1) //nolint:errcheck
		return
	}
	s.Exit(0) //nolint:errcheck
}

func (d *Driver) Pause(_ context.Context, name string) error {
	if d.PauseDelay > 0 {
		time.Sleep(d.PauseDelay) // before the lock: this is latency, not work
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	vm, ok := d.vms[name]
	if !ok {
		return fmt.Errorf("vm %q not found", name)
	}
	if vm.paused {
		return nil
	}
	vm.server.Close() //nolint:errcheck
	vm.stopProcs()
	vm.listener = nil
	vm.server = nil
	vm.paused = true
	return nil
}

func (d *Driver) Resume(_ context.Context, name string) (*vmm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	vm, ok := d.vms[name]
	if !ok {
		return nil, fmt.Errorf("vm %q not found", name)
	}
	if !vm.paused {
		return d.instance(vm), nil
	}
	key, err := os.ReadFile(filepath.Join(vm.workdir, ".gateway_key"))
	if err != nil {
		return nil, fmt.Errorf("read gateway key: %w", err)
	}
	if err := d.start(vm, string(key)); err != nil {
		return nil, err
	}
	return d.instance(vm), nil
}

func (d *Driver) Destroy(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	vm, ok := d.vms[name]
	if !ok {
		// No record, but possibly a disk: d.vms is per-process and nothing
		// rehydrates it, so a controller that restarted since this VM last
		// booted must still reclaim the workdir. See the firecracker driver.
		return os.RemoveAll(d.vmDir(name))
	}
	if vm.server != nil {
		vm.server.Close() //nolint:errcheck
	}
	vm.stopProcs()
	delete(d.vms, name)
	return os.RemoveAll(vm.workdir)
}

func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, vm := range d.vms {
		if vm.server != nil {
			vm.server.Close() //nolint:errcheck
		}
		vm.stopProcs()
	}
	return nil
}

// SetBalloonTarget records the balloon target so tests can assert the manager's
// reclaim logic. The mock has no real memory to hand back — it just tracks the
// number. Implements vmm.Ballooner. Errors on a paused/missing VM, matching the
// firecracker driver (you can't balloon a VM that isn't running).
func (d *Driver) SetBalloonTarget(_ context.Context, name string, targetMiB int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	vm, ok := d.vms[name]
	if !ok || vm.paused {
		return fmt.Errorf("vm %q not running", name)
	}
	if targetMiB < 0 {
		targetMiB = 0
	}
	vm.balloonTarget = targetMiB
	return nil
}

// BalloonStats synthesises a plausible picture from the recorded target so the
// control plane and console have something to render under the mock driver.
func (d *Driver) BalloonStats(_ context.Context, name string) (vmm.BalloonStats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	vm, ok := d.vms[name]
	if !ok || vm.paused {
		return vmm.BalloonStats{}, fmt.Errorf("vm %q not running", name)
	}
	inUse := vm.inUseMiB
	if inUse <= 0 {
		inUse = defaultInUseMiB
	}
	available := max64(0, vm.memMB-vm.balloonTarget-inUse)
	return vmm.BalloonStats{
		TargetMiB:    vm.balloonTarget,
		ActualMiB:    vm.balloonTarget,
		FreeMiB:      available / 2, // some of the available RAM is reclaimable cache
		AvailableMiB: available,     // the rest is what the guest is really touching
	}, nil
}

// defaultInUseMiB is the working set a mock guest reports when a test has not
// said otherwise: small enough that the balloon policy's live-working-set floor
// never binds, so tests about other things are unaffected by it.
const defaultInUseMiB = 256

// SetInUseMiB makes the named mock guest report a working set of inUseMiB, the
// way a real guest mid-build or mid-boot would. It is how a test reaches the
// balloon policy's "never squeeze a guest below what it is actually using"
// rule, which is invisible while every guest claims to be using 256 MiB.
func (d *Driver) SetInUseMiB(name string, inUseMiB int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if vm, ok := d.vms[name]; ok {
		vm.inUseMiB = inUseMiB
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// --- Archivable + DiskReporter (the disk-lifecycle capabilities) -----------
//
// The mock's "disk" is a workdir tree. PackRootfs tars it to a single file the
// manager can hand to a (fake) object store; UnpackRootfs reverses it; Snapshot
// copies it into a template dir Create seeds forks from. Pure stdlib so the full
// archive/restore/snapshot/fork lifecycle is exercisable in `go test` with no
// KVM, tar, or zstd on the machine.

func (d *Driver) workdirFor(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if vm, ok := d.vms[name]; ok {
		return vm.workdir
	}
	return d.vmDir(name)
}

// vmDir is where a sandbox's "disk" lives, derived from its name alone so the
// lifecycle paths that run without a record (Destroy after a restart, Create
// before there is one) agree with the ones that have one. Caller need not hold
// d.mu — it reads only immutable driver config.
func (d *Driver) vmDir(name string) string {
	return filepath.Join(d.stateDir, "mock-vms", name)
}

func (d *Driver) notRunning(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if vm, ok := d.vms[name]; ok && !vm.paused {
		return fmt.Errorf("vm %q is running", name)
	}
	return nil
}

func (d *Driver) PackRootfs(_ context.Context, name string) (string, error) {
	if err := d.notRunning(name); err != nil {
		return "", err
	}
	workdir := d.workdirFor(name)
	out := workdir + ".pack.tar"
	if err := tarTree(workdir, out); err != nil {
		return "", err
	}
	return out, nil
}

func (d *Driver) UnpackRootfs(_ context.Context, name, inPath string) error {
	workdir := filepath.Join(d.stateDir, "mock-vms", name)
	parent := filepath.Dir(workdir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+name+".rootfs-restoring-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck
	if err := untarTree(inPath, tmp); err != nil {
		return err
	}
	backup, err := os.MkdirTemp(parent, "."+name+".rootfs-previous-")
	if err != nil {
		return err
	}
	os.RemoveAll(backup)       //nolint:errcheck // reserve only a unique sibling name
	defer os.RemoveAll(backup) //nolint:errcheck
	if err := os.Rename(workdir, backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, workdir); err != nil {
		os.Rename(backup, workdir) //nolint:errcheck
		return err
	}
	os.RemoveAll(backup) //nolint:errcheck
	return nil
}

func (d *Driver) RootfsPresent(name string) (bool, error) {
	workdir := filepath.Join(d.stateDir, "mock-vms", name)
	info, err := os.Stat(workdir)
	if err == nil {
		// The directory itself is the mock's disk. A newly created sandbox may
		// legitimately have no user files yet, so emptiness is not loss.
		return info.IsDir(), nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (d *Driver) Snapshot(_ context.Context, name, newImage string) error {
	if err := d.notRunning(name); err != nil {
		return err
	}
	tpl := filepath.Join(d.stateDir, "mock-templates", newImage)
	os.RemoveAll(tpl) //nolint:errcheck
	return copyTree(d.workdirFor(name), tpl)
}

func (d *Driver) RemoveTemplate(_ context.Context, image string) error {
	return os.RemoveAll(filepath.Join(d.stateDir, "mock-templates", image))
}

// DiskUsageMB reports the workdir's byte size in MiB, so pooled-disk accounting
// has a real (if tiny) signal under the mock — a test writes a sized file to
// move the needle.
func (d *Driver) DiskUsageMB(_ context.Context, name string) (int64, error) {
	workdir := filepath.Join(d.stateDir, "mock-vms", name)
	var total int64
	filepath.Walk(workdir, func(_ string, info os.FileInfo, err error) error { //nolint:errcheck
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total / (1024 * 1024), nil
}

// mockDiskCapacityMB is the synthetic per-sandbox disk ceiling, matching the
// 25 GiB the real templates are built at. ResizeDisk overrides it per VM.
const mockDiskCapacityMB int64 = 25600

// DiskCapacityMB implements vmm.DiskReporter with a fixed ceiling for any VM
// the mock knows about, standing in for the rootfs image's apparent size.
func (d *Driver) DiskCapacityMB(_ context.Context, name string) (int64, error) {
	if _, err := os.Stat(filepath.Join(d.stateDir, "mock-vms", name)); err != nil {
		return 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if mb, ok := d.diskCap[name]; ok {
		return mb, nil
	}
	return mockDiskCapacityMB, nil
}

// TemplateUsageMB implements vmm.TemplateReporter over the mock's template
// dirs, with the same arithmetic DiskUsageMB uses on a workdir. Create seeds a
// fork by copyTree-ing the template, so a fresh fork's usage and its baseline
// are identical and its pooled figure nets to zero — which is exactly the
// property the pooled accounting rests on, and this is what lets `go test ./...`
// exercise the whole subtraction with no KVM.
//
// A template this driver does not hold is an error wrapping os.ErrNotExist, not
// zero, per the vmm.TemplateReporter contract: a deleted snapshot must leave the
// manager's stored baseline alone rather than re-charge every fork for it.
func (d *Driver) TemplateUsageMB(_ context.Context, image string) (int64, error) {
	tpl := filepath.Join(d.stateDir, "mock-templates", image)
	if !dirExists(tpl) {
		return 0, fmt.Errorf("mock template %q: %w", image, os.ErrNotExist)
	}
	var total int64
	filepath.Walk(tpl, func(_ string, info os.FileInfo, err error) error { //nolint:errcheck
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total / (1024 * 1024), nil
}

// ResizeDisk implements vmm.DiskResizer. It records the new ceiling rather than
// moving real bytes, but keeps the two rules that matter to callers: it refuses
// a VM the driver still has running, and it refuses to shrink.
func (d *Driver) ResizeDisk(_ context.Context, name string, sizeMB int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if vm, ok := d.vms[name]; ok && !vm.paused {
		return fmt.Errorf("vm %q is running; pause it first", name)
	}
	cur := mockDiskCapacityMB
	if mb, ok := d.diskCap[name]; ok {
		cur = mb
	}
	if sizeMB <= cur {
		return fmt.Errorf("disk is already %d MB; resize only grows (asked for %d MB)", cur, sizeMB)
	}
	if d.diskCap == nil {
		d.diskCap = map[string]int64{}
	}
	d.diskCap[name] = sizeMB
	return nil
}

// --- Renamer + Rebooter + CPUStatser (the user-console capabilities) --------

// DropSnapshots implements vmm.Rebooter. The mock keeps no memory snapshot —
// its "disk" is the workdir — so the only work is forgetting the paused VM,
// which makes the manager's next EnsureRunning recreate it from the persisted
// workdir: the same cold-boot-observable semantics as firecracker.
func (d *Driver) DropSnapshots(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if vm, ok := d.vms[name]; ok && !vm.paused {
		return fmt.Errorf("vm %q is running; pause it first", name)
	}
	delete(d.vms, name)
	return nil
}

// RenameVM implements vmm.Renamer: move the stopped VM's workdir and rekey the
// driver's record. The .gateway_key rides along inside the workdir, so a later
// Resume under the new name authenticates unchanged.
func (d *Driver) RenameVM(oldName, newName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if vm, ok := d.vms[oldName]; ok && !vm.paused {
		return fmt.Errorf("vm %q is running; pause it first", oldName)
	}
	if _, ok := d.vms[newName]; ok {
		return fmt.Errorf("vm %q already exists", newName)
	}
	oldDir := filepath.Join(d.stateDir, "mock-vms", oldName)
	newDir := filepath.Join(d.stateDir, "mock-vms", newName)
	if _, err := os.Stat(newDir); err == nil {
		return fmt.Errorf("workdir for %q already exists", newName)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}
	if vm, ok := d.vms[oldName]; ok {
		delete(d.vms, oldName)
		vm.name = newName
		vm.workdir = newDir
		d.vms[newName] = vm
	}
	return nil
}

// mockCPUTickNanos is the synthetic CPU time each CPUTimeNanos call accrues.
// Fixed so tests computing client-side percentages from poll deltas get exact,
// deterministic numbers.
const mockCPUTickNanos uint64 = 50_000_000 // 50ms

// CPUTimeNanos implements vmm.CPUStatser with a monotonic synthetic counter:
// every call on a running VM accrues mockCPUTickNanos. Errors on a
// paused/missing VM, matching the firecracker driver; the counter survives
// pause/resume (the fakeVM record persists) but not DropSnapshots, matching
// firecracker's reset-on-cold-boot.
func (d *Driver) CPUTimeNanos(_ context.Context, name string) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	vm, ok := d.vms[name]
	if !ok || vm.paused {
		return 0, fmt.Errorf("vm %q not running", name)
	}
	vm.cpuNanos += mockCPUTickNanos
	return vm.cpuNanos, nil
}

// NetBytes implements vmm.NetStatser. Unlike CPUTimeNanos it does not accrue on
// its own — the counters only move when a test calls SetNetBytes — so a test
// can hold a sandbox network-silent (the idle case) or step it by an exact
// number of bytes. Errors on a paused/missing VM, matching firecracker.
func (d *Driver) NetBytes(_ context.Context, name string) (rx, tx uint64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	vm, ok := d.vms[name]
	if !ok || vm.paused {
		return 0, 0, fmt.Errorf("vm %q not running", name)
	}
	return vm.netRx, vm.netTx, nil
}

// SetNetBytes drives the synthetic network counters for a running VM. Passing
// values below the current ones simulates the reset a tap teardown causes, so
// tests can cover the accumulator's reset handling.
func (d *Driver) SetNetBytes(name string, rx, tx uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	vm, ok := d.vms[name]
	if !ok {
		return fmt.Errorf("vm %q not found", name)
	}
	vm.netRx, vm.netTx = rx, tx
	return nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isEmptyDir(p string) bool {
	entries, err := os.ReadDir(p)
	return err != nil || len(entries) == 0 // missing counts as empty (fresh)
}

// copyTree recursively copies src into dst (files + dirs, mode-preserving).
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		if !info.Mode().IsRegular() {
			return nil // skip sockets/symlinks — the mock's workdir has none
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// tarTree writes a plain (uncompressed) tar of dir's regular files to out.
func tarTree(dir, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: rel, Mode: int64(info.Mode().Perm()), Size: info.Size()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
}

// untarTree extracts a tarTree archive into dir.
func untarTree(in, dir string) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.Clean("/"+hdr.Name)) // defuse path traversal
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // mock-only, sizes are ours
			out.Close()
			return err
		}
		out.Close()
	}
}

func (d *Driver) instance(vm *fakeVM) *vmm.Instance {
	user := d.LoginUser
	if user == "" {
		user = "root"
	}
	inst := &vmm.Instance{Name: vm.name, SSHUser: user}
	if vm.paused {
		inst.State = vmm.StatePaused
	} else {
		inst.State = vmm.StateRunning
		inst.SSHAddr = vm.listener.Addr().String()
		// Fake VMs live on loopback; a real service the user starts inside is
		// reachable at 127.0.0.1:<port> from the proxy's point of view.
		inst.HostIP = "127.0.0.1"
	}
	return inst
}
