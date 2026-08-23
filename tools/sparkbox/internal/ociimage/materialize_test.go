package ociimage

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ext4"
)

// testRegistry starts an in-process OCI registry and returns its host:port.
// Everything below is hermetic — no network, no daemon, no fixtures on disk.
func testRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// layerFrom builds a single-layer image content from a set of files.
func layerFrom(t *testing.T, files map[string]string) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Directories first so the entries land in a valid order. usr/share/doc is
	// deliberately 0555 with a file in it: that is the ordinary shape of a real
	// rootfs, and it is what makes a plain os.RemoveAll of the staging tree fail
	// for a non-root build. Every image these tests push carries one so the
	// cleanup path is covered rather than assumed.
	for _, dir := range []struct {
		name string
		mode int64
	}{
		{"etc/", 0o755}, {"usr/", 0o755}, {"usr/local/", 0o755},
		{"usr/local/sbin/", 0o755}, {"usr/share/", 0o755}, {"usr/share/doc/", 0o555},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: dir.name, Typeflag: tar.TypeDir, Mode: dir.mode}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "usr/share/doc/copyright", Typeflag: tar.TypeReg, Mode: 0o444, Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("MIT\n")); err != nil {
		t.Fatal(err)
	}
	for path, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: path, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

// pushImage publishes an image with the given files and labels, returning its
// full reference.
func pushImage(t *testing.T, host, repo, tag string, files map[string]string, labels map[string]string) string {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layerFrom(t, files))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.DeepCopy()
	cfg.OS = "linux"
	cfg.Architecture = "amd64"
	cfg.Config.Labels = labels
	if img, err = mutate.ConfigFile(img, cfg); err != nil {
		t.Fatal(err)
	}

	ref := host + "/" + repo + ":" + tag
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parsed, img, remote.WithAuth(authn.Anonymous)); err != nil {
		t.Fatal(err)
	}
	return ref
}

// testPuller matches the platform the test images declare, so the same test
// runs on an amd64 CI box and an arm64 laptop.
func testPuller() *Puller {
	return NewPuller(WithKeychain(anonymousKeychain{}), WithPlatform("linux", "amd64"))
}

// anonymousKeychain answers every registry with anonymous access, so the tests
// never consult the developer's ~/.docker/config.json.
type anonymousKeychain struct{}

func (anonymousKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return authn.Anonymous, nil
}

func requireTools(t *testing.T) {
	t.Helper()
	if err := ext4.Available(); err != nil {
		t.Skipf("skipping: %v", err)
	}
}

func TestResolveReadsDigestAndLabels(t *testing.T) {
	host := testRegistry(t)
	ref := pushImage(t, host, "sparkbox-rootfs", "edge",
		map[string]string{"etc/hostname": "sparky\n"},
		map[string]string{LoginUserLabel: "sparky"})

	img, err := testPuller().Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasPrefix(img.Digest, "sha256:") {
		t.Errorf("Digest = %q", img.Digest)
	}
	if got := img.LoginUser(); got != "sparky" {
		t.Errorf("LoginUser = %q, want sparky", got)
	}
}

// An image that predates the label is a root-login image; that is what every
// template was before hack/images/Dockerfile declared one.
func TestLoginUserDefaultsToRoot(t *testing.T) {
	host := testRegistry(t)
	ref := pushImage(t, host, "bare", "v1", map[string]string{"etc/hostname": "x\n"}, nil)
	img, err := testPuller().Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := img.LoginUser(); got != "root" {
		t.Errorf("LoginUser = %q, want root", got)
	}
}

