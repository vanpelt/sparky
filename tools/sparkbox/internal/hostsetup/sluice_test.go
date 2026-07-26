package hostsetup

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/deploy"
)

// sluiceManifest builds a manifest that publishes a sluice binary whose bytes
// are `body`, plus the URL the fetcher must serve it at. The checksum is
// computed from the body rather than typed, so a test cannot accidentally
// assert that verification passed when it was skipped.
func sluiceManifest(release, body string) (Manifest, string) {
	sum := sha256.Sum256([]byte(body))
	m := Manifest{
		Release:      release,
		Arch:         "arm64",
		Platform:     "linux",
		SluiceAsset:  "sluice-linux-arm64",
		SHA256Sluice: hex.EncodeToString(sum[:]),
	}
	return m, DefaultArtifactBase + "/download/" + release + "/sluice-linux-arm64"
}

// TestManifestSluiceIsAllOrNothing pins the rule that keeps setup from
// inventing a download: a release either NAMES its sluice asset and its
// checksum, or it has none and --sluice is refused with a reason.
//
// The checksum half is not pedantry. downloadVerify treats SHA256 == "" as "do
// not verify", so a manifest carrying only SLUICE_ASSET would fetch whatever
// answered that URL and chmod 0755 it — for a binary systemd is about to run as
// root with CAP_BPF and CAP_NET_ADMIN.
func TestManifestSluiceIsAllOrNothing(t *testing.T) {
	tests := []struct {
		name  string
		asset string
		sha   string
		want  bool
	}{
		{"both present", "sluice-linux-arm64", "abc123", true},
		{"release predates sluice", "", "", false},
		{"named but unverifiable", "sluice-linux-arm64", "", false},
		{"checksum with no asset to apply it to", "", "abc123", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Manifest{Release: "v0.5.0", Arch: "arm64", SluiceAsset: tt.asset, SHA256Sluice: tt.sha}
			art, ok := m.Sluice(DefaultArtifactBase, "/usr/local/bin/sluice")
			if ok != tt.want {
				t.Fatalf("ok = %v, want %v", ok, tt.want)
			}
			if !ok {
				return
			}
			if want := DefaultArtifactBase + "/download/v0.5.0/sluice-linux-arm64"; art.URL != want {
				t.Errorf("URL = %q, want %q", art.URL, want)
			}
			// 0755 or the unit's ExecStart fails with EACCES at boot, which
			// reads as a systemd problem rather than a permissions one.
			if art.Mode != 0o755 {
				t.Errorf("Mode = %o, want 0755 (it is an executable)", art.Mode)
			}
			if art.SHA256 != tt.sha {
				t.Errorf("SHA256 = %q, want %q", art.SHA256, tt.sha)
			}
		})
	}
}

