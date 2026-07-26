package hostsetup

import (
	"fmt"
	"io"
	"runtime"
	"strings"
)

// Manifest is the subset of a release's manifest file that setup consumes.
// Release artifacts live in a GitHub Release's flat asset namespace, so every
// name carries its arch and one release serves both linux/amd64 and linux/arm64
// hosts (see .github/workflows/build-artifacts.yml and hack/stage-artifacts.sh,
// which produce exactly these names).
//
// # Why darwin gets its OWN manifest instead of sharing the linux one
//
// A release now also ships sparkbox-darwin-arm64, and most of what a Mac needs
// to know about a release is what a linux host needs to know — which is the
// argument for one shared file. Three things settle it the other way:
//
//  1. The names would collide, silently. The manifest a host reads used to be
//     chosen by runtime.GOARCH alone, with no OS dimension, so a darwin/arm64
//     Mac asking for "its" manifest fetched manifest-arm64.env — the LINUX
//     arm64 one — and Artifacts() would then have handed it firecracker-arm64,
//     a linux ELF, as if it were the Mac's own. Nothing would have failed until
//     something tried to exec it. manifestAsset now takes the OS too.
//  2. A Mac and a linux host do not install the same sparkbox. The Mac's own
//     binary is sparkbox-darwin-arm64; the binary it installs into the nested
//     machine it provisions is sparkbox-linux-arm64. One file cannot carry two
//     SHA256_SPARKBOX values without one of them being a lie.
//  3. B2 adds vmlinux-macos-arm64 — the outer KVM kernel — which only a Mac
//     ever downloads. There is no honest place for its checksum in a file that
//     linux hosts also read and would have to ignore.
//
// So manifest-darwin-arm64.env pins the same tag and repeats the linux guest
// keys verbatim (a Mac provisions exactly those artifacts into its machine),
// and adds the darwin-only ones on top. hack/stage-darwin-artifacts.sh derives
// it FROM manifest-arm64.env for that reason: the shared checksums have one
// source, so the two files cannot disagree about what a release contains.
type Manifest struct {
	Release string
	Arch    string // amd64 | arm64 — the arch these assets are for
	// Platform is the host OS this manifest serves: "linux" or "darwin".
	// Every manifest published before darwin shipped carries no PLATFORM key,
	// so it defaults to linux and those releases keep parsing unchanged.
	Platform       string
	SHA256Vmlinux  string
	SHA256Firecrkr string
	RootfsName     string
	RootfsAsset    string // release asset name, e.g. universal-arm64.ext4.zst
	SHA256Rootfs   string
	RootfsLogin    string // guest account the gateway SSHes in as
	// SparkboxAsset / SHA256Sparkbox describe the sparkbox binary built for
	// Platform/Arch. setup does not download it — stepInstallBinary copies the
	// running executable, which is the whole point of F0 — but a checksum is
	// what lets doctor or a curl-style installer say whether the file on disk
	// is the release it claims to be. SHA256_SPARKBOX has been emitted by
	// stage-artifacts.sh since the first release and read by nothing until now.
	SparkboxAsset  string
	SHA256Sparkbox string
	// SluiceAsset / SHA256Sluice describe the sluice binary — the per-VM egress
	// gateway (DNS allowlist + eBPF meter/enforcer) that `setup --sluice`
	// installs. Always the LINUX build on either platform: sluice attaches eBPF
	// to the host side of guest taps, and on darwin those taps live inside the
	// nested machine, so a Mac ships it there the same way it ships the rootfs.
	// That is why it is not MACHINE_* the way the machine's sparkbox is — there
	// is no darwin sluice to tell it apart from.
	//
	// Empty on any release cut before sluice was published, which is what makes
	// `--sluice` refusable with a precise reason ("this release has no sluice
	// asset") instead of a 404 from a URL we invented.
	SluiceAsset  string
	SHA256Sluice string
	// MachineSparkboxAsset / SHA256MachineSparkbox are the LINUX sparkbox a
	// darwin host installs into the nested machine it provisions (Workstream
	// B4). "Machine" and not "guest" on purpose: in this codebase a guest is a
	// firecracker microVM, while the macOS machine is the linux VM that runs
	// the gateway and creates those microVMs. Empty on a linux manifest, where
	// the host binary and the binary being installed are the same file.
	MachineSparkboxAsset  string
	SHA256MachineSparkbox string
	// OuterKernelAsset / SHA256OuterKernel are the macOS OUTER kernel: the
	// KVM-capable arm64 Image that Apple's `container machine` boots, inside
	// which the linux gateway runs firecracker. Two kernels ship in a release
	// and confusing them is easy — SHA256Vmlinux above is the GUEST kernel a
	// microVM boots, and it is not KVM-capable — so this one is named for the
	// platform rather than the arch and appears only in a darwin manifest.
	//
	// Empty on every linux manifest, and on any darwin manifest cut before B2.
	// It is what replaces compiling Linux 6.14.9 on the user's laptop as an
	// onboarding step (macos/kernel/build.sh survives as the escape hatch).
	//
	// The checksum is an integrity claim about the published file, not a
	// promise that a rebuild reproduces it: the builder's gcc/binutils versions
	// are compiled into the kernel banner and the Ubuntu archive that supplies
	// them is not pinned. See the header of macos/kernel/build.sh.
	OuterKernelAsset  string
	SHA256OuterKernel string
}