func TestMaterializeBuildsATemplateFromARegistry(t *testing.T) {
	requireTools(t)
	host := testRegistry(t)
	ref := pushImage(t, host, "sparkbox-rootfs", "edge", map[string]string{
		"etc/hostname":                   "\n",
		"usr/local/sbin/sparkbox-netcfg": "#!/bin/sh\nexit 0\n",
	}, map[string]string{LoginUserLabel: "sparky"})

	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{Ref: ref, SizeMB: 64, OverlayRevision: "1", AllowUnownedFiles: true}

	tmpl, err := cache.Materialize(context.Background(), testPuller(), spec)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if !strings.HasPrefix(tmpl, "oci-") {
		t.Errorf("template name = %q", tmpl)
	}

	// The template must be a real ext4 the driver can reflink, holding the
	// image's files.
	got, err := ext4.ReadFile(context.Background(), cache.Path(tmpl), "/usr/local/sbin/sparkbox-netcfg")
	if err != nil {
		t.Fatalf("read back from the template: %v", err)
	}
	if string(got) != "#!/bin/sh\nexit 0\n" {
		t.Errorf("content = %q", got)
	}
}

func TestMaterializeRunsTheOverlay(t *testing.T) {
	requireTools(t)
	host := testRegistry(t)
	ref := pushImage(t, host, "sparkbox-rootfs", "edge",
		map[string]string{"etc/hostname": "\n"},
		map[string]string{LoginUserLabel: "sparky"})

	var sawLoginUser string
	spec := Spec{
		Ref: ref, SizeMB: 64, OverlayRevision: "1", AllowUnownedFiles: true,
		Overlay: func(_ context.Context, root string, img *Image) error {
			sawLoginUser = img.LoginUser()
			dir := filepath.Join(root, "home/sparky/.ssh")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "authorized_keys"), []byte("ssh-ed25519 AAAA gw\n"), 0o600)
		},
	}
	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := cache.Materialize(context.Background(), testPuller(), spec)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if sawLoginUser != "sparky" {
		t.Errorf("overlay saw login user %q", sawLoginUser)
	}
	got, err := ext4.ReadFile(context.Background(), cache.Path(tmpl), "/home/sparky/.ssh/authorized_keys")
	if err != nil {
		t.Fatalf("overlay output missing from the template: %v", err)
	}
	if !strings.Contains(string(got), "ssh-ed25519") {
		t.Errorf("authorized_keys = %q", got)
	}
}

