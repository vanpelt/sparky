package hostsetup

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// sparkbox.env is shared property. Setup owns some of the lines in it, the
// operator owns the rest, and until now setup owned them only on the first run:
// stepEnvFile's Satisfied was a bare os.Stat, so the file was written once and
// never looked at again. That is why `setup --proxy-addr :443` could move the
// edge and leave PROXY_PORT=8081 behind it — and PROXY_PORT is what
// sparkbox-net.sh forwards every any-port connection to, so the whole web-route
// surface of the box pointed at a closed port with nothing reporting it.
//
// The split below is the fix: the managed keys are reconciled on every run, and
// everything else in the file is preserved byte for byte.

// envSetting is one managed KEY=value in sparkbox.env.
type envSetting struct {
	key string
	val string
}

// managedEnv is the set of sparkbox.env settings that are pure functions of this
// run's configuration — the ones setup is entitled to correct. kv is the file
// as it exists on disk (nil on a fresh host), because two of these keys can only
// be answered correctly by looking at what is already there.
//
// Deliberately short. EXTRA_FLAGS, TLS_FLAGS, OVERCOMMIT_FLAGS, SUBNET6 and
// SPARKBOX_CONSOLE_PASSWORD are the operator's: setup has no opinion it could
// defend about any of them, and rewriting one would delete a live host's
// configuration to no purpose.
func (e *Env) managedEnv(kv map[string]string) []envSetting {
	var out []envSetting
	// PROXY_DOMAIN only when the operator NAMED it, or when the file has no line
	// for it at all.
	//
	// A default is not an opinion. --proxy-domain carries the compiled-in
	// hivemind.tools, so an upgrade run that never mentioned the domain is
	// indistinguishable from one that asked for it — and reconciling on that
	// basis would rewrite the live DGX's PROXY_DOMAIN=catnip.sh, move every
	// sandbox web route, the console and the user console onto a domain the box
	// does not serve, and start DNS-01 orders for *.hivemind.tools against a
	// zone the Cloudflare token cannot touch. Same guard as SPARKBOX_EDGE_* and
	// GATEWAY_FLAG below, for the same reason.
	if _, present := kv["PROXY_DOMAIN"]; e.Cfg.flagGiven("proxy-domain") || !present {
		out = append(out, envSetting{"PROXY_DOMAIN", e.Cfg.ProxyDomain})
	}
	// The one that silently breaks the box when it drifts — and it must be
	// derived from the address the daemon will ACTUALLY bind, not from the one
	// this run templated into the unit. An existing TLS_FLAGS/EXTRA_FLAGS bundle
	// is appended after the templated flags in ExecStart and a repeated flag
	// wins in Go, so on the DGX (`TLS_FLAGS=… --proxy-addr 10.66.0.1:443 …`) the
	// config says :8081 while the edge listens on 443. Reconciling from the
	// config would have written PROXY_PORT=8081 over a correct 443 and pointed
	// every any-port DNAT at a closed port — the exact breakage this file exists
	// to prevent, caused by the fix for it.
	//
	// A malformed address cannot reach here (validateAddrs runs first in
	// Provision), so a parse failure means a hand-built Config in a test — leave
	// the key alone rather than writing PROXY_PORT=0 over a working value.
	if _, port, err := splitAddr(effectiveAddr(e.Cfg, kv, "--proxy-addr")); err == nil {
		out = append(out, envSetting{"PROXY_PORT", strconv.Itoa(port)})
	}
	out = append(out, guestSubnetSettings(e.Cfg, kv)...)
	if manageTransportSetting(e.Cfg, kv) {
		out = append(out, transportSetting(e.Cfg, kv))
		out = append(out, routedGuestCanaryIntentSetting(e.Cfg, kv))
	}
	out = append(out, sshNetSettings(e.Cfg, kv)...)
	// Comes from the release manifest, so it is only known once resolve-release
	// has run: a release that changes the rootfs login user would otherwise
	// never reach a host that already has this file.
	if e.Manifest.RootfsLogin != "" {
		out = append(out, envSetting{"LOGIN_USER_FLAG", "--default-login-user=" + e.Manifest.RootfsLogin})
	}
	// The any-port forwarding mode, and the ONE part of the DGX's configuration
	// that is not a `serve` flag: sparkbox-net.sh reads both of these from this
	// file (see checkNAT, which reads them back to decide which nat chain the
	// host should have).
	//
	// Managed only when --edge-ip is given, deliberately. An unset flag means
	// "setup has no opinion", and writing SPARKBOX_EDGE_REDIRECT=1 on that
	// basis would flip a hand-configured tunnel-mode host — the live DGX, for
	// one — back into hijacking every inbound TCP port above 1024 on its uplink,
	// on a run whose only stated purpose was an upgrade.
	if e.Cfg.EdgeIP != "" {
		out = append(out,
			envSetting{"SPARKBOX_EDGE_IP", e.Cfg.EdgeIP},
			// The two modes answer the same question. Leaving the uplink
			// REDIRECT on beside a dedicated edge IP means every port above 1024
			// arriving on the uplink is still hijacked into the edge, which is
			// exactly what giving the edge its own address was meant to stop.
			envSetting{"SPARKBOX_EDGE_REDIRECT", "0"},
		)
	}
	// The address sluice's resolver binds, for sparkbox-net.sh to CREATE on a
	// dummy interface. Managed only when this run installs sluice with a
	// concrete resolver IP, because that is the only case where the key means
	// anything — and, like SPARKBOX_EDGE_IP above, an unset --sluice must not
	// rewrite a hand-configured host's line.
	//
	// Without it the whole recommended shape ("give sluice 172.30.0.53:53") was
	// unrunnable: nothing put the address on the host, the bind failed with
	// EADDRNOTAVAIL, and Restart=always turned that into a permanent loop that
	// every surface reported as a successful provision.
	if ip := e.Cfg.sluiceResolverIP(); ip != "" {
		out = append(out, envSetting{"SLUICE_DNS_IP", ip})
	}
	// Managed only when this run IS a node. A standalone run must never write
	// GATEWAY_FLAG= over an existing node's link — checkRoleSwitch refuses that
	// combination outright rather than letting a merge quietly demote the host.
	if e.Cfg.Gateway != "" {
		flag := "--gateway " + e.Cfg.Gateway
		if e.Cfg.NodeName != "" {
			flag += " --node-name " + e.Cfg.NodeName
		}
		out = append(out, envSetting{"GATEWAY_FLAG", flag})
	}
	return out
}

