package hostsetup

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/vanpelt/sparky/tools/sparkbox/deploy"
)

// Installing sluice — the per-VM egress gateway (DNS allowlist + eBPF
// meter/enforcer) that lives in tools/sluice.
//
// # Why this could not be written until now
//
// A2-A5 added `--guest-dns` and `--sluice-socket` and a doctor check that
// reports a gateway with neither, and stopped there with a TODO: tools/sluice
// is a SEPARATE Go module, no CI built it, and no release published a binary,
// so `setup` could offer to talk to a daemon somebody else had installed but
// could not put one on the box. Inventing a download URL for an asset that did
// not exist would have been worse than saying so.
//
// The release now publishes sluice-linux-<arch> with a SHA256_SLUICE in the
// manifest (hack/stage-artifacts.sh), and go.yml builds, vets and tests the
// module on every push. So this step is an ordinary artifact fetch.
//
// # Why shipping a prebuilt binary is sound
//
// The question that had to be answered first, because the wrong answer is an
// asset that installs cleanly and fails to load on somebody's kernel: sluice
// embeds a COMPILED eBPF object (internal/meter/sluice_bpfel.o, committed to
// the repo and pulled in with //go:embed), so there is no clang anywhere in the
// path — but a compiled BPF object can be fragile in two ways, and neither
// applies here.
//
//   - Architecture: it is not host machine code. The object's ELF e_machine is
//     EM_BPF; BPF is its own instruction set, and the only target property that
//     matters is endianness (hence "bpfel"). amd64 and arm64 are both little
//     endian, so one object serves both release arches.
//   - Kernel version: bpf/sluice.c is deliberately CO-RE-free — fixed
//     Ethernet/IP offsets read through bpf_skb_load_bytes, UAPI headers only, no
//     vmlinux.h and no BPF_CORE_READ. The compiled object bears that out: its
//     .BTF.ext carries core_relo_len = 0, i.e. zero CO-RE relocations, so there
//     is no kernel struct layout for a different kernel to invalidate. The one
//     kernel type it names is struct __sk_buff, which is UAPI and whose field
//     accesses the verifier rewrites per kernel by design.
//
// Guest kernels never enter into it at all: sluice runs on the HOST and
// attaches to the host side of each guest's sbtap device.
//
// What IS version-dependent is the runtime attach path, and it is a property of
// the box rather than of the artifact — see kernelSupportsSluice.
//
// # What this step lays down
//
// The binary, an allowlist, an env file and a unit. Three of the four have
// non-obvious rules, each recording a way to get a daemon that is "active" and
// useless:
//
//	/usr/local/bin/sluice     fetched + sha256-verified, 0755
//	<root>/allowlist.txt      SEEDED ONLY IF ABSENT. sluice exits 1 when the
//	                          file its --allowlist names is not there, so not
//	                          writing it is a Restart=always crash loop;
//	                          rewriting it would revert an operator's policy on
//	                          an upgrade run.
//	<root>/sluice.env         SEEDED ONLY IF ABSENT, same argument — SLUICE_ARGS
//	                          is the operator's editing surface, exactly like
//	                          sparkbox.env's flag bundles.
//	sluice.service            REWRITTEN to match, byte for byte, like every
//	                          other unit: it is generated from this config, so a
//	                          stale one is a host running a configuration no
//	                          `setup` invocation describes (F0's staleness).

// sluiceServiceParams fills the sluice.service template.
type sluiceServiceParams struct {
	BinPath       string
	EnvPath       string
	AllowlistPath string
	DNSAddr       string
	SocketPath    string
	TapPrefix     string
}

