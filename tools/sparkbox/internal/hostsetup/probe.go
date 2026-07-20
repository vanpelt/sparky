package hostsetup

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Probe abstracts every read-only interaction a check makes with the host, so
// checks are pure functions of a Probe and tests can substitute an in-memory
// fake instead of standing up a real KVM box. The system implementation is
// sysProbe; tests use fakeProbe (checks_test.go).
type Probe interface {
	GOOS() string
	GOARCH() string
	// Uid is the effective uid (0 == root). -1 off platforms without it.
	Uid() int
	// Stat reports whether a path exists (and what it is).
	Stat(path string) (os.FileInfo, error)
	// Writable reports whether the path can be opened for writing.
	Writable(path string) bool
	// ReadFile reads a small file (e.g. /proc/cpuinfo, users.conf).
	ReadFile(path string) ([]byte, error)
	// Sysctl reads a sysctl value by dotted key (net.ipv4.ip_forward).
	Sysctl(key string) (string, error)
	// LookPath resolves a binary on PATH.
	LookPath(bin string) (string, error)
	// Run executes a command and returns its combined output. A non-zero exit
	// is returned as an error, but the output is still returned — callers that
	// tolerate failure (e.g. `systemctl is-active` on an inactive unit) read
	// the output regardless.
	Run(name string, args ...string) (string, error)
	// DiskFreeBytes reports free space on the filesystem backing path.
	DiskFreeBytes(path string) (uint64, error)
}

// sysProbe is the real, host-backed Probe.
type sysProbe struct{}

// System returns a Probe backed by the real host.
func System() Probe { return sysProbe{} }

func (sysProbe) GOOS() string   { return runtime.GOOS }
func (sysProbe) GOARCH() string { return runtime.GOARCH }
func (sysProbe) Uid() int       { return os.Geteuid() }

func (sysProbe) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }

func (sysProbe) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (sysProbe) LookPath(bin string) (string, error) { return exec.LookPath(bin) }

func (sysProbe) Writable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// Sysctl reads the runtime value from /proc/sys, translating the dotted key to
// its path (net.ipv4.ip_forward -> /proc/sys/net/ipv4/ip_forward).
func (sysProbe) Sysctl(key string) (string, error) {
	b, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(key, ".", "/"))
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(b)), nil
}

func (sysProbe) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(bytes.TrimSpace(out)), err
}