// guestSubnetSettings keeps the daemon and packet filter on one prefix. The
// daemon consumes GUEST_SUBNET_FLAG; sparkbox-net.sh consumes
// SPARKBOX_GUEST_SUBNET.
func guestSubnetSettings(cfg Config, kv map[string]string) []envSetting {
	subnet := effectiveGuestSubnet(cfg, kv)
	return []envSetting{
		{"SPARKBOX_GUEST_SUBNET", subnet},
		{"GUEST_SUBNET_FLAG", "--guest-subnet=" + subnet},
	}
}

// effectiveGuestSubnet protects a live custom value during an upgrade. When
// --guest-subnet was explicitly supplied this run it is the starting point;
// otherwise an existing environment value wins over the compiled-in default.
// Finally inspect the flag bundles in their ExecStart order because a repeated
// flag there wins in Go. Mirroring that value into SPARKBOX_GUEST_SUBNET keeps
// NAT aligned with what the daemon actually uses without rewriting the
// operator-owned bundle.
func effectiveGuestSubnet(cfg Config, kv map[string]string) string {
	subnet := cfg.guestSubnet()
	if !cfg.flagGiven("guest-subnet") {
		for _, raw := range []string{
			kv["SPARKBOX_GUEST_SUBNET"],
			flagValue(kv["GUEST_SUBNET_FLAG"], "--guest-subnet"),
		} {
			if normalized, err := NormalizeGuestSubnet(raw); raw != "" && err == nil {
				subnet = normalized
			}
		}
	}
	for _, key := range []string{"TLS_FLAGS", "GATEWAY_FLAG", "EXTRA_FLAGS"} {
		if raw := flagValue(kv[key], "--guest-subnet"); raw != "" {
			if normalized, err := NormalizeGuestSubnet(raw); err == nil {
				subnet = normalized
			}
		}
	}
	return subnet
}

func flagValue(bundle, name string) string {
	words := strings.Fields(bundle)
	for i := 0; i < len(words); i++ {
		if words[i] == name && i+1 < len(words) {
			return words[i+1]
		}
		if value, ok := strings.CutPrefix(words[i], name+"="); ok {
			return value
		}
	}
	return ""
}

