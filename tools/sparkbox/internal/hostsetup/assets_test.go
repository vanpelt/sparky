package hostsetup

import (
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/deploy"
)

func TestEmbeddedAssetsNonEmpty(t *testing.T) {
	if len(deploy.NetScript) == 0 || !strings.Contains(string(deploy.NetScript), "SPARKBOX_EDGE") {
		t.Error("NetScript embed missing or wrong")
	}
	if len(deploy.NetService) == 0 || !strings.Contains(string(deploy.NetService), "sparkbox-net.sh") {
		t.Error("NetService embed missing or wrong")
	}
	if len(deploy.SysctlConf) == 0 || !strings.Contains(string(deploy.SysctlConf), "ip_forward") {
		t.Error("SysctlConf embed missing or wrong")
	}
	if !strings.Contains(deploy.StandaloneServiceTemplate, "{{.StateDir}}") {
		t.Error("service template should be a text/template with placeholders")
	}
}

func TestRenderServiceStandalone(t *testing.T) {
	cfg := DefaultConfig()
	// Standalone must NOT reference fleet-only knobs.
	out, err := renderService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "--require-keys") || strings.Contains(out, "--key-dir") {
		t.Error("standalone unit must omit --require-keys/--key-dir (keys are local)")
	}
	if strings.Contains(out, "{{") {
		t.Errorf("unrendered template placeholder remains:\n%s", out)
	}
	if !strings.Contains(out, "--ssh-addr :2222") {
		t.Error("default (admin ssh not moved) should bind the gateway on :2222")
	}
	if !strings.Contains(out, "--state-dir "+cfg.StateDir) {
		t.Error("state-dir not templated")
	}
	// The unit must run the binary setup installs, not a hardcoded path that
	// nothing populates (F0). A misspelled placeholder renders as "<no value>"
	// rather than erroring, so assert the whole ExecStart prefix.
	if !strings.Contains(out, "ExecStart="+cfg.BinPath+" serve ") {
		t.Errorf("ExecStart should run %s\n%s", cfg.BinPath, out)
	}

	cfg.MoveAdminSSH = true
	out, _ = renderService(cfg)
	if !strings.Contains(out, "--ssh-addr :22 ") {
		t.Error("after taking port 22 the gateway should bind :22")
	}
	cfg.MoveAdminSSH = false

	// A custom --bin-path is honoured…
	cfg.BinPath = "/opt/sparkbox/bin/sparkbox"
	out, _ = renderService(cfg)
	if !strings.Contains(out, "ExecStart=/opt/sparkbox/bin/sparkbox serve ") {
		t.Errorf("custom --bin-path not templated into ExecStart\n%s", out)
	}
	// …and a Config that never saw DefaultConfig still renders a runnable unit
	// rather than "ExecStart= serve …".
	out, err = renderService(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ExecStart="+defaultBinPath+" serve ") {
		t.Errorf("empty BinPath should fall back to %s\n%s", defaultBinPath, out)
	}
}

