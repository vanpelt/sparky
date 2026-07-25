package hostsetup

import (
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestUnitAndEnvFileAgreeOnTheProxyPort is the coupling A2 exists to make
// unbreakable: the unit's --proxy-addr and sparkbox.env's PROXY_PORT are read
// by two different programs (systemd and sparkbox-net.sh) and a disagreement
// does not fail — it silently DNATs every sandbox web route to a port nothing
// is listening on. The old `const proxyPort = 8081` fed both renders and still
// could not hold, because an operator moving the edge did it from a flag
// bundle the constant never saw.
func TestUnitAndEnvFileAgreeOnTheProxyPort(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		wantPort string
	}{
		{"default", "", "8081"},
		{"bare port on every interface", ":443", "443"},
		{"the DGX's dedicated edge /32", "10.66.0.1:443", "443"},
		{"loopback", "127.0.0.1:9443", "9443"},
		{"ipv6 literal", "[fd7a:115c:a1e0::1]:8443", "8443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := testEnv(t, false)
			e.Cfg.ProxyAddr = tt.addr
			unit, err := renderService(e.Cfg)
			if err != nil {
				t.Fatal(err)
			}
			wantAddr := tt.addr
			if wantAddr == "" {
				wantAddr = defaultProxyAddr
			}
			if !strings.Contains(unit, "--proxy-addr "+wantAddr) {
				t.Errorf("unit does not bind %s:\n%s", wantAddr, unit)
			}
			env := e.renderEnvFile()
			if !strings.Contains(env, "\nPROXY_PORT="+tt.wantPort+"\n") {
				t.Errorf("sparkbox.env should carry PROXY_PORT=%s for --proxy-addr %s:\n%s", tt.wantPort, wantAddr, env)
			}
			// And the two must be the same number, derived once — not two
			// literals that happen to match in this table.
			port, err := e.Cfg.proxyPortNum()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(wantAddr, ":"+strconv.Itoa(port)) {
				t.Errorf("PROXY_PORT %d is not the port half of %s", port, wantAddr)
			}
		})
	}
}

func TestSplitAddr(t *testing.T) {
	tests := []struct {
		addr    string
		host    string
		port    int
		wantErr bool
	}{
		{":8081", "", 8081, false},
		{"10.66.0.1:443", "10.66.0.1", 443, false},
		{"[fd7a::1]:53", "fd7a::1", 53, false},
		{"0.0.0.0:22", "0.0.0.0", 22, false},
		{"8081", "", 0, true},                    // no port separator at all
		{":https", "", 0, true},                  // named ports never reach iptables
		{"10.66.0.1", "", 0, true},               // host with no port
		{":0", "", 0, true},                      // "pick one for me" is not a listen address setup can write down
		{":70000", "", 0, true},                  // out of range
		{"", "", 0, true},                        //
		{"1.2.3.4:5:6", "", 0, true},             // malformed
		{"localhost:22", "localhost", 22, false}, // serve resolves it, so we accept it
	}
	for _, tt := range tests {
		host, port, err := splitAddr(tt.addr)
		if (err != nil) != tt.wantErr {
			t.Errorf("splitAddr(%q) err = %v, wantErr %v", tt.addr, err, tt.wantErr)
			continue
		}
		if err == nil && (host != tt.host || port != tt.port) {
			t.Errorf("splitAddr(%q) = (%q, %d), want (%q, %d)", tt.addr, host, port, tt.host, tt.port)
		}
	}
}

