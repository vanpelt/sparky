package hostsetup

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
)

// mapFetcher serves canned bodies keyed by URL.
type mapFetcher map[string]string

func (m mapFetcher) Get(_ context.Context, url string) (io.ReadCloser, error) {
	if v, ok := m[url]; ok {
		return io.NopCloser(strings.NewReader(v)), nil
	}
	return nil, io.EOF
}

func TestParseManifestModern(t *testing.T) {
	src := `RELEASE=v0.3.0
ARCH=arm64
FIRECRACKER_VERSION=v1.16.1
SHA256_VMLINUX=aaa
SHA256_FIRECRACKER=bbb
SHA256_SPARKBOX=ccc
SLUICE_ASSET=sluice-linux-arm64
SHA256_SLUICE=eee
ROOTFS_NAME=universal
ROOTFS_ASSET=universal-arm64.ext4.zst
SHA256_ROOTFS=ddd
ROOTFS_LOGIN_USER=sparky
`
	m, err := ParseManifest(strings.NewReader(src), "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if m.Release != "v0.3.0" || m.Arch != "arm64" || m.RootfsName != "universal" ||
		m.RootfsAsset != "universal-arm64.ext4.zst" ||
		m.SHA256Rootfs != "ddd" || m.RootfsLogin != "sparky" || m.SHA256Vmlinux != "aaa" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if m.SluiceAsset != "sluice-linux-arm64" || m.SHA256Sluice != "eee" {
		t.Fatalf("sluice keys not read: %+v", m)
	}
}

// TestSluiceAssetIsNotDerivedFromTheArch pins a deliberate asymmetry with
// ROOTFS_ASSET and SPARKBOX_ASSET, both of which fall back to a
// <thing>-<arch> spelling when the key is absent.
//
// sluice must NOT: a release cut before the binary was published has no such
// asset, and deriving the name would turn "this release does not ship one" into
// a plausible URL and a 404 from downloadVerify. Emptiness is the signal
// stepSluice reads to refuse --sluice with a reason.
func TestSluiceAssetIsNotDerivedFromTheArch(t *testing.T) {
	m, err := ParseManifest(strings.NewReader("RELEASE=v0.4.0\nARCH=arm64\n"), "v0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if m.SluiceAsset != "" || m.SHA256Sluice != "" {
		t.Errorf("a pre-sluice release must report no sluice asset, got %q/%q", m.SluiceAsset, m.SHA256Sluice)
	}
	// While the two that DO derive keep deriving, so this change cannot have
	// quietly altered them.
	if want := "sparkbox-linux-arm64"; m.SparkboxAsset != want {
		t.Errorf("SparkboxAsset = %q, want the derived %q", m.SparkboxAsset, want)
	}
	if _, ok := m.Sluice(DefaultArtifactBase, "/usr/local/bin/sluice"); ok {
		t.Error("Sluice() must report no artifact for a release that publishes none")
	}
}

func TestParseManifestFallbacks(t *testing.T) {
	// A manifest omitting ARCH / ROOTFS_ASSET / ROOTFS_LOGIN_USER: the arch
	// defaults to this host's, and the rootfs asset is derived from it.
	src := `RELEASE=old
SHA256_VMLINUX=aaa
`
	m, err := ParseManifest(strings.NewReader(src), "old")
	if err != nil {
		t.Fatal(err)
	}
	if m.RootfsName != "universal" {
		t.Errorf("RootfsName fallback = %q, want universal", m.RootfsName)
	}
	if want := "universal-" + runtime.GOARCH + ".ext4.zst"; m.RootfsAsset != want {
		t.Errorf("RootfsAsset fallback = %q, want %q", m.RootfsAsset, want)
	}
	if m.Arch != runtime.GOARCH {
		t.Errorf("Arch fallback = %q, want %q", m.Arch, runtime.GOARCH)
	}
	if m.RootfsLogin != "root" {
		t.Errorf("RootfsLogin fallback = %q, want root", m.RootfsLogin)
	}
}

// manifestAsset carries an OS dimension only where it has to. The linux name
// is frozen: every release already published spells it manifest-<arch>.env, so
// a linux host pinned to an old tag must go on resolving exactly that. Adding
// the qualifier there instead would 404 every existing release.
func TestManifestAssetNames(t *testing.T) {
	for _, tc := range []struct{ goos, arch, want string }{
		{"linux", "amd64", "manifest-amd64.env"},
		{"linux", "arm64", "manifest-arm64.env"},
		{"darwin", "arm64", "manifest-darwin-arm64.env"},
		// An empty GOOS can only come from a hand-built Manifest literal in a
		// test; treat it as linux rather than emitting "manifest--arm64.env".
		{"", "arm64", "manifest-arm64.env"},
	} {
		if got := manifestAsset(tc.goos, tc.arch); got != tc.want {
			t.Errorf("manifestAsset(%q, %q) = %q, want %q", tc.goos, tc.arch, got, tc.want)
		}
	}
}

func TestManifestURLLatestAndPinned(t *testing.T) {
	const base = "https://github.com/vanpelt/sparky/releases"
	// Whatever this host is: on darwin that is manifest-darwin-arm64.env, and
	// resolving the linux one instead is the bug ManifestURL exists to stop —
	// it parses cleanly and every checksum in it is right, for someone else's
	// binaries.
	name := manifestAsset(runtime.GOOS, runtime.GOARCH)
	goos, arch := runtime.GOOS, runtime.GOARCH
	// "latest" rides GitHub's redirect; a concrete tag addresses the release
	// directly. A trailing slash on the base must not double up.
	if got, want := ManifestURL(goos, arch, base+"/", "latest"), base+"/latest/download/"+name; got != want {
		t.Errorf("latest manifest URL = %q, want %q", got, want)
	}
	if got, want := ManifestURL(goos, arch, base, "v0.3.0"), base+"/download/v0.3.0/"+name; got != want {
		t.Errorf("pinned manifest URL = %q, want %q", got, want)
	}
	if got, want := ManifestURL(goos, arch, base, ""), base+"/latest/download/"+name; got != want {
		t.Errorf("empty release URL = %q, want %q", got, want)
	}
	// The platform is a PARAMETER now, so a linux CI runner can ask for the URL
	// a Mac would resolve — which is what makes the whole darwin pipeline
	// testable off a Mac.
	if got, want := ManifestURL("darwin", "arm64", base, "v0.4.0"),
		base+"/download/v0.4.0/manifest-darwin-arm64.env"; got != want {
		t.Errorf("darwin manifest URL = %q, want %q", got, want)
	}
}

func TestArtifactsURLs(t *testing.T) {
	cfg := Config{
		KernelPath:     "/srv/sparkbox/vmlinux",
		FirecrackerBin: "/usr/local/bin/firecracker",
		ImageDir:       "/srv/sparkbox/data/images",
	}
	m := Manifest{
		Release: "v0.3.0", Arch: "arm64", SHA256Vmlinux: "a", SHA256Firecrkr: "b",
		RootfsName: "universal", RootfsAsset: "universal-arm64.ext4.zst", SHA256Rootfs: "d",
	}
	arts := m.Artifacts("https://github.com/vanpelt/sparky/releases/", cfg)
	byName := map[string]Artifact{}
	for _, a := range arts {
		byName[a.Name] = a
	}
	if len(arts) != 3 {
		t.Fatalf("want 3 artifacts (no sparkbox self-fetch), got %d", len(arts))
	}
	const dl = "https://github.com/vanpelt/sparky/releases/download/v0.3.0/"
	if byName["vmlinux"].URL != dl+"vmlinux-arm64" {
		t.Errorf("vmlinux URL = %q", byName["vmlinux"].URL)
	}
	if byName["firecracker"].URL != dl+"firecracker-arm64" {
		t.Errorf("firecracker URL = %q", byName["firecracker"].URL)
	}
	if byName["firecracker"].Mode != 0o755 {
		t.Errorf("firecracker should be executable")
	}
	if byName["rootfs"].URL != dl+"universal-arm64.ext4.zst" {
		t.Errorf("rootfs URL = %q", byName["rootfs"].URL)
	}
	// The asset is arch-suffixed; what lands on disk is not — it must decompress
	// to the universal.ext4 the server's --default-image resolves.
	if byName["rootfs"].Dest != "/srv/sparkbox/data/images/universal.ext4.zst" {
		t.Errorf("rootfs dest = %q", byName["rootfs"].Dest)
	}
}

// The darwin manifest, exactly as hack/stage-darwin-artifacts.sh derives it
// from manifest-arm64.env. Two things it must get right: SHA256_SPARKBOX means
// "the binary THIS host runs" on both platforms (so on darwin it is the darwin
// build, and the linux one it installs into the machine moves to
// MACHINE_SPARKBOX_*), and every guest key is the linux arm64 value verbatim.
func TestParseManifestDarwin(t *testing.T) {
	src := `RELEASE=v0.5.0
ARCH=arm64
PLATFORM=darwin
FIRECRACKER_VERSION=v1.16.1
SHA256_VMLINUX=aaa
SHA256_FIRECRACKER=bbb
SPARKBOX_ASSET=sparkbox-darwin-arm64
SHA256_SPARKBOX=mac
MACHINE_SPARKBOX_ASSET=sparkbox-linux-arm64
SHA256_MACHINE_SPARKBOX=lin
OUTER_KERNEL_ASSET=vmlinux-macos-arm64
SHA256_OUTER_KERNEL=mackern
ROOTFS_NAME=universal
ROOTFS_ASSET=universal-arm64.ext4.zst
SHA256_ROOTFS=ddd
ROOTFS_LOGIN_USER=sparky
`
	m, err := ParseManifest(strings.NewReader(src), "v0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"Platform", m.Platform, "darwin"},
		// ARCH is the arch of the LINUX assets this Mac provisions, which on
		// Apple Silicon is also the Mac's own arch.
		{"Arch", m.Arch, "arm64"},
		{"SparkboxAsset", m.SparkboxAsset, "sparkbox-darwin-arm64"},
		{"SHA256Sparkbox", m.SHA256Sparkbox, "mac"},
		{"MachineSparkboxAsset", m.MachineSparkboxAsset, "sparkbox-linux-arm64"},
		{"SHA256MachineSparkbox", m.SHA256MachineSparkbox, "lin"},
		// The OUTER kernel, which is not SHA256_VMLINUX above: that one is the
		// guest kernel a microVM boots and has no KVM at all.
		{"OuterKernelAsset", m.OuterKernelAsset, "vmlinux-macos-arm64"},
		{"SHA256OuterKernel", m.SHA256OuterKernel, "mackern"},
		{"RootfsAsset", m.RootfsAsset, "universal-arm64.ext4.zst"},
		{"RootfsLogin", m.RootfsLogin, "sparky"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	// The Mac provisions exactly the linux artifacts the linux arm64 manifest
	// describes — same names, same shas, same release — because that is the
	// whole point of pinning them in a second file.
	arts := m.Artifacts("https://ex.test/releases", Config{})
	if len(arts) != 3 {
		t.Fatalf("want the same 3 gateway artifacts on darwin, got %d", len(arts))
	}
	for _, a := range arts {
		if !strings.HasPrefix(a.URL, "https://ex.test/releases/download/v0.5.0/") {
			t.Errorf("%s resolved off-release: %s", a.Name, a.URL)
		}
		if strings.Contains(a.URL, "darwin") {
			t.Errorf("%s is a linux guest asset, not a darwin one: %s", a.Name, a.URL)
		}
		// The Mac's own outer kernel must not leak into the set it hands the
		// machine. Both files are called some flavour of "vmlinux" and only one
		// of them boots a microVM; a machine handed the KVM machine kernel as
		// its guest kernel would be a very confusing failure.
		if strings.Contains(a.URL, "macos") {
			t.Errorf("%s is the macOS outer kernel, which no guest wants: %s", a.Name, a.URL)
		}
	}
}

// The outer kernel: the one artifact a Mac downloads for ITSELF rather than for
// the machine it provisions. It replaces compiling Linux on the user's laptop,
// so the thing that must not regress is that an unverifiable one is refused
// outright — downloadVerify reads SHA256 "" as "do not verify", and this is the
// image the host's hypervisor boots.
func TestOuterKernel(t *testing.T) {
	const base = "https://ex.test/releases"
	full := Manifest{
		Release: "v0.5.0", Arch: "arm64", Platform: "darwin",
		OuterKernelAsset: "vmlinux-macos-arm64", SHA256OuterKernel: "kern",
	}
	a, ok := full.OuterKernel(base, "/tmp/out/vmlinux-kvm")
	if !ok {
		t.Fatal("a darwin manifest naming the asset + sha must offer it")
	}
	if a.URL != base+"/download/v0.5.0/vmlinux-macos-arm64" {
		t.Errorf("URL = %q", a.URL)
	}
	if a.SHA256 != "kern" || a.Dest != "/tmp/out/vmlinux-kvm" {
		t.Errorf("artifact = %+v", a)
	}
	// A kernel image is data the VMM reads, not something exec'd, so it stays
	// 0644 — unlike MachineSparkbox, which is 0755.
	if a.Mode != 0 {
		t.Errorf("Mode = %#o, want the 0644 default", a.Mode)
	}
	for _, tc := range []struct {
		name string
		m    Manifest
	}{
		{"no asset name", Manifest{Release: "v0.5.0", SHA256OuterKernel: "kern"}},
		{"no checksum", Manifest{Release: "v0.5.0", OuterKernelAsset: "vmlinux-macos-arm64"}},
		// Every release cut before B2, and every linux release forever.
		{"linux manifest", Manifest{Release: "v0.4.0", Arch: "arm64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.m.OuterKernel(base, "/tmp/out/vmlinux-kvm"); ok {
				t.Error("must not offer an unverifiable kernel")
			}
		})
	}
}

// A manifest published before darwin existed carries no PLATFORM key. It has to
// keep meaning linux, or every host pinned to an older tag breaks.
func TestParseManifestPlatformDefaultsToLinux(t *testing.T) {
	m, err := ParseManifest(strings.NewReader("RELEASE=v0.4.0\nARCH=arm64\n"), "v0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if m.Platform != "linux" {
		t.Errorf("Platform = %q, want linux", m.Platform)
	}
	if m.SparkboxAsset != "sparkbox-linux-arm64" {
		t.Errorf("SparkboxAsset = %q, want sparkbox-linux-arm64", m.SparkboxAsset)
	}
	if _, ok := m.MachineSparkbox("https://ex.test/releases", "/usr/local/bin/sparkbox"); ok {
		t.Error("a linux manifest must not offer a machine binary — the host binary IS it")
	}
}

func TestCheckPlatform(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest Manifest
		goos     string
		wantErr  bool
	}{
		{"linux host, linux manifest", Manifest{Platform: "linux", Arch: "amd64"}, "linux", false},
		{"linux host, legacy manifest", Manifest{Arch: "amd64"}, "linux", false},
		{"darwin host, darwin manifest", Manifest{Platform: "darwin", Arch: "arm64"}, "darwin", false},
		// The one that matters: a Mac handed the linux manifest. Every key in
		// it is well-formed and every checksum correct, so nothing else in the
		// pipeline would have objected.
		{"darwin host, linux manifest", Manifest{Platform: "linux", Arch: "arm64"}, "darwin", true},
		{"darwin host, legacy manifest", Manifest{Arch: "arm64"}, "darwin", true},
		{"linux host, darwin manifest", Manifest{Platform: "darwin", Arch: "arm64"}, "linux", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.manifest.CheckPlatform(tc.goos)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckPlatform(%q) error = %v, wantErr %v", tc.goos, err, tc.wantErr)
			}
			// The message has to name the file that was actually read, or the
			// operator has no way to tell which of the two they fetched.
			if err != nil && !strings.Contains(err.Error(), manifestAsset(tc.manifest.platform(), tc.manifest.Arch)) {
				t.Errorf("error does not name the manifest: %v", err)
			}
		})
	}
}