// TestRenderServiceListenAddresses is F1: every address the gateway binds must
// come from a real setup flag and land in the unit as an ordinary templated
// word. Before this, --api-addr was a literal in the template and --proxy-addr
// came from a package constant, so the only way to move either was to re-set it
// from a flag bundle appended later in the same ExecStart and rely on Go's
// last-flag-wins. That worked, and it read like a bug.
func TestRenderServiceListenAddresses(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		out, err := renderService(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		// :8080 is the one default this change exists to kill: it is taken on
		// any workstation-class host (on the DGX by an unrelated python
		// process), so a fresh setup produced a gateway that lost a port race
		// at boot and crash-looped forever.
		if strings.Contains(out, "127.0.0.1:8080") {
			t.Errorf("the control API must not default to :8080 any more:\n%s", out)
		}
		if !strings.Contains(out, "--api-addr 127.0.0.1:8079") {
			t.Errorf("control API should default to 127.0.0.1:8079:\n%s", out)
		}
		if !strings.Contains(out, "--proxy-addr :8081") {
			t.Errorf("edge should default to :8081:\n%s", out)
		}
		// An unset --dns-addr means the responder is off, and the flag is then
		// omitted rather than passed empty: `--dns-addr` followed by
		// --proxy-domain would swallow the domain as its value.
		if strings.Contains(execStart(out), "--dns-addr") {
			t.Errorf("--dns-addr must be omitted when the DNS responder is off:\n%s", out)
		}
	})

	t.Run("the live DGX gateway's binds are reproducible from flags alone", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.SSHAddr = "10.66.0.1:2222"
		cfg.ProxyAddr = "10.66.0.1:443"
		cfg.APIAddr = "127.0.0.1:8079"
		cfg.DNSAddr = "10.66.0.1:53"
		out, err := renderService(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"--ssh-addr 10.66.0.1:2222",
			"--proxy-addr 10.66.0.1:443",
			"--api-addr 127.0.0.1:8079",
			"--dns-addr 10.66.0.1:53",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("unit missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("an explicit --ssh-addr wins over the move-admin-ssh default", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.SSHAddr = "10.66.0.1:2222"
		out, _ := renderService(cfg)
		if !strings.Contains(out, "--ssh-addr 10.66.0.1:2222") {
			t.Errorf("explicit --ssh-addr not templated:\n%s", out)
		}
	})

	t.Run("EXTRA_FLAGS is appended last, after the legacy TLS_FLAGS", func(t *testing.T) {
		cmd := execStart(renderOrDie(t, DefaultConfig()))
		tls := strings.Index(cmd, "$TLS_FLAGS")
		extra := strings.Index(cmd, "$EXTRA_FLAGS")
		if tls < 0 || extra < 0 {
			t.Fatalf("both bundles must be referenced (TLS_FLAGS for hosts provisioned before EXTRA_FLAGS existed):\n%s", cmd)
		}
		if extra < tls {
			t.Error("EXTRA_FLAGS must come after TLS_FLAGS so the current name wins a stale one")
		}
		// Braced would make an empty bundle one empty argv word, which
		// terminates Go's flag parsing and silently drops everything after it.
		if strings.Contains(cmd, "${EXTRA_FLAGS}") || strings.Contains(cmd, "${TLS_FLAGS}") {
			t.Errorf("flag bundles must be referenced unbraced:\n%s", cmd)
		}
	})
}

// dgxConfig is the live DGX gateway's configuration, expressed entirely in
// setup flags. It is the acceptance criterion for A4 ("the DGX's config should
// be reproducible from flags alone"), so it is written out here in full rather
// than assembled per-test: if a future change makes any part of that host
// unreachable from a flag again, this stops compiling or stops passing.
func dgxConfig() Config {
	cfg := DefaultConfig()
	cfg.ProxyDomain = "catnip.sh"
	// A dedicated tailnet /32 for the SSH door and the edge, so the any-port
	// DNATs key off the destination IP and cannot collide with host services.
	cfg.SSHAddr = "10.66.0.1:2222"
	cfg.ProxyAddr = "10.66.0.1:443"
	cfg.EdgeIP = "10.66.0.1"
	// :8080 was already held by an unrelated python process on that box.
	cfg.APIAddr = "127.0.0.1:8079"
	// The built-in dnsedge responder behind the Tailscale split-DNS entry.
	cfg.DNSAddr = "10.66.0.1:53"
	cfg.DNSAnswer = "10.66.0.1"
	// `ssh ctl@catnip.sh` works bare via a :22 -> :2222 DNAT.
	cfg.SSHAdvertisePort = 22
	cfg.ProxyTLS = true
	cfg.TLSProvider = "cloudflare"
	// Per-VM egress control: the sluice resolver on its own dummy address.
	cfg.GuestDNS = "172.30.0.53"
	cfg.SluiceSocket = "/run/sluice.sock"
	// R2 object storage for archived sandboxes.
	cfg.ArchiveRemote = "r2"
	cfg.ArchiveBucket = "catnip-sparkbox"
	return cfg
}

