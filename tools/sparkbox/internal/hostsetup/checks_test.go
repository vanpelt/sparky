package hostsetup

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeProbe is an in-memory Probe so checks run with no real host. Every field
// is a canned answer; unset fields behave like "absent".
type fakeProbe struct {
	goos, goarch string
	uid          int
	files        map[string]string // path -> contents (presence == exists)
	writable     map[string]bool
	sysctls      map[string]string
	paths        map[string]string // LookPath: bin -> resolved path ("" absent)
	runs         map[string]runResult
	diskFree     uint64
}

type runResult struct {
	out string
	err error
}

type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

func (p fakeProbe) GOOS() string   { return orDefault(p.goos, "linux") }
func (p fakeProbe) GOARCH() string { return orDefault(p.goarch, "amd64") }
func (p fakeProbe) Uid() int       { return p.uid }

func (p fakeProbe) Stat(path string) (os.FileInfo, error) {
	if _, ok := p.files[path]; ok {
		return fakeFileInfo{name: path}, nil
	}
	// A directory registered as a "dir/" prefix.
	if v, ok := p.files[path+"/"]; ok {
		_ = v
		return fakeFileInfo{name: path, dir: true}, nil
	}
	return nil, os.ErrNotExist
}

func (p fakeProbe) Writable(path string) bool { return p.writable[path] }

func (p fakeProbe) ReadFile(path string) ([]byte, error) {
	if v, ok := p.files[path]; ok {
		return []byte(v), nil
	}
	return nil, os.ErrNotExist
}

func (p fakeProbe) Sysctl(key string) (string, error) {
	if v, ok := p.sysctls[key]; ok {
		return v, nil
	}
	return "", os.ErrNotExist
}

func (p fakeProbe) LookPath(bin string) (string, error) {
	if v, ok := p.paths[bin]; ok && v != "" {
		return v, nil
	}
	return "", os.ErrNotExist
}

func (p fakeProbe) Run(name string, args ...string) (string, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if r, ok := p.runs[key]; ok {
		return r.out, r.err
	}
	return "", os.ErrNotExist
}

func (p fakeProbe) DiskFreeBytes(string) (uint64, error) { return p.diskFree, nil }

// Sleep is a no-op: tests drive the liveness window with Config.ServiceSettle,
// which is zero in every table row, so nothing here ever waits.
func (p fakeProbe) Sleep(time.Duration) {}

// scriptedProbe adds sequenced answers for commands whose output must DIFFER
// between calls. The liveness check samples `systemctl show` twice and the
// entire question it asks is whether the two samples disagree, which a
// map-keyed fake can never express. Wrapping (rather than teaching fakeProbe to
// count) keeps the big table above untouched and keeps fakeProbe's value
// receivers valid.
type scriptedProbe struct {
	fakeProbe
	seq   map[string][]string // command line -> successive outputs; the last repeats
	n     map[string]int
	slept time.Duration
}

func newScriptedProbe(seq map[string][]string) *scriptedProbe {
	return &scriptedProbe{seq: seq, n: map[string]int{}}
}

func (p *scriptedProbe) Run(name string, args ...string) (string, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if outs, ok := p.seq[key]; ok && len(outs) > 0 {
		i := p.n[key]
		if i >= len(outs) {
			i = len(outs) - 1
		}
		p.n[key]++
		return outs[i], nil
	}
	return p.fakeProbe.Run(name, args...)
}

func (p *scriptedProbe) Sleep(d time.Duration) { p.slept += d }

// showCmd is the exact `systemctl show` command line the liveness probe issues,
// built from the same const so a property-list edit cannot silently orphan the
// fixtures below.
var showCmd = "systemctl show " + serviceUnit + " --property=" + serviceShowProps

