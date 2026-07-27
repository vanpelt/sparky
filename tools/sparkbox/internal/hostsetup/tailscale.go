package hostsetup

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
)

type tailscalePrefs struct {
	AdvertiseRoutes []string `json:"AdvertiseRoutes"`
	RouteAll        bool     `json:"RouteAll"`
}

type tailscaleStatus struct {
	BackendState string `json:"BackendState"`
	Self         struct {
		Online     bool     `json:"Online"`
		AllowedIPs []string `json:"AllowedIPs"`
	} `json:"Self"`
	Peer map[string]struct {
		HostName   string   `json:"HostName"`
		Online     bool     `json:"Online"`
		AllowedIPs []string `json:"AllowedIPs"`
	} `json:"Peer"`
}

func fleetRoutingExpected(cfg Config) bool {
	// A node is always part of the routed fleet. A gateway becomes one when
	// setup is explicitly asked for any fleet-only transport surface. Do not
	// rely only on FlagsGiven: doctor reconstructs the effective service config
	// from sparkbox.env, where those values are durable but the original CLI
	// visitation bits are (correctly) gone.
	return cfg.Gateway != "" ||
		cfg.flagGiven("guest-subnet") ||
		cfg.GatewayGRPCAddr != "" ||
		cfg.ClusterID != "" ||
		cfg.NodeControlTransport == "grpc" ||
		cfg.GuestDataTransport == "routed"
}

func parseTailscalePrefs(raw []byte) (tailscalePrefs, error) {
	var prefs tailscalePrefs
	if err := json.Unmarshal(raw, &prefs); err != nil {
		return prefs, fmt.Errorf("decode tailscale preferences: %w", err)
	}
	return prefs, nil
}

func parseTailscaleStatus(raw string) (tailscaleStatus, error) {
	var status tailscaleStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		return status, fmt.Errorf("decode tailscale status: %w", err)
	}
	return status, nil
}

// mergeAdvertisedRoutes adds wanted without changing or reordering any other
// route. Equivalent CIDRs are recognized after masking, so a non-canonical
// spelling does not create a duplicate.
func mergeAdvertisedRoutes(existing []string, wanted string) ([]string, bool) {
	normalized, err := NormalizeGuestSubnet(wanted)
	if err != nil {
		return append([]string(nil), existing...), false
	}
	wantPrefix, _ := netip.ParsePrefix(normalized)
	for _, raw := range existing {
		if prefix, err := netip.ParsePrefix(raw); err == nil &&
			prefix.Masked() == wantPrefix {
			return append([]string(nil), existing...), false
		}
	}
	merged := append([]string(nil), existing...)
	merged = append(merged, normalized)
	return merged, true
}

