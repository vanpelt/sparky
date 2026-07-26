package hostsetup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
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
	// Sysctl reads a sysctl value by dotted key. On linux that is a /proc/sys
	// path (net.ipv4.ip_forward); on darwin it is a real sysctl name
	// (kern.osproductversion, machdep.cpu.brand_string). Same interface, two
	// namespaces — which is right, because the checks that read it are
	// platform-specific in either direction.
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
	// Sleep waits. It is on the Probe — the one thing a check is allowed to
	// touch the outside world with — because the service liveness check has to
	// sample the unit twice with a gap in between, and a test must be able to
	// drive that gap without waiting one out. Config.ServiceSettle carries the
	// duration; a zero duration means the check never calls this at all.
	Sleep(d time.Duration)
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

// Sysctl reads a sysctl value. The body is per-platform (see probe_linux.go /
// probe_darwin.go / probe_other.go) because linux exposes these as /proc/sys
// files and darwin exposes them through the sysctl syscall, but the CONTRACT is
// one dotted key in and one trimmed value out.
func (sysProbe) Sysctl(key string) (string, error) { return sysctlRead(key) }

func (sysProbe) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	// Wrap a non-zero exit so a caller that cares can read the CODE, while
	// every existing caller — all of which only test err != nil — is unchanged.
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		err = &ExitError{Code: ee.ExitCode()}
	}
	return string(bytes.TrimSpace(out)), err
}

// ExitError carries a command's exit status out of Probe.Run.
//
// Additive: Probe.Run still returns a plain error for every existing caller,
// which all test only `err != nil`. It exists because the darwin path has to
// tell "this command reported a failure" (an inner doctor exiting 1, which is a
// verdict) from "this command could not be run at all" (which is not).
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

func (sysProbe) Sleep(d time.Duration) { time.Sleep(d) }