func TestValidateAddrs(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"defaults are valid", DefaultConfig(), ""},
		{"explicit addresses", Config{SSHAddr: "10.66.0.1:2222", ProxyAddr: "10.66.0.1:443", APIAddr: "127.0.0.1:8079", DNSAddr: "10.66.0.1:53"}, ""},
		{"empty dns-addr just means off", Config{DNSAddr: ""}, ""},
		{"unparseable proxy addr", Config{ProxyAddr: "8081"}, "--proxy-addr"},
		{"named port", Config{ProxyAddr: ":https"}, "non-numeric port"},
		// Whitespace would not reach the daemon as one ExecStart word: it would
		// become extra argv entries and kill the service on an opaque parse
		// error at boot.
		{"whitespace in an address", Config{APIAddr: "127.0.0.1:8079 --open-signup"}, "no whitespace"},
		// serve derives the DNS answer from --proxy-addr's IP host when there is
		// no --dns-answer, so a wildcard edge address means the responder has
		// nothing to answer with and `serve` exits at startup.
		{"dns-addr with a wildcard edge", Config{DNSAddr: "10.66.0.1:53"}, "needs an answer address"},
		{"dns-addr with a concrete edge", Config{DNSAddr: "10.66.0.1:53", ProxyAddr: "10.66.0.1:443"}, ""},
		{"move-admin-ssh alone", Config{MoveAdminSSH: true}, ""},
		{"move-admin-ssh with an explicit :22", Config{MoveAdminSSH: true, SSHAddr: "10.66.0.1:22"}, ""},
		{"move-admin-ssh with a non-22 ssh-addr", Config{MoveAdminSSH: true, SSHAddr: "10.66.0.1:2222"}, "cannot be combined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAddrs(tt.cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("expected an error mentioning %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q should mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestEffectiveAddrsHonoursEnvBundles: sparkbox.env is never rewritten once it
// exists, so an upgraded host still carries the pre-A2 workaround — real bind
// configuration smuggled into TLS_FLAGS — and a repeated flag wins in Go. The
// preflight has to probe what the daemon will END UP binding, or it validates
// ports the service never touches while the real one is busy.
func TestEffectiveAddrsHonoursEnvBundles(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SSHAddr = ":2222"
	kv := map[string]string{
		"TLS_FLAGS": "--proxy-addr :443 --proxy-tls --ssh-addr 10.66.0.1:2222",
		// Appended after TLS_FLAGS in the unit, so it wins the tie.
		"EXTRA_FLAGS": "--api-addr=127.0.0.1:9999 --proxy-addr 10.66.0.1:443",
	}
	got, notes := effectiveAddrs(cfg, kv)
	want := map[string]string{
		"--ssh-addr":   "10.66.0.1:2222",
		"--proxy-addr": "10.66.0.1:443",
		"--api-addr":   "127.0.0.1:9999",
		"--dns-addr":   "",
	}
	for flag, w := range want {
		if got[flag] != w {
			t.Errorf("effective %s = %q, want %q", flag, got[flag], w)
		}
	}
	if len(notes) == 0 {
		t.Error("an override that changes an address must be reported, not applied silently")
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"TLS_FLAGS", "EXTRA_FLAGS", "10.66.0.1:2222"} {
		if !strings.Contains(joined, want) {
			t.Errorf("override notes should mention %q:\n%s", want, joined)
		}
	}
}

func TestParseFlagBundle(t *testing.T) {
	got := parseFlagBundle("--mem-reserve-mb 1024 --ssh-addr 10.66.0.1:2222 --proxy-tls --api-addr=127.0.0.1:8079 --dns-addr")
	want := map[string]string{
		"--ssh-addr": "10.66.0.1:2222",
		"--api-addr": "127.0.0.1:8079",
		// A trailing --dns-addr with no value is not an address; it must not
		// land as an empty override that blanks the configured one.
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// --- the preflight ----------------------------------------------------------

// portEnv builds an Env whose ports are answered from a map and whose host
// commands are canned, so the preflight is exercised without binding anything.
func portEnv(t *testing.T, busy map[string]bool, runs map[string]string) (*Env, *fakeListener) {
	t.Helper()
	e, _ := testEnv(t, false)
	fl := &fakeListener{busy: busy}
	e.Listen = fl
	if runs != nil {
		e.Run = runnerWith(runs)
	}
	return e, fl
}

const ssBusyAPI = `Netid State  Recv-Q Send-Q Local Address:Port Peer Address:Port Process
tcp   LISTEN 0      5      127.0.0.1:8079     0.0.0.0:*         users:(("python3",pid=4711,fd=3))
tcp   LISTEN 0      128    0.0.0.0:22         0.0.0.0:*         users:(("sshd",pid=901,fd=4))
`

func TestPortPreflightFailsOnABusyPort(t *testing.T) {
	e, _ := portEnv(t, map[string]bool{"tcp/127.0.0.1:8079": true}, map[string]string{"ss -lntup": ssBusyAPI})
	err := preflightPorts(e)
	if err == nil {
		t.Fatal("a busy control-API port must fail the preflight, not the first boot")
	}
	// The whole point is that the operator learns the address AND the culprit
	// here, instead of reading "bind: address already in use" out of a journal
	// after a green provisioning run.
	for _, want := range []string{"--api-addr", "127.0.0.1:8079", "python3", "4711"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q:\n%v", want, err)
		}
	}
}

// TestPortPreflightReportsEveryConflict: one conflict per run would mean one
// re-run per busy port, and every re-run is another chance to half-provision.
func TestPortPreflightReportsEveryConflict(t *testing.T) {
	e, _ := portEnv(t, map[string]bool{"tcp/:2222": true, "tcp/:8081": true}, map[string]string{"ss -lntup": ""})
	err := preflightPorts(e)
	if err == nil {
		t.Fatal("two busy ports must fail")
	}
	for _, want := range []string{"--ssh-addr", "--proxy-addr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q:\n%v", want, err)
		}
	}
}

