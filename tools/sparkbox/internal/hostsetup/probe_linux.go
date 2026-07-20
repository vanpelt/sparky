//go:build linux

package hostsetup

import "golang.org/x/sys/unix"

// DiskFreeBytes reports the free space on the filesystem backing path via
// statfs. Used by the disk check and never on the hot path, so the syscall
// cost is irrelevant.
func (sysProbe) DiskFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
