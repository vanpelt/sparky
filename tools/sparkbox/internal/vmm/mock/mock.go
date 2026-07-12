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
	name     string
	workdir  string
	listener net.Listener
	server   *gssh.Server
	paused   bool
}

type Driver struct {
	mu       sync.Mutex
	stateDir string
	hostKey  xssh.Signer
	vms      map[string]*fakeVM
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
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, err
	}
	vm := &fakeVM{name: cfg.Name, workdir: workdir}
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

func (d *Driver) instance(vm *fakeVM) *vmm.Instance {
	inst := &vmm.Instance{Name: vm.name, SSHUser: "root"}
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