// hostArch is the arch suffix this host's assets carry, and hostOS the OS half
// of the platform. sparkbox ships linux/amd64, linux/arm64 and darwin/arm64;
// on anything else there are no artifacts to fetch and the caller surfaces the
// 404 with the URL in hand.
func hostArch() string { return runtime.GOARCH }
func hostOS() string   { return runtime.GOOS }

// Asset names. The manifest names itself so a host can fetch its own platform's
// manifest before knowing anything else about the release.
//
// linux keeps the historical unqualified spelling: every release already
// published names it manifest-<arch>.env, and a linux host pinned to one of
// those tags has to go on resolving it. The OS qualifier is therefore added
// only for the platforms that did not exist before it.
func manifestAsset(goos, arch string) string {
	if goos == "" || goos == "linux" {
		return "manifest-" + arch + ".env"
	}
	return "manifest-" + goos + "-" + arch + ".env"
}

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

// platform returns the manifest's host OS, defaulting to linux — which is what
// every manifest cut before darwin shipped implicitly is.
func (m Manifest) platform() string {
	if m.Platform != "" {
		return m.Platform
	}
	return "linux"
}

// ParseManifest reads KEY=VALUE lines from a release's manifest file
// (manifest-<arch>.env on linux, manifest-<os>-<arch>.env elsewhere). release
// is the tag that was requested, used when the file omits RELEASE=.
func ParseManifest(r io.Reader, release string) (Manifest, error) {
	kv, err := parseEnv(r)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{
		Release:               firstNonEmpty(kv["RELEASE"], release),
		Arch:                  firstNonEmpty(kv["ARCH"], hostArch()),
		Platform:              firstNonEmpty(kv["PLATFORM"], "linux"),
		SHA256Vmlinux:         kv["SHA256_VMLINUX"],
		SHA256Firecrkr:        kv["SHA256_FIRECRACKER"],
		RootfsName:            firstNonEmpty(kv["ROOTFS_NAME"], "universal"),
		RootfsLogin:           firstNonEmpty(kv["ROOTFS_LOGIN_USER"], "root"),
		SHA256Rootfs:          kv["SHA256_ROOTFS"],
		SHA256Sparkbox:        kv["SHA256_SPARKBOX"],
		SluiceAsset:           kv["SLUICE_ASSET"],
		SHA256Sluice:          kv["SHA256_SLUICE"],
		MachineSparkboxAsset:  kv["MACHINE_SPARKBOX_ASSET"],
		SHA256MachineSparkbox: kv["SHA256_MACHINE_SPARKBOX"],
		OuterKernelAsset:      kv["OUTER_KERNEL_ASSET"],
		SHA256OuterKernel:     kv["SHA256_OUTER_KERNEL"],
	}
	// Asset names are data, not code, wherever the <thing>-<goarch> mould does
	// not fit — the same reason ROOTFS_ASSET is a key. The derivations below
	// are only what an older manifest that omits them must mean.
	m.RootfsAsset = firstNonEmpty(kv["ROOTFS_ASSET"],
		fmt.Sprintf("%s-%s.ext4.zst", m.RootfsName, m.arch()))
	m.SparkboxAsset = firstNonEmpty(kv["SPARKBOX_ASSET"],
		fmt.Sprintf("sparkbox-%s-%s", m.platform(), m.arch()))
	return m, nil
}