// TestPortPreflightToleratesOurOwnService is the difference between a preflight
// and a tool that cannot be run twice. A live gateway holds every one of these
// ports by design, so `sparkbox setup` on a working host — the idempotent
// re-run the smoke test asserts — must not report its own listener as a
// conflict.
func TestPortPreflightToleratesOurOwnService(t *testing.T) {
	busy := map[string]bool{"tcp/:2222": true, "tcp/:8081": true, "tcp/127.0.0.1:8079": true}
	const ours = `tcp LISTEN 0 4096 0.0.0.0:2222 0.0.0.0:* users:(("sparkbox",pid=4242,fd=7))
tcp LISTEN 0 4096 0.0.0.0:8081 0.0.0.0:* users:(("sparkbox",pid=4242,fd=8))
tcp LISTEN 0 4096 127.0.0.1:8079 0.0.0.0:* users:(("sparkbox",pid=4242,fd=9))
`
	live := fakeProbe{runs: map[string]runResult{
		showCmd: {out: unitState("active", "running", "0", "4242", "9000")},
	}}

	t.Run("owning pid is the service main pid", func(t *testing.T) {
		e, _ := portEnv(t, busy, map[string]string{"ss -lntup": ours})
		e.Probe = live
		if err := preflightPorts(e); err != nil {
			t.Fatalf("re-running setup on a live host must not fail: %v", err)
		}
	})

	t.Run("owning process is named sparkbox even with no systemd answer", func(t *testing.T) {
		// A container, or a doctor run that cannot query systemd: the PID is
		// unknown but the name is not.
		e, _ := portEnv(t, busy, map[string]string{"ss -lntup": ours})
		if err := preflightPorts(e); err != nil {
			t.Fatalf("a sparkbox-owned port must not read as a conflict: %v", err)
		}
	})

	t.Run("owner unidentifiable while our service runs", func(t *testing.T) {
		// Neither ss nor lsof available. Presuming it is ours is deliberate:
		// the cost of guessing wrong is bounded (the post-apply liveness check
		// still FAILs on the crash loop), while the cost of guessing the other
		// way is a host that can never be re-provisioned.
		e, _ := portEnv(t, busy, nil)
		e.Probe = live
		if err := preflightPorts(e); err != nil {
			t.Fatalf("a live service plus an unidentifiable owner must not fail: %v", err)
		}
	})

	t.Run("a stranger on the port still fails while our service runs", func(t *testing.T) {
		e, _ := portEnv(t, map[string]bool{"tcp/127.0.0.1:8079": true}, map[string]string{"ss -lntup": ssBusyAPI})
		e.Probe = live
		err := preflightPorts(e)
		if err == nil {
			t.Fatal("an identified stranger must fail even on a host running sparkbox")
		}
		if !strings.Contains(err.Error(), "python3") {
			t.Errorf("error should name the stranger: %v", err)
		}
	})
}

