package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmhelper"
)

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)
	if len(os.Args) < 2 {
		logger.Fatal("usage: sparkbox-vmm-helper serve|launch|ping")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, logger, os.Args[2:])
	case "launch":
		err = launch(ctx, os.Args[2:])
	case "ping":
		err = ping(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		logger.Fatal(err)
	}
}

func serve(ctx context.Context, logger *log.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	socket := fs.String("socket", "", "Unix socket exposed to the unprivileged controller")
	backend := fs.String("backend", "", "VMM this helper launches: firecracker (default) or qemu")
	firecracker := fs.String("firecracker", "", "fixed Firecracker executable")
	qemuBin := fs.String("qemu-bin", "", "fixed QEMU system emulator, for --backend qemu")
	machineType := fs.String("machine-type", "", "qemu -M machine type; must be the versioned type the controller uses, because the migration stream is bound to it")
	kernel := fs.String("kernel", "", "fixed guest kernel")
	vmState := fs.String("vm-state-dir", "", "per-VM state root")
	chrootBase := fs.String("chroot-base", "", "per-slot chroot root")
	subnet := fs.String("subnet", "", "guest IPv4 subnet")
	subnet6 := fs.String("subnet6", "", "optional guest IPv6 subnet")
	restrictInternalEgress := fs.Bool("restrict-internal-egress", false, "require restricted guest packet-filter chains and per-TAP anti-spoofing")
	sluiceSocket := fs.String("sluice-socket", "", "optional Sluice socket that must confirm TAP enforcement before VMM launch")
	uidBase := fs.Int("jailer-uid-base", 100000, "first per-slot VMM UID")
	controllerUID := fs.Int("controller-uid", 65532, "only UID accepted on the helper socket")
	controllerGID := fs.Int("controller-gid", 65532, "group allowed to access VMM files and sockets")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Parsed here rather than inside RunServer so an unknown value is a startup
	// error with the flag's name in it, not a helper that silently comes up as
	// Firecracker on a node the operator meant to run QEMU.
	vmmBackend, err := vmhelper.ParseBackend(*backend)
	if err != nil {
		return err
	}
	return vmhelper.RunServer(ctx, vmhelper.ServerOptions{
		SocketPath: *socket, Backend: vmmBackend,
		QemuBin: *qemuBin, MachineType: *machineType,
		FirecrackerBin: *firecracker, KernelPath: *kernel,
		VMStateDir: *vmState, ChrootBase: *chrootBase, Subnet: *subnet, Subnet6: *subnet6,
		RestrictInternalEgress: *restrictInternalEgress,
		SluiceSocket:           *sluiceSocket,
		JailerUIDBase:          *uidBase, ControllerUID: *controllerUID, ControllerGID: *controllerGID,
		Logger: logger,
	})
}

func launch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	socket := fs.String("socket", "", "helper Unix socket")
	name := fs.String("name", "", "validated VM name")
	slot := fs.Int("slot", -1, "validated VM network slot")
	resume := fs.Bool("resume", false, "stage snapshot inputs")
	// The machine. A QEMU helper requires all three and a Firecracker one
	// ignores them; neither is decided here, because this client does not know
	// which backend it is talking to and must not start guessing.
	vcpus := fs.Int64("vcpus", 0, "guest vCPU count (qemu backend)")
	memMB := fs.Int64("mem-mb", 0, "guest memory in MiB (qemu backend)")
	cmdline := fs.String("cmdline", "", "guest kernel command line (qemu backend)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return vmhelper.RunLaunchClient(ctx, vmhelper.Launch{
		Socket: *socket, Name: *name, Slot: *slot, Resume: *resume,
		VCPUs: *vcpus, MemMB: *memMB, Cmdline: *cmdline,
	})
}

func ping(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	socket := fs.String("socket", "", "helper Unix socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return vmhelper.Ping(ctx, *socket)
}
