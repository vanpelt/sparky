//go:build linux

// Package hoststat reads what the host kernel knows about a running guest: the
// VMM process's CPU time from /proc, and its tap device's byte counters from
// sysfs.
//
// Neither reading involves the VMM. Both drivers implement vmm.CPUStatser and
// vmm.NetStatser out of exactly these two functions, and before this package
// they held byte-identical copies of them — with the ONE unit test for
// parseCPUTicks's awkward case (a process whose comm field contains a
// ')' , which is why the parser scans for the LAST one) covering only the
// firecracker copy.
package hoststat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// userHZ is the fixed unit /proc/<pid>/stat reports utime/stime in (10ms
// ticks). It is part of the kernel's userspace ABI, constant regardless of
// the kernel's CONFIG_HZ.
const userHZ = 100

// parseCPUTicks sums the utime and stime fields (14 and 15) of a
// /proc/<pid>/stat line. The comm field may itself contain spaces and ')',
// so fields are counted from the last ')' rather than split naively.
func parseCPUTicks(stat string) (uint64, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed stat line %q", stat)
	}
	fields := strings.Fields(stat[i+1:])
	// fields[0] is field 3 (state), so utime/stime land at indices 11/12.
	if len(fields) < 13 {
		return 0, fmt.Errorf("stat line has %d fields after comm, want >= 13", len(fields))
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("utime: %w", err)
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("stime: %w", err)
	}
	return utime + stime, nil
}

// TapCounter reads one of a tap device's sysfs byte counters.
func TapCounter(tap, stat string) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/statistics/%s", tap, stat))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s/%s: %w", tap, stat, err)
	}
	return n, nil
}

// CPUNanos is the cumulative utime+stime of a VMM process, in nanoseconds.
// This measures the WHOLE process — vCPU threads plus emulation and I/O
// overhead — so callers should surface it as "host CPU", not guest CPU.
func CPUNanos(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	ticks, err := parseCPUTicks(string(data))
	if err != nil {
		return 0, err
	}
	return ticks * (1_000_000_000 / userHZ), nil
}
