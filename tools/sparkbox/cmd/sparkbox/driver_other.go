//go:build !linux

package main

import (
	"fmt"
	"runtime"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func newFirecrackerDriver(kernelPath, imageDir, vmStateDir, guestSubnet, subnet6, loginUser, guestDNS string) (vmm.Driver, error) {
	return nil, fmt.Errorf("the firecracker driver requires a Linux host with KVM (this is %s); use --driver mock for local development", runtime.GOOS)
}
