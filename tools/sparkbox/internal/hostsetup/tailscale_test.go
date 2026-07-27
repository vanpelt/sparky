package hostsetup

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestGuestSubnetSettingsPreserveLiveOverrides(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		env  map[string]string
		want string
	}{
		{
			name: "compiled default",
			cfg:  DefaultConfig(),
			want: "172.30.0.0/16",
		},
		{
			name: "existing network-script value",
			cfg:  DefaultConfig(),
			env:  map[string]string{"SPARKBOX_GUEST_SUBNET": "10.44.7.9/20"},
			want: "10.44.0.0/20",
		},
		{
			name: "operator bundle wins exactly as ExecStart does",
			cfg:  DefaultConfig(),
			env: map[string]string{
				"SPARKBOX_GUEST_SUBNET": "10.44.0.0/20",
				"EXTRA_FLAGS":           "--guest-subnet=10.88.7.9/20 --something-else",
			},
			want: "10.88.0.0/20",
		},
		{
			name: "explicit setup flag updates an old managed value",
			cfg: func() Config {
				cfg := DefaultConfig()
				cfg.GuestSubnet = "10.99.7.9/20"
				cfg.FlagsGiven = map[string]bool{"guest-subnet": true}
				return cfg
			}(),
			env:  map[string]string{"SPARKBOX_GUEST_SUBNET": "10.44.0.0/20"},
			want: "10.99.0.0/20",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := guestSubnetSettings(test.cfg, test.env)
			got := map[string]string{}
			for _, setting := range settings {
				got[setting.key] = setting.val
			}
			if got["SPARKBOX_GUEST_SUBNET"] != test.want {
				t.Errorf("SPARKBOX_GUEST_SUBNET = %q, want %q", got["SPARKBOX_GUEST_SUBNET"], test.want)
			}
			if got["GUEST_SUBNET_FLAG"] != "--guest-subnet="+test.want {
				t.Errorf("GUEST_SUBNET_FLAG = %q, want --guest-subnet=%s", got["GUEST_SUBNET_FLAG"], test.want)
			}
		})
	}
}

func TestMergeAdvertisedRoutesPreservesUnrelatedRoutes(t *testing.T) {
	existing := []string{"10.20.0.0/24", "192.168.50.0/24"}
	got, changed := mergeAdvertisedRoutes(existing, "10.44.7.9/20")
	want := []string{"10.20.0.0/24", "192.168.50.0/24", "10.44.0.0/20"}
	if !changed || !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %v, %v; want %v, true", got, changed, want)
	}
	got, changed = mergeAdvertisedRoutes(got, "10.44.0.1/20")
	if changed || !reflect.DeepEqual(got, want) {
		t.Fatalf("idempotent merge = %v, %v; want %v, false", got, changed, want)
	}
}

func TestFleetRoutingExpectedFromDurableGatewayTransport(t *testing.T) {
	for _, cfg := range []Config{
		{GatewayGRPCAddr: "100.64.0.10:9444"},
		{ClusterID: "prod-a"},
		{NodeControlTransport: "grpc"},
		{GuestDataTransport: "routed"},
	} {
		if !fleetRoutingExpected(cfg) {
			t.Fatalf("durable fleet gateway config was treated as standalone: %+v", cfg)
		}
	}
	if fleetRoutingExpected(Config{
		NodeControlTransport: "auto", GuestDataTransport: "auto",
	}) {
		t.Fatal("an unconfigured standalone gateway unexpectedly requires Tailscale")
	}
}

func TestGuestSubnetForwardsToDarwinInnerSetup(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Gateway = "gw.tailnet:2222"
	cfg.GuestSubnet = "10.44.0.0/20"
	cfg.FlagsGiven = map[string]bool{"guest-subnet": true}
	args, err := innerSetupArgs(cfg, "v1.2.3", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--guest-subnet" && args[i+1] == "10.44.0.0/20" {
			return
		}
	}
	t.Fatalf("inner setup args omit guest subnet: %v", args)
}

func TestTailscaleSetupMutatesOnlyTheIntendedPreference(t *testing.T) {
	t.Run("node merges advertised route", func(t *testing.T) {
		runner := &fakeRunner{results: map[string]struct {
			out []byte
			err error
		}{
			"tailscale debug prefs": {
				out: []byte(`{"AdvertiseRoutes":["10.20.0.0/24","192.168.50.0/24"],"RouteAll":false}`),
			},
			"tailscale set --advertise-routes=10.20.0.0/24,192.168.50.0/24,10.44.0.0/20": {},
		}}
		cfg := DefaultConfig()
		cfg.Gateway = "gw.tailnet:2222"
		cfg.GuestSubnet = "10.44.0.0/20"
		cfg.FlagsGiven = map[string]bool{"guest-subnet": true}
		env := &Env{Ctx: context.Background(), Cfg: cfg, Run: runner}
		step := stepTailscaleRoutes()
		if sat, _, err := step.Satisfied(env); err != nil || sat {
			t.Fatalf("Satisfied = %v, %v; want false, nil", sat, err)
		}
		if err := step.Apply(env); err != nil {
			t.Fatal(err)
		}
		last := runner.calls[len(runner.calls)-1]
		if last != "tailscale set --advertise-routes=10.20.0.0/24,192.168.50.0/24,10.44.0.0/20" {
			t.Fatalf("mutation = %q", last)
		}
	})

	t.Run("gateway changes accept-routes only", func(t *testing.T) {
		runner := &fakeRunner{results: map[string]struct {
			out []byte
			err error
		}{
			"tailscale debug prefs":              {out: []byte(`{"AdvertiseRoutes":["10.20.0.0/24"],"RouteAll":false}`)},
			"tailscale set --accept-routes=true": {},
		}}
		cfg := DefaultConfig()
		cfg.GuestSubnet = "10.60.0.0/20"
		cfg.FlagsGiven = map[string]bool{"guest-subnet": true}
		env := &Env{Ctx: context.Background(), Cfg: cfg, Run: runner}
		if err := stepTailscaleRoutes().Apply(env); err != nil {
			t.Fatal(err)
		}
		last := runner.calls[len(runner.calls)-1]
		if last != "tailscale set --accept-routes=true" {
			t.Fatalf("mutation = %q", last)
		}
	})
}