const (
	transportFlagsEnv               = "TRANSPORT_FLAGS"
	routedGuestCanaryExplicitEnv    = "SPARKBOX_ROUTED_GUEST_CANARY_EXPLICIT"
	routedGuestCanaryExplicitMarker = "1"
)

// manageTransportSetting keeps upgrades inert unless transport configuration
// already exists or this run actually asks to create/change it. Fresh files go
// through renderEnvFile and always receive the explicit auto defaults.
func manageTransportSetting(cfg Config, kv map[string]string) bool {
	if _, present := kv[transportFlagsEnv]; present {
		return true
	}
	for _, name := range []string{
		"node-control-transport", "node-control-rollout",
		"node-grpc-addr", "gateway-grpc-addr",
		"guest-data-transport", "routed-guest-canary-percent", "cluster-id",
	} {
		if cfg.flagGiven(name) {
			return true
		}
	}
	control := strings.TrimSpace(cfg.NodeControlTransport)
	data := strings.TrimSpace(cfg.GuestDataTransport)
	return (control != "" && control != "auto") ||
		(strings.TrimSpace(cfg.NodeControlRollout) != "" &&
			strings.TrimSpace(cfg.NodeControlRollout) != "inherit") ||
		strings.TrimSpace(cfg.NodeGRPCAddr) != "" ||
		strings.TrimSpace(cfg.GatewayGRPCAddr) != "" ||
		(data != "" && data != "auto") ||
		(cfg.RoutedGuestCanaryPercent != 0 && cfg.RoutedGuestCanaryPercent != 100) ||
		strings.TrimSpace(cfg.ClusterID) != ""
}

// transportSetting persists the fleet transport and workload-identity knobs in one
// systemd-word-split bundle. effectiveTransportConfig preserves values from a
// live host when an upgrade run did not name their setup flags, while still
// respecting later operator-owned bundles in their actual ExecStart order.
func transportSetting(cfg Config, kv map[string]string) envSetting {
	effective := effectiveTransportConfig(cfg, kv)
	flags := []string{
		"--node-control-transport=" + effective.NodeControlTransport,
		"--node-control-rollout=" + effective.NodeControlRollout,
		"--guest-data-transport=" + effective.GuestDataTransport,
		"--routed-guest-canary-percent=" + strconv.Itoa(effective.RoutedGuestCanaryPercent),
	}
	if effective.NodeGRPCAddr != "" {
		flags = append(flags, "--node-grpc-addr="+effective.NodeGRPCAddr)
	}
	if effective.GatewayGRPCAddr != "" {
		flags = append(flags, "--gateway-grpc-addr="+effective.GatewayGRPCAddr)
	}
	if effective.ClusterID != "" {
		flags = append(flags, "--cluster-id="+effective.ClusterID)
	}
	return envSetting{transportFlagsEnv, strings.Join(flags, " ")}
}

// routedGuestCanaryIntentSetting preserves the one distinction the numeric
// canary cannot encode by itself: 100 is both the compiled-in default and a
// valid operator-selected full rollout. A fresh standalone host writes 0;
// explicitly selecting the canary writes 1. Reading TRANSPORT_FLAGS alone
// must not promote setup's generated auto/100 defaults into a fleet-routing
// request on the second setup run.
func routedGuestCanaryIntentSetting(cfg Config, kv map[string]string) envSetting {
	effective := effectiveTransportConfig(cfg, kv)
	value := "0"
	if effective.RoutedGuestCanaryExplicit || cfg.flagGiven("routed-guest-canary-percent") {
		value = routedGuestCanaryExplicitMarker
	}
	return envSetting{routedGuestCanaryExplicitEnv, value}
}

