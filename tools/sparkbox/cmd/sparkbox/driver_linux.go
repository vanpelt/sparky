//go:build linux

package main

import (
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	fcdriver "github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/firecracker"
)

func newFirecrackerDriver(kernelPath, imageDir, stateDir, subnet6, loginUser, guestDNS string) (vmm.Driver, error) {
	return fcdriver.New(fcdriver.Options{
		KernelPath: kernelPath, ImageDir: imageDir, StateDir: stateDir,
		Subnet6: subnet6, LoginUser: loginUser, GuestDNS: guestDNS,
	})
}
