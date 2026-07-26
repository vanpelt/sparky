package hostsetup

import (
	"fmt"
	"strconv"
	"strings"
)

// The passthrough policy for the inner `sparkbox setup`.
//
// # The rule
//
// The Mac does not re-derive the operator's intent; it FORWARDS it. Everything
// in Config already describes the machine's layout and the gateway it will run,
// so the inner invocation is the outer one minus the flags that describe the
// Mac, plus the two the Mac has to rewrite (a resolved release tag, and the
// operator key's path inside the machine).
//
// # Why the tables exist
//
// So that a new setup flag has a DEFINED fate. Every flag is in exactly one of
// four sets — always-emitted, forwardable, refused, or Mac-only — and
// TestEverySetupFlagHasAFate (in cmd/sparkbox) fails the build if one is added
// without being classified. A flag silently dropped at this boundary is a
// gateway quietly running a configuration nobody asked for, discovered months
// later; that is F2's shape.
//
// # Why forwarding keys off FlagsGiven
//
// Because a Go flag with a default is indistinguishable from one the operator
// typed, and the inner setup RECONCILES sparkbox.env on every run. Forwarding
// the Mac's compiled-in defaults would let an upgrade run that never mentioned
// --proxy-domain rewrite a live machine's PROXY_DOMAIN. FlagsGiven exists for
// exactly that hazard, and the darwin path inherits it for free.

// forwardableFlag is one setup flag the Mac passes through when the operator
// actually typed it.
type forwardableFlag struct {
	name string
	// render returns the argv words for this flag, or nil when the config value
	// says there is nothing to pass.
	render func(Config) []string
}

func strFlag(name string, get func(Config) string) forwardableFlag {
	return forwardableFlag{name: name, render: func(c Config) []string {
		v := get(c)
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{"--" + name, v}
	}}
}

// intFlag forwards the value UNCONDITIONALLY, zero included.
//
// render is only ever called for a flag the operator actually typed (the loop
// in innerSetupArgs checks flagGiven first), so there is no default to suppress
// here — and suppressing a zero would suppress a CHOICE. `--swap-gb 0` is the
// motivating case and it is not a corner: dropping it left the inner setup to
// apply its own default of 16, putting a 16 GiB swapfile inside a
// thin-provisioned virtual disk where it is charged twice, which is the exact
// thing the untyped-default branch below emits `--swap-gb 0` to prevent. An
// operator asking for it explicitly must not be the one person who loses it.
//
// Contrast boolFlag, which legitimately renders nothing for false: its flags
// are presence flags, so "not typed" and "typed false" produce the same argv by
// construction. An int has no such spelling.
func intFlag(name string, get func(Config) int) forwardableFlag {
	return forwardableFlag{name: name, render: func(c Config) []string {
		return []string{"--" + name, strconv.Itoa(get(c))}
	}}
}

func boolFlag(name string, get func(Config) bool) forwardableFlag {
	return forwardableFlag{name: name, render: func(c Config) []string {
		if !get(c) {
			return nil
		}
		return []string{"--" + name}
	}}
}

// boolValFlag forwards a bool as --name=false / --name=true, and is the correct
// helper for any bool whose DEFAULT IS TRUE.
//
// boolFlag above is fine for presence flags (--sluice, --proxy-tls): they
// default to false, so "not typed" and "typed false" are the same argv by
// construction and rendering nothing for false loses nothing. A default-true
// flag breaks that equivalence in the one direction that matters — an operator
// who typed --agent-tools=false would have it rendered as nothing, the inner
// setup would apply its own default of true, and the Mac would come back with a
// machine baking agent CLIs the operator explicitly declined. Same class as
// intFlag's --swap-gb 0: suppressing the zero value suppresses a CHOICE.
func boolValFlag(name string, get func(Config) bool) forwardableFlag {
	return forwardableFlag{name: name, render: func(c Config) []string {
		return []string{fmt.Sprintf("--%s=%t", name, get(c))}
	}}
}