// A second Materialize for the same inputs must not rebuild. This is the
// difference between a manifest fetch and a full pull on every sandbox create.
func TestMaterializeReusesACachedTemplate(t *testing.T) {
	requireTools(t)
	host := testRegistry(t)
	ref := pushImage(t, host, "sparkbox-rootfs", "edge", map[string]string{"etc/hostname": "\n"}, nil)

	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	overlayRuns := 0
	spec := Spec{
		Ref: ref, SizeMB: 64, OverlayRevision: "1", AllowUnownedFiles: true,
		Overlay: func(context.Context, string, *Image) error { overlayRuns++; return nil },
	}

	first, err := cache.Materialize(context.Background(), testPuller(), spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Materialize(context.Background(), testPuller(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("template name changed between calls: %q then %q", first, second)
	}
	if overlayRuns != 1 {
		t.Errorf("overlay ran %d times, want 1 — the cache did not hit", overlayRuns)
	}
}

// Everything baked into a template is part of its identity. A host that reused
// a template across a different gateway key would be serving sandboxes holding
// another host's key.
func TestTemplateNameCoversEveryBakedInput(t *testing.T) {
	base := Spec{SizeMB: 25600, OverlayRevision: "3", OverlayInputs: [][]byte{[]byte("gateway-key-a")}}
	name := TemplateName("sha256:aaa", base)

	vary := func(mutate func(*Spec)) string {
		s := base
		mutate(&s)
		return TemplateName("sha256:aaa", s)
	}
	cases := map[string]string{
		"overlay revision": vary(func(s *Spec) { s.OverlayRevision = "4" }),
		"size ceiling":     vary(func(s *Spec) { s.SizeMB = 51200 }),
		"gateway key":      vary(func(s *Spec) { s.OverlayInputs = [][]byte{[]byte("gateway-key-b")} }),
		"image digest":     TemplateName("sha256:bbb", base),
	}
	for what, got := range cases {
		if got == name {
			t.Errorf("changing the %s did not change the template name", what)
		}
	}
	if TemplateName("sha256:aaa", base) != name {
		t.Error("TemplateName is not deterministic")
	}
}

// Length-prefixing the hashed fields is what stops two different field splits
// from hashing the same. Without it these two specs collide.
func TestTemplateNameResistsFieldSplitCollisions(t *testing.T) {
	a := TemplateName("sha256:aa", Spec{OverlayRevision: "bb", SizeMB: 1})
	b := TemplateName("sha256:a", Spec{OverlayRevision: "abb", SizeMB: 1})
	if a == b {
		t.Error("distinct specs produced the same template name")
	}
}

func TestMaterializeRefusesASizeTheImageDoesNotFitIn(t *testing.T) {
	requireTools(t)
	host := testRegistry(t)
	ref := pushImage(t, host, "big", "v1",
		map[string]string{"etc/payload": strings.Repeat("x", 8<<20)}, nil)

	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = cache.Materialize(context.Background(), testPuller(),
		Spec{Ref: ref, SizeMB: 4, OverlayRevision: "1", AllowUnownedFiles: true})
	if err == nil {
		t.Fatal("Materialize accepted a ceiling smaller than the image")
	}
	if !strings.Contains(err.Error(), "needs at least") {
		t.Errorf("error should name the size needed, got: %v", err)
	}
}

// A failed build must leave nothing behind — neither a half-built template nor
// the multi-gigabyte staging tree.
func TestMaterializeLeavesNoDebrisOnFailure(t *testing.T) {
	requireTools(t)
	host := testRegistry(t)
	ref := pushImage(t, host, "sparkbox-rootfs", "edge", map[string]string{"etc/hostname": "\n"}, nil)

	dir := t.TempDir()
	cache, err := NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cache.Materialize(context.Background(), testPuller(), Spec{
		Ref: ref, SizeMB: 64, OverlayRevision: "1", AllowUnownedFiles: true,
		Overlay: func(context.Context, string, *Image) error { return io.ErrUnexpectedEOF },
	})
	if err == nil {
		t.Fatal("Materialize succeeded despite a failing overlay")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("failed build left %v in the cache directory", names)
	}
}

func TestMaterializeRefusesUnownedFilesByDefault(t *testing.T) {
	requireTools(t)
	if os.Geteuid() == 0 {
		t.Skip("skipping: running as root, ownership always applies")
	}
	host := testRegistry(t)
	ref := pushImage(t, host, "sparkbox-rootfs", "edge", map[string]string{"etc/hostname": "\n"}, nil)
	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = cache.Materialize(context.Background(), testPuller(),
		Spec{Ref: ref, SizeMB: 64, OverlayRevision: "1"})
	if err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Errorf("want an ownership refusal, got %v", err)
	}
}

// Prune must only ever reclaim templates this package minted. An image
// directory also holds hand-managed base templates and snapshot templates, and
// neither is ours to delete.
func TestPruneOnlyTouchesItsOwnTemplates(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oci-keep.ext4", "oci-stale.ext4", "universal.ext4", "snap-mine.ext4", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := cache.Prune(map[string]bool{"oci-keep": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "oci-stale" {
		t.Errorf("removed = %v, want [oci-stale]", removed)
	}
	for _, name := range []string{"oci-keep.ext4", "universal.ext4", "snap-mine.ext4", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("Prune removed %s, which is not its to remove", name)
		}
	}
}

func TestMaterializeValidatesItsSpec(t *testing.T) {
	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Materialize(context.Background(), testPuller(), Spec{Ref: "x", SizeMB: 0}); err == nil {
		t.Error("want an error for a zero size ceiling")
	}
}

func TestNewCacheRequiresADirectory(t *testing.T) {
	if _, err := NewCache(""); err == nil {
		t.Error("want an error for an empty cache directory")
	}
}