// unitState renders a `systemctl show` reply. Written as a helper because every
// liveness fixture differs in one or two properties out of six.
func unitState(active, sub, restarts, pid, startedAt string) string {
	return "LoadState=loaded\nActiveState=" + active + "\nSubState=" + sub +
		"\nNRestarts=" + restarts + "\nExecMainPID=" + pid +
		"\nExecMainStartTimestampMonotonic=" + startedAt + "\n"
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func TestChecks(t *testing.T) {
	cfg := Config{
		Root: "/srv/sparkbox", StateDir: "/srv/sparkbox/data/state",
		ImageDir: "/srv/sparkbox/data/images", KernelPath: "/srv/sparkbox/vmlinux",
		DefaultImage: "universal", UsersPath: "/srv/sparkbox/users.conf",
		FirecrackerBin: "/usr/local/bin/firecracker",
		BinPath:        "/usr/local/bin/sparkbox",
		// ServiceSettle stays zero: the liveness check still takes both samples,
		// it just never waits, so the whole table runs instantly.
	}
	const goodKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f me@host"

	tests := []struct {
		name  string
		check func(Probe, Config) Result
		probe fakeProbe
		want  Status
	}{
		{"os linux", checkOS, fakeProbe{goos: "linux"}, Pass},
		{"os windows", checkOS, fakeProbe{goos: "windows"}, Fail},
		{"arch amd64", checkArch, fakeProbe{goarch: "amd64"}, Pass},
		{"arch riscv", checkArch, fakeProbe{goarch: "riscv64"}, Warn},
		{"root", checkRoot, fakeProbe{uid: 0}, Pass},
		{"non-root", checkRoot, fakeProbe{uid: 1000}, Warn},
		{"kvm present", checkKVM, fakeProbe{uid: 0, files: map[string]string{"/dev/kvm": ""}, writable: map[string]bool{"/dev/kvm": true}}, Pass},
		{"kvm missing", checkKVM, fakeProbe{uid: 0}, Fail},
		{"kvm not writable", checkKVM, fakeProbe{uid: 0, files: map[string]string{"/dev/kvm": ""}}, Warn},
		{"virt vmx", checkVirt, fakeProbe{files: map[string]string{"/proc/cpuinfo": "flags: fpu vme vmx"}}, Pass},
		{"virt svm", checkVirt, fakeProbe{files: map[string]string{"/proc/cpuinfo": "flags: fpu svm"}}, Pass},
		{"virt none", checkVirt, fakeProbe{files: map[string]string{"/proc/cpuinfo": "flags: fpu vme"}}, Fail},
		{"virt arm64 kvm", checkVirt, fakeProbe{goarch: "arm64", files: map[string]string{"/dev/kvm": ""}}, Pass},
		{"virt arm64 no duplicate kvm failure", checkVirt, fakeProbe{goarch: "arm64"}, Pass},
		{"ip_forward on", checkIPForward, fakeProbe{sysctls: map[string]string{"net.ipv4.ip_forward": "1"}}, Pass},
		{"ip_forward off", checkIPForward, fakeProbe{sysctls: map[string]string{"net.ipv4.ip_forward": "0"}}, Warn},
		{"rp_filter strict", checkRPFilter, fakeProbe{sysctls: map[string]string{"net.ipv4.conf.all.rp_filter": "1"}}, Pass},
		{"rp_filter loose", checkRPFilter, fakeProbe{sysctls: map[string]string{"net.ipv4.conf.all.rp_filter": "2"}}, Warn},
		{"firecracker ok", checkFirecracker, fakeProbe{paths: map[string]string{"firecracker": "/usr/local/bin/firecracker"}, runs: map[string]runResult{"firecracker --version": {out: "Firecracker v1.7.0"}}}, Pass},
		{"firecracker missing", checkFirecracker, fakeProbe{}, Fail},
		{"kernel present", checkKernel, fakeProbe{files: map[string]string{"/srv/sparkbox/vmlinux": ""}}, Pass},
		{"kernel missing", checkKernel, fakeProbe{}, Fail},
		{"rootfs present", checkRootfs, fakeProbe{files: map[string]string{"/srv/sparkbox/data/images/universal.ext4": ""}}, Pass},
		{"rootfs missing", checkRootfs, fakeProbe{}, Fail},
		{"keys present", checkFleetKeys, fakeProbe{files: map[string]string{
			"/srv/sparkbox/data/state/gateway_host_key.pem":     "",
			"/srv/sparkbox/data/state/gateway_upstream_key.pem": "",
			"/srv/sparkbox/data/state/oidc_signing_key.pem":     "",
		}}, Pass},
		{"keys missing warns", checkFleetKeys, fakeProbe{}, Warn},
		{"users ok", checkUsers, fakeProbe{files: map[string]string{"/srv/sparkbox/users.conf": "me " + goodKey}}, Pass},
		{"users missing", checkUsers, fakeProbe{}, Fail},
		{"users empty", checkUsers, fakeProbe{files: map[string]string{"/srv/sparkbox/users.conf": "# just a comment\n"}}, Fail},
		{"users bad key", checkUsers, fakeProbe{files: map[string]string{"/srv/sparkbox/users.conf": "me not-a-key"}}, Fail},
		{"disk ample", checkDisk, fakeProbe{files: map[string]string{"/srv/sparkbox/data/": ""}, diskFree: 200 << 30}, Pass},
		{"disk low", checkDisk, fakeProbe{files: map[string]string{"/srv/sparkbox/data/": ""}, diskFree: 5 << 30}, Warn},
		{"nat present", checkNAT, fakeProbe{runs: map[string]runResult{"iptables -t nat -nL SPARKBOX_EDGE": {out: "Chain SPARKBOX_EDGE (1 references)"}}}, Pass},
		{"nat absent", checkNAT, fakeProbe{}, Warn},
		{"service active", checkService, fakeProbe{runs: map[string]runResult{showCmd: {out: unitState("active", "running", "0", "42", "9000")}}}, Pass},
		{"service inactive", checkService, fakeProbe{runs: map[string]runResult{showCmd: {out: unitState("inactive", "dead", "0", "0", "0")}}}, Warn},
		{"service failed", checkService, fakeProbe{runs: map[string]runResult{showCmd: {out: unitState("failed", "failed", "3", "0", "0")}}}, Fail},
		{"service unit not installed", checkService, fakeProbe{runs: map[string]runResult{showCmd: {out: "LoadState=not-found\nActiveState=inactive\n"}}}, Warn},
		{"service no systemd", checkService, fakeProbe{}, Warn},
		{"versions agree", checkVersions, fakeProbe{
			files: map[string]string{"/usr/local/bin/sparkbox": ""},
			runs: map[string]runResult{
				"/usr/local/bin/sparkbox version": {out: "sparkbox v0.4.0 (linux/arm64)"},
				showCmd:                           {out: unitState("active", "running", "0", "77", "9000")},
				"/proc/77/exe version":            {out: "sparkbox v0.4.0 (linux/arm64)"},
			},
		}, Pass},
		{"versions missing binary", checkVersions, fakeProbe{}, Warn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.check(tt.probe, cfg)
			if got.Status != tt.want {
				t.Fatalf("status = %v, want %v (detail %q)", got.Status, tt.want, got.Detail)
			}
			if got.Status != Pass && got.Hint == "" {
				t.Errorf("non-pass result should carry a remediation hint")
			}
		})
	}
}

