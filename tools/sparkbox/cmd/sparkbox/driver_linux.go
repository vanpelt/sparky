//go:build linux

package main

import (
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	fcdriver "github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/firecracker"
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
