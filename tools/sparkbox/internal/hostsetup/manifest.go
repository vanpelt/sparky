package hostsetup

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
)

// Manifest is the subset of a release manifest.env that setup consumes. The
// build publishes it at <base>/releases/<tag>/manifest.env alongside the
// artifacts (see hack/build-artifacts.sh).
type Manifest struct {
	Release        string
	SHA256Vmlinux  string
	SHA256Firecrkr string
	RootfsName     string
	RootfsPath     string // bucket-relative, e.g. rootfs/<key>/universal.ext4.zst
	SHA256Rootfs   string
	RootfsLogin    string // guest account the gateway SSHes in as
}

// ParseManifest reads KEY=VALUE lines and applies the same fallbacks the shell
// provisioner uses for manifests predating a field (deploy/cloud-init.yaml's
// sparkbox-provision.sh). release is the resolved tag, used to fill a legacy
// per-release rootfs path when ROOTFS_PATH is absent.
func ParseManifest(r io.Reader, release string) (Manifest, error) {
	kv, err := parseEnv(r)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{
		Release:        firstNonEmpty(kv["RELEASE"], release),
		SHA256Vmlinux:  kv["SHA256_VMLINUX"],
		SHA256Firecrkr: kv["SHA256_FIRECRACKER"],
		RootfsName:     firstNonEmpty(kv["ROOTFS_NAME"], "ubuntu"),
		RootfsLogin:    firstNonEmpty(kv["ROOTFS_LOGIN_USER"], "root"),
		// SHA256_ROOTFS falls back to the legacy gzip field.
		SHA256Rootfs: firstNonEmpty(kv["SHA256_ROOTFS"], kv["SHA256_ROOTFS_GZ"]),
	}
	// ROOTFS_PATH default: the legacy per-release gzip artifact.
	m.RootfsPath = firstNonEmpty(kv["ROOTFS_PATH"],
		fmt.Sprintf("releases/%s/%s.ext4.gz", m.Release, m.RootfsName))
	return m, nil
}

// ResolveRelease turns "latest" into a concrete tag by reading <base>/latest.env
// (RELEASE=<tag>). Any other value is returned unchanged.
func ResolveRelease(ctx context.Context, base, release string, f Fetcher) (string, error) {
	if release != "" && release != "latest" {
		return release, nil
	}
	rc, err := f.Get(ctx, strings.TrimRight(base, "/")+"/latest.env")
	if err != nil {
		return "", fmt.Errorf("resolve latest: %w", err)
	}
	defer rc.Close()
	kv, err := parseEnv(rc)
	if err != nil {
		return "", err
	}
	tag := kv["RELEASE"]
	if tag == "" {
		return "", fmt.Errorf("latest.env has no RELEASE=")
	}
	return tag, nil
}

// Artifact identifies one downloadable file: its URL, its expected sha256, the
// destination path, and whether it must be made executable.
type Artifact struct {
	Name   string
	URL    string
	SHA256 string
	Dest   string
	Mode   uint32 // 0 keeps the default 0644
}

// ManifestURL returns the URL of the release manifest.env.
func ManifestURL(base, release string) string {
	return fmt.Sprintf("%s/releases/%s/manifest.env", strings.TrimRight(base, "/"), release)
}

// Artifacts computes the download set for a release: kernel, firecracker, and
// the rootfs. sparkbox itself is NOT fetched — the running binary is already
// sparkbox. The rootfs dest keeps the bucket basename (so .zst/.gz is preserved
// for the decompress step).
func (m Manifest) Artifacts(base string, cfg Config) []Artifact {
	base = strings.TrimRight(base, "/")
	rel := "releases/" + m.Release
	return []Artifact{
		{
			Name: "vmlinux", URL: base + "/" + rel + "/vmlinux",
			SHA256: m.SHA256Vmlinux, Dest: cfg.KernelPath,
		},
		{
			Name: "firecracker", URL: base + "/" + rel + "/firecracker",
			SHA256: m.SHA256Firecrkr, Dest: cfg.FirecrackerBin, Mode: 0o755,
		},
		{
			Name: "rootfs", URL: base + "/" + strings.TrimLeft(m.RootfsPath, "/"),
			SHA256: m.SHA256Rootfs, Dest: cfg.ImageDir + "/" + path.Base(m.RootfsPath),
		},
	}
}

// parseEnv reads simple KEY=VALUE lines (ignoring blanks and # comments),
// trimming surrounding quotes. Good enough for our machine-generated env files.
func parseEnv(r io.Reader) (map[string]string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
