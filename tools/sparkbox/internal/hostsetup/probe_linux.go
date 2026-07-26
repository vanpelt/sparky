//go:build linux

package hostsetup

import (
	"bytes"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

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

// sysctlRead reads the runtime value from /proc/sys, translating the dotted key
// to its path (net.ipv4.ip_forward -> /proc/sys/net/ipv4/ip_forward). Moved out
// of probe.go unchanged when darwin grew a sysctl of its own.
func sysctlRead(key string) (string, error) {
	b, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(key, ".", "/"))
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(b)), nil
}
