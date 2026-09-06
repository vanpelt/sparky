//go:build linux

package vmhelper

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/qemuargs"
)

// The QEMU half of the privileged helper.
//
// # WHY THIS IS NOT THE FIRECRACKER PATH WITH A DIFFERENT BINARY
//
// The Firecracker path confines the child from the PARENT: prepareJail copies
// the executable into a per-slot chroot, mknod's /dev/kvm and /dev/net/tun
// inside it, and launch hands the child a SysProcAttr carrying Chroot and
// Credential, so Firecracker is unprivileged from its very first instruction.
// That works because Firecracker opens everything it needs through its API
// socket, after it is already inside.
//
// QEMU cannot be started that way. It must open /dev/kvm, the tap, the kernel
// and the rootfs with the caller's privileges while it builds the machine, and
// only then drop — which QEMU does itself, in os_setup_post(), from flags on
// its own command line. So this path builds no device nodes, copies no
// executable, and sets no SysProcAttr; it passes `-run-with chroot=`,
// `-runas <uid>:<uid>` and `-sandbox on` instead, and the resulting process was
// MEASURED on the production x86_64 CKS node to reach the same posture the
// Firecracker launcher reaches: uid and gid 100000, CapEff and CapPrm zero,
// NoNewPrivs 1, Seccomp 2, and a path outside the jail resolving to ENOENT.
//
// The window this leaves is real and worth naming: between execve and
// os_setup_post, QEMU runs as root with this helper's capabilities. Kata
// Containers runs QEMU the same way for the same reason. It is a smaller window
// than the alternative on offer, which was running the VMM as the controller
// with no confinement at all.
//
// # WHY EVERY PER-VM PATH IN THE ARGV IS RELATIVE
//
// Two different moments resolve paths on this command line. -kernel, -drive,
// -qmp, -serial and -incoming are opened during startup, BEFORE the chroot; the
// runtime `migrate uri=file:` that Pause issues over QMP is resolved from the
// monitor long AFTER it. Absolute host paths would work for the first group and
// fail for the second, and the failure would be a pause that breaks on a
// sandbox that booted perfectly.
//
// Relative names make the question moot. This helper sets the child's working
// directory to the jail root, and QEMU's change_root() does chroot(dir) then
// chdir("/"), so "rootfs.ext4" names the same file before and after.

const (
	jailedQMPSocketName = "qmp.sock"
	// jailedSnapshotName is the whole memory snapshot: QEMU's migrate produces
	// exactly one file where Firecracker produces mem.snap + state.snap. It
	// must match internal/vmm/qemu's snapshotName.
	jailedSnapshotName  = "state.migrate"
	jailedSerialLogName = "serial.log"
	// jailedVMMLogName holds the child's stdout and stderr. It is NOT created
	// in the jail: cleanupSlot removes the whole jail workspace at every VMM
	// exit, which is exactly when a failed boot's diagnostic is wanted, so this
	// one lives in the controller-visible VM directory and the child is handed
	// the open descriptor.
	jailedVMMLogName = "qemu.log"
	// qemuJailDir is a FIXED path component, where the Firecracker jail uses
	// filepath.Base(FirecrackerBin). The controller has to derive the QMP
	// socket path to dial it, and deriving it from a binary name would mean the
	// unprivileged container needs the emulator's path just to build a string —
	// and would silently break the day the two containers disagree about it.
	qemuJailDir = "qemu"
)

func (s *server) qemu() bool { return s.opts.Backend == BackendQEMU }

// prepareQemuJail stages the per-slot chroot for a QEMU launch. It shares the
// ownership and permission model with prepareJail — root-owned traversable
// base, 0710 jail root, resources owned by the per-slot uid and the controller
// group — and differs only in what goes inside.
func (s *server) prepareQemuJail(req Request) error {
	root := s.jailRoot(req.Slot)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create chroot jail: %w", err)
	}
	uid := s.jailUID(req.Slot)
	if err := os.Chown(root, uid, s.opts.ControllerGID); err != nil {
		return fmt.Errorf("own chroot root: %w", err)
	}
	// The controller group can traverse to the QMP socket but cannot list the
	// jail. Other VMM UIDs cannot traverse at all.
	if err := os.Chmod(root, 0o710); err != nil {
		return fmt.Errorf("protect chroot root: %w", err)
	}
	if err := s.linkTrustedResource(root, s.opts.KernelPath, jailedKernelName); err != nil {
		return err
	}
	resources := []string{jailedRootfsName}
	if req.Resume {
		resources = append(resources, jailedSnapshotName)
	}
	for _, name := range resources {
		if err := s.linkStateResource(root, uid, filepath.Join(s.vmRel(req.Name), name), name); err != nil {
			return err
		}
	}
	// The serial log is created in the VM directory and hardlinked in, for the
	// same reason qemu.go's is: cleanupSlot removes this jail at every exit, so
	// a log QEMU created inside it would be destroyed precisely when a boot
	// failed. QEMU opens it O_TRUNC per launch, so the content is this boot's.
	return s.linkFreshOutput(req, uid, jailedSerialLogName)
}

// linkFreshOutput creates name in the VM directory, gives it to the per-slot
// uid and the controller group, and hardlinks it into the jail — the same
// create-chown-link dance prepareSnapshotOutputs uses, and for the same reason:
// a confined QEMU cannot create a file the controller is able to read, and a
// file the controller creates is not one the confined QEMU can write.
func (s *server) linkFreshOutput(req Request, uid int, name string) error {
	dirFD, err := s.openState(s.vmRel(req.Name), unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("open VM directory: %w", err)
	}
	defer unix.Close(dirFD) //nolint:errcheck
	guest := filepath.Join(s.jailRoot(req.Slot), name)
	unix.Unlinkat(dirFD, name, 0) //nolint:errcheck
	os.Remove(guest)              //nolint:errcheck
	fd, err := unix.Openat(dirFD, name,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o660)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	defer unix.Close(fd) //nolint:errcheck
	if err := unix.Fchown(fd, uid, s.opts.ControllerGID); err != nil {
		return fmt.Errorf("own %s: %w", name, err)
	}
	if err := unix.Linkat(fd, "", unix.AT_FDCWD, guest, unix.AT_EMPTY_PATH); err != nil {
		return fmt.Errorf("link %s into the jail: %w", name, err)
	}
	return nil
}