// TestPortPreflightAllowsTheSSHDMoveAdminSSHEvicts: --move-admin-ssh means
// exactly "take :22 from the host's own sshd", so Config.sshAddr answers ":22"
// and the preflight — which runs before every step, including the stepAdminSSH
// that writes the `Port 2222` drop-in and restarts ssh.service — found sshd
// there and aborted the run whose entire purpose was to move it. The remedy it
// printed ("pick others with --ssh-addr") is refused by validateAddrs for this
// same flag combination, so the documented onboarding path had no way through
// at all.
func TestPortPreflightAllowsTheSSHDMoveAdminSSHEvicts(t *testing.T) {
	// Ubuntu 24.04 socket-activates ssh, so the listener can belong to systemd
	// (pid 1) rather than sshd, and a host with neither `ss` nor `lsof` cannot
	// name it at all. All three are the same host and the same next step.
	owners := map[string]string{
		"sshd": ssBusyAPI,
		"socket-activated systemd": `Netid State  Recv-Q Send-Q Local Address:Port Peer Address:Port Process
tcp   LISTEN 0      4096   0.0.0.0:22         0.0.0.0:*         users:(("systemd",pid=1,fd=71))
`,
		"unidentifiable": "",
	}
	for name, ss := range owners {
		t.Run(name, func(t *testing.T) {
			runs := map[string]string{"ss -lntup": ss}
			e, _ := portEnv(t, map[string]bool{"tcp/:22": true}, runs)
			e.Cfg.MoveAdminSSH = true
			var log strings.Builder
			e.Log = &log
			if err := preflightPorts(e); err != nil {
				t.Fatalf("--move-admin-ssh must be able to run on a host whose sshd holds :22: %v", err)
			}
			if !strings.Contains(log.String(), "--move-admin-ssh") {
				t.Errorf("the skipped conflict must still be reported:\n%s", log.String())
			}
		})
	}

	// An explicit --ssh-addr <ip>:22 is the other half validateAddrs permits,
	// and sshd's 0.0.0.0:22 wildcard collides with it.
	t.Run("explicit :22 on an address of its own", func(t *testing.T) {
		e, _ := portEnv(t, map[string]bool{"tcp/10.66.0.1:22": true}, map[string]string{"ss -lntup": ssBusyAPI})
		e.Cfg.MoveAdminSSH = true
		e.Cfg.SSHAddr = "10.66.0.1:22"
		if err := preflightPorts(e); err != nil {
			t.Fatalf("--ssh-addr <ip>:22 with --move-admin-ssh must be provisionable: %v", err)
		}
	})

	// The carve-out is for the sshd this run is about to move, not for :22 in
	// general: something else squatting there is still a conflict, and without
	// --move-admin-ssh so is sshd.
	t.Run("a stranger on :22 still fails", func(t *testing.T) {
		const squatter = `Netid State  Recv-Q Send-Q Local Address:Port Peer Address:Port Process
tcp   LISTEN 0      128    0.0.0.0:22         0.0.0.0:*         users:(("nginx",pid=777,fd=6))
`
		e, _ := portEnv(t, map[string]bool{"tcp/:22": true}, map[string]string{"ss -lntup": squatter})
		e.Cfg.MoveAdminSSH = true
		err := preflightPorts(e)
		if err == nil {
			t.Fatal("only the admin sshd is excused from :22")
		}
		if !strings.Contains(err.Error(), "nginx") {
			t.Errorf("error should name the stranger: %v", err)
		}
	})

	t.Run("without --move-admin-ssh a busy gateway port is still a conflict", func(t *testing.T) {
		e, _ := portEnv(t, map[string]bool{"tcp/10.66.0.1:22": true}, map[string]string{"ss -lntup": ssBusyAPI})
		e.Cfg.SSHAddr = "10.66.0.1:22"
		if err := preflightPorts(e); err == nil {
			t.Fatal("nothing asked to move sshd, so :22 is simply taken")
		}
	})
}

// TestPortPreflightIgnoresNonConflictBindErrors: the DGX's own configuration
// binds a dedicated tailnet /32 (10.66.0.1) that sparkbox-net.sh creates on the
// `sparkedge` dummy interface at boot — i.e. AFTER the step running here. A
// bind against an address the host does not carry yet fails with
// EADDRNOTAVAIL, which is not a conflict, and treating it as one would make the
// very setup this flag exists for impossible to run.
func TestPortPreflightIgnoresNonConflictBindErrors(t *testing.T) {
	e, _ := portEnv(t, nil, nil)
	e.Cfg.ProxyAddr = "10.66.0.1:443"
	var log strings.Builder
	e.Log = &log
	e.Listen = &fakeListener{errs: map[string]error{
		"tcp/10.66.0.1:443": &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRNOTAVAIL},
	}}
	if err := preflightPorts(e); err != nil {
		t.Fatalf("an address this host does not carry yet is not a conflict: %v", err)
	}
	if !strings.Contains(log.String(), "10.66.0.1:443") {
		t.Errorf("it should still be reported, so a typo'd IP is visible:\n%s", log.String())
	}
}

// TestPortPreflightDryRunBindsNothing: a dry run must not open a socket any
// more than it writes a file — and on a live host it would otherwise report
// the operator's own gateway as a wall of conflicts.
func TestPortPreflightDryRunBindsNothing(t *testing.T) {
	e, fl := portEnv(t, map[string]bool{"tcp/:2222": true}, nil)
	e.Cfg.DryRun = true
	var log strings.Builder
	e.Log = &log
	if err := preflightPorts(e); err != nil {
		t.Fatalf("a dry run must not fail on a busy port: %v", err)
	}
	if len(fl.calls) != 0 {
		t.Errorf("dry run bound %v", fl.calls)
	}
	for _, want := range []string{"would probe", ":2222", ":8081", "127.0.0.1:8079"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the dry-run plan should report %q:\n%s", want, log.String())
		}
	}
}