func effectiveTransportConfig(cfg Config, kv map[string]string) Config {
	if cfg.NodeControlTransport == "" {
		cfg.NodeControlTransport = "auto"
	}
	if cfg.NodeControlRollout == "" {
		cfg.NodeControlRollout = "inherit"
	}
	if cfg.GuestDataTransport == "" {
		cfg.GuestDataTransport = "auto"
	}
	if cfg.flagGiven("routed-guest-canary-percent") {
		cfg.RoutedGuestCanaryExplicit = true
	}
	if cfg.RoutedGuestCanaryPercent == 0 && !cfg.flagGiven("routed-guest-canary-percent") {
		cfg.RoutedGuestCanaryPercent = 100
	}
	apply := func(bundle string, onlyUngiven, operatorOwned bool) {
		if !onlyUngiven || !cfg.flagGiven("node-control-transport") {
			if value := flagValue(bundle, "--node-control-transport"); value != "" {
				cfg.NodeControlTransport = value
			}
		}
		if !onlyUngiven || !cfg.flagGiven("guest-data-transport") {
			if value := flagValue(bundle, "--guest-data-transport"); value != "" {
				cfg.GuestDataTransport = value
			}
		}
		if !onlyUngiven || !cfg.flagGiven("node-control-rollout") {
			if value := flagValue(bundle, "--node-control-rollout"); value != "" {
				cfg.NodeControlRollout = value
			}
		}
		if !onlyUngiven || !cfg.flagGiven("routed-guest-canary-percent") {
			if value := flagValue(bundle, "--routed-guest-canary-percent"); value != "" {
				if percent, err := strconv.Atoi(value); err == nil {
					cfg.RoutedGuestCanaryPercent = percent
					marker, marked := kv[routedGuestCanaryExplicitEnv]
					switch {
					case operatorOwned:
						// EXTRA_FLAGS and the other later bundles are edited by
						// an operator, so even their 100 is intentional.
						cfg.RoutedGuestCanaryExplicit = true
					case marker == routedGuestCanaryExplicitMarker:
						cfg.RoutedGuestCanaryExplicit = true
					case marked:
						// The managed marker is authoritative when present.
						cfg.RoutedGuestCanaryExplicit = false
					default:
						// Backward compatibility for files written before the
						// marker: every non-default percentage was necessarily
						// selected intentionally.
						cfg.RoutedGuestCanaryExplicit = percent != 100
					}
				}
			}
		}
		if !onlyUngiven || !cfg.flagGiven("node-grpc-addr") {
			if value := flagValue(bundle, "--node-grpc-addr"); value != "" {
				cfg.NodeGRPCAddr = value
			}
		}
		if !onlyUngiven || !cfg.flagGiven("gateway-grpc-addr") {
			if value := flagValue(bundle, "--gateway-grpc-addr"); value != "" {
				cfg.GatewayGRPCAddr = value
			}
		}
		if !onlyUngiven || !cfg.flagGiven("cluster-id") {
			if value := flagValue(bundle, "--cluster-id"); value != "" {
				cfg.ClusterID = value
			}
		}
	}
	apply(kv[transportFlagsEnv], true, false)
	// These bundles appear after TRANSPORT_FLAGS in ExecStart. A repeated Go
	// flag wins last, so doctor and setup reconciliation must observe the same
	// effective values the daemon does.
	for _, key := range []string{
		"SUBNET6_FLAG", "OVERCOMMIT_FLAGS", "TLS_FLAGS", "GATEWAY_FLAG", "EXTRA_FLAGS",
	} {
		apply(kv[key], false, true)
	}
	return cfg
}

// EffectiveTransportConfig reads the service environment through Probe and
// returns the transport values the daemon actually receives. Explicit doctor
// flags win the managed TRANSPORT_FLAGS value; later operator bundles retain
// their documented last-flag-wins precedence.
func EffectiveTransportConfig(p Probe, cfg Config) Config {
	kv, ok := effectiveEnvKV(p, cfg)
	if !ok {
		return cfg
	}
	return effectiveTransportConfig(cfg, kv)
}

// EffectiveFleetConfig resolves the transport settings plus the fleet role and
// exact node name persisted by setup in GATEWAY_FLAG. Doctor uses this so a
// plain invocation on a node validates the certificate against the configured
// SPIFFE name rather than accepting any otherwise-valid node identity.
func EffectiveFleetConfig(p Probe, cfg Config) Config {
	kv, ok := effectiveEnvKV(p, cfg)
	if !ok {
		return cfg
	}
	cfg = effectiveTransportConfig(cfg, kv)
	for _, bundle := range bundleOrder {
		if !cfg.flagGiven("gateway") {
			if value := flagValue(kv[bundle], "--gateway"); value != "" {
				cfg.Gateway = value
			}
		}
		if !cfg.flagGiven("node-name") {
			if value := flagValue(kv[bundle], "--node-name"); value != "" {
				cfg.NodeName = value
			}
		}
	}
	return cfg
}

