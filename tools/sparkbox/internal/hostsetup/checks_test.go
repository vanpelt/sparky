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
		{"service active", checkService, fakeProbe{runs: map[string]runResult{"systemctl is-active sparkbox.service": {out: "active"}}}, Pass},
		{"service inactive", checkService, fakeProbe{runs: map[string]runResult{"systemctl is-active sparkbox.service": {out: "inactive", err: os.ErrPermission}}}, Warn},
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
