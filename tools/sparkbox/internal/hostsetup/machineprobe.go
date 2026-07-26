package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
)

// machineProbe implements Probe over machine.Driver, so the check battery that
// already knows how to diagnose a linux gateway can be pointed AT the nested
// one with no new check code at all.
//
// That reuse is the whole point. checkService is A1's two-sample crash-loop
// detector — the thing that stopped `setup` printing a green banner over a
// gateway that had never once stayed up — and it works by asking systemd for
// six properties twice with a gap in between. Reimplementing that against a
// guest would be reimplementing the bug's fix. Instead the sampler runs
// unmodified and its `systemctl show` goes down the exec transport.
//
// A machineProbe is created per verify pass and thrown away: it caches, and a
// cache that outlived the pass would report last run's host.
type machineProbe struct {
	ctx  context.Context
	d    machine.Driver
	name string
	arch string

	// memo caches the READ-ONLY, idempotent lookups: Stat, ReadFile, LookPath
	// and Sysctl. A battery asks for /dev/kvm, users.conf and the kernel
	// version several times over, and each of those is a round trip through a
	// VM.
	//
	// Run is NEVER memoised, and that is not an oversight. showUnit
	// deliberately issues the IDENTICAL `systemctl show` command twice and the
	// entire question it asks is whether the two answers differ; a cache would
	// collapse them into one and silently reinstate exactly the crash-loop
	// blindness A1 removed. TestMachineProbeMemoRules pins both halves.
	memo map[string]memoEntry
}

type memoEntry struct {
	out string
	err error
}

func newMachineProbe(ctx context.Context, d machine.Driver, name, arch string) *machineProbe {
	if arch == "" {
		arch = "arm64"
	}
	return &machineProbe{ctx: ctx, d: d, name: name, arch: arch, memo: map[string]memoEntry{}}
}

// The machine is linux and we always exec as root (--root), so these are facts
// rather than probes.
func (m *machineProbe) GOOS() string   { return "linux" }
func (m *machineProbe) GOARCH() string { return m.arch }
func (m *machineProbe) Uid() int       { return 0 }

// exec runs one guest script and returns its stdout. A non-zero inner exit
// comes back as an error carrying the output, matching Probe.Run's contract
// ("the output is still returned").
func (m *machineProbe) exec(op, script string) (string, error) {
	res, err := m.d.Exec(m.ctx, machine.ExecSpec{
		Machine: m.name, Op: op, Script: script, ReadOnly: true,
		Timeout: 2 * time.Minute,
	})
	out := strings.TrimRight(string(res.Stdout), "\n")
	if err != nil {
		var ee *machine.ExitError
		if errors.As(err, &ee) {
			// A command that ran and failed. Report it the way Probe.Run does,
			// with the code available to anyone who wants it.
			return out, &ExitError{Code: ee.Code}
		}
		return out, err
	}
	return out, nil
}

// memoExec is exec with caching, for the idempotent lookups only.
func (m *machineProbe) memoExec(key, op, script string) (string, error) {
	if e, ok := m.memo[key]; ok {
		return e.out, e.err
	}
	out, err := m.exec(op, script)
	m.memo[key] = memoEntry{out: out, err: err}
	return out, err
}

func (m *machineProbe) Stat(path string) (os.FileInfo, error) {
	q, err := shellQuote(path)
	if err != nil {
		return nil, err
	}
	// %f is the raw mode in hex, %s the size: enough for IsDir() and the
	// non-zero-size checks, in one round trip.
	out, err := m.memoExec("stat:"+path, "probe-stat:"+path, "stat -c '%f %s' "+q)
	if err != nil {
		// Every caller treats a Stat error as "absent", and a guest `stat` on a
		// missing path is exactly that.
		return nil, os.ErrNotExist
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return nil, os.ErrNotExist
	}
	mode, _ := strconv.ParseUint(f[0], 16, 64)
	size, _ := strconv.ParseInt(f[1], 10, 64)
	const sIFMT, sIFDIR = 0o170000, 0o040000
	return guestFileInfo{name: path, size: size, dir: mode&sIFMT == sIFDIR}, nil
}

func (m *machineProbe) Writable(path string) bool {
	q, err := shellQuote(path)
	if err != nil {
		return false
	}
	_, err = m.memoExec("writable:"+path, "probe-writable:"+path, "test -w "+q)
	return err == nil
}