func TestAnyFailAndReport(t *testing.T) {
	results := []Result{
		{Name: "a", Status: Pass, Detail: "ok"},
		{Name: "bee", Status: Warn, Detail: "meh", Hint: "do x"},
	}
	if AnyFail(results) {
		t.Fatal("no Fail present, AnyFail should be false")
	}
	results = append(results, Result{Name: "c", Status: Fail, Detail: "bad", Hint: "fix it"})
	if !AnyFail(results) {
		t.Fatal("Fail present, AnyFail should be true")
	}
	var buf bytes.Buffer
	PrintResults(&buf, results)
	out := buf.String()
	for _, want := range []string{"PASS", "WARN", "FAIL", "do x", "fix it", "1 passed, 1 warnings, 1 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n%s", want, out)
		}
	}
}

func TestCheckVirtARM64ReportsArchitectureSignal(t *testing.T) {
	got := checkVirt(fakeProbe{
		goarch: "arm64",
		files:  map[string]string{"/dev/kvm": ""},
	}, Config{})
	if got.Status != Pass {
		t.Fatalf("status = %v, want PASS (detail %q)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "ARM64") || !strings.Contains(got.Detail, "/dev/kvm") {
		t.Fatalf("detail = %q, want ARM64 /dev/kvm explanation", got.Detail)
	}
}

func TestNodeChecksSkipGatewayIdentityFiles(t *testing.T) {
	cfg := Config{Gateway: "gateway.example:2222"}
	p := fakeProbe{}
	if got := checkFleetKeys(p, cfg); got.Status != Pass {
		t.Fatalf("fleet keys on node = %v (%q), want PASS", got.Status, got.Detail)
	}
	if got := checkUsers(p, cfg); got.Status != Pass {
		t.Fatalf("users.conf on node = %v (%q), want PASS", got.Status, got.Detail)
	}
}

// TestServiceLiveness is the F7 regression: a unit that systemd calls "active"
// at every instant while it is in fact dying every two seconds must FAIL, and
// the failure must carry the journal so the operator reads the real error here.
func TestServiceLiveness(t *testing.T) {
	const journal = "sparkbox: listen tcp 127.0.0.1:8080: bind: address already in use"
	cfg := Config{ServiceSettle: 10 * time.Second}

	tests := []struct {
		name       string
		samples    []string
		want       Status
		wantDetail string
	}{
		{
			name:       "stable service passes",
			samples:    []string{unitState("active", "running", "0", "42", "9000")},
			want:       Pass,
			wantDetail: "stable",
		},
		{
			// The exact DGX shape: "active" both times, restart counter moving.
			name: "climbing NRestarts fails",
			samples: []string{
				unitState("active", "running", "3", "800", "50000"),
				unitState("active", "running", "5", "930", "70000"),
			},
			want:       Fail,
			wantDetail: "NRestarts climbed 3 → 5",
		},
		{
			// systemd < 235 reports no NRestarts at all; the start timestamp is
			// the signal that still works there.
			name: "moved start timestamp fails",
			samples: []string{
				unitState("active", "running", "", "800", "50000"),
				unitState("active", "running", "", "930", "70000"),
			},
			want:       Fail,
			wantDetail: "the main process restarted",
		},
		{
			// Died for good rather than looping: the restart counter does not
			// move, but the unit is no longer running.
			name: "stopped during the window fails",
			samples: []string{
				unitState("active", "running", "0", "42", "9000"),
				unitState("failed", "failed", "0", "0", "9000"),
			},
			want:       Fail,
			wantDetail: "failed during the settle window",
		},
		{
			// A unit that had a rough boot but has since settled is history,
			// not a fault — it must not FAIL, but it should say so.
			name:       "past restarts that stopped climbing pass",
			samples:    []string{unitState("active", "running", "7", "42", "9000")},
			want:       Pass,
			wantDetail: "7 lifetime restarts",
		},
		{
			// Caught inside the RestartSec gap on systemd < 235: no NRestarts key
			// at all, and the start timestamp still holds the *last* start, so
			// neither restart signal moves. SubState is the only evidence — and
			// "active (auto-restart), stable" is what the check used to print.
			name: "auto-restart backoff fails without NRestarts",
			samples: []string{
				unitState("active", "running", "", "800", "50000"),
				unitState("activating", "auto-restart", "", "0", "50000"),
			},
			want:       Fail,
			wantDetail: "auto-restart",
		},
		{
			// Same shape on modern systemd: n_restarts is incremented when the
			// restart is *issued*, not when the unit enters the backoff, so a
			// gateway that dies in the last RestartSec seconds of the window
			// reports an unchanged counter.
			name: "auto-restart backoff fails with a static NRestarts",
			samples: []string{
				unitState("active", "running", "12", "800", "50000"),
				unitState("activating", "auto-restart", "12", "0", "50000"),
			},
			want:       Fail,
			wantDetail: "crash-looping",
		},
		{
			// Both samples caught in the gap: "activating" twice would otherwise
			// read as "still starting" (a WARN) even though systemd is telling us
			// in as many words that it is restarting the unit.
			name: "both samples in the restart backoff fail rather than warn",
			samples: []string{
				unitState("activating", "auto-restart", "", "0", "50000"),
				unitState("activating", "auto-restart", "", "0", "50000"),
			},
			want:       Fail,
			wantDetail: "crash-looping",
		},
		{
			// The replacement process has not started yet, so the start timestamp
			// has not moved either — but a unit that was active and is activating
			// again lost the process we sampled first.
			name: "active then activating again fails",
			samples: []string{
				unitState("active", "running", "0", "800", "50000"),
				unitState("activating", "start", "0", "0", "50000"),
			},
			want:       Fail,
			wantDetail: "restarted during the settle window",
		},
		{
			// enable --now: no main process in the first sample, one in the
			// second. That is a boot, not a crash loop.
			name: "first start during the window is not a restart",
			samples: []string{
				unitState("activating", "start", "0", "0", "0"),
				unitState("active", "running", "0", "42", "9000"),
			},
			want:       Pass,
			wantDetail: "active",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newScriptedProbe(map[string][]string{
				showCmd: tt.samples,
				"journalctl -u " + serviceUnit + " -n 20 --no-pager": {journal},
			})
			got := checkService(p, cfg)
			if got.Status != tt.want {
				t.Fatalf("status = %v, want %v (detail %q)", got.Status, tt.want, got.Detail)
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tt.wantDetail)
			}
			if got.Status != Pass && got.Hint == "" {
				t.Error("non-pass result should carry a remediation hint")
			}
			if got.Status == Fail && !strings.Contains(got.Output, journal) {
				t.Errorf("a FAIL must inline the journal; output = %q", got.Output)
			}
			// The settle window is only worth paying for once the unit claims
			// to be running — and it must be honoured exactly once.
			if p.slept != cfg.ServiceSettle {
				t.Errorf("slept %v, want exactly one %v settle window", p.slept, cfg.ServiceSettle)
			}
		})
	}
}