// TestSluiceImpliesTheFlagsThatMakeItDoAnything is the whole point of --sluice
// being one flag instead of three.
//
// Installing the daemon and leaving --sluice-socket unset means the gateway
// pushes no policy; leaving --guest-dns unset means guests never resolve
// through the allowlist. Either one produces a host with sluice running,
// systemd green, doctor's service check passing — and every sandbox reaching
// the whole internet. That is the exact state checkEgress exists to report, and
// it would be absurd to ship a flag that creates it.
func TestSluiceImpliesTheFlagsThatMakeItDoAnything(t *testing.T) {
	tests := []struct {
		name        string
		cfg         Config
		wantSocket  string
		wantGuest   string
		wantDNSAddr string
	}{
		{
			name:        "bare --sluice fills in all three",
			cfg:         Config{Sluice: true},
			wantSocket:  "/run/sluice.sock",
			wantDNSAddr: ":53",
			// A wildcard resolver answers on the host end of every tap, which
			// is what "gateway" means to the firecracker driver — so no extra
			// address has to exist on the box.
			wantGuest: "gateway",
		},
		{
			// The DGX shape: sluice gets a dedicated resolver address because
			// sparkbox's own wildcard responder already holds :53. Guests must
			// then be handed that literal, and deriving it is what stops the
			// two from drifting apart by hand.
			name:        "a concrete resolver address becomes the guests' resolver",
			cfg:         Config{Sluice: true, SluiceDNSAddr: "172.30.0.53:53"},
			wantSocket:  "/run/sluice.sock",
			wantDNSAddr: "172.30.0.53:53",
			wantGuest:   "172.30.0.53",
		},
		{
			name:        "explicit values win over the derivation",
			cfg:         Config{Sluice: true, SluiceSocket: "/run/other.sock", GuestDNS: "10.0.0.53"},
			wantSocket:  "/run/other.sock",
			wantDNSAddr: ":53",
			wantGuest:   "10.0.0.53",
		},
		{
			// Without --sluice nothing is implied: a host that has not asked
			// for egress control must not acquire flags pointing at a daemon
			// nobody installed.
			name:       "no --sluice implies nothing",
			cfg:        Config{},
			wantSocket: "", wantGuest: "", wantDNSAddr: "",
		},
		{
			// Talking to a hand-installed sluice, which was the only way to
			// have one before this step existed. Still no resolver address,
			// because setup is not the thing that binds it.
			name:        "an explicit socket without --sluice is left exactly as given",
			cfg:         Config{SluiceSocket: "/run/sluice.sock", GuestDNS: "gateway"},
			wantSocket:  "/run/sluice.sock",
			wantGuest:   "gateway",
			wantDNSAddr: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.sluiceSocket(); got != tt.wantSocket {
				t.Errorf("sluiceSocket() = %q, want %q", got, tt.wantSocket)
			}
			if got := tt.cfg.guestDNS(); got != tt.wantGuest {
				t.Errorf("guestDNS() = %q, want %q", got, tt.wantGuest)
			}
			if got := tt.cfg.sluiceDNSAddr(); got != tt.wantDNSAddr {
				t.Errorf("sluiceDNSAddr() = %q, want %q", got, tt.wantDNSAddr)
			}
			// And the derivation has to reach the unit, not just the accessor:
			// optionalFlags is what ExecStart is rendered from.
			flags := strings.Join(optionalFlags(tt.cfg), " ")
			if tt.wantSocket != "" && !strings.Contains(flags, "--sluice-socket "+tt.wantSocket) {
				t.Errorf("ExecStart flags %q missing --sluice-socket %s", flags, tt.wantSocket)
			}
			if tt.wantSocket == "" && strings.Contains(flags, "--sluice-socket") {
				t.Errorf("ExecStart flags %q must not carry --sluice-socket", flags)
			}
		})
	}
}