// A fleet node opens none of these listeners (serveNode returns before the SSH
// door, the edge, the API and the DNS responder exist), so probing them there
// would invent conflicts on a machine that is configured correctly.
func TestPortPreflightSkippedOnAFleetNode(t *testing.T) {
	e, fl := portEnv(t, map[string]bool{"tcp/:2222": true, "tcp/:8081": true, "tcp/127.0.0.1:8079": true}, nil)
	e.Cfg.Gateway = "gw.example:2222"
	if err := preflightPorts(e); err != nil {
		t.Fatalf("a node must not preflight gateway ports: %v", err)
	}
	if len(fl.calls) != 0 {
		t.Errorf("a node bound %v", fl.calls)
	}
}

// The wildcard DNS responder serves UDP and TCP on the same address, and it is
// the UDP half that resolvers actually use — checking only TCP would miss the
// collision that matters (systemd-resolved on :53 is the classic one).
func TestPortPreflightProbesDNSOnBothNetworks(t *testing.T) {
	e, fl := portEnv(t, nil, nil)
	e.Cfg.DNSAddr = "10.66.0.1:53"
	if err := preflightPorts(e); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"udp/10.66.0.1:53", "tcp/10.66.0.1:53"} {
		if !contains(fl.calls, want) {
			t.Errorf("preflight did not probe %s; probed %v", want, fl.calls)
		}
	}
}

// TestPortPreflightWarnsOnProxyPortSkew: the packet filter forwards any-port
// traffic to PROXY_PORT, so an env file left behind by an older provision whose
// port no longer matches the edge breaks every sandbox web route with no error
// anywhere. setup does not rewrite sparkbox.env (it is the operator's editing
// surface), so the least it can do is say so.
func TestPortPreflightWarnsOnProxyPortSkew(t *testing.T) {
	e, _ := portEnv(t, nil, nil)
	var log strings.Builder
	e.Log = &log
	if err := os.MkdirAll(e.Cfg.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Cfg.envPath(), []byte("PROXY_PORT=8081\nTLS_FLAGS=--proxy-addr :443 --proxy-tls\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := preflightPorts(e); err != nil {
		t.Fatal(err)
	}
	out := log.String()
	for _, want := range []string{"PROXY_PORT=8081", "443"} {
		if !strings.Contains(out, want) {
			t.Errorf("the skew warning should mention %q:\n%s", want, out)
		}
	}
}

func TestListenerOwnerParsing(t *testing.T) {
	t.Run("ss names the process", func(t *testing.T) {
		e, _ := portEnv(t, nil, map[string]string{"ss -lntup": ssBusyAPI})
		got, ok := listenerOwner(e, "tcp", "127.0.0.1:8079")
		if !ok || got.name != "python3" || got.pid != "4711" {
			t.Fatalf("owner = %+v ok=%v, want python3/4711", got, ok)
		}
	})
	t.Run("a wildcard listener blocks a specific bind", func(t *testing.T) {
		// This is why the DGX gives the edge a /32 of its own: anything on
		// 0.0.0.0 collides with every address on that port.
		e, _ := portEnv(t, nil, map[string]string{"ss -lntup": ssBusyAPI})
		got, ok := listenerOwner(e, "tcp", "10.66.0.1:22")
		if !ok || got.name != "sshd" {
			t.Fatalf("owner = %+v ok=%v, want the 0.0.0.0:22 sshd", got, ok)
		}
	})
	t.Run("lsof is the fallback when ss is absent", func(t *testing.T) {
		e, _ := portEnv(t, nil, map[string]string{
			"lsof -nP -iTCP:8079,UDP:8079 -sTCP:LISTEN": "COMMAND  PID USER   FD   TYPE NODE NAME\npython3 4711 root    3u  IPv4  TCP 127.0.0.1:8079 (LISTEN)\n",
		})
		got, ok := listenerOwner(e, "tcp", "127.0.0.1:8079")
		if !ok || got.name != "python3" || got.pid != "4711" {
			t.Fatalf("owner = %+v ok=%v, want python3/4711 from lsof", got, ok)
		}
	})
	t.Run("neither tool available", func(t *testing.T) {
		e, _ := portEnv(t, nil, nil)
		if _, ok := listenerOwner(e, "tcp", "127.0.0.1:8079"); ok {
			t.Fatal("no tool should mean no owner, not a fabricated one")
		}
	})
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