// The linux sparkbox a Mac installs into the machine it provisions (B4). It is
// the first artifact sparkbox downloads that is an executable it will then run
// as root, so an absent checksum must disable it rather than silently skip
// verification — downloadVerify treats SHA256 "" as "do not verify".
func TestMachineSparkbox(t *testing.T) {
	const base = "https://ex.test/releases"
	full := Manifest{
		Release: "v0.5.0", Arch: "arm64", Platform: "darwin",
		MachineSparkboxAsset: "sparkbox-linux-arm64", SHA256MachineSparkbox: "lin",
	}
	a, ok := full.MachineSparkbox(base, "/usr/local/bin/sparkbox")
	if !ok {
		t.Fatal("a darwin manifest naming the asset + sha must offer it")
	}
	if a.URL != base+"/download/v0.5.0/sparkbox-linux-arm64" {
		t.Errorf("URL = %q", a.URL)
	}
	if a.SHA256 != "lin" || a.Dest != "/usr/local/bin/sparkbox" || a.Mode != 0o755 {
		t.Errorf("artifact = %+v", a)
	}
	for _, tc := range []struct {
		name string
		m    Manifest
	}{
		{"no asset name", Manifest{Release: "v0.5.0", SHA256MachineSparkbox: "lin"}},
		{"no checksum", Manifest{Release: "v0.5.0", MachineSparkboxAsset: "sparkbox-linux-arm64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.m.MachineSparkbox(base, "/usr/local/bin/sparkbox"); ok {
				t.Error("must not offer an unverifiable binary")
			}
		})
	}
}

// A "latest" setup must not fetch artifacts through the latest redirect: the
// manifest's concrete tag pins every asset to the same build.
func TestArtifactsPinToManifestRelease(t *testing.T) {
	m := Manifest{Release: "v0.3.0", Arch: "amd64", RootfsAsset: "universal-amd64.ext4.zst"}
	for _, a := range m.Artifacts("https://ex.test/releases", Config{}) {
		if strings.Contains(a.URL, "/latest/") {
			t.Errorf("%s resolved through the latest redirect: %s", a.Name, a.URL)
		}
	}
}