func TestTailscaleDoctorChecksNodeRouteAndReachability(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Gateway = "gw.tailnet:2222"
	cfg.GuestSubnet = "10.44.0.0/20"
	cfg.FlagsGiven = map[string]bool{"guest-subnet": true}
	probe := fakeProbe{
		runs: map[string]runResult{
			"tailscale status --json": {
				out: `{"BackendState":"Running","Self":{"Online":true},"Peer":{}}`,
			},
			"tailscale debug prefs": {
				out: `{"AdvertiseRoutes":["10.20.0.0/24","10.44.0.0/20"],"RouteAll":false}`,
			},
			"ip route get gw.tailnet": {
				out: "100.64.0.1 dev tailscale0 src 100.64.0.2",
			},
			"tailscale ping --c=1 --timeout=5s gw.tailnet": {
				out: "pong from gw.tailnet",
			},
		},
	}
	for name, check := range map[string]func(Probe, Config) Result{
		"status":       checkTailscaleStatus,
		"preferences":  checkTailscalePreferences,
		"routes":       checkTailscaleRoutes,
		"reachability": checkNodeControlReachability,
	} {
		if result := check(probe, cfg); result.Status != Pass {
			t.Errorf("%s = %+v", name, result)
		}
	}
}

func TestTailscaleDoctorRejectsPeerPrefixOverlap(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GuestSubnet = "10.44.0.0/20"
	cfg.FlagsGiven = map[string]bool{"guest-subnet": true}
	probe := fakeProbe{runs: map[string]runResult{
		"tailscale status --json": {
			out: `{
				"BackendState":"Running",
				"Self":{"Online":true},
				"Peer":{"key":{"HostName":"node-b","AllowedIPs":["100.64.0.3/32","10.44.8.0/21"]}}
			}`,
		},
	}}
	result := checkTailscaleRoutes(probe, cfg)
	if result.Status != Fail || !strings.Contains(result.Detail, "overlaps") {
		t.Fatalf("overlap check = %+v", result)
	}
}

func TestNATCheckUsesPersistedGuestSubnet(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Root = "/srv/test"
	cfg.GuestSubnet = "10.44.0.0/20"
	probe := fakeProbe{
		files: map[string]string{
			cfg.envPath(): "SPARKBOX_GUEST_SUBNET=10.44.0.0/20\nGUEST_SUBNET_FLAG=--guest-subnet=10.44.0.0/20\n",
		},
		runs: map[string]runResult{
			natList("POSTROUTING"): {out: "MASQUERADE all -- 10.44.0.0/20 0.0.0.0/0"},
			natList(edgeChain):     {out: "Chain " + edgeChain},
		},
	}
	result := checkNAT(probe, cfg)
	if result.Status != Pass || !strings.Contains(result.Detail, "10.44.0.0/20") {
		t.Fatalf("custom NAT check = %+v", result)
	}
}

func TestDoctorNodeControlHealthIsPluggable(t *testing.T) {
	env := &Env{
		Cfg:   Config{Gateway: "gw.tailnet:2222"},
		Probe: fakeProbe{},
		NodeControlHealth: &Check{Run: func(Probe, Config) Result {
			return pass("certificate and gRPC health verified")
		}},
	}
	checks := DoctorChecksFor(env)
	for _, check := range checks {
		if check.Name != nodeControlMTLSCheckName {
			continue
		}
		result := check.Run(env.Probe, env.Cfg)
		if result.Status != Pass || !strings.Contains(result.Detail, "verified") {
			t.Fatalf("injected mTLS check = %+v", result)
		}
		return
	}
	t.Fatal("node control mTLS check missing")
}

func TestTailscaleSetupReportsUnavailableDaemon(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Gateway = "gw.tailnet:2222"
	cfg.FlagsGiven = map[string]bool{"guest-subnet": true}
	env := &Env{
		Ctx: context.Background(), Cfg: cfg,
		Run: &fakeRunner{results: map[string]struct {
			out []byte
			err error
		}{
			"tailscale debug prefs": {err: errors.New("not running")},
		}},
	}
	if err := stepTailscaleRoutes().Apply(env); err == nil {
		t.Fatal("unavailable Tailscale daemon was accepted")
	}
}

var _ Runner = (*fakeRunner)(nil)
