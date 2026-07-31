//go:build linux

package main

import (
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	fcdriver "github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/firecracker"
)

func newFirecrackerDriver(
	kernelPath, imageDir, vmStateDir, jailerBin, jailerChrootBase string,
	jailerUIDBase int,
	guestSubnet, subnet6, loginUser, guestDNS string,
) (vmm.Driver, error) {
	return fcdriver.New(fcdriver.Options{
		KernelPath: kernelPath, ImageDir: imageDir, VMStateDir: vmStateDir,
		JailerBin: jailerBin, JailerChrootBase: jailerChrootBase, JailerUIDBase: jailerUIDBase,
		Subnet: guestSubnet, Subnet6: subnet6, LoginUser: loginUser, GuestDNS: guestDNS,
	})
}