// forwardableFlags is every inner-meaningful setup flag, in a stable order so
// the rendered command line — which the dry-run plan prints verbatim — does not
// shuffle between runs.
func forwardableFlags() []forwardableFlag {
	return []forwardableFlag{
		// The gateway's base domain. Forwardable and NOT always-emitted, for
		// the reason at the top of this file: --proxy-domain has a compiled-in
		// default (hivemind.tools), the inner setup rebuilds FlagsGiven from
		// its own argv, and managedEnv rewrites PROXY_DOMAIN whenever that flag
		// counts as given. Emitting it unconditionally therefore made every
		// plain `sparkbox setup` upgrade run rewrite a live machine's
		// PROXY_DOMAIN=catnip.sh back to hivemind.tools — moving every sandbox
		// route, both consoles, and the DNS-01 orders onto a domain the box
		// does not own, and reporting PASS while doing it.
		//
		// Nothing is lost by forwarding only when typed: the inner binary is
		// the same release, so its own default is the identical string, and a
		// machine whose sparkbox.env has no PROXY_DOMAIN line yet gets one
		// written from that default anyway (see managedEnv's `|| !present`).
		strFlag("proxy-domain", func(c Config) string { return c.ProxyDomain }),
		// Layout. These describe the MACHINE's filesystem on darwin, which is
		// why they forward rather than being refused.
		strFlag("root", func(c Config) string { return c.Root }),
		strFlag("state-dir", func(c Config) string { return c.StateDir }),
		strFlag("kernel", func(c Config) string { return c.KernelPath }),
		strFlag("image-dir", func(c Config) string { return c.ImageDir }),
		strFlag("users", func(c Config) string { return c.UsersPath }),
		// Listen addresses.
		strFlag("ssh-addr", func(c Config) string { return c.SSHAddr }),
		strFlag("proxy-addr", func(c Config) string { return c.ProxyAddr }),
		strFlag("api-addr", func(c Config) string { return c.APIAddr }),
		strFlag("dns-addr", func(c Config) string { return c.DNSAddr }),
		strFlag("dns-answer", func(c Config) string { return c.DNSAnswer }),
		// Optional subsystems.
		strFlag("edge-ip", func(c Config) string { return c.EdgeIP }),
		intFlag("ssh-advertise-port", func(c Config) int { return c.SSHAdvertisePort }),
		boolFlag("proxy-tls", func(c Config) bool { return c.ProxyTLS }),
		strFlag("tls-provider", func(c Config) string { return c.TLSProvider }),
		strFlag("tls-email", func(c Config) string { return c.TLSEmail }),
		strFlag("guest-dns", func(c Config) string { return c.GuestDNS }),
		boolFlag("sluice", func(c Config) bool { return c.Sluice }),
		// Default-true, so boolValFlag rather than boolFlag — see there.
		boolValFlag("agent-tools", func(c Config) bool { return c.AgentTools }),
		strFlag("sluice-dns-addr", func(c Config) string { return c.SluiceDNSAddr }),
		strFlag("sluice-socket", func(c Config) string { return c.SluiceSocket }),
		strFlag("archive-remote", func(c Config) string { return c.ArchiveRemote }),
		strFlag("archive-bucket", func(c Config) string { return c.ArchiveBucket }),
		// Behaviour.
		boolFlag("force", func(c Config) bool { return c.Force }),
		boolFlag("adopt-legacy", func(c Config) bool { return c.AdoptLegacy }),
		// swap-gb is forwardable AND has a darwin default of its own (0); see
		// innerSetupArgs.
		intFlag("swap-gb", func(c Config) int { return c.SwapGB }),
	}
}

// alwaysEmittedFlags are passed on every darwin run whether or not the operator
// typed them, because for each of them the Mac holds a fact the machine cannot
// re-derive: the release tag the Mac PINNED (letting the inner setup resolve
// its own "latest" would straddle two releases), the artifact base it resolved
// it from, the size of the volume the Mac created, the operator key's path
// INSIDE the machine, and the role this machine was created for.
//
// --proxy-domain deliberately does NOT belong here; see forwardableFlags.
var alwaysEmittedFlags = []string{
	"release", "artifact-base", "data-volume-gb",
	"operator-handle", "operator-key", "gateway", "node-name",
}

