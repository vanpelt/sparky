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
	guestSubnet, subnet6, loginUser, guestDNS, blockIOEngine string,
) (vmm.Driver, error) {
	return nil, fmt.Errorf("the firecracker driver requires a Linux host with KVM (this is %s); use --driver mock for local development", runtime.GOOS)
}