// renderSluiceService renders sluice.service for cfg. binPath is where the
// binary was installed — an Env field rather than a Config one, for the same
// reason SystemdDir and SbinDir are: it is a system path no operator has ever
// needed to move, and a test that could not redirect it would have to write to
// the developer's real /usr/local/bin.
func renderSluiceService(cfg Config, binPath string) (string, error) {
	tmpl, err := template.New("sluice").Parse(deploy.SluiceServiceTemplate)
	if err != nil {
		return "", fmt.Errorf("parse sluice service template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, sluiceServiceParams{
		BinPath:       binPath,
		EnvPath:       cfg.sluiceEnvPath(),
		AllowlistPath: cfg.sluiceAllowlistPath(),
		DNSAddr:       cfg.sluiceDNSAddr(),
		SocketPath:    cfg.sluiceSocket(),
		TapPrefix:     sluiceTapPrefix,
	}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// sluiceEnvSeed is the initial <root>/sluice.env.
//
// `--enforce --open-untagged` is the sparkbox tag model, and it is the safe
// default in the direction that matters: enforcement applies only to sandboxes
// whose tags carry a network rule, which the gateway pushes over the control
// socket. A sandbox nobody has written a policy for keeps unrestricted egress
// rather than losing the internet the moment an operator types --sluice.
//
// Dropping --open-untagged enforces the base allowlist on every VM instead.
// That is a real choice with real consequences for existing sandboxes, so it is
// documented here rather than made on the operator's behalf.
func sluiceEnvSeed(cfg Config) string {
	var b strings.Builder
	b.WriteString("# sluice runtime flags, sourced by " + sluiceUnit + ".\n")
	b.WriteString("# Referenced unbraced in the unit so systemd word-splits it (empty -> zero args);\n")
	b.WriteString("# see the note in sparkbox.service about ${VAR} vs $VAR.\n")
	b.WriteString("#\n")
	b.WriteString("# --enforce      turn the eBPF drop path on (without it sluice only meters)\n")
	b.WriteString("# --open-untagged  enforce ONLY on sandboxes whose tags carry a network rule.\n")
	b.WriteString("#                Drop it to enforce " + cfg.sluiceAllowlistPath() + " on every VM,\n")
	b.WriteString("#                which will cut off sandboxes that are running right now.\n")
	b.WriteString("#\n")
	b.WriteString("# `sparkbox setup` writes this file once and never rewrites it: it is yours.\n")
	b.WriteString("SLUICE_ARGS=--enforce --open-untagged\n")
	return b.String()
}

// kernelSupportsSluice reports whether this host's kernel can run sluice's data
// plane, and what it is.
//
// The floor is 6.6, and it is about the attach API rather than about BPF: the
// meter attaches its two TC programs with a TCX link (link.AttachTCX), which
// landed in 6.6. Below it the program still LOADS — so the daemon starts, logs
// nothing alarming and sits there "active" — and every Attach fails, which is
// to say it meters nothing and enforces nothing.
//
// The unit carries ConditionKernelVersion=>=6.6 as well, and that is not enough
// by itself: systemd SKIPS a unit whose condition fails, and `systemctl start`
// on a skipped unit exits 0. Installing on a 6.1 box would have printed a clean
// provisioning report over an egress filter that never ran — F7's shape, with a
// different cause. So setup refuses up front.
//
// ok is false when the version cannot be determined at all. That is NOT treated
// as a refusal: /proc/sys is missing in some container images, and stranding a
// host because a proc file could not be read would be worse than installing and
// letting doctor report what actually happened. The caller says so out loud.
func kernelSupportsSluice(p Probe) (release string, supported, ok bool) {
	if p == nil {
		return "", false, false
	}
	v, err := p.Sysctl("kernel.osrelease")
	if err != nil {
		return "", false, false
	}
	v = strings.TrimSpace(v)
	major, minor, parsed := parseKernelVersion(v)
	if !parsed {
		return v, false, false
	}
	if major != minSluiceKernelMajor {
		return v, major > minSluiceKernelMajor, true
	}
	return v, minor >= minSluiceKernelMinor, true
}

// parseKernelVersion pulls the major and minor out of a uname release string
// ("6.8.0-31-generic", "6.14.9-sparkbox-poc", "6.1.155"). Anything that does not
// start with <int>.<int> is not something to guess about.
func parseKernelVersion(v string) (major, minor int, ok bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	// The minor may carry a suffix on a two-component release ("6.6-rc1").
	m := parts[1]
	if i := strings.IndexFunc(m, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		m = m[:i]
	}
	minor, err = strconv.Atoi(m)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// sluiceSeedFiles are the files written only when absent — the operator's, not
// ours. Listed once so the "already there" check and the write agree.
func sluiceSeedFiles(e *Env) []fileAsset {
	return []fileAsset{
		{e.Cfg.sluiceAllowlistPath(), deploy.SluiceAllowlistSeed, 0o644},
		{e.Cfg.sluiceEnvPath(), []byte(sluiceEnvSeed(e.Cfg)), 0o644},
	}
}

func stepSluice() Step {
	return Step{
		Name: "sluice",
		Satisfied: func(e *Env) (bool, string, error) {
			if !e.Cfg.Sluice {
				// Named rather than silent. "No egress control" is the default
				// and nothing else about a green run hints at it; printConnect
				// and doctor say the same thing in their own words.
				return true, "skipped (--sluice not set; sandboxes reach the whole internet)", nil
			}
			// The binary first, and against the manifest's checksum rather than
			// mere existence — the same lesson as fetch-artifacts. A host that
			// upgraded from a release with an older sluice would otherwise keep
			// running the old daemon forever, and unlike the gateway there is no
			// version banner anywhere that would show it.
			fi, err := os.Stat(e.SluiceBinPath)
			if err != nil || fi.Size() == 0 {
				return false, "", nil
			}
			if want := e.Manifest.SHA256Sluice; want != "" {
				have, herr := sha256File(e.SluiceBinPath)
				if herr != nil {
					return false, "", fmt.Errorf("hash %s: %w", e.SluiceBinPath, herr)
				}
				if have != want {
					return false, "", nil
				}
			}
			for _, a := range sluiceSeedFiles(e) {
				if _, err := os.Stat(a.path); err != nil {
					return false, "", nil
				}
			}
			unit, err := renderSluiceService(e.Cfg, e.SluiceBinPath)
			if err != nil {
				return false, "", err
			}
			got, err := os.ReadFile(filepath.Join(e.SystemdDir, sluiceUnit))
			if err != nil || string(got) != unit {
				return false, "", nil
			}
			if e.Manifest.Release == "" {
				// --dry-run: resolve-release only runs its Apply, so there is no
				// checksum to have compared against. Do not imply one happened.
				return true, "sluice installed (release unresolved — binary sha unchecked)", nil
			}
			return true, "sluice matches " + e.Manifest.Release + ", unit current", nil
		},
		Plan: func(e *Env) string {
			var notes []string
			// A dry run is the last cheap moment to learn the host cannot run
			// this at all, so say it here rather than letting Apply be the first
			// to find out on the real run.
			if release, supported, known := kernelSupportsSluice(e.Probe); known && !supported {
				notes = append(notes, fmt.Sprintf("REFUSED on this host: kernel %s is below %d.%d, where sluice's TCX attach exists",
					release, minSluiceKernelMajor, minSluiceKernelMinor))
			} else if !known {
				notes = append(notes, "kernel version unreadable — the "+
					fmt.Sprintf("%d.%d", minSluiceKernelMajor, minSluiceKernelMinor)+" floor cannot be checked here")
			}
			line := fmt.Sprintf("install sluice to %s, seed %s + %s, write %s (resolver %s, socket %s)",
				e.SluiceBinPath, e.Cfg.sluiceAllowlistPath(), e.Cfg.sluiceEnvPath(),
				sluiceUnit, e.Cfg.sluiceDNSAddr(), e.Cfg.sluiceSocket())
			if len(notes) > 0 {
				line += " [" + strings.Join(notes, "; ") + "]"
			}
			return line
		},
		Apply: func(e *Env) error {
			release, supported, known := kernelSupportsSluice(e.Probe)
			switch {
			case known && !supported:
				return fmt.Errorf("this host runs kernel %s and sluice needs >= %d.%d: "+
					"its meter attaches with a TCX link, which does not exist below that, so the daemon would start, "+
					"attach to nothing, and report itself healthy while every sandbox stayed unfiltered. "+
					"Upgrade the kernel, or drop --sluice and leave the gateway honestly unfiltered",
					release, minSluiceKernelMajor, minSluiceKernelMinor)
			case !known:
				// Proceeding rather than refusing: /proc/sys is absent in some
				// container images and a host with an unreadable version is far
				// more likely to be fine than not. Say it, so a later "sluice is
				// installed but attached to nothing" is not a mystery.
				e.logf("   WARNING: could not read this host's kernel version, so the >= %d.%d floor was not checked; "+
					"if the meter attaches to nothing, that is why (journalctl -u %s)\n",
					minSluiceKernelMajor, minSluiceKernelMinor, sluiceUnit)
			}

			if e.Manifest.Release == "" {
				return fmt.Errorf("no manifest resolved (resolve-release must run first)")
			}
			art, ok := e.Manifest.Sluice(e.Cfg.ArtifactBase, e.SluiceBinPath)
			if !ok {
				// The manifest is the authority on what a release contains, so
				// this is a fact rather than a guess. Guessing the URL is what
				// the TODO this step replaces refused to do.
				return fmt.Errorf("release %s publishes no sluice binary (its manifest has no SLUICE_ASSET/SHA256_SLUICE), "+
					"so --sluice cannot be honoured: pin a release that ships one (--release <tag>), "+
					"or build tools/sluice yourself and install it to %s before re-running without --sluice",
					e.Manifest.Release, e.SluiceBinPath)
			}
			dl, err := downloadVerify(e.Ctx, e.Fetch, art)
			if err != nil {
				return err
			}
			verb := "present"
			if dl {
				verb = "downloaded"
			}
			e.logf("   sluice binary: %s (%s)\n", verb, e.SluiceBinPath)

			// Seeds. Absent-only, and the log says which way it went — an
			// operator who expects a fresh allowlist and gets their old one is
			// owed the reason on the spot rather than after reading the file.
			for _, a := range sluiceSeedFiles(e) {
				if _, err := os.Stat(a.path); err == nil {
					e.logf("   %s exists — left alone (it is yours to edit; setup never rewrites it)\n", a.path)
					continue
				}
				if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(a.path, a.body, a.mode); err != nil {
					return err
				}
				e.logf("   seeded %s\n", a.path)
			}

			unit, err := renderSluiceService(e.Cfg, e.SluiceBinPath)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(e.SystemdDir, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(e.SystemdDir, sluiceUnit), []byte(unit), 0o644); err != nil {
				return err
			}
			// systemd holds the previous text until daemon-reload and a running
			// unit keeps its old ExecStart until restarted; both belong to
			// enable-services. Same contract as UnitsChanged.
			e.SluiceChanged = true
			return nil
		},
	}
}
