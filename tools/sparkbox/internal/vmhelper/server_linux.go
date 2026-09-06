//go:build linux

package vmhelper

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
)

const (
	jailedSocketName = "fc.sock"
	jailedKernelName = "vmlinux"
	jailedRootfsName = "rootfs.ext4"
	jailedMemName    = "mem.snap"
	jailedStateName  = "state.snap"
	userHZ           = 100
	guestOutChain    = "SPARKBOX_GUEST_OUT"
	guestInChain     = "SPARKBOX_GUEST_IN"
	guestHostChain   = "SPARKBOX_GUEST_HOST"
)

var vmNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,126}$`)

type ServerOptions struct {
	SocketPath string
	// Backend is the VMM this helper launches for its whole life. Empty means
	// Firecracker, so an existing deployment keeps its behaviour untouched.
	Backend Backend
	// QemuBin and MachineType are read only when Backend is BackendQEMU. They
	// live here rather than in a Request because they select what gets executed
	// and how the machine is modelled — see the Backend doc comment.
	QemuBin                string
	MachineType            string
	FirecrackerBin         string
	KernelPath             string
	VMStateDir             string
	ChrootBase             string
	Subnet                 string
	Subnet6                string
	JailerUIDBase          int
	ControllerUID          int
	ControllerGID          int
	RestrictInternalEgress bool
	SluiceSocket           string
	Logger                 *log.Logger
}

type activeVM struct {
	name string
	pid  int
}

type server struct {
	opts    ServerOptions
	network guestnet.Network
	prefix6 net.IP
	uplink6 string
	mu      sync.Mutex
	active  map[int]activeVM
	stateFD int
}

func RunServer(ctx context.Context, opts ServerOptions) error {
	s, err := newServer(opts)
	if err != nil {
		return err
	}
	defer unix.Close(s.stateFD) //nolint:errcheck
	if opts.RestrictInternalEgress {
		if err := s.validateRestrictedEgress(ctx); err != nil {
			return err
		}
		s.opts.Logger.Printf("restricted guest egress enabled with per-TAP source pinning")
	}
	if err := os.MkdirAll(filepath.Dir(opts.SocketPath), 0o750); err != nil {
		return fmt.Errorf("create helper socket directory: %w", err)
	}
	if err := os.Chown(filepath.Dir(opts.SocketPath), 0, opts.ControllerGID); err != nil {
		return fmt.Errorf("own helper socket directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(opts.SocketPath), 0o750); err != nil {
		return fmt.Errorf("protect helper socket directory: %w", err)
	}
	if err := os.Remove(opts.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale helper socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: opts.SocketPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on helper socket: %w", err)
	}
	defer func() {
		listener.Close()           //nolint:errcheck
		os.Remove(opts.SocketPath) //nolint:errcheck
	}()
	if err := os.Chown(opts.SocketPath, opts.ControllerUID, opts.ControllerGID); err != nil {
		return fmt.Errorf("own helper socket: %w", err)
	}
	if err := os.Chmod(opts.SocketPath, 0o600); err != nil {
		return fmt.Errorf("protect helper socket: %w", err)
	}

	s.sweepStaleTaps()
	go func() {
		<-ctx.Done()
		listener.Close() //nolint:errcheck
	}()
	s.opts.Logger.Printf("privileged VM helper listening on %s for uid %d", opts.SocketPath, opts.ControllerUID)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept helper connection: %w", err)
		}
		uid, err := peerUID(conn)
		if err != nil || int(uid) != opts.ControllerUID {
			s.opts.Logger.Printf("refused helper peer uid=%d: %v", uid, err)
			conn.Close() //nolint:errcheck
			continue
		}
		go s.handle(ctx, conn)
	}
}

func newServer(opts ServerOptions) (*server, error) {
	paths := map[string]string{
		"socket": opts.SocketPath,
		"kernel": opts.KernelPath, "vm state": opts.VMStateDir, "chroot": opts.ChrootBase,
	}
	// The VMM binary is checked by backend, not both ways: a QEMU node's image
	// carries firecracker too, but requiring a flag the operator has no reason
	// to pass would make the QEMU deployment fail on a path it never uses.
	if opts.Backend == BackendQEMU {
		if err := defaultMachineType(&opts); err != nil {
			return nil, err
		}
		if err := validateQemuOptions(opts); err != nil {
			return nil, err
		}
	} else {
		paths["firecracker"] = opts.FirecrackerBin
	}
	for label, value := range paths {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" {
			return nil, fmt.Errorf("%s path must be an absolute, clean, non-root path", label)
		}
	}
	if opts.JailerUIDBase < 1 || opts.ControllerUID < 1 || opts.ControllerGID < 1 {
		return nil, errors.New("helper UIDs and GIDs must be positive")
	}
	if opts.SluiceSocket != "" && (!filepath.IsAbs(opts.SluiceSocket) || filepath.Clean(opts.SluiceSocket) != opts.SluiceSocket) {
		return nil, errors.New("sluice socket path must be absolute and clean")
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	network, err := guestnet.Parse(opts.Subnet)
	if err != nil {
		return nil, err
	}
	if opts.JailerUIDBase > int(^uint32(0))-network.Capacity() {
		return nil, errors.New("jailer UID range exceeds uint32")
	}
	if opts.Backend != BackendQEMU {
		if _, err := os.Stat(opts.FirecrackerBin); err != nil {
			return nil, fmt.Errorf("firecracker binary: %w", err)
		}
	}
	if _, err := os.Stat(opts.KernelPath); err != nil {
		return nil, fmt.Errorf("kernel image: %w", err)
	}
	for _, device := range []string{"/dev/kvm", "/dev/net/tun"} {
		var st unix.Stat_t
		if err := unix.Stat(device, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR {
			return nil, fmt.Errorf("helper requires character device %s", device)
		}
	}
	if err := ensureChrootBase(opts.ChrootBase); err != nil {
		return nil, fmt.Errorf("create chroot base: %w", err)
	}
	stateFD, err := unix.Open(opts.VMStateDir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open VM state root: %w", err)
	}
	s := &server{opts: opts, network: network, active: make(map[int]activeVM), stateFD: stateFD}
	if opts.Subnet6 != "" {
		_, ipNet, err := net.ParseCIDR(opts.Subnet6)
		if err != nil {
			unix.Close(stateFD) //nolint:errcheck
			return nil, fmt.Errorf("subnet6 %q: %w", opts.Subnet6, err)
		}
		s.prefix6 = ipNet.IP.To16()
		s.uplink6 = defaultRoute6Dev()
	}
	return s, nil
}

// ensureChrootBase makes the root-owned jail collection searchable but not
// listable. The controller needs to traverse this fixed path to the one
// per-VM API socket that the helper explicitly shares with its group. Each
// jail root remains 0710, and the VMM device nodes remain 0600 under the
// slot-scoped UID, so traversal does not expose KVM or TUN to the controller.
func ensureChrootBase(path string) error {
	if err := os.MkdirAll(path, 0o711); err != nil {
		return err
	}
	return os.Chmod(path, 0o711)
}

func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		cred, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if controlErr != nil {
		return 0, controlErr
	}
	return cred.Uid, nil
}

func (s *server) handle(ctx context.Context, conn *net.UnixConn) {
	defer conn.Close()                                    //nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	var req Request
	if err := json.NewDecoder(io.LimitReader(conn, maxMessageBytes)).Decode(&req); err != nil {
		s.respond(conn, Response{Error: "invalid request"})
		return
	}
	conn.SetReadDeadline(time.Time{}) //nolint:errcheck
	if err := s.validateRequest(req); err != nil {
		s.respond(conn, Response{Error: err.Error()})
		return
	}
	switch req.Op {
	case OpPing:
		s.respond(conn, Response{OK: true})
	case OpSnapshot:
		err := s.prepareSnapshotOutputs(req)
		s.respondErr(conn, err)
	case OpCPUTime:
		nanos, err := s.cpuTime(req)
		if err != nil {
			s.respond(conn, Response{Error: err.Error()})
		} else {
			s.respond(conn, Response{OK: true, CPUTimeNanos: nanos})
		}
	case OpLaunch:
		s.launch(ctx, conn, req)
	}
}

func (s *server) validateRequest(req Request) error {
	// A RANGE, not an equality. The controller and this helper are separate
	// containers of one Pod with independent restarts, so a rolling update has a
	// window with one on each image. An equality check turns that window into
	// "every launch fails"; accepting the previous version turns it into "the
	// old client keeps working, without the fields it never sent".
	if req.Version < MinProtocolVersion || req.Version > ProtocolVersion {
		return fmt.Errorf("unsupported helper protocol version %d (this helper speaks %d..%d)",
			req.Version, MinProtocolVersion, ProtocolVersion)
	}
	switch req.Op {
	case OpPing:
		return nil
	case OpLaunch, OpSnapshot, OpCPUTime:
	default:
		return fmt.Errorf("unsupported helper operation %q", req.Op)
	}
	if !vmNameRE.MatchString(req.Name) {
		return errors.New("invalid VM name")
	}
	if req.Slot < 0 || req.Slot >= s.network.Capacity() {
		return errors.New("VM slot is outside the configured subnet")
	}
	// The machine fields are required exactly when this helper is going to build
	// a QEMU argv out of them, and meaningless otherwise — Firecracker is
	// launched with an empty argv and configured over its own socket afterwards.
	// Checking them by backend rather than by protocol version is what lets a
	// version-2 Firecracker client keep sending nothing.
	if req.Op == OpLaunch && s.opts.Backend == BackendQEMU {
		if req.Version < 2 {
			return fmt.Errorf("this helper launches QEMU and needs protocol version 2 or later, "+
				"but the client sent version %d; the controller container is running an older image", req.Version)
		}
		if err := ValidateMachine(req); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) respond(conn net.Conn, response Response) {
	json.NewEncoder(conn).Encode(response) //nolint:errcheck
}

func (s *server) respondErr(conn net.Conn, err error) {
	if err != nil {
		s.respond(conn, Response{Error: err.Error()})
		return
	}
	s.respond(conn, Response{OK: true})
}

func (s *server) launch(ctx context.Context, conn *net.UnixConn, req Request) {
	launchCtx, cancelLaunch := context.WithCancel(ctx)
	defer cancelLaunch()
	disconnected := make(chan struct{}, 1)
	go func() {
		var one [1]byte
		conn.Read(one[:]) //nolint:errcheck
		cancelLaunch()
		disconnected <- struct{}{}
	}()

	s.mu.Lock()
	if active, exists := s.active[req.Slot]; exists {
		s.mu.Unlock()
		s.respond(conn, Response{Error: fmt.Sprintf("slot already belongs to %s", active.name)})
		return
	}
	s.active[req.Slot] = activeVM{name: req.Name}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, req.Slot)
		s.mu.Unlock()
	}()

	if err := s.cleanupSlot(req.Slot); err != nil {
		s.respond(conn, Response{Error: err.Error()})
		return
	}
	if err := s.createTap(launchCtx, req.Slot); err != nil {
		s.cleanupSlot(req.Slot) //nolint:errcheck
		s.respond(conn, Response{Error: err.Error()})
		return
	}
	if err := s.waitForSluice(launchCtx, tapName(req.Slot)); err != nil {
		s.cleanupSlot(req.Slot) //nolint:errcheck
		s.respond(conn, Response{Error: err.Error()})
		return
	}
	if err := s.prepareJail(req); err != nil {
		s.cleanupSlot(req.Slot) //nolint:errcheck
		s.respond(conn, Response{Error: err.Error()})
		return
	}
	if err := launchCtx.Err(); err != nil {
		s.cleanupSlot(req.Slot) //nolint:errcheck
		s.respond(conn, Response{Error: "launch client disconnected"})
		return
	}

	cmd, logFile, err := s.vmmCommand(req)
	if err != nil {
		s.cleanupSlot(req.Slot) //nolint:errcheck
		s.respond(conn, Response{Error: err.Error()})
		return
	}
	// The child gets its own descriptor at Start; ours is held for the VMM's
	// lifetime only because that is when this function returns, and released
	// here so a launch that fails before Start does not leak it. nil on the
	// Firecracker path, which logs to this process's own stdout and stderr.
	if logFile != nil {
		defer logFile.Close() //nolint:errcheck
	}
	if err := cmd.Start(); err != nil {
		s.cleanupSlot(req.Slot) //nolint:errcheck
		s.respond(conn, Response{Error: fmt.Sprintf("start %s: %v", s.vmmName(), err)})
		return
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	if err := s.publishSocket(launchCtx, req.Slot, waitCh); err != nil {
		cmd.Process.Kill() //nolint:errcheck
		<-waitCh
		s.cleanupSlot(req.Slot) //nolint:errcheck
		s.respond(conn, Response{Error: err.Error()})
		return
	}
	s.mu.Lock()
	s.active[req.Slot] = activeVM{name: req.Name, pid: cmd.Process.Pid}
	s.mu.Unlock()
	if err := json.NewEncoder(conn).Encode(Response{OK: true}); err != nil {
		cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck
		<-waitCh
		s.cleanupSlot(req.Slot) //nolint:errcheck
		return
	}

	var processErr error
	stopping := false
	select {
	case processErr = <-waitCh:
	case <-disconnected:
		stopping = true
		processErr = stopProcess(cmd, waitCh)
	case <-ctx.Done():
		stopping = true
		processErr = stopProcess(cmd, waitCh)
	}
	if stopping {
		processErr = nil
	}
	cleanupErr := s.cleanupSlot(req.Slot)
	processErr = errors.Join(processErr, cleanupErr)
	if processErr != nil {
		s.respond(conn, Response{Error: fmt.Sprintf("%s exited: %v", s.vmmName(), processErr)})
	} else {
		s.respond(conn, Response{OK: true})
	}
}

// vmmCommand builds the child process for this helper's backend. It returns an
// optional log file the caller must close after Start: the QEMU path writes the
// child's output to a per-VM qemu.log the controller can read back, where the
// Firecracker path writes to this process's own stdout and stderr.
//
// The two branches confine the child in fundamentally different ways — see
// qemu_linux.go's file comment — and that difference is the whole reason this
// is a switch rather than a binary name.
func (s *server) vmmCommand(req Request) (*exec.Cmd, *os.File, error) {
	if s.qemu() {
		return s.qemuCommand(req)
	}
	uid := uint32(s.jailUID(req.Slot))
	cmd := exec.Command("/firecracker", "--api-sock", jailedSocketName)
	cmd.Dir = "/"
	cmd.Env = []string{}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:     s.jailRoot(req.Slot),
		Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{uid}},
	}
	return cmd, nil, nil
}

func (s *server) waitForSluice(ctx context.Context, tap string) error {
	if s.opts.SluiceSocket == "" {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.opts.SluiceSocket)
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(waitCtx, http.MethodPost, "http://sluice/ready/"+tap, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return fmt.Errorf("wait for sluice on %s: %w", tap, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("wait for sluice on %s: %s: %s", tap, resp.Status, strings.TrimSpace(string(body)))
	}
	s.opts.Logger.Printf("sluice enforcement ready on %s", tap)
	return nil
}

func stopProcess(cmd *exec.Cmd, waitCh <-chan error) error {
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case err := <-waitCh:
		return err
	case <-time.After(10 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		return <-waitCh
	}
}

func (s *server) publishSocket(ctx context.Context, slot int, waitCh chan error) error {
	socket := filepath.Join(s.jailRoot(slot), s.launchSocketName())
	// Firecracker binds its API socket as almost its first action; QEMU creates
	// the QMP chardev while it builds the machine from a much longer command
	// line, so this gate is sized for the slower of the two rather than for the
	// one it was written against.
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			if err := os.Chown(socket, s.jailUID(slot), s.opts.ControllerGID); err != nil {
				return fmt.Errorf("share %s control socket: %w", s.vmmName(), err)
			}
			if err := os.Chmod(socket, 0o660); err != nil {
				return fmt.Errorf("protect %s control socket: %w", s.vmmName(), err)
			}
			return nil
		}
		select {
		case err := <-waitCh:
			// The launch path still owns the one Wait result. Put it back so its
			// failure cleanup cannot block trying to wait a second time.
			waitCh <- err
			return fmt.Errorf("%s exited before creating its control socket: %w", s.vmmName(), err)
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for the %s control socket", s.vmmName())
		case <-ticker.C:
		}
	}
}

func (s *server) prepareJail(req Request) error {
	if s.qemu() {
		return s.prepareQemuJail(req)
	}
	root := s.jailRoot(req.Slot)
	if err := os.MkdirAll(filepath.Join(root, "dev", "net"), 0o755); err != nil {
		return fmt.Errorf("create chroot jail: %w", err)
	}
	uid := s.jailUID(req.Slot)
	if err := copyExecutable(s.opts.FirecrackerBin, filepath.Join(root, "firecracker")); err != nil {
		return err
	}
	for _, device := range []struct{ source, destination string }{
		{"/dev/kvm", filepath.Join(root, "dev", "kvm")},
		{"/dev/net/tun", filepath.Join(root, "dev", "net", "tun")},
	} {
		if err := cloneDevice(device.source, device.destination, uid); err != nil {
			return err
		}
	}
	if err := os.Chown(root, uid, s.opts.ControllerGID); err != nil {
		return fmt.Errorf("own chroot root: %w", err)
	}
	// The controller group can traverse to the API socket but cannot list the
	// jail. Other VMM UIDs cannot traverse at all.
	if err := os.Chmod(root, 0o710); err != nil {
		return fmt.Errorf("protect chroot root: %w", err)
	}
	if err := s.linkTrustedResource(root, s.opts.KernelPath, jailedKernelName); err != nil {
		return err
	}
	resources := []struct{ source, name string }{
		{filepath.Join(s.vmRel(req.Name), jailedRootfsName), jailedRootfsName},
	}
	if req.Resume {
		resources = append(resources,
			struct{ source, name string }{filepath.Join(s.vmRel(req.Name), jailedMemName), jailedMemName},
			struct{ source, name string }{filepath.Join(s.vmRel(req.Name), jailedStateName), jailedStateName},
		)
	}
	for _, resource := range resources {
		if err := s.linkStateResource(root, uid, resource.source, resource.name); err != nil {
			return err
		}
	}
	return nil
}

func copyExecutable(source, destination string) (retErr error) {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open Firecracker executable: %w", err)
	}
	defer in.Close() //nolint:errcheck
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("Firecracker executable is not a regular file")
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
	if err != nil {
		return fmt.Errorf("create jailed Firecracker executable: %w", err)
	}
	defer func() {
		if err := out.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(0o555)
}

func cloneDevice(source, destination string, uid int) error {
	var st unix.Stat_t
	if err := unix.Stat(source, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR {
		return fmt.Errorf("invalid jail device %s", source)
	}
	if err := unix.Mknod(destination, unix.S_IFCHR|0o600, int(st.Rdev)); err != nil {
		return fmt.Errorf("create jail device %s: %w", destination, err)
	}
	if err := os.Chown(destination, uid, uid); err != nil {
		return fmt.Errorf("own jail device %s: %w", destination, err)
	}
	return nil
}

func (s *server) linkTrustedResource(root, source, name string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("stat jail resource: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("jail resource is not a regular file")
	}
	if err := os.Chmod(source, 0o444); err != nil {
		return fmt.Errorf("make kernel readable: %w", err)
	}
	if err := os.Link(source, filepath.Join(root, name)); err != nil {
		return fmt.Errorf("link jail resource: %w", err)
	}
	return nil
}

// linkStateResource resolves the controller-owned source beneath the fixed VM
// state root with openat2. RESOLVE_BENEATH|NO_SYMLINKS is the critical part:
// even a compromised controller racing parent-directory symlinks cannot turn
// this helper's chown/chmod/link operations into access outside its state tree.
func (s *server) linkStateResource(root string, uid int, source, name string) error {
	fd, err := s.openState(source, unix.O_RDWR)
	if err != nil {
		return fmt.Errorf("open jail resource: %w", err)
	}
	defer unix.Close(fd) //nolint:errcheck
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("jail resource is not a regular file")
	}
	if err := unix.Fchown(fd, uid, s.opts.ControllerGID); err != nil {
		return fmt.Errorf("own jail resource: %w", err)
	}
	if err := unix.Fchmod(fd, 0o660); err != nil {
		return fmt.Errorf("protect jail resource: %w", err)
	}
	if err := unix.Linkat(fd, "", unix.AT_FDCWD, filepath.Join(root, name), unix.AT_EMPTY_PATH); err != nil {
		return fmt.Errorf("link jail resource: %w", err)
	}
	return nil
}

func (s *server) openState(name string, flags uint64) (int, error) {
	return unix.Openat2(s.stateFD, name, &unix.OpenHow{
		Flags: flags | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH |
			unix.RESOLVE_NO_MAGICLINKS |
			unix.RESOLVE_NO_SYMLINKS,
	})
}

func (s *server) prepareSnapshotOutputs(req Request) error {
	s.mu.Lock()
	active, ok := s.active[req.Slot]
	s.mu.Unlock()
	if !ok || active.name != req.Name || active.pid == 0 {
		return errors.New("VM slot is not active for that name")
	}
	uid := s.jailUID(req.Slot)
	root := s.jailRoot(req.Slot)
	dirFD, err := s.openState(s.vmRel(req.Name), unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("open VM directory: %w", err)
	}
	defer unix.Close(dirFD) //nolint:errcheck
	for _, name := range s.snapshotOutputNames() {
		guest := filepath.Join(root, name)
		unix.Unlinkat(dirFD, name, 0) //nolint:errcheck
		os.Remove(guest)              //nolint:errcheck
		fd, err := unix.Openat(dirFD, name,
			unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o660)
		if err != nil {
			return err
		}
		if err := unix.Fchown(fd, uid, s.opts.ControllerGID); err != nil {
			unix.Close(fd) //nolint:errcheck
			return err
		}
		if err := unix.Linkat(fd, "", unix.AT_FDCWD, guest, unix.AT_EMPTY_PATH); err != nil {
			unix.Close(fd) //nolint:errcheck
			return err
		}
		if err := unix.Close(fd); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) cpuTime(req Request) (uint64, error) {
	s.mu.Lock()
	active, ok := s.active[req.Slot]
	s.mu.Unlock()
	if !ok || active.name != req.Name || active.pid == 0 {
		return 0, errors.New("VM slot is not active for that name")
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(active.pid), "stat"))
	if err != nil {
		return 0, err
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return 0, fmt.Errorf("malformed %s proc stat", s.vmmName())
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	if len(fields) < 13 {
		return 0, fmt.Errorf("short %s proc stat", s.vmmName())
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return (utime + stime) * uint64(time.Second) / userHZ, nil
}

func (s *server) createTap(ctx context.Context, slot int) error {
	tap := tapName(slot)
	guestSlot, _ := s.network.Slot(slot)
	commands := [][]string{
		{"ip", "tuntap", "add", "dev", tap, "mode", "tap"},
		{"ip", "addr", "add", guestSlot.Host.String() + "/30", "dev", tap},
	}
	if s.prefix6 != nil {
		commands = append(commands, []string{"ip", "-6", "addr", "add", s.hostIP6(slot) + "/127", "dev", tap})
	}
	commands = append(commands, []string{"ip", "link", "set", "dev", tap, "up"})
	for _, command := range commands {
		if out, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w: %s", command, err, out)
		}
	}
	if s.opts.RestrictInternalEgress {
		if err := s.installTapSourceRules(ctx, slot); err != nil {
			return err
		}
	}
	exec.CommandContext(ctx, "sysctl", "-qw", "net.ipv4.conf."+tap+".rp_filter=1").Run() //nolint:errcheck
	if s.prefix6 != nil && s.uplink6 != "" {
		exec.CommandContext(ctx, "sysctl", "-qw", "net.ipv6.conf."+s.uplink6+".proxy_ndp=1").Run()              //nolint:errcheck
		exec.CommandContext(ctx, "ip", "-6", "neigh", "del", "proxy", s.guestIP6(slot), "dev", s.uplink6).Run() //nolint:errcheck
		exec.CommandContext(ctx, "ip", "-6", "neigh", "add", "proxy", s.guestIP6(slot), "dev", s.uplink6).Run() //nolint:errcheck
	}
	return nil
}

func (s *server) cleanupSlot(slot int) error {
	if s.opts.RestrictInternalEgress {
		s.removeTapSourceRules(slot)
	}
	if s.prefix6 != nil && s.uplink6 != "" {
		exec.Command("ip", "-6", "neigh", "del", "proxy", s.guestIP6(slot), "dev", s.uplink6).Run() //nolint:errcheck
	}
	exec.Command("ip", "link", "del", tapName(slot)).Run() //nolint:errcheck
	return os.RemoveAll(s.jailWorkspace(slot))
}

type packetFilterRule struct {
	binary string
	chain  string
	args   []string
}

// validateRestrictedEgress makes the helper fail closed if its entrypoint did
// not install the immutable base chains. The helper adds only slot-derived
// anti-spoof rules; the controller cannot choose an interface, source, chain,
// or arbitrary iptables argument through the Unix protocol.
func (s *server) validateRestrictedEgress(ctx context.Context) error {
	checks := []packetFilterRule{
		{binary: "iptables", chain: "FORWARD", args: []string{"-i", "sbtap+", "-j", guestOutChain}},
		{binary: "iptables", chain: "FORWARD", args: []string{"-o", "sbtap+", "-j", guestInChain}},
		{binary: "iptables", chain: "INPUT", args: []string{"-i", "sbtap+", "-j", guestHostChain}},
	}
	if s.prefix6 != nil {
		checks = append(checks,
			packetFilterRule{binary: "ip6tables", chain: "FORWARD", args: []string{"-i", "sbtap+", "-j", guestOutChain}},
			packetFilterRule{binary: "ip6tables", chain: "FORWARD", args: []string{"-o", "sbtap+", "-j", guestInChain}},
			packetFilterRule{binary: "ip6tables", chain: "INPUT", args: []string{"-i", "sbtap+", "-j", guestHostChain}},
		)
	}
	for _, check := range checks {
		args := append([]string{"-w", "-C", check.chain}, check.args...)
		if out, err := exec.CommandContext(ctx, check.binary, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("required restricted-egress rule missing (%s %s): %w: %s", check.binary, strings.Join(args, " "), err, out)
		}
	}
	return nil
}

func (s *server) tapSourceRules(slot int) []packetFilterRule {
	tap := tapName(slot)
	guestSlot, _ := s.network.Slot(slot)
	rules := []packetFilterRule{{
		binary: "iptables",
		chain:  guestOutChain,
		args:   []string{"-i", tap, "!", "-s", guestSlot.Guest.String() + "/32", "-j", "DROP"},
	}}
	if s.prefix6 != nil {
		rules = append(rules, packetFilterRule{
			binary: "ip6tables",
			chain:  guestOutChain,
			args:   []string{"-i", tap, "!", "-s", s.guestIP6(slot) + "/128", "-j", "DROP"},
		})
	}
	return rules
}

func (s *server) installTapSourceRules(ctx context.Context, slot int) error {
	for _, rule := range s.tapSourceRules(slot) {
		checkArgs := append([]string{"-w", "-C", rule.chain}, rule.args...)
		if exec.CommandContext(ctx, rule.binary, checkArgs...).Run() == nil {
			continue
		}
		insertArgs := append([]string{"-w", "-I", rule.chain, "1"}, rule.args...)
		if out, err := exec.CommandContext(ctx, rule.binary, insertArgs...).CombinedOutput(); err != nil {
			return fmt.Errorf("pin %s source for slot %d: %w: %s", rule.binary, slot, err, out)
		}
	}
	return nil
}

func (s *server) removeTapSourceRules(slot int) {
	for _, rule := range s.tapSourceRules(slot) {
		deleteArgs := append([]string{"-w", "-D", rule.chain}, rule.args...)
		exec.Command(rule.binary, deleteArgs...).Run() //nolint:errcheck
	}
}

func (s *server) sweepStaleTaps() {
	out, err := exec.Command("ip", "-o", "link", "show").Output()
	if err != nil {
		return
	}
	validTap := regexp.MustCompile(`^sbtap[0-9]+$`)
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(line, ": ", 3)
		if len(parts) < 2 {
			continue
		}
		name := strings.SplitN(parts[1], "@", 2)[0]
		if validTap.MatchString(name) {
			exec.Command("ip", "link", "del", name).Run() //nolint:errcheck
		}
	}
}

// vmRel is the per-VM directory beneath VMStateDir, and it must agree with the
// driver's: internal/vmm/firecracker uses fc-vms, internal/vmm/qemu uses
// qemu-vms, and the two layouts are deliberately disjoint so one VMM's node
// cannot half-read the other's sandboxes.
func (s *server) vmRel(name string) string {
	if s.qemu() {
		return filepath.Join("qemu-vms", name)
	}
	return filepath.Join("fc-vms", name)
}

func (s *server) jailUID(slot int) int { return s.opts.JailerUIDBase + slot }

func (s *server) jailWorkspace(slot int) string {
	dir := filepath.Base(s.opts.FirecrackerBin)
	if s.qemu() {
		dir = qemuJailDir
	}
	return filepath.Join(s.opts.ChrootBase, dir, fmt.Sprintf("sparkbox-%d", slot))
}

// vmmName is what this helper calls its VMM in an error a human will read.
func (s *server) vmmName() string {
	if s.qemu() {
		return "QEMU"
	}
	return "Firecracker"
}

// snapshotOutputNames is every file a snapshot of this backend writes. QEMU's
// migrate produces exactly one where Firecracker produces a pair, and a
// predicate lifted across that difference matches nothing and silently stops
// refusing what it exists to refuse.
func (s *server) snapshotOutputNames() []string {
	if s.qemu() {
		return []string{jailedSnapshotName + ".next"}
	}
	return []string{jailedMemName + ".next", jailedStateName + ".next"}
}

// launchSocketName is the VMM's control socket inside the jail: Firecracker's
// REST API, or QEMU's QMP monitor. publishSocket shares whichever it is with
// the controller group, and the driver derives the same path to dial it.
func (s *server) launchSocketName() string {
	if s.qemu() {
		return jailedQMPSocketName
	}
	return jailedSocketName
}

func (s *server) jailRoot(slot int) string { return filepath.Join(s.jailWorkspace(slot), "root") }

func tapName(slot int) string { return fmt.Sprintf("sbtap%d", slot) }

func (s *server) addr6(offset int) string {
	ip := make(net.IP, net.IPv6len)
	copy(ip, s.prefix6)
	binary.BigEndian.PutUint32(ip[12:], uint32(offset))
	return ip.String()
}

func (s *server) hostIP6(slot int) string  { return s.addr6((slot + 1) * 2) }
func (s *server) guestIP6(slot int) string { return s.addr6((slot+1)*2 + 1) }

func defaultRoute6Dev() string {
	out, err := exec.Command("ip", "-6", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, field := range fields {
		if field == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
