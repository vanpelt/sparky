//go:build linux

package main

import (
	fcdriver "github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/firecracker"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func newFirecrackerDriver(kernelPath, imageDir, stateDir, subnet6, loginUser string) (vmm.Driver, error) {
	return fcdriver.New(fcdriver.Options{
		KernelPath: kernelPath, ImageDir: imageDir, StateDir: stateDir,
		Subnet6: subnet6, LoginUser: loginUser,
	})
}