// stepTailscaleRoutes changes exactly one Tailscale preference. A node merges
// its prefix into AdvertiseRoutes; a fleet-ready gateway enables RouteAll
// (accept-routes). `tailscale set` changes only the named preference, unlike
// `tailscale up`, so auth keys, exit-node choices, DNS, SSH, and unrelated
// advertised routes remain untouched.
func stepTailscaleRoutes() Step {
	return Step{
		Name: "tailscale-routes",
		Satisfied: func(e *Env) (bool, string, error) {
			if !fleetRoutingExpected(e.Cfg) {
				return true, "standalone route integration not requested", nil
			}
			raw, err := e.run("tailscale", "debug", "prefs")
			if err != nil {
				return false, "", nil
			}
			prefs, err := parseTailscalePrefs(raw)
			if err != nil {
				return false, "", err
			}
			if e.Cfg.Gateway != "" {
				_, changed := mergeAdvertisedRoutes(prefs.AdvertiseRoutes, e.Cfg.guestSubnet())
				if !changed {
					return true, "advertising " + e.Cfg.guestSubnet(), nil
				}
				return false, "", nil
			}
			if prefs.RouteAll {
				return true, "accept-routes enabled", nil
			}
			return false, "", nil
		},
		Plan: func(e *Env) string {
			if e.Cfg.Gateway != "" {
				return "merge " + e.Cfg.guestSubnet() + " into Tailscale advertised routes"
			}
			return "enable Tailscale accept-routes without changing unrelated preferences"
		},
		Apply: func(e *Env) error {
			raw, err := e.run("tailscale", "debug", "prefs")
			if err != nil {
				return fmt.Errorf("read tailscale preferences: %w", err)
			}
			prefs, err := parseTailscalePrefs(raw)
			if err != nil {
				return err
			}
			if e.Cfg.Gateway != "" {
				routes, changed := mergeAdvertisedRoutes(prefs.AdvertiseRoutes, e.Cfg.guestSubnet())
				if !changed {
					return nil
				}
				if _, err := e.run("tailscale", "set", "--advertise-routes="+strings.Join(routes, ",")); err != nil {
					return fmt.Errorf("set tailscale advertised routes: %w", err)
				}
				return nil
			}
			if prefs.RouteAll {
				return nil
			}
			if _, err := e.run("tailscale", "set", "--accept-routes=true"); err != nil {
				return fmt.Errorf("enable tailscale accept-routes: %w", err)
			}
			return nil
		},
	}
}

func checkTailscaleStatus(p Probe, cfg Config) Result {
	if !fleetRoutingExpected(cfg) {
		return pass("standalone route integration not requested")
	}
	raw, err := p.Run("tailscale", "status", "--json")
	if err != nil {
		return fail("tailscale status unavailable",
			"install and authenticate Tailscale, then re-run `sparkbox setup --guest-subnet "+cfg.guestSubnet()+"`")
	}
	status, err := parseTailscaleStatus(raw)
	if err != nil {
		return fail(err.Error(), "upgrade Tailscale or inspect `tailscale status --json`")
	}
	if status.BackendState != "Running" || !status.Self.Online {
		return fail("backend="+orDash(status.BackendState)+" online="+fmt.Sprint(status.Self.Online),
			"bring Tailscale online before enabling fleet guest routing")
	}
	return pass("Running and online")
}

func checkTailscalePreferences(p Probe, cfg Config) Result {
	if !fleetRoutingExpected(cfg) {
		return pass("standalone route integration not requested")
	}
	raw, err := p.Run("tailscale", "debug", "prefs")
	if err != nil {
		return fail("could not read Tailscale preferences",
			"run `tailscale debug prefs`; setup needs an authenticated local Tailscale daemon")
	}
	prefs, err := parseTailscalePrefs([]byte(raw))
	if err != nil {
		return fail(err.Error(), "inspect `tailscale debug prefs`")
	}
	if cfg.Gateway != "" {
		_, missing := mergeAdvertisedRoutes(prefs.AdvertiseRoutes, probeGuestSubnet(p, cfg))
		if missing {
			return fail("not advertising "+probeGuestSubnet(p, cfg),
				"re-run setup with this node's explicit --guest-subnet")
		}
		return pass("advertising " + probeGuestSubnet(p, cfg))
	}
	if !prefs.RouteAll {
		return fail("accept-routes is disabled",
			"run `tailscale set --accept-routes=true` or re-run setup")
	}
	return pass("accept-routes enabled")
}