func effectiveEnvKV(p Probe, cfg Config) (map[string]string, bool) {
	if p == nil {
		return nil, false
	}
	raw, err := p.ReadFile(cfg.envPath())
	if err != nil {
		return nil, false
	}
	kv, err := parseEnv(strings.NewReader(string(raw)))
	if err != nil {
		return nil, false
	}
	return kv, true
}

// netScriptSSHPort is the gateway SSH port deploy/sparkbox-net.sh assumes when
// nothing tells it otherwise. It appears there three times — the uplink
// REDIRECT exclude list (PROXY_REDIRECT_EXCLUDE), the dest-scoped tailnet
// exclude list (SPARKBOX_TAILNET_EXCLUDE) and the target of the edge IP's
// :22 → gateway DNAT (SPARKBOX_GATEWAY_PORT) — and all three were hardcoded
// back when :2222 was the only port the gateway could have.
const netScriptSSHPort = 2222

// sshNetSettings tells sparkbox-net.sh where the gateway's SSH door actually
// is, when it is not where the script assumes.
//
// --ssh-addr made that port configurable and nothing carried the new value into
// the packet filter, so `setup --ssh-addr :2200` produced a host where the
// any-port rules swallowed inbound 2200 (the SSH gateway becomes unreachable)
// while sparing an unused 2222, and the edge IP's :22 DNAT pointed at 2222
// where nothing listened — so the bare `ssh ctl@<domain>` the connect banner
// prints was dead. Both failures are silent: the listener is up, the packets
// just never arrive.
//
// Written only when the port is NOT the script's own default. At :2222 the
// script is already right, and emitting the keys anyway would overwrite an
// operator's hand-tuned exclude list to say exactly what it said before.
func sshNetSettings(cfg Config, kv map[string]string) []envSetting {
	if cfg.Gateway != "" {
		return nil // a fleet node serves no SSH door and no edge
	}
	_, port, err := splitAddr(effectiveAddr(cfg, kv, "--ssh-addr"))
	if err != nil || port == netScriptSSHPort {
		return nil
	}
	p := strconv.Itoa(port)
	return []envSetting{
		{"SPARKBOX_GATEWAY_PORT", p},
		{"PROXY_REDIRECT_EXCLUDE", p},
		{"SPARKBOX_TAILNET_EXCLUDE", tailnetExclude(cfg, kv, p)},
	}
}

// tailnetExclude mirrors the script's own two defaults for
// SPARKBOX_TAILNET_EXCLUDE with the real SSH port substituted. The two modes
// spare different things: with a dedicated edge IP the rules match by
// destination, so only that IP's own in-range service (the SSH gateway) needs
// sparing, while the interface-scoped mode matches every packet on the host's
// tailnet IP and has to protect the host stack's ports as well. Writing the
// dest-scoped value on an interface-scoped host would un-protect sshd, :53 and
// a co-tenant `tailscale serve` — i.e. lock the operator out.
func tailnetExclude(cfg Config, kv map[string]string, sshPort string) string {
	if cfg.EdgeIP != "" || kv["SPARKBOX_EDGE_IP"] != "" {
		return sshPort
	}
	return "22 " + sshPort + " 53 8443"
}

// edgeRedirectValue is what SPARKBOX_EDGE_REDIRECT should say for this config:
// off when the edge has an address of its own, otherwise the script's own
// default. One derivation point, read by both renderEnvFile (fresh host) and
// managedEnv (reconcile), so the two renders cannot describe different modes.
func edgeRedirectValue(cfg Config) string {
	if cfg.EdgeIP != "" {
		return "0"
	}
	return "1"
}

// readEnvFile parses an existing sparkbox.env. The bool is false when there is
// no file to reconcile against (the fresh-host case), which is different from an
// empty one.
func readEnvFile(path string) (map[string]string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	kv, err := parseEnv(strings.NewReader(string(b)))
	if err != nil {
		return nil, false
	}
	return kv, true
}

