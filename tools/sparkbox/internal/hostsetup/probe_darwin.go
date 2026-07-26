//go:build darwin

package hostsetup

import (
	"strings"

	"golang.org/x/sys/unix"
)

// DiskFreeBytes reports free space on the filesystem backing path.
//
// It used to return (0, nil) here — the "sparkbox only runs on Linux" stub —
// which made the disk check WARN "unknown" on every Mac. On a darwin host the
// answer matters more than on a linux one, not less: the machine's whole data
// volume is a file on this filesystem, so running out of room here is how a
// gateway fills up.
func (sysProbe) DiskFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// sysctlRead reads a real darwin sysctl by name — no subprocess, no parsing of
// somebody's prose.
//
// The two keys that matter are kern.osproductversion ("26.5.2") and
// machdep.cpu.brand_string ("Apple M4 Max"). Note it is NOT kern.osrelease:
// that is the Darwin version (25.5.0 on the same machine), which is neither the
// macOS product version nor comparable with it.
func sysctlRead(key string) (string, error) {
	v, err := unix.Sysctl(key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}