// TestRenderServiceReproducesTheLiveDGXGateway is work item A4's acceptance
// criterion.
//
// Every flag below was on the live DGX unit and none of them had a `setup`
// flag: they were hand-written into sparkbox.env's TLS_FLAGS/EXTRA_FLAGS
// bundles, which the unit appends after the templated flags so a repeated flag
// wins last (F1/F2). That worked, and it meant the host's real configuration
// lived in a file setup never rewrites, in a variable named after a subsystem
// it has nothing to do with, reproducible by nobody.
func TestRenderServiceReproducesTheLiveDGXGateway(t *testing.T) {
	cfg := dgxConfig()
	if err := validateAddrs(cfg); err != nil {
		t.Fatalf("the live DGX configuration must pass validation: %v", err)
	}
	if err := validateSubsystems(cfg); err != nil {
		t.Fatalf("the live DGX configuration must pass validation: %v", err)
	}
	cmd := execStart(renderOrDie(t, cfg))
	for _, want := range []string{
		"--ssh-addr 10.66.0.1:2222",
		"--proxy-addr 10.66.0.1:443",
		"--proxy-tls",
		"--api-addr 127.0.0.1:8079",
		"--dns-addr 10.66.0.1:53",
		"--dns-answer 10.66.0.1",
		"--guest-dns 172.30.0.53",
		"--sluice-socket /run/sluice.sock",
		"--archive-remote r2",
		"--archive-bucket catnip-sparkbox",
		"--ssh-advertise-port 22",
		"--tls-provider cloudflare",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("ExecStart missing %q:\n%s", want, cmd)
		}
	}
	// The last piece of that host's configuration is not a serve flag at all:
	// sparkbox-net.sh reads it from sparkbox.env. It has to be reproducible from
	// the same run, or "reproducible from flags alone" is still not true.
	e := &Env{Cfg: cfg}
	want := map[string]string{"SPARKBOX_EDGE_IP": "10.66.0.1", "SPARKBOX_EDGE_REDIRECT": "0"}
	got := map[string]string{}
	for _, s := range e.managedEnv(nil) {
		if _, ok := want[s.key]; ok {
			got[s.key] = s.val
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("sparkbox.env %s = %q, want %q — the dedicated-edge-IP mode must come from --edge-ip", k, got[k], v)
		}
	}
}

// TestOptionalFlagsAreOmittedWhenUnset guards the rule the template comment
// states: a flag rendered with an empty value becomes an empty argv word, Go's
// flag package treats the first non-flag argument as the end of the flag list,
// and `serve` never looks at fs.Args() — so one stray "" silently drops
// everything after it and the gateway comes up missing half its configuration.
func TestOptionalFlagsAreOmittedWhenUnset(t *testing.T) {
	cmd := execStart(renderOrDie(t, DefaultConfig()))
	for _, flag := range []string{
		"--dns-addr", "--dns-answer", "--proxy-tls", "--tls-provider", "--tls-email",
		"--guest-dns", "--sluice-socket", "--archive-remote", "--archive-bucket",
		"--ssh-advertise-port",
	} {
		if strings.Contains(cmd, flag) {
			t.Errorf("%s must not appear at all when unset:\n%s", flag, cmd)
		}
	}
	// Not one empty word anywhere: every continuation line ends in " \" and no
	// line is a bare backslash.
	for _, line := range strings.Split(cmd, "\n") {
		if strings.TrimSpace(line) == "\\" {
			t.Errorf("empty flag line in ExecStart:\n%s", cmd)
		}
	}

	// Turning exactly one subsystem on adds exactly that one flag.
	cfg := DefaultConfig()
	cfg.ArchiveRemote, cfg.ArchiveBucket = "r2", "bucket"
	cmd = execStart(renderOrDie(t, cfg))
	if !strings.Contains(cmd, "--archive-remote r2") || strings.Contains(cmd, "--guest-dns") {
		t.Errorf("only the configured subsystem should appear:\n%s", cmd)
	}
}

// TestOptionalFlagsRenderStably is not cosmetic: stepSystemdUnits decides
// whether to rewrite the unit (and restart the gateway) by comparing the
// rendered text byte for byte, so a render whose order wobbled would report
// drift and bounce the service on every `sparkbox setup`.
func TestOptionalFlagsRenderStably(t *testing.T) {
	cfg := dgxConfig()
	first := renderOrDie(t, cfg)
	for i := 0; i < 8; i++ {
		if out := renderOrDie(t, cfg); out != first {
			t.Fatalf("render is not deterministic:\n%s\n---\n%s", first, out)
		}
	}
}

// execStart returns just the unit's ExecStart command line. The template's
// comments legitimately name the same flags the command line carries, so an
// assertion about what the gateway is *told* has to look at the command, not
// at the whole file.
func execStart(unit string) string {
	i := strings.Index(unit, "ExecStart=")
	if i < 0 {
		return ""
	}
	rest := unit[i:]
	if j := strings.Index(rest, "\nRestart="); j >= 0 {
		return rest[:j]
	}
	return rest
}

func renderOrDie(t *testing.T, cfg Config) string {
	t.Helper()
	out, err := renderService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
