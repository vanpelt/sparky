//go:build linux

package main

import (
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	fcdriver "github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/firecracker"
	qemudriver "github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/qemu"
)

func newFirecrackerDriver(
	kernelPath, imageDir, templateDir, vmStateDir, jailerBin, jailerChrootBase string,
	jailerUIDBase int, chrootJailer bool,
	privilegedHelperSocket, privilegedHelperBin string, helperControllerGID int,
	disableHostRootfsMounts bool,
	guestSubnet, subnet6, loginUser, guestDNS string,
) (vmm.Driver, error) {
	return fcdriver.New(fcdriver.Options{
		KernelPath: kernelPath, ImageDir: imageDir, TemplateDir: templateDir, VMStateDir: vmStateDir,
		JailerBin: jailerBin, ChrootJailer: chrootJailer,
		JailerChrootBase: jailerChrootBase, JailerUIDBase: jailerUIDBase,
		PrivilegedHelperSocket:  privilegedHelperSocket,
		PrivilegedHelperBin:     privilegedHelperBin,
		HelperControllerGID:     helperControllerGID,
		DisableHostRootfsMounts: disableHostRootfsMounts,
		Subnet:                  guestSubnet, Subnet6: subnet6, LoginUser: loginUser, GuestDNS: guestDNS,
	})
}

// newQemuDriver builds the QEMU backend. Its argument list is still shorter
// than the firecracker one rather than a copy of it, and the gap is now exactly
// two: --jailer and --chroot-jailer describe how Firecracker is confined by its
// parent, which QEMU cannot be asked to honour because it confines itself from
// its own argv. qemu.Options has no field for them, and passing them here so
// the two calls "look the same" would mean silently dropping flags an operator
// believed they had set. The switch in serve/runNode rejects them explicitly.
//
// jailerUIDBase is likewise absent, and that one is not a gap: the per-slot uid
// is chosen by the helper and written into the argv it builds, so the
// controller never needs to know it.
func newQemuDriver(
	kernelPath, imageDir, templateDir, vmStateDir string,
	qemuBin, machineType string,
	privilegedHelperSocket, privilegedHelperBin string, helperControllerGID int,
	jailerChrootBase string,
	disableHostRootfsMounts bool,
	guestSubnet, subnet6, loginUser, guestDNS string,
) (vmm.Driver, error) {
	return qemudriver.New(qemudriver.Options{
		KernelPath: kernelPath, ImageDir: imageDir, TemplateDir: templateDir, VMStateDir: vmStateDir,
		QemuBin: qemuBin, MachineType: machineType,
		PrivilegedHelperSocket: privilegedHelperSocket,
		PrivilegedHelperBin:    privilegedHelperBin,
		HelperControllerGID:    helperControllerGID,
		JailerChrootBase:       jailerChrootBase,

		DisableHostRootfsMounts: disableHostRootfsMounts,
		Subnet:                  guestSubnet, Subnet6: subnet6, LoginUser: loginUser, GuestDNS: guestDNS,
	})
}
