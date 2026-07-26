package hostsetup

import (
	"strings"
	"testing"
)

// TestValidateSubsystems covers the combinations `sparkbox serve` would either
// crash-loop on at boot or — worse — accept and silently ignore. The silent
// half is the reason this validator exists: an --archive-remote with no bucket
// disables archiving without an error anywhere, so the operator gets a green
// host that does not archive.
func TestValidateSubsystems(t *testing.T) {
	base := func() Config {
		c := DefaultConfig()
		return c
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // "" means it must be accepted
	}{
		{"the defaults turn nothing on", func(c *Config) {}, ""},
		{
			"the whole live DGX configuration",
			func(c *Config) { *c = dgxConfig() },
			"",
		},
		{
			"an archive remote with no bucket is silently ignored by serve",
			func(c *Config) { c.ArchiveRemote = "r2" },
			"must be given together",
		},
		{
			"an archive bucket with no remote likewise",
			func(c *Config) { c.ArchiveBucket = "catnip-sparkbox" },
			"must be given together",
		},
		{
			"a tls provider with no --proxy-tls is never read",
			func(c *Config) { c.TLSProvider = "cloudflare" },
			"need --proxy-tls",
		},
		{
			"an unknown tls provider",
			func(c *Config) { c.ProxyTLS, c.TLSProvider = true, "letsencrypt" },
			"unknown --tls-provider",
		},
		{
			// The firecracker driver refuses a hostname at VM-create time, which
			// is hours after the operator typed it.
			"a hostname guest resolver",
			func(c *Config) { c.GuestDNS = "resolver.internal" },
			"expected an IP address",
		},
		{"the gateway sentinel resolver", func(c *Config) { c.GuestDNS = "gateway" }, ""},
		{
			"a relative sluice socket",
			func(c *Config) { c.SluiceSocket = "run/sluice.sock" },
			"absolute path",
		},
		{
			"a dns answer that is not an IP",
			func(c *Config) { c.DNSAddr, c.DNSAnswer = "10.66.0.1:53", "edge.catnip.sh" },
			"is not an IP address",
		},
		{
			"a dns answer with nothing serving it",
			func(c *Config) { c.DNSAnswer = "10.66.0.1" },
			"no effect without --dns-addr",
		},
		{
			"multiple dns answers",
			func(c *Config) { c.DNSAddr, c.DNSAnswer = "10.66.0.1:53", "10.66.0.1,fd7a::1" },
			"",
		},
		{
			"an out-of-range advertised ssh port",
			func(c *Config) { c.SSHAdvertisePort = 70000 },
			"expected 1-65535",
		},
		{
			// Everything here is templated into ExecStart as a bare word, so
			// whitespace becomes extra arguments and the gateway dies at boot on
			// an opaque flag-parse error rather than here.
			"whitespace in a value would become extra ExecStart arguments",
			func(c *Config) { c.ArchiveRemote, c.ArchiveBucket = "r2 --api-addr :1", "b" },
			"whitespace is not allowed",
		},
		{
			"an edge IP that is not an IP",
			func(c *Config) { c.EdgeIP = "edge.catnip.sh" },
			"expected a bare IP address",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mutate(&cfg)
			err := validateSubsystems(cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("expected an error mentioning %q, got none", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestDNSAnswerSatisfiesTheWildcardEdgeRequirement: A2 refused --dns-addr on a
// wildcard --proxy-addr because the responder then had nothing to answer with
// and `serve` exits at startup. --dns-answer is the other way to satisfy that,
// so the refusal has to know about it — otherwise the new flag is unusable in
// exactly the configuration it exists for.
func TestDNSAnswerSatisfiesTheWildcardEdgeRequirement(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DNSAddr = ":53" // wildcard edge address, no IP host to fall back on
	if err := validateAddrs(cfg); err == nil {
		t.Fatal("a wildcard edge with no answer must still be refused")
	}
	cfg.DNSAnswer = "10.66.0.1"
	if err := validateAddrs(cfg); err != nil {
		t.Fatalf("--dns-answer should satisfy the requirement: %v", err)
	}
}

// TestEdgeIPWritesBothNetScriptSelectors: the two any-port modes answer the
// same question, and leaving the uplink REDIRECT on beside a dedicated edge IP
// means every inbound port above 1024 is still hijacked into the edge — which
// is precisely what giving the edge its own address was meant to stop.
func TestEdgeIPWritesBothNetScriptSelectors(t *testing.T) {
	e := &Env{Cfg: DefaultConfig()}
	// Without the flag, setup has no opinion and must leave both alone: writing
	// SPARKBOX_EDGE_REDIRECT=1 on an upgrade run would flip a hand-configured
	// tunnel-mode host (the DGX) back into hijacking its uplink.
	for _, s := range e.managedEnv(nil) {
		if s.key == "SPARKBOX_EDGE_REDIRECT" || s.key == "SPARKBOX_EDGE_IP" {
			t.Errorf("without --edge-ip setup must not manage %s (it would overwrite a hand-configured mode)", s.key)
		}
	}
	// And a merge must therefore preserve an operator's own tunnel-mode setting.
	merged, _ := mergeEnv("SPARKBOX_EDGE_REDIRECT=0\nSPARKBOX_EDGE_IP=10.66.0.1\n", e.managedEnv(nil))
	if !strings.Contains(merged, "SPARKBOX_EDGE_REDIRECT=0") || !strings.Contains(merged, "SPARKBOX_EDGE_IP=10.66.0.1") {
		t.Errorf("a hand-configured edge mode must survive an upgrade run:\n%s", merged)
	}

	e.Cfg.EdgeIP = "10.66.0.1"
	got := map[string]string{}
	for _, s := range e.managedEnv(nil) {
		got[s.key] = s.val
	}
	if got["SPARKBOX_EDGE_IP"] != "10.66.0.1" {
		t.Errorf("SPARKBOX_EDGE_IP = %q, want 10.66.0.1", got["SPARKBOX_EDGE_IP"])
	}
	if got["SPARKBOX_EDGE_REDIRECT"] != "0" {
		t.Errorf("SPARKBOX_EDGE_REDIRECT = %q, want 0 — a dedicated edge IP replaces the uplink REDIRECT", got["SPARKBOX_EDGE_REDIRECT"])
	}
	// The fresh-host render and the reconcile path must describe the same mode,
	// or which one ran decides how the host forwards traffic.
	fresh := e.renderEnvFile()
	if !strings.Contains(fresh, "SPARKBOX_EDGE_IP=10.66.0.1") || !strings.Contains(fresh, "SPARKBOX_EDGE_REDIRECT=0") {
		t.Errorf("renderEnvFile disagrees with managedEnv:\n%s", fresh)
	}
	// And checkNAT must read that file back as the mode it just wrote.
	mode := readNATMode(fakeProbe{files: map[string]string{e.Cfg.envPath(): fresh}}, e.Cfg)
	if !mode.known || mode.redirect || !mode.tnet {
		t.Errorf("checkNAT reads the file setup wrote as %+v, want tunnel mode", mode)
	}
}