// TestValidateSluiceRefusesConfigurationsThatQuietlyDoNothing covers the
// combinations that produce a daemon which is "active" and useless — the
// failure mode this whole workstream is about.
func TestValidateSluiceRefusesConfigurationsThatQuietlyDoNothing(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string // substring; "" means it must be accepted
	}{
		{
			name: "plain --sluice",
			cfg:  Config{Sluice: true, ProxyDomain: "example.com"},
		},
		{
			// Two DNS servers on one host. sparkbox's wildcard responder holds
			// --dns-addr for the edge; sluice's holds the guest allowlist. The
			// DGX hit exactly this and answered it with a dedicated resolver IP.
			name:    "sluice's resolver collides with the sparkbox responder",
			cfg:     Config{Sluice: true, DNSAddr: "10.66.0.1:53", DNSAnswer: "10.66.0.1", ProxyDomain: "example.com"},
			wantErr: "collides with --dns-addr",
		},
		{
			name: "a dedicated resolver address resolves the collision",
			cfg: Config{Sluice: true, SluiceDNSAddr: "172.30.0.53:53",
				DNSAddr: "10.66.0.1:53", DNSAnswer: "10.66.0.1", ProxyDomain: "example.com"},
		},
		{
			// Different ports, same host: no collision, so no refusal.
			name: "different ports do not collide",
			cfg: Config{Sluice: true, SluiceDNSAddr: "10.66.0.1:5353",
				DNSAddr: "10.66.0.1:53", DNSAnswer: "10.66.0.1", ProxyDomain: "example.com"},
		},
		{
			// "Bind it privately" is the right instinct for every other address
			// in this config and the wrong one here: a guest's loopback is its
			// own, so the resolver would be unreachable and the sandbox would
			// look like it had no network rather than a filtered one.
			name:    "a loopback resolver is unreachable from every guest",
			cfg:     Config{Sluice: true, SluiceDNSAddr: "127.0.0.1:53", ProxyDomain: "example.com"},
			wantErr: "no guest can ever reach it",
		},
		{
			// Plausible typo with a plausible reading, and no effect at all.
			name:    "--sluice-dns-addr without --sluice",
			cfg:     Config{SluiceDNSAddr: "172.30.0.53:53", ProxyDomain: "example.com"},
			wantErr: "has no effect without --sluice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSubsystems(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got none", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestKernelFloorIsCheckedBySetupNotOnlyByTheUnit.
//
// sluice.service carries ConditionKernelVersion=>=6.6, and that is not enough
// on its own: systemd SKIPS a unit whose condition fails and `systemctl start`
// on a skipped unit exits 0. Installing on a 6.1 host would therefore have
// produced a clean provisioning report over an egress filter that never ran,
// which is F7's shape with a different cause. So setup decides for itself.
func TestKernelFloorIsCheckedBySetupNotOnlyByTheUnit(t *testing.T) {
	tests := []struct {
		name          string
		release       string // "" means /proc/sys is unreadable
		wantSupported bool
		wantKnown     bool
	}{
		{"6.8 ubuntu", "6.8.0-31-generic", true, true},
		{"exactly the floor", "6.6.0", true, true},
		{"the DGX guest kernel is below it", "6.1.155", false, true},
		{"5.x is far below it", "5.15.0-89-generic", false, true},
		{"a major bump is above it", "7.0.1", true, true},
		{"the macOS outer kernel", "6.14.9-sparkbox-poc", true, true},
		{"a two-component release candidate", "6.6-rc1", true, true},
		// Not a refusal: /proc/sys is missing in some container images, and
		// stranding a host over an unreadable file would be worse than
		// installing and letting doctor report what happened.
		{"unreadable", "", false, false},
		{"unparseable", "weird", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := fakeProbe{sysctls: map[string]string{}}
			if tt.release != "" {
				p.sysctls["kernel.osrelease"] = tt.release
			}
			_, supported, known := kernelSupportsSluice(p)
			if known != tt.wantKnown {
				t.Fatalf("known = %v, want %v", known, tt.wantKnown)
			}
			if supported != tt.wantSupported {
				t.Errorf("supported = %v, want %v", supported, tt.wantSupported)
			}
		})
	}
}

// TestSluiceStepRefusesAReleaseWithoutTheAsset.
//
// This is the honest half of closing the TODO. The manifest is the authority on
// what a release contains, so a tag cut before sluice shipped gets a refusal
// that names the cause, rather than a fabricated URL and a 404 from
// downloadVerify — which is precisely what the old TODO declined to do.
func TestSluiceStepRefusesAReleaseWithoutTheAsset(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Cfg.Sluice = true
	e.Probe = fakeProbe{sysctls: map[string]string{"kernel.osrelease": "6.8.0-generic"}}
	e.Manifest = Manifest{Release: "v0.4.0", Arch: "arm64", Platform: "linux"} // no SLUICE_ASSET

	err := stepSluice().Apply(e)
	if err == nil {
		t.Fatal("expected a refusal for a release with no sluice asset")
	}
	for _, want := range []string{"v0.4.0", "SLUICE_ASSET", "--release"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestSluiceStepRefusesAKernelBelowTheFloor: the daemon would start, attach to
// nothing and report itself healthy. Refusing is the only outcome that does not
// end in a host believing it is filtered.
func TestSluiceStepRefusesAKernelBelowTheFloor(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Cfg.Sluice = true
	e.Probe = fakeProbe{sysctls: map[string]string{"kernel.osrelease": "6.1.155"}}
	m, url := sluiceManifest("v0.5.0", "#!/bin/false\n")
	e.Manifest = m
	e.Fetch = mapFetcher{url: "#!/bin/false\n"}

	err := stepSluice().Apply(e)
	if err == nil {
		t.Fatal("expected a refusal on a kernel below 6.6")
	}
	for _, want := range []string{"6.1.155", "6.6", "TCX"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestSluiceStepIsSkippedAndSaysSo: the default is no egress control, and the
// one thing that must never happen is it being silent. Every other surface
// (printConnect, doctor's checkEgress) says it too.
func TestSluiceStepIsSkippedAndSaysSo(t *testing.T) {
	e, _ := testEnv(t, false)
	sat, note, err := stepSluice().Satisfied(e)
	if err != nil {
		t.Fatal(err)
	}
	if !sat {
		t.Fatal("a config without --sluice must be satisfied, not planned")
	}
	if !strings.Contains(note, "whole internet") {
		t.Errorf("skip note = %q, want it to say sandboxes are unfiltered", note)
	}
}

// TestRenderedSluiceUnitMatchesTheConfig. The unit is generated, so a value
// that reaches the ExecStart from anywhere but this config is a host running a
// configuration no `setup` invocation describes — F0's staleness, one daemon
// over.
func TestRenderedSluiceUnitMatchesTheConfig(t *testing.T) {
	cfg := DefaultConfigAt("/srv/sparkbox")
	cfg.Sluice = true
	cfg.SluiceDNSAddr = "172.30.0.53:53"
	got, err := renderSluiceService(cfg, sluiceBinPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ExecStart=/usr/local/bin/sluice run",
		"--allowlist /srv/sparkbox/allowlist.txt",
		"--dns-listen 172.30.0.53:53",
		"--api-listen /run/sluice.sock",
		"--tap-prefix sbtap",
		"EnvironmentFile=-/srv/sparkbox/sluice.env",
		// The floor the setup step also enforces. Belt and braces, and the
		// comment in the unit says which is which.
		"ConditionKernelVersion=>=6.6",
		// Unbraced, so systemd word-splits it and an empty value contributes
		// zero arguments rather than one empty one — the same rule the gateway
		// unit's flag bundles follow.
		"$SLUICE_ARGS",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered unit missing %q:\n%s", want, got)
		}
	}
	// A template that left a placeholder unexpanded would still install and
	// still start, and would then fail on an argument nobody typed.
	if strings.Contains(got, "{{") {
		t.Errorf("unit has an unexpanded template action:\n%s", got)
	}
	// Rendering must be stable: stepSluice compares the installed unit byte for
	// byte, so a render that shuffled would restart the daemon on every setup.
	again, _ := renderSluiceService(cfg, sluiceBinPath)
	if again != got {
		t.Error("renderSluiceService is not deterministic")
	}
}

// TestSluiceStepInstallsEverythingItNeeds is the end-to-end shape: one Apply on
// a clean host must leave a verified binary, a usable allowlist, an env file and
// a unit — and then read as satisfied, so a second `setup` neither re-downloads
// nor restarts a working daemon.
//
// The allowlist is the row that matters most. sluice EXITS 2 without one, under
// Restart=always, so a step that installed the unit and skipped the seed would
// have produced a permanent crash loop presented as a successful provision:
// F7's exact shape, from the very feature added to close F2.
func TestSluiceStepInstallsEverythingItNeeds(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Cfg.Sluice = true
	e.Probe = fakeProbe{sysctls: map[string]string{"kernel.osrelease": "6.8.0-31-generic"}}
	const body = "\x7fELF-pretend-this-is-sluice\n"
	m, url := sluiceManifest("v0.5.0", body)
	e.Manifest = m
	e.Fetch = mapFetcher{url: body}

	// A clean host is not satisfied — nothing is installed yet.
	if sat, _, err := stepSluice().Satisfied(e); err != nil || sat {
		t.Fatalf("clean host: satisfied = %v, err = %v; want false, nil", sat, err)
	}
	if err := stepSluice().Apply(e); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	bin, err := os.ReadFile(e.SluiceBinPath)
	if err != nil {
		t.Fatalf("binary not installed: %v", err)
	}
	if string(bin) != body {
		t.Error("the installed binary is not the bytes the manifest pinned")
	}
	fi, err := os.Stat(e.SluiceBinPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("binary mode %v is not executable — the unit's ExecStart would fail with EACCES at boot", fi.Mode())
	}
	allow, err := os.ReadFile(e.Cfg.sluiceAllowlistPath())
	if err != nil {
		t.Fatalf("no allowlist seeded — sluice exits 1 when that file is absent and Restart=always makes it permanent: %v", err)
	}
	if len(allow) == 0 {
		t.Error("the seeded allowlist is empty, which NXDOMAINs everything a tagged sandbox asks for")
	}
	if _, err := os.ReadFile(e.Cfg.sluiceEnvPath()); err != nil {
		t.Errorf("no sluice.env seeded: %v", err)
	}
	unit, err := os.ReadFile(filepath.Join(e.SystemdDir, sluiceUnit))
	if err != nil {
		t.Fatalf("no unit installed: %v", err)
	}
	if !strings.Contains(string(unit), "ExecStart="+e.SluiceBinPath) {
		t.Errorf("unit does not run the binary that was installed:\n%s", unit)
	}
	// enable-services has to know, or the unit sits on disk unstarted and the
	// re-fetched binary keeps running as the old inode.
	if !e.SluiceChanged {
		t.Error("SluiceChanged must be set so enable-services enables and restarts the unit")
	}

	// Idempotence: a second run changes nothing. Without the sha comparison
	// this would still pass, so also assert it is the CHECKSUM that satisfies
	// it — a host on an older release must not read as current.
	sat, note, err := stepSluice().Satisfied(e)
	if err != nil {
		t.Fatal(err)
	}
	if !sat {
		t.Fatal("a freshly installed sluice must read as satisfied on the next run")
	}
	if !strings.Contains(note, "v0.5.0") {
		t.Errorf("satisfied note = %q, want it to name the release it matched", note)
	}
	newer, _ := sluiceManifest("v0.6.0", "\x7fELF-a-different-sluice\n")
	e.Manifest = newer
	if sat, _, err := stepSluice().Satisfied(e); err != nil || sat {
		t.Errorf("a host holding the PREVIOUS release's sluice must not read as satisfied (sat=%v err=%v)", sat, err)
	}
}

// TestSluiceSeedsAreWrittenOnceAndNeverRewritten.
//
// The allowlist and sluice.env are the operator's policy. Not writing them is a
// crash loop (sluice exits 1 when the allowlist file is absent, under
// Restart=always); writing them on every run silently reverts a curated
// allowlist during an upgrade, which is the same class of surprise as F0
// pointed the other way. So: seed if absent, never touch again.
func TestSluiceSeedsAreWrittenOnceAndNeverRewritten(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Cfg.Sluice = true

	seeds := sluiceSeedFiles(e)
	if len(seeds) != 2 {
		t.Fatalf("expected the allowlist and the env file, got %d", len(seeds))
	}
	if err := os.MkdirAll(e.Cfg.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A hand-curated allowlist that must survive.
	mine := "# my policy\ninternal.example.com\n"
	if err := os.WriteFile(e.Cfg.sluiceAllowlistPath(), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, a := range seeds {
		if _, err := os.Stat(a.path); err == nil {
			continue // pre-existing: the step leaves it alone
		}
		if err := os.WriteFile(a.path, a.body, a.mode); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(e.Cfg.sluiceAllowlistPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != mine {
		t.Errorf("the operator's allowlist was overwritten:\n%s", got)
	}
	// The seeded env file must actually carry the sparkbox tag model, or
	// installing sluice would cut off every sandbox on the box the moment it
	// starts rather than only the tagged ones.
	env, err := os.ReadFile(e.Cfg.sluiceEnvPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "SLUICE_ARGS=--enforce --open-untagged") {
		t.Errorf("sluice.env should enforce only tagged sandboxes:\n%s", env)
	}
	// And the seeded allowlist must be a real one: sluice exits 1 when the file
	// its --allowlist names is not there, and an empty one would NXDOMAIN
	// everything a tagged sandbox asks.
	if len(seeds[0].body) == 0 || !strings.Contains(string(seeds[0].body), "api.anthropic.com") {
		t.Error("the allowlist seed should carry a usable default policy")
	}
}

// TestSluiceResolverIsPortPreflighted.
//
// :53 is the likeliest busy port on the whole host — a stock Ubuntu server runs
// systemd-resolved there — and sluice's unit is Restart=always, so losing that
// bind is a permanent loop that `systemctl is-active` calls "active". The
// preflight already exists for exactly this failure on the gateway's ports; the
// only question was whether a second daemon's address counts, and it does:
// setup is what causes it to be bound.
func TestSluiceResolverIsPortPreflighted(t *testing.T) {
	tests := []struct {
		name  string
		cfg   Config
		probe bool
	}{
		{"--sluice probes its resolver", Config{Sluice: true}, true},
		{"no --sluice, no probe", Config{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addrs, _ := effectiveAddrs(tt.cfg, map[string]string{})
			var udp, tcp bool
			for _, p := range wantedPorts(addrs) {
				if p.flag != "--sluice-dns-addr" {
					continue
				}
				if p.addr != ":53" {
					t.Errorf("probing %s, want :53", p.addr)
				}
				udp = udp || p.network == "udp"
				tcp = tcp || p.network == "tcp"
			}
			// UDP is the half that matters: it is what a resolver actually
			// answers on, and it is where systemd-resolved collides.
			if udp != tt.probe || tcp != tt.probe {
				t.Errorf("udp=%v tcp=%v, want both %v", udp, tcp, tt.probe)
			}
		})
	}
}

// showSluiceCmd is the exact `systemctl show` line the sluice liveness probe
// issues, built from the same consts as the production code so a property-list
// edit cannot silently orphan these fixtures.
var showSluiceCmd = "systemctl show " + sluiceUnit + " --property=" + serviceShowProps

// TestSluiceServiceLivenessIsProven is F7, on the unit F7's fix never covered.
//
// `setup --sluice` ran `systemctl enable --now sluice.service` and looked no
// further. The unit is Type=simple, so systemd returns 0 at the fork; it is
// Restart=always with StartLimitIntervalSec=0, so a daemon that dies on every
// start loops forever; the A1 liveness probe only ever sampled
// sparkbox.service; and checkEgress could only WARN. AnyFail was therefore
// false, setup exited 0, and the connect banner announced a filtered egress
// over a daemon that had never answered a query — while the guests handed
// --guest-dns had no resolver at all.
//
// The rows below are the three real ways in (a resolver address the host does
// not hold, an eBPF load the kernel refused, a missing allowlist file) reduced
// to the only two shapes systemd shows for them: a unit whose main process is
// being replaced, and one caught inside the restart backoff. Both must be a
// FAIL, because a FAIL is the only status that stops the banner.
func TestSluiceServiceLivenessIsProven(t *testing.T) {
	journal := "sluice[811]: listen udp 172.30.0.53:53: bind: cannot assign requested address"
	tests := []struct {
		name    string
		sluice  bool     // did this run ask for --sluice?
		samples []string // successive `systemctl show sluice.service` replies
		want    Status
		mention string
	}{
		{
			name:    "crash loop: NRestarts climbs across the settle window",
			sluice:  true,
			samples: []string{unitState("active", "running", "12", "811", "9000"), unitState("active", "running", "17", "902", "9600")},
			want:    Fail,
			mention: "crash-looping",
		},
		{
			// The sample landed in the RestartSec gap. NEITHER restart signal
			// moves across it (the timestamp only advances when a new main
			// process starts, and NRestarts is bumped when the restart is
			// issued), so without the auto-restart rule this reads as stable.
			name:    "crash loop: caught in the restart backoff",
			sluice:  true,
			samples: []string{unitState("activating", "auto-restart", "3", "0", "9000")},
			want:    Fail,
			mention: "auto-restart",
		},
		{
			name:    "the DNS-bind loop that never reaches ActiveState=failed",
			sluice:  true,
			samples: []string{unitState("active", "running", "40", "811", "9000"), unitState("activating", "start", "41", "0", "9000")},
			want:    Fail,
			mention: "crash-looping",
		},
		{
			// --sluice just ran `enable --now` on it, so "not running" is this
			// run's own work not having happened. It is also how a
			// condition-skipped unit looks (ConditionKernelVersion=>=6.6), which
			// the hint disambiguates by asking systemd for ConditionResult.
			name:    "installed but dead after setup asked for it",
			sluice:  true,
			samples: []string{unitState("inactive", "dead", "0", "0", "0")},
			want:    Fail,
			mention: "inactive",
		},
		{
			name:    "--sluice was asked for but the unit is not installed",
			sluice:  true,
			samples: []string{"LoadState=not-found\nActiveState=inactive\n"},
			want:    Fail,
			mention: "not installed",
		},
		{
			name:    "alive and stable is the only PASS",
			sluice:  true,
			samples: []string{unitState("active", "running", "0", "811", "9000")},
			want:    Pass,
			mention: "active",
		},
		{
			// A doctor run on an ordinary gateway. checkEgress is the check with
			// an opinion about having no egress filter; saying it twice would
			// only teach the operator to skim the report.
			name:    "no sluice on the host and none asked for",
			samples: []string{"LoadState=not-found\nActiveState=inactive\n"},
			want:    Pass,
			mention: "no egress gateway",
		},
		{
			// Same crash loop, seen by `sparkbox doctor` (which has no --sluice
			// flag at all). The host is broken either way, so the verdict must
			// not depend on which command is looking.
			name:    "a doctor run sees the same crash loop",
			samples: []string{unitState("active", "running", "12", "811", "9000"), unitState("active", "running", "13", "902", "9600")},
			want:    Fail,
			mention: "crash-looping",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newScriptedProbe(map[string][]string{
				showSluiceCmd: tt.samples,
				"journalctl -u " + sluiceUnit + " -n 20 --no-pager": {journal},
			})
			got := checkSluiceService(p, Config{Sluice: tt.sluice})
			if got.Status != tt.want {
				t.Fatalf("status = %v, want %v (detail %q)", got.Status, tt.want, got.Detail)
			}
			if !strings.Contains(got.Detail, tt.mention) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tt.mention)
			}
			if got.Status == Fail {
				if got.Hint == "" {
					t.Error("a FAIL must carry a remediation hint")
				}
				// The one line that names the cause lives in sluice's OWN
				// journal, not the gateway's; an operator who has to go and find
				// it reads "crash-looping" as a mystery. A unit that was never
				// installed has no journal to inline, and says so instead.
				if strings.Contains(tt.samples[0], "LoadState=loaded") &&
					!strings.Contains(got.Output, "cannot assign requested address") {
					t.Errorf("a crash-loop FAIL should inline sluice's journal, got %q", got.Output)
				}
			}
		})
	}
}

// TestDefaultChecksProveSluiceCameUp guards the wiring rather than the logic:
// the check above is only worth anything if the battery `setup`'s verify pass
// and `doctor` actually run contains it, and AnyFail is what turns it into a
// non-zero exit and a suppressed "== sparkbox is provisioned ==" banner.
func TestDefaultChecksProveSluiceCameUp(t *testing.T) {
	var found bool
	for _, c := range DefaultChecks() {
		if c.Name == "sluice service" {
			found = true
		}
	}
	if !found {
		t.Fatal("DefaultChecks must include the sluice liveness check, or setup goes back to reporting a crash loop as success")
	}
	p := newScriptedProbe(map[string][]string{
		showSluiceCmd: {unitState("active", "running", "2", "811", "9000"), unitState("active", "running", "3", "902", "9600")},
	})
	res := RunChecks(p, Config{Sluice: true}, []Check{{"sluice service", checkSluiceService}})
	if !AnyFail(res) {
		t.Fatalf("a crash-looping sluice must make AnyFail true; got %+v", res)
	}
}

// TestPortPreflightToleratesOurOwnSluice: the resolver address is held by
// sluice.service, not by the gateway, so the "is it ours?" test had to learn
// about a second unit. Without this, `setup --sluice` was a one-shot command —
// the second run (an upgrade, say) aborted at the preflight with "in use by
// sluice (pid N)" before the download, and the only remedy was an undocumented
// `systemctl stop sluice` first.
func TestPortPreflightToleratesOurOwnSluice(t *testing.T) {
	const ours = `udp UNCONN 0 0 0.0.0.0:53 0.0.0.0:* users:(("sluice",pid=777,fd=3))
tcp LISTEN 0 4096 0.0.0.0:53 0.0.0.0:* users:(("sluice",pid=777,fd=4))
`
	busy := map[string]bool{"udp/:53": true, "tcp/:53": true}

	t.Run("owning pid is sluice.service's main pid", func(t *testing.T) {
		e, _ := portEnv(t, busy, map[string]string{"ss -lntup": ours})
		e.Cfg.Sluice = true
		e.Probe = fakeProbe{runs: map[string]runResult{
			showSluiceCmd: {out: unitState("active", "running", "0", "777", "9000")},
		}}
		if err := preflightPorts(e); err != nil {
			t.Fatalf("re-running setup --sluice on a live host must not fail: %v", err)
		}
	})

	t.Run("owning process is named sluice even with no systemd answer", func(t *testing.T) {
		e, _ := portEnv(t, busy, map[string]string{"ss -lntup": ours})
		e.Cfg.Sluice = true
		if err := preflightPorts(e); err != nil {
			t.Fatalf("a sluice-owned resolver port must not read as a conflict: %v", err)
		}
	})

	t.Run("a stranger on :53 still fails, and names the flag that moves it", func(t *testing.T) {
		// systemd-resolved is the likeliest thing on :53 of any stock Ubuntu
		// host, and it is a real conflict. The remedy sentence used to list four
		// flags and omit --sluice-dns-addr — the only one that would move this
		// probe — so the operator was told there was a flag and not which.
		const resolved = `udp UNCONN 0 0 0.0.0.0:53 0.0.0.0:* users:(("systemd-resolve",pid=610,fd=12))
`
		e, _ := portEnv(t, busy, map[string]string{"ss -lntup": resolved})
		e.Cfg.Sluice = true
		err := preflightPorts(e)
		if err == nil {
			t.Fatal("a foreign listener on the resolver address must fail the preflight")
		}
		for _, want := range []string{"--sluice-dns-addr", "systemd-resolve", "610"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should name %q:\n%v", want, err)
			}
		}
	})
}

// TestSluiceResolverAddressIsCreated: recommending an address is not creating
// one. validateSluice tells an operator whose gateway already runs the wildcard
// DNS responder to give sluice `--sluice-dns-addr 172.30.0.53:53`, and nothing
// put that address on the host — so the bind failed with EADDRNOTAVAIL (which
// the port preflight steps over by design), Restart=always looped forever, and
// every guest handed that literal had no DNS at all. sparkbox-net.sh now creates
// it on a dummy interface, from this key.
func TestSluiceResolverAddressIsCreated(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string // "" == no key at all
	}{
		{"concrete resolver address", Config{Sluice: true, SluiceDNSAddr: "172.30.0.53:53"}, "172.30.0.53"},
		// A wildcard binds every address the host already has, so there is
		// nothing to create — and writing the key anyway would have the boot
		// script add a bogus interface.
		{"wildcard needs no address of its own", Config{Sluice: true}, ""},
		{"no sluice, no key", Config{SluiceDNSAddr: "172.30.0.53:53"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.sluiceResolverIP(); got != tt.want {
				t.Fatalf("sluiceResolverIP() = %q, want %q", got, tt.want)
			}
			e := &Env{Cfg: tt.cfg}
			var have string
			for _, s := range e.managedEnv(map[string]string{}) {
				if s.key == "SLUICE_DNS_IP" {
					have = s.val
				}
			}
			if have != tt.want {
				t.Errorf("managedEnv SLUICE_DNS_IP = %q, want %q", have, tt.want)
			}
		})
	}
	// And the boot script must actually consume it: the env key alone would be
	// a value nobody reads, which is the same silent nothing it replaced.
	if !strings.Contains(string(deploy.NetScript), "SLUICE_DNS_IP") ||
		!strings.Contains(string(deploy.NetScript), "sparkdns") {
		t.Error("deploy/sparkbox-net.sh must create SLUICE_DNS_IP on its dummy interface")
	}
}