func checkTailscaleRoutes(p Probe, cfg Config) Result {
	if !fleetRoutingExpected(cfg) {
		return pass("standalone route integration not requested")
	}
	raw, err := p.Run("tailscale", "status", "--json")
	if err != nil {
		return fail("could not inspect peer routes", "fix the Tailscale status check first")
	}
	status, err := parseTailscaleStatus(raw)
	if err != nil {
		return fail(err.Error(), "inspect `tailscale status --json`")
	}
	local, err := guestnet.Parse(probeGuestSubnet(p, cfg))
	if err != nil {
		return fail(err.Error(), "repair SPARKBOX_GUEST_SUBNET in "+cfg.envPath())
	}

	var peerRoutes []netip.Prefix
	for _, peer := range status.Peer {
		for _, rawPrefix := range peer.AllowedIPs {
			prefix, err := netip.ParsePrefix(rawPrefix)
			if err != nil || !guestRouteCandidate(prefix) {
				continue
			}
			prefix = prefix.Masked()
			if guestnet.Overlaps(local.Prefix(), prefix) {
				return fail(local.String()+" overlaps peer "+orDash(peer.HostName)+" route "+prefix.String(),
					"assign a unique non-overlapping --guest-subnet and re-approve the node route")
			}
			peerRoutes = append(peerRoutes, prefix)
		}
	}

	if cfg.Gateway != "" {
		host, _, err := net.SplitHostPort(cfg.Gateway)
		if err != nil {
			return fail("invalid gateway address "+cfg.Gateway, "pass --gateway as host:port")
		}
		out, err := p.Run("ip", "route", "get", strings.Trim(host, "[]"))
		if err != nil || !strings.Contains(out, "tailscale") {
			return fail("gateway route is not using Tailscale",
				"ensure the gateway name resolves to its tailnet address and Tailscale is online")
		}
		return pass("gateway route uses Tailscale; no peer prefix overlaps " + local.String())
	}

	if len(peerRoutes) == 0 {
		return warn("no approved peer guest routes visible",
			"approve node subnet routes in the Tailscale admin console")
	}
	sort.Slice(peerRoutes, func(i, j int) bool { return peerRoutes[i].String() < peerRoutes[j].String() })
	for _, prefix := range peerRoutes {
		network, err := guestnet.FromPrefix(prefix)
		if err != nil {
			continue
		}
		slot, _ := network.Slot(0)
		out, err := p.Run("ip", "route", "get", slot.Guest.String())
		if err != nil || !strings.Contains(out, "tailscale") {
			return fail("kernel route to "+prefix.String()+" is not using Tailscale",
				"approve the advertised route and enable accept-routes on this gateway")
		}
	}
	return pass(fmt.Sprintf("%d peer guest route(s) use Tailscale; no overlap with %s", len(peerRoutes), local))
}

func checkNodeControlReachability(p Probe, cfg Config) Result {
	if cfg.Gateway == "" {
		return pass("gateway-local control plane")
	}
	host, _, err := net.SplitHostPort(cfg.Gateway)
	if err != nil {
		return fail("invalid gateway address "+cfg.Gateway, "pass --gateway as host:port")
	}
	host = strings.Trim(host, "[]")
	if _, err := p.Run("tailscale", "ping", "--c=1", "--timeout=5s", host); err != nil {
		return fail("gateway "+host+" is not reachable over Tailscale",
			"check Tailscale ACLs, DNS, and node/gateway online status")
	}
	return pass("gateway " + host + " reachable over Tailscale")
}

const nodeControlMTLSCheckName = "node control mTLS"

func checkNodeControlMTLSUnavailable(_ Probe, cfg Config) Result {
	if cfg.Gateway == "" {
		return pass("gateway-local control plane")
	}
	return warn("certificate health probe is not configured",
		"wire Env.NodeControlHealth when node certificate paths and the gRPC health endpoint are available")
}

func probeGuestSubnet(p Probe, cfg Config) string {
	raw, err := p.ReadFile(cfg.envPath())
	if err != nil {
		return cfg.guestSubnet()
	}
	kv, err := parseEnv(strings.NewReader(string(raw)))
	if err != nil {
		return cfg.guestSubnet()
	}
	return effectiveGuestSubnet(cfg, kv)
}

func guestRouteCandidate(prefix netip.Prefix) bool {
	prefix = prefix.Masked()
	return prefix.IsValid() && prefix.Addr().Is4() && prefix.Addr().IsPrivate() &&
		prefix.Bits() >= 8 && prefix.Bits() <= guestnet.SlotBits
}