// CheckPlatform rejects a manifest describing a different OS than the host
// about to provision from it.
//
// This exists because the failure it catches is invisible: before manifest
// names carried an OS, a Mac resolved manifest-arm64.env and got a file whose
// every key is well-formed and whose every checksum is correct — for linux
// binaries. Nothing detects that by inspection; the only tell is the PLATFORM
// key. An operator who points --artifact-base at a mirror, or who hand-writes
// a manifest, deserves to be told at resolve time rather than at exec time.
func (m Manifest) CheckPlatform(goos string) error {
	if goos == "" {
		goos = "linux"
	}
	if got := m.platform(); got != goos {
		return fmt.Errorf("manifest %s describes %s hosts but this host is %s "+
			"(wrong --artifact-base, or a release that predates %s support)",
			manifestAsset(got, m.arch()), got, goos, goos)
	}
	return nil
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

// ManifestURL returns the URL of this host's platform-specific manifest for the
// given release ("latest" allowed). A darwin host resolves
// manifest-darwin-arm64.env; a linux host keeps resolving manifest-<arch>.env.
//
// A Mac on a release cut before B1 therefore gets a 404 naming the asset it
// wanted, which is the correct answer: that release genuinely has nothing for
// it, and the alternative — quietly reading the linux manifest — is the bug
// this function exists to prevent.
func ManifestURL(base, release string) string {
	return assetURL(base, release, manifestAsset(hostOS(), hostArch()))
}

// Artifacts computes the download set for a release: kernel, firecracker, and
// the rootfs. sparkbox itself is NOT fetched — the running binary is already
// sparkbox, and stepInstallBinary copies *it* to Config.BinPath rather than
// downloading a second copy that could differ from the one being run.
// Everything resolves against m.Release (the concrete tag the
// manifest reported), never "latest": a release published mid-setup would
// otherwise hand us a kernel and a rootfs from different builds.
//
// These three are the same three on either platform, and deliberately so: they
// are what a *gateway* needs, and on darwin the gateway is the nested linux
// machine rather than the Mac. The darwin manifest repeats the linux arm64
// checksums verbatim so that B4 can hand this exact set to the machine it
// creates and know it matches the tag the Mac pinned. Nothing here is written
// to a Mac's own filesystem — the Dest paths come from Config and describe the
// machine's layout.
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

// SparkboxURL is where the sparkbox binary for this manifest's platform lives.
// Nothing in setup downloads it (see Artifacts), but doctor and the release
// notes both need to be able to name it.
func (m Manifest) SparkboxURL(base string) string {
	return assetURL(base, m.Release, m.SparkboxAsset)
}

// MachineSparkbox is the linux sparkbox binary a darwin host installs into the
// machine it provisions, ready to hand to downloadVerify. ok is false on any
// manifest that does not name one — every linux manifest, and any darwin
// manifest missing the checksum.
//
// The checksum is required rather than optional because downloadVerify treats
// an empty SHA256 as "do not verify". Silently skipping verification for the
// one artifact that is an executable we are about to run as root is not a
// degradation worth having; a caller that gets ok=false can say so.
func (m Manifest) MachineSparkbox(base, dest string) (Artifact, bool) {
	if m.MachineSparkboxAsset == "" || m.SHA256MachineSparkbox == "" {
		return Artifact{}, false
	}
	return Artifact{
		Name:   "sparkbox (machine)",
		URL:    assetURL(base, m.Release, m.MachineSparkboxAsset),
		SHA256: m.SHA256MachineSparkbox,
		Dest:   dest,
		Mode:   0o755,
	}, true
}

// Sluice is the egress gateway binary this manifest publishes, ready to hand to
// downloadVerify. ok is false on any release cut before sluice was published,
// and the caller must then refuse `--sluice` rather than guess a URL: the whole
// point of naming the asset in the manifest is that "this release has one" is a
// fact we read instead of a convention we assume.
//
// NOT part of Artifacts(). Those three are unconditional — every gateway needs
// a kernel, a VMM and a rootfs — while sluice is opt-in, and folding an
// optional download into the set would mean Artifacts() returning a different
// length depending on Config, with stepFetchArtifacts' Satisfied check having
// to grow a matching conditional. The install has its own step, so it owns its
// own download, exactly like the outer kernel does on darwin.
//
// The checksum is required rather than optional for the same reason it is on
// MachineSparkbox and OuterKernel: downloadVerify treats an empty SHA256 as "do
// not verify", and this is a binary systemd is about to run as root with
// CAP_BPF and CAP_NET_ADMIN.
func (m Manifest) Sluice(base, dest string) (Artifact, bool) {
	if m.SluiceAsset == "" || m.SHA256Sluice == "" {
		return Artifact{}, false
	}
	return Artifact{
		Name:   "sluice",
		URL:    assetURL(base, m.Release, m.SluiceAsset),
		SHA256: m.SHA256Sluice,
		Dest:   dest,
		Mode:   0o755,
	}, true
}

// OuterKernel is the macOS outer kernel this manifest publishes, ready to hand
// to downloadVerify. ok is false on any manifest that does not name one, which
// is every linux manifest and every darwin manifest cut before B2 shipped.
//
// It is deliberately NOT part of Artifacts(): those three are what a *gateway*
// needs, and on darwin the gateway is the nested linux machine, so a Mac hands
// that set onward rather than downloading it for itself. This file is the one
// artifact a Mac fetches for its own use, before any machine exists to have a
// layout — hence a separate accessor with the destination passed in.
//
// The checksum is required rather than optional for the same reason it is on
// MachineSparkbox: downloadVerify treats an empty SHA256 as "do not verify",
// and this file is the kernel the host's hypervisor is about to boot. A caller
// that gets ok=false can tell the operator to build it locally instead.
func (m Manifest) OuterKernel(base, dest string) (Artifact, bool) {
	if m.OuterKernelAsset == "" || m.SHA256OuterKernel == "" {
		return Artifact{}, false
	}
	return Artifact{
		Name:   "vmlinux (macOS machine)",
		URL:    assetURL(base, m.Release, m.OuterKernelAsset),
		SHA256: m.SHA256OuterKernel,
		Dest:   dest,
	}, true
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
