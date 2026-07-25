package hostsetup

import (
	"fmt"
	"io"
	"runtime"
	"strings"
)

// Manifest is the subset of a release's manifest-<arch>.env that setup
// consumes. Release artifacts live in a GitHub Release's flat asset namespace,
// so every name carries its arch and one release serves both linux/amd64 and
// linux/arm64 hosts (see .github/workflows/build-artifacts.yml and
// hack/stage-artifacts.sh, which produce exactly these names).
type Manifest struct {
	Release        string
	Arch           string // amd64 | arm64 — the arch these assets are for
	SHA256Vmlinux  string
	SHA256Firecrkr string
	RootfsName     string
	RootfsAsset    string // release asset name, e.g. universal-arm64.ext4.zst
	SHA256Rootfs   string
	RootfsLogin    string // guest account the gateway SSHes in as
}

// hostArch is the arch suffix this host's assets carry. sparkbox only ships
// linux/amd64 + linux/arm64; on anything else there are no artifacts to fetch
// and the caller surfaces the 404 with the URL in hand.
func hostArch() string { return runtime.GOARCH }

// Asset names. The manifest names itself so a host can fetch its own arch's
// manifest before knowing anything else about the release.
func manifestAsset(arch string) string    { return "manifest-" + arch + ".env" }
func kernelAsset(arch string) string      { return "vmlinux-" + arch }
func firecrackerAsset(arch string) string { return "firecracker-" + arch }

// arch returns the manifest's arch, defaulting to this host's when the field is
// absent.
func (m Manifest) arch() string {
	if m.Arch != "" {
		return m.Arch
	}
	return hostArch()
}

// ParseManifest reads KEY=VALUE lines from a release's manifest-<arch>.env.
// release is the tag that was requested, used when the file omits RELEASE=.
func ParseManifest(r io.Reader, release string) (Manifest, error) {
	kv, err := parseEnv(r)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{
		Release:        firstNonEmpty(kv["RELEASE"], release),
		Arch:           firstNonEmpty(kv["ARCH"], hostArch()),
		SHA256Vmlinux:  kv["SHA256_VMLINUX"],
		SHA256Firecrkr: kv["SHA256_FIRECRACKER"],
		RootfsName:     firstNonEmpty(kv["ROOTFS_NAME"], "universal"),
		RootfsLogin:    firstNonEmpty(kv["ROOTFS_LOGIN_USER"], "root"),
		SHA256Rootfs:   kv["SHA256_ROOTFS"],
	}
	m.RootfsAsset = firstNonEmpty(kv["ROOTFS_ASSET"],
		fmt.Sprintf("%s-%s.ext4.zst", m.RootfsName, m.arch()))
	return m, nil
}

// assetURL builds the download URL for one release asset. An empty or "latest"
// release uses GitHub's /releases/latest/download/ redirect, which resolves to
// the newest non-prerelease release — that redirect IS our "latest" pointer, so
// nothing has to publish a separate index file.
func assetURL(base, release, name string) string {
	base = strings.TrimRight(base, "/")
	if release == "" || release == "latest" {
		return base + "/latest/download/" + name
	}
	return base + "/download/" + release + "/" + name
}

// ManifestURL returns the URL of this host's arch-specific manifest for the
// given release ("latest" allowed).
func ManifestURL(base, release string) string {
	return assetURL(base, release, manifestAsset(hostArch()))
}

// Artifacts computes the download set for a release: kernel, firecracker, and
// the rootfs. sparkbox itself is NOT fetched — the running binary is already
// sparkbox, and stepInstallBinary copies *it* to Config.BinPath rather than
// downloading a second copy that could differ from the one being run.
// Everything resolves against m.Release (the concrete tag the
// manifest reported), never "latest": a release published mid-setup would
// otherwise hand us a kernel and a rootfs from different builds.
func (m Manifest) Artifacts(base string, cfg Config) []Artifact {
	arch := m.arch()
	return []Artifact{
		{
			Name: "vmlinux", URL: assetURL(base, m.Release, kernelAsset(arch)),
			SHA256: m.SHA256Vmlinux, Dest: cfg.KernelPath,
		},
		{
			Name: "firecracker", URL: assetURL(base, m.Release, firecrackerAsset(arch)),
			SHA256: m.SHA256Firecrkr, Dest: cfg.FirecrackerBin, Mode: 0o755,
		},
		{
			// The arch suffix belongs to the *asset* namespace, not to disk: the
			// dest drops it so the template decompresses to <rootfs-name>.ext4,
			// which is what --default-image resolves (cfg.rootfsPath()). Keep the
			// .zst though — the decompress step dispatches on the extension.
			Name: "rootfs", URL: assetURL(base, m.Release, m.RootfsAsset),
			SHA256: m.SHA256Rootfs, Dest: cfg.ImageDir + "/" + m.RootfsName + ".ext4.zst",
		},
	}
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