// refusedOnDarwin are flags that cannot mean anything here, with the reason.
// Refused loudly rather than dropped silently: an operator who typed
// --move-admin-ssh and got a gateway that did not move any sshd would have no
// way to find out.
var refusedOnDarwin = map[string]string{
	"move-admin-ssh": "the machine has no admin sshd to relocate — it runs one process, the gateway, " +
		"and nothing competes with it for :22 inside the VM",
	"bin-path": "the machine's sparkbox is installed by the inner setup from the binary the bootstrap " +
		"staged, so there is no path here for it to go to; the Mac's own binary is not what runs the gateway",
}

// darwinOnlyFlags describe the Mac and are therefore never forwarded.
var darwinOnlyFlags = []string{
	"machine-name", "machine-cpus", "machine-memory-gb", "machine-image",
	"outer-kernel", "container-bin",
	// --dry-run is deliberately not forwarded: a darwin dry run never reaches
	// the machine at all.
	"dry-run",
}

// FlagFate classifies a `sparkbox setup` flag for the darwin boundary. It is
// exported so cmd/sparkbox's test can walk the real FlagSet and assert that
// every declared flag lands in exactly one bucket.
func FlagFate(name string) string {
	for _, n := range alwaysEmittedFlags {
		if n == name {
			return "always"
		}
	}
	for _, f := range forwardableFlags() {
		if f.name == name {
			return "forwardable"
		}
	}
	if _, ok := refusedOnDarwin[name]; ok {
		return "refused"
	}
	for _, n := range darwinOnlyFlags {
		if n == name {
			return "darwin-only"
		}
	}
	return ""
}

// innerSetupArgs builds the argv for the `sparkbox setup` that runs INSIDE the
// machine. Pure function, table-tested, and the single place the passthrough
// policy lives.
//
// resolvedTag is the concrete release the Mac pinned — never "latest": a
// release published mid-setup would otherwise straddle the Mac's outer kernel
// and the machine's rootfs. keyPath is where the operator's public key was
// staged inside the machine; the key TEXT never touches argv.
func innerSetupArgs(cfg Config, resolvedTag, keyPath string) ([]string, error) {
	for name, why := range refusedOnDarwin {
		if cfg.flagGiven(name) {
			return nil, fmt.Errorf("--%s cannot be used on macOS: %s", name, why)
		}
	}

	args := []string{"setup"}
	if strings.TrimSpace(resolvedTag) == "" {
		return nil, fmt.Errorf("no resolved release tag for the inner setup (resolve-release must run first)")
	}
	args = append(args, "--release", resolvedTag)
	if b := strings.TrimSpace(cfg.ArtifactBase); b != "" {
		args = append(args, "--artifact-base", b)
	}
	if cfg.DataVolumeGB > 0 {
		args = append(args, "--data-volume-gb", strconv.Itoa(cfg.DataVolumeGB))
	}

	// Role. The two node flags are already validated by cmd/sparkbox's
	// validateNodeFlags, so the darwin path inherits the whitespace and
	// name rules for free.
	if g := strings.TrimSpace(cfg.Gateway); g != "" {
		args = append(args, "--gateway", g)
		if n := strings.TrimSpace(cfg.NodeName); n != "" {
			args = append(args, "--node-name", n)
		}
	} else {
		if h := strings.TrimSpace(cfg.OperatorHandle); h != "" {
			args = append(args, "--operator-handle", h)
		}
		if strings.TrimSpace(keyPath) == "" {
			return nil, fmt.Errorf("no operator key path inside the machine (the key must be staged before setup runs)")
		}
		args = append(args, "--operator-key", keyPath)
	}

	// Swap, with poc.sh's rule kept: 0 unless the operator asked otherwise. The
	// machine's virtual disk is thin-provisioned, so a swapfile inside it is
	// charged twice — once as guest blocks and once as host image growth.
	if !cfg.flagGiven("swap-gb") {
		args = append(args, "--swap-gb", "0")
	}

	// Everything else the operator ACTUALLY typed.
	for _, f := range forwardableFlags() {
		if !cfg.flagGiven(f.name) {
			continue
		}
		args = append(args, f.render(cfg)...)
	}
	return args, nil
}