// openVMMLog returns the child's stdout and stderr: a truncated qemu.log in the
// controller-visible VM directory, owned by the controller group so the driver
// can read it back and quote it in an error.
//
// It is a descriptor rather than a path because it never has to be resolved by
// the child, so it is immune to the chroot entirely — and because the file must
// outlive the jail. With no PL011 driver in the arm64 guest kernel this is the
// only thing a rejected argv or an unloadable migration stream leaves behind.
func (s *server) openVMMLog(req Request) (*os.File, error) {
	fd, err := s.openState(filepath.Join(s.vmRel(req.Name), jailedVMMLogName),
		unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC)
	if err != nil {
		return nil, fmt.Errorf("open vmm log: %w", err)
	}
	if err := unix.Fchown(fd, s.jailUID(req.Slot), s.opts.ControllerGID); err != nil {
		unix.Close(fd) //nolint:errcheck
		return nil, fmt.Errorf("own vmm log: %w", err)
	}
	if err := unix.Fchmod(fd, 0o660); err != nil {
		unix.Close(fd) //nolint:errcheck
		return nil, fmt.Errorf("protect vmm log: %w", err)
	}
	return os.NewFile(uintptr(fd), jailedVMMLogName), nil
}

// qemuCommand builds the whole QEMU invocation for one slot.
//
// The argv comes from internal/vmm/qemuargs, the SAME function the driver's
// direct launcher calls. That is not tidiness: QEMU matches an incoming
// migration stream positionally against the machine the command line describes,
// so a snapshot taken by one builder and restored by another fails on stderr,
// after exec, on a resume, of a sandbox that paused an hour earlier. There is
// one list, and both processes read it.
//
// Everything variable here is either this helper's own startup configuration or
// a validated field of the request. Nothing on the command line is a path or a
// binary the controller chose.
func (s *server) qemuCommand(req Request) (*exec.Cmd, *os.File, error) {
	spec := qemuargs.Spec{
		MachineType: s.opts.MachineType,
		KernelPath:  jailedKernelName,
		Cmdline:     req.Cmdline,
		VCPUs:       req.VCPUs,
		MemMB:       req.MemMB,
		RootfsPath:  jailedRootfsName,
		TapName:     tapName(req.Slot),
		// The MAC comes from guestnet, which the driver also uses, because this
		// process picks it on this path and the driver picks it on the direct
		// one. QEMU takes the MAC from the argv on a RESTORE as well as a cold
		// boot, so a drift between the two would present as a sandbox losing
		// its network on resume — the guest kernel seeing an interface its
		// netcfg hook has never seen.
		MAC:       guestnet.MACFor(req.Slot),
		QMPSocket: jailedQMPSocketName,
		SerialLog: jailedSerialLogName,
		Confine: qemuargs.Confine{
			ChrootDir: s.jailRoot(req.Slot),
			UID:       s.jailUID(req.Slot),
			Sandbox:   true,
		},
	}
	if req.Resume {
		spec.RestoreFrom = jailedSnapshotName
	}
	args, err := qemuargs.Build(spec)
	if err != nil {
		return nil, nil, err
	}
	logFile, err := s.openVMMLog(req)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(s.opts.QemuBin, args...)
	// The working directory is the jail root, which is what makes the relative
	// paths on the argv resolve to the same files before the chroot as after.
	cmd.Dir = s.jailRoot(req.Slot)
	cmd.Env = []string{}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Deliberately NO SysProcAttr. See the file comment: chroot(2) here would
	// take effect before QEMU has opened /dev/kvm or the tap, and setuid(2)
	// here would take away the privilege it needs to open them at all.
	return cmd, logFile, nil
}

// validateQemuOptions checks the startup configuration a QEMU helper needs.
// Kept beside the code that uses it so a new field cannot be added to
// ServerOptions and quietly left unchecked.
func validateQemuOptions(opts ServerOptions) error {
	if !filepath.IsAbs(opts.QemuBin) || filepath.Clean(opts.QemuBin) != opts.QemuBin {
		return errors.New("qemu binary path must be an absolute, clean path")
	}
	if _, err := os.Stat(opts.QemuBin); err != nil {
		return fmt.Errorf("qemu binary: %w", err)
	}
	return nil
}

// defaultMachineType fills in --machine-type from the SAME pin the driver uses
// when neither side was given one, which is the case the deployment actually
// runs: the entrypoints pass an explicit type only when an operator sets
// SPARKBOX_QEMU_MACHINE_TYPE, and then they pass it to both containers.
//
// It matters that this comes from qemuargs rather than from a constant here.
// The controller decides what machine a sandbox is booted with and this process
// decides what machine its snapshot is restored onto; two independently pinned
// defaults would agree right up until one was edited, and the disagreement
// would surface as a resume that cannot load its stream — on a node where
// nothing about the sandbox had changed.
func defaultMachineType(opts *ServerOptions) error {
	if opts.MachineType != "" {
		return nil
	}
	machineType, err := qemuargs.DefaultMachineType()
	if err != nil {
		return fmt.Errorf("qemu helper: %w", err)
	}
	opts.MachineType = machineType
	return nil
}
