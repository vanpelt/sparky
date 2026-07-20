//go:build !linux

package hostsetup

// DiskFreeBytes is unsupported off Linux (sparkbox only runs on Linux hosts);
// return 0 so the disk check degrades to a WARN on a dev machine rather than
// failing the build.
func (sysProbe) DiskFreeBytes(path string) (uint64, error) { return 0, nil }
