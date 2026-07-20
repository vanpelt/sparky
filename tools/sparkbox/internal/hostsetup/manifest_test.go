package hostsetup

import (
	"context"
	"io"
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
	src := `RELEASE=20260720-abc
FIRECRACKER_VERSION=v1.7.0
SHA256_VMLINUX=aaa
SHA256_FIRECRACKER=bbb
SHA256_SPARKBOX=ccc
ROOTFS_NAME=universal
ROOTFS_PATH=rootfs/deadbeef00000000/universal.ext4.zst
SHA256_ROOTFS=ddd
ROOTFS_LOGIN_USER=sparky
`
	m, err := ParseManifest(strings.NewReader(src), "20260720-abc")
	if err != nil {
		t.Fatal(err)
	}
	if m.Release != "20260720-abc" || m.RootfsName != "universal" ||
		m.RootfsPath != "rootfs/deadbeef00000000/universal.ext4.zst" ||
		m.SHA256Rootfs != "ddd" || m.RootfsLogin != "sparky" || m.SHA256Vmlinux != "aaa" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}

func TestParseManifestLegacyFallbacks(t *testing.T) {
	// A manifest predating ROOTFS_PATH / SHA256_ROOTFS / ROOTFS_LOGIN_USER.
	src := `RELEASE=old
SHA256_VMLINUX=aaa
SHA256_ROOTFS_GZ=eee
`
	m, err := ParseManifest(strings.NewReader(src), "old")
	if err != nil {
		t.Fatal(err)
	}
	if m.RootfsName != "ubuntu" {
		t.Errorf("RootfsName fallback = %q, want ubuntu", m.RootfsName)
	}
	if m.RootfsPath != "releases/old/ubuntu.ext4.gz" {
		t.Errorf("RootfsPath fallback = %q", m.RootfsPath)
	}
	if m.SHA256Rootfs != "eee" {
		t.Errorf("SHA256_ROOTFS should fall back to SHA256_ROOTFS_GZ, got %q", m.SHA256Rootfs)
	}
	if m.RootfsLogin != "root" {
		t.Errorf("RootfsLogin fallback = %q, want root", m.RootfsLogin)
	}
}

func TestResolveRelease(t *testing.T) {
	base := "https://ex.test"
	f := mapFetcher{base + "/latest.env": "RELEASE=20260720-xyz\n"}
	got, err := ResolveRelease(context.Background(), base, "latest", f)
	if err != nil {
		t.Fatal(err)
	}
	if got != "20260720-xyz" {
		t.Fatalf("resolved %q", got)
	}
	// A concrete tag is returned without fetching.
	got, err = ResolveRelease(context.Background(), base, "pinned", nil)
	if err != nil || got != "pinned" {
		t.Fatalf("pinned tag: %q %v", got, err)
	}
}

func TestArtifactsURLs(t *testing.T) {
	cfg := Config{
		KernelPath:     "/srv/sparkbox/vmlinux",
		FirecrackerBin: "/usr/local/bin/firecracker",
		ImageDir:       "/srv/sparkbox/data/images",
	}
	m := Manifest{
		Release: "r1", SHA256Vmlinux: "a", SHA256Firecrkr: "b",
		RootfsName: "universal", RootfsPath: "rootfs/key/universal.ext4.zst", SHA256Rootfs: "d",
	}
	arts := m.Artifacts("https://ex.test/", cfg)
	byName := map[string]Artifact{}
	for _, a := range arts {
		byName[a.Name] = a
	}
	if len(arts) != 3 {
		t.Fatalf("want 3 artifacts (no sparkbox self-fetch), got %d", len(arts))
	}
	if byName["vmlinux"].URL != "https://ex.test/releases/r1/vmlinux" {
		t.Errorf("vmlinux URL = %q", byName["vmlinux"].URL)
	}
	if byName["firecracker"].Mode != 0o755 {
		t.Errorf("firecracker should be executable")
	}
	if byName["rootfs"].URL != "https://ex.test/rootfs/key/universal.ext4.zst" {
		t.Errorf("rootfs URL = %q", byName["rootfs"].URL)
	}
	if byName["rootfs"].Dest != "/srv/sparkbox/data/images/universal.ext4.zst" {
		t.Errorf("rootfs dest = %q", byName["rootfs"].Dest)
	}
	if ManifestURL("https://ex.test", "r1") != "https://ex.test/releases/r1/manifest.env" {
		t.Errorf("manifest URL wrong")
	}
}
