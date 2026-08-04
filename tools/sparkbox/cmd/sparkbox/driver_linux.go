//go:build linux

package main

import (
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	fcdriver "github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/firecracker"
)

func newFirecrackerDriver(
	kernelPath, imageDir, vmStateDir, jailerBin, jailerChrootBase string,
	jailerUIDBase int, chrootJailer, disableHostRootfsMounts bool,
	guestSubnet, subnet6, loginUser, guestDNS string,
) (vmm.Driver, error) {
	return fcdriver.New(fcdriver.Options{
		KernelPath: kernelPath, ImageDir: imageDir, VMStateDir: vmStateDir,
		JailerBin: jailerBin, ChrootJailer: chrootJailer,
		JailerChrootBase: jailerChrootBase, JailerUIDBase: jailerUIDBase,
		DisableHostRootfsMounts: disableHostRootfsMounts,
		Subnet:                  guestSubnet, Subnet6: subnet6, LoginUser: loginUser, GuestDNS: guestDNS,
	})
}
