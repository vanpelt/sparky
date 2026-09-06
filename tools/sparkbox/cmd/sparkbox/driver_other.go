//go:build !linux

package main

import (
	"fmt"
	"runtime"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func newFirecrackerDriver(
	kernelPath, imageDir, templateDir, vmStateDir, jailerBin, jailerChrootBase string,
	jailerUIDBase int, chrootJailer bool,
	privilegedHelperSocket, privilegedHelperBin string, helperControllerGID int,
	disableHostRootfsMounts bool,
	guestSubnet, subnet6, loginUser, guestDNS string,
) (vmm.Driver, error) {
	return nil, fmt.Errorf("the firecracker driver requires a Linux host with KVM (this is %s); use --driver mock for local development", runtime.GOOS)
}

// Kept signature-for-signature with the Linux definition. That is not
// cosmetic: this file is the only thing that compiles on a Mac, so a parameter
// added to one and not the other is a build break nobody sees until CI's darwin
// job runs — which is exactly how it was found.
func newQemuDriver(
	kernelPath, imageDir, templateDir, vmStateDir string,
	qemuBin, machineType string,
	privilegedHelperSocket, privilegedHelperBin string, helperControllerGID int,
	jailerChrootBase string,
	disableHostRootfsMounts bool,
	guestSubnet, subnet6, loginUser, guestDNS string,
) (vmm.Driver, error) {
	return nil, fmt.Errorf("the qemu driver requires a Linux host with KVM (this is %s); use --driver mock for local development", runtime.GOOS)
}