func (m *machineProbe) ReadFile(path string) ([]byte, error) {
	q, err := shellQuote(path)
	if err != nil {
		return nil, err
	}
	out, err := m.memoExec("read:"+path, "probe-read:"+path, "cat "+q)
	if err != nil {
		return nil, os.ErrNotExist
	}
	return []byte(out), nil
}

// Sysctl reads /proc/sys inside the machine — the linux body verbatim, which is
// the point: checkIPForward and checkRPFilter then answer about the host that
// actually forwards sandbox packets.
func (m *machineProbe) Sysctl(key string) (string, error) {
	path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
	q, err := shellQuote(path)
	if err != nil {
		return "", err
	}
	out, err := m.memoExec("sysctl:"+key, "probe-sysctl:"+key, "cat "+q)
	if err != nil {
		return "", os.ErrNotExist
	}
	return strings.TrimSpace(out), nil
}

func (m *machineProbe) LookPath(bin string) (string, error) {
	q, err := shellQuote(bin)
	if err != nil {
		return "", err
	}
	out, err := m.memoExec("lookpath:"+bin, "probe-lookpath:"+bin, "command -v "+q)
	if err != nil || strings.TrimSpace(out) == "" {
		return "", os.ErrNotExist
	}
	return strings.TrimSpace(out), nil
}

// Run executes a command inside the machine and returns its COMBINED output,
// exactly like sysProbe.Run.
//
// Never memoised — see the note on machineProbe.memo.
func (m *machineProbe) Run(name string, args ...string) (string, error) {
	words := append([]string{name}, args...)
	line, err := shellQuoteAll(words)
	if err != nil {
		return "", err
	}
	// 2>&1 for the combined-output contract. The receipt lines are on stdout
	// too and StripReceipt removes them, so merging is safe.
	return m.exec("probe-run:"+strings.Join(words, " "), line+" 2>&1")
}

func (m *machineProbe) DiskFreeBytes(path string) (uint64, error) {
	q, err := shellQuote(path)
	if err != nil {
		return 0, err
	}
	// -P for POSIX output (one line per filesystem, never wrapped), -k for
	// 1 KiB blocks, so field 4 is available KiB regardless of the guest's
	// BLOCKSIZE.
	out, err := m.memoExec("df:"+path, "probe-df:"+path, "df -kP "+q+" | tail -n 1")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(out)
	if len(f) < 4 {
		return 0, fmt.Errorf("could not parse `df -kP %s` output %q", path, out)
	}
	kib, err := strconv.ParseUint(f[3], 10, 64)
	if err != nil {
		return 0, err
	}
	return kib * 1024, nil
}

// Sleep waits on the MAC. The settle window is wall-clock either way, and
// sleeping in the guest would cost an extra round trip to do it worse.
func (m *machineProbe) Sleep(d time.Duration) { time.Sleep(d) }

// guestFileInfo is the os.FileInfo a `stat` inside the machine can honestly
// fill: name, size and directory-ness. Mode/ModTime are not read by any check.
type guestFileInfo struct {
	name string
	size int64
	dir  bool
}

func (f guestFileInfo) Name() string       { return f.name }
func (f guestFileInfo) Size() int64        { return f.size }
func (f guestFileInfo) Mode() os.FileMode  { return 0 }
func (f guestFileInfo) ModTime() time.Time { return time.Time{} }
func (f guestFileInfo) IsDir() bool        { return f.dir }
func (f guestFileInfo) Sys() any           { return nil }

// shellQuote wraps one word in single quotes for the guest shell, and REFUSES
// any word that already contains one.
//
// Refusing rather than escaping is deliberate and safe: every word that reaches
// here comes from a package constant (unit names, /proc paths, systemctl
// properties) or from a Config path the operator gave, and a single quote in
// any of those is a bug worth surfacing rather than a case worth handling. The
// alternative — '\” splicing — is correct but is precisely the kind of clever
// quoting that this whole package exists to keep out of the transport.
func shellQuote(word string) (string, error) {
	if strings.Contains(word, "'") {
		return "", fmt.Errorf("refusing to send %q into the machine: it contains a single quote, "+
			"which this transport does not escape (values with quotes must travel through ExecSpec.Env)", word)
	}
	return "'" + word + "'", nil
}

func shellQuoteAll(words []string) (string, error) {
	out := make([]string, 0, len(words))
	for _, w := range words {
		q, err := shellQuote(w)
		if err != nil {
			return "", err
		}
		out = append(out, q)
	}
	return strings.Join(out, " "), nil
}

var _ Probe = (*machineProbe)(nil)
