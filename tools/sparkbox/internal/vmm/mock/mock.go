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

	"github.com/creack/pty"
	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

type fakeVM struct {
	name          string
	workdir       string
	listener      net.Listener
	server        *gssh.Server
	paused        bool
	memMB         int64
	balloonTarget int64  // MiB the balloon is holding (0 = deflated)
	cpuNanos      uint64 // synthetic cumulative CPU time (see CPUTimeNanos)
	netRx, netTx  uint64 // synthetic cumulative network bytes (see NetBytes)
}

type Driver struct {
	mu       sync.Mutex
	stateDir string
	hostKey  xssh.Signer
	vms      map[string]*fakeVM
	// LoginUser mirrors the firecracker driver's login user so a mock host
	// reports the same SSHUser the real fleet would. Empty defaults root.
	LoginUser string
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
	workdir := filepath.Join(d.stateDir, "mock-vms", cfg.Name)
	seed := isEmptyDir(workdir) // fresh dir? then a snapshot template may seed it
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
	srv := &gssh.Server{
		Handler: func(s gssh.Session) { handleSession(s, vm.workdir) },
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

// handleSession runs the requested command (or an interactive shell) in the
// sandbox workdir. This is intentionally NOT an isolation boundary — the mock
// driver exists to exercise the control plane and gateway, not to sandbox.
func handleSession(s gssh.Session, workdir string) {
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

	ptyReq, winCh, isPty := s.Pty()
	if isPty {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
		f, err := pty.Start(cmd)
		if err != nil {
			fmt.Fprintf(s.Stderr(), "pty: %v\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		defer f.Close()
		go func() {
			for win := range winCh {
				pty.Setsize(f, &pty.Winsize{Rows: uint16(win.Height), Cols: uint16(win.Width)}) //nolint:errcheck
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
		return nil
	}
	if vm.server != nil {
		vm.server.Close() //nolint:errcheck
	}
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
	return vmm.BalloonStats{
		TargetMiB: vm.balloonTarget,
		ActualMiB: vm.balloonTarget,
		FreeMiB:   max64(0, vm.memMB-vm.balloonTarget-256), // pretend ~256MiB in use
	}, nil
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
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	return untarTree(inPath, workdir)
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
// 25 GiB the real templates are built at.
const mockDiskCapacityMB int64 = 25600

// DiskCapacityMB implements vmm.DiskReporter with a fixed ceiling for any VM
// the mock knows about, standing in for the rootfs image's apparent size.
func (d *Driver) DiskCapacityMB(_ context.Context, name string) (int64, error) {
	if _, err := os.Stat(filepath.Join(d.stateDir, "mock-vms", name)); err != nil {
		return 0, nil
	}
	return mockDiskCapacityMB, nil
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