// TestServiceLivenessNeverSleepsWithoutALoadedUnit guards the cost side: a
// machine with no systemd (or no unit installed) must answer instantly, or
// every doctor run on a dev box — and TestDefaultChecksNamesFilled below —
// pays the settle window.
func TestServiceLivenessNeverSleepsWithoutALoadedUnit(t *testing.T) {
	cfg := Config{ServiceSettle: time.Hour}
	for name, out := range map[string]string{
		"no systemd":       "System has not been booted with systemd as init system",
		"unit not found":   "LoadState=not-found\nActiveState=inactive\n",
		"unit not started": unitState("inactive", "dead", "0", "0", "0"),
	} {
		t.Run(name, func(t *testing.T) {
			p := newScriptedProbe(map[string][]string{showCmd: {out}})
			if got := checkService(p, cfg); got.Status == Fail {
				t.Errorf("status = FAIL (%q), want a soft verdict for a unit that is not claiming to run", got.Detail)
			}
			if p.slept != 0 {
				t.Errorf("slept %v, want 0 — nothing is running to settle", p.slept)
			}
		})
	}
}

// TestVersionSkew is the check that would have caught the DGX silently running
// v0.3.0 after a "v0.4.0" setup.
func TestVersionSkew(t *testing.T) {
	binPath := "/usr/local/bin/sparkbox"
	base := fakeProbe{files: map[string]string{binPath: ""}}

	t.Run("running service older than the installed binary", func(t *testing.T) {
		p := newScriptedProbe(map[string][]string{
			binPath + " version":   {"sparkbox v0.4.0 (linux/arm64)"},
			showCmd:                {unitState("active", "running", "0", "77", "9000")},
			"/proc/77/exe version": {"sparkbox v0.3.0 (linux/arm64)"},
		})
		p.fakeProbe = base
		got := checkVersions(p, Config{BinPath: binPath, Release: "v0.4.0"})
		if got.Status != Warn {
			t.Fatalf("status = %v, want WARN (detail %q)", got.Status, got.Detail)
		}
		// All three values must be named — the operator needs to see which one
		// is the odd one out without running anything else.
		for _, want := range []string{"v0.4.0", "v0.3.0", binPath, "requested v0.4.0"} {
			if !strings.Contains(got.Detail+" "+got.Hint, want) {
				t.Errorf("result should name %q; detail=%q hint=%q", want, got.Detail, got.Hint)
			}
		}
	})

	t.Run("installed binary is not the requested release", func(t *testing.T) {
		p := newScriptedProbe(map[string][]string{
			binPath + " version":   {"sparkbox v0.3.0 (linux/arm64)"},
			showCmd:                {unitState("active", "running", "0", "77", "9000")},
			"/proc/77/exe version": {"sparkbox v0.3.0 (linux/arm64)"},
		})
		p.fakeProbe = base
		got := checkVersions(p, Config{BinPath: binPath, Release: "v0.4.0"})
		if got.Status != Warn {
			t.Fatalf("status = %v, want WARN (detail %q)", got.Status, got.Detail)
		}
		if !strings.Contains(got.Hint, "v0.4.0") || !strings.Contains(got.Hint, "v0.3.0") {
			t.Errorf("hint should name both versions, got %q", got.Hint)
		}
	})

	t.Run("dev build against latest is not a skew", func(t *testing.T) {
		// The common developer path: a hand-built binary says "dev" and
		// --release defaults to "latest". Neither is a comparable tag, so this
		// must stay quiet rather than warn on every machine.
		p := newScriptedProbe(map[string][]string{
			binPath + " version":   {"sparkbox dev (linux/arm64)"},
			showCmd:                {unitState("active", "running", "0", "77", "9000")},
			"/proc/77/exe version": {"sparkbox dev (linux/arm64)"},
		})
		p.fakeProbe = base
		if got := checkVersions(p, Config{BinPath: binPath, Release: "latest"}); got.Status != Pass {
			t.Fatalf("status = %v (%q), want PASS", got.Status, got.Detail)
		}
	})
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b    string
		cmp     int
		ok      bool
		comment string
	}{
		{"v0.4.0", "v0.4.0", 0, true, "equal"},
		{"v0.4.1", "v0.4.0", 1, true, "patch newer"},
		{"v0.3.0", "v0.4.0", -1, true, "minor older"},
		{"0.4.0", "v0.4.0", 0, true, "the v prefix is optional"},
		{"v0.5.0-rc1", "v0.5.0", -1, true, "a prerelease sorts below its release"},
		{"v1.0", "v0.9.9", 1, true, "missing segments read as zero"},
		{"dev", "v0.4.0", 0, false, "a hand build is not comparable"},
		{"v0.4.0", "latest", 0, false, "'latest' is not a tag"},
		{"nightly-7", "v0.4.0", 0, false, "unparseable is not comparable"},
	}
	for _, tt := range tests {
		cmp, ok := compareVersions(tt.a, tt.b)
		if ok != tt.ok || (ok && cmp != tt.cmp) {
			t.Errorf("compareVersions(%q, %q) = (%d, %v), want (%d, %v) — %s", tt.a, tt.b, cmp, ok, tt.cmp, tt.ok, tt.comment)
		}
	}
}

func TestPrintResultsInlinesOutput(t *testing.T) {
	var buf bytes.Buffer
	PrintResults(&buf, []Result{{
		Name: "sparkbox service", Status: Fail,
		Detail: "crash-looping", Hint: "read the journal",
		Output: "line one\nbind: address already in use\n",
	}})
	out := buf.String()
	for _, want := range []string{"crash-looping", "read the journal", "bind: address already in use"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n%s", want, out)
		}
	}
	// Evidence must stay inside the indented block, never flush-left, or the
	// aligned report falls apart.
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "  ") {
			t.Errorf("unindented report line %q", line)
		}
	}
}

func TestDefaultChecksNamesFilled(t *testing.T) {
	// RunChecks should backfill the Check.Name onto results that omit it.
	res := RunChecks(fakeProbe{}, Config{StateDir: "/x", ImageDir: "/x", KernelPath: "/x/v", DefaultImage: "u", UsersPath: "/x/u"}, DefaultChecks())
	if len(res) != len(DefaultChecks()) {
		t.Fatalf("got %d results, want %d", len(res), len(DefaultChecks()))
	}
	for _, r := range res {
		if r.Name == "" {
			t.Error("result missing name")
		}
	}
}
