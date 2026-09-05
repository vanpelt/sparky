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

// newQemuDriver builds the QEMU backend. Its argument list is deliberately
// shorter than the firecracker one rather than a copy of it: the jailer,
// chroot-jailer, privileged-helper and jailer-uid arguments have no counterpart
// in qemu.Options, and passing them here so the two calls "look the same" would
// mean silently dropping four flags an operator believed they had set. The
// switch in serve/runNode rejects that combination explicitly instead.
func newQemuDriver(
	kernelPath, imageDir, templateDir, vmStateDir string,
	qemuBin, machineType string,
	disableHostRootfsMounts bool,
	guestSubnet, subnet6, loginUser, guestDNS string,
) (vmm.Driver, error) {
	return qemudriver.New(qemudriver.Options{
		KernelPath: kernelPath, ImageDir: imageDir, TemplateDir: templateDir, VMStateDir: vmStateDir,
		QemuBin: qemuBin, MachineType: machineType,
		DisableHostRootfsMounts: disableHostRootfsMounts,
		Subnet:                  guestSubnet, Subnet6: subnet6, LoginUser: loginUser, GuestDNS: guestDNS,
	})
}