// envDrift lists the managed keys whose on-disk value disagrees with this
// config, in the wording both the plan and the apply log use.
func envDrift(kv map[string]string, want []envSetting) []string {
	var out []string
	for _, w := range want {
		have, ok := kv[w.key]
		if ok && have == w.val {
			continue
		}
		out = append(out, envChange(w.key, have, ok, w.val))
	}
	return out
}

func envChange(key, have string, present bool, want string) string {
	if !present {
		return fmt.Sprintf("%s=%s (missing)", key, want)
	}
	return fmt.Sprintf("%s: %s → %s", key, orDash(have), orDash(want))
}

// mergeEnv rewrites only the managed keys in an existing sparkbox.env, leaving
// every other line — comments, blank lines, operator settings, keys from a
// future release this binary knows nothing about — exactly as it found them.
//
// A re-render would be simpler and wrong: this file is where an operator puts
// EXTRA_FLAGS and the console password, and losing those on an upgrade is a
// bigger outage than the drift being fixed.
func mergeEnv(existing string, want []envSetting) (string, []string) {
	lines := strings.Split(strings.TrimRight(existing, "\n"), "\n")
	seen := map[string]bool{}
	var changed []string
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		w, managed := lookupSetting(want, k)
		if !managed || seen[k] {
			continue
		}
		seen[k] = true
		if strings.Trim(strings.TrimSpace(v), `"'`) == w {
			continue
		}
		lines[i] = k + "=" + w
		changed = append(changed, envChange(k, strings.TrimSpace(v), true, w))
	}
	// Keys the file predates (LOGIN_USER_FLAG on a host provisioned before the
	// manifest carried one, say) are appended in the order managedEnv lists
	// them, so the result is deterministic and diffable.
	for _, w := range want {
		if seen[w.key] {
			continue
		}
		lines = append(lines, w.key+"="+w.val)
		changed = append(changed, envChange(w.key, "", false, w.val))
	}
	return strings.Join(lines, "\n") + "\n", changed
}

func lookupSetting(want []envSetting, key string) (string, bool) {
	for _, w := range want {
		if w.key == key {
			return w.val, true
		}
	}
	return "", false
}

// checkRoleSwitch refuses to provision a gateway over a fleet node, or a node
// over a gateway, in place.
//
// The role lives in one variable — sparkbox.env's GATEWAY_FLAG — and the two
// roles want different things on disk: a gateway holds accounts, fleet keys and
// a cert cache; a node holds none of them and authenticates nobody. Setup does
// not rewrite that variable (see mergeEnv's contract), so a run that disagrees
// with it would lay down half of one role over the other and then report
// success, with the running service still doing whatever GATEWAY_FLAG says.
//
// macos/poc.sh already refuses this by shelling out to read the file and
// comparing the whole bundle as a string; doing it here means every host gets
// the guard, and a mere --node-name change is a rename rather than a role
// switch.
func checkRoleSwitch(e *Env) error {
	kv, ok := readEnvFile(e.Cfg.envPath())
	if !ok {
		return nil // fresh host: no role to contradict
	}
	haveNode := strings.Contains(kv["GATEWAY_FLAG"], "--gateway")
	wantNode := e.Cfg.Gateway != ""
	if haveNode == wantNode {
		return nil
	}
	if haveNode {
		return fmt.Errorf("this host is provisioned as a fleet NODE (GATEWAY_FLAG=%s in %s), "+
			"but this run has no --gateway and would provision it as a standalone gateway.\n"+
			"  Keep the role: re-run with the same --gateway (and --node-name) it is linked with.\n"+
			"  Or convert it deliberately: systemctl stop sparkbox; remove the GATEWAY_FLAG value from %s; re-run.",
			kv["GATEWAY_FLAG"], e.Cfg.envPath(), e.Cfg.envPath())
	}
	return fmt.Errorf("this host is provisioned as a standalone GATEWAY (GATEWAY_FLAG is empty in %s), "+
		"but --gateway %s asks to run it as a fleet node.\n"+
		"  A node holds no accounts and serves no edge, so the gateway's users, fleet keys and cert cache\n"+
		"  under %s would simply stop being used — without being removed, and without a word.\n"+
		"  Convert it deliberately: systemctl stop sparkbox; mv %s %s.gateway; re-run with --gateway.",
		e.Cfg.envPath(), e.Cfg.Gateway, e.Cfg.StateDir, e.Cfg.StateDir, e.Cfg.StateDir)
}
