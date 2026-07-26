//go:build !linux && !darwin

package hostsetup

import "fmt"

// DiskFreeBytes is unsupported on the platforms sparkbox neither runs on nor
// provisions from (linux is the host, darwin provisions one); return 0 so the
// disk check degrades to a WARN on a dev machine rather than failing the build.
func (sysProbe) DiskFreeBytes(path string) (uint64, error) { return 0, nil }

// sysctlRead has no meaning here. The error text says which platforms do have
// one, because the checks that call it (ip_forward on linux, the macOS product
// version on darwin) report "unknown" and would otherwise be a mystery.
func sysctlRead(key string) (string, error) {
	return "", fmt.Errorf("sysctl %q: only implemented on linux (/proc/sys) and darwin (sysctl by name)", key)
}
