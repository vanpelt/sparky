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

func TestManifestURLLatestAndPinned(t *testing.T) {
	const base = "https://github.com/vanpelt/sparky/releases"
	name := "manifest-" + runtime.GOARCH + ".env"
	// "latest" rides GitHub's redirect; a concrete tag addresses the release
	// directly. A trailing slash on the base must not double up.
	if got, want := ManifestURL(base+"/", "latest"), base+"/latest/download/"+name; got != want {
		t.Errorf("latest manifest URL = %q, want %q", got, want)
	}
	if got, want := ManifestURL(base, "v0.3.0"), base+"/download/v0.3.0/"+name; got != want {
		t.Errorf("pinned manifest URL = %q, want %q", got, want)
	}
	if got, want := ManifestURL(base, ""), base+"/latest/download/"+name; got != want {
		t.Errorf("empty release URL = %q, want %q", got, want)
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
