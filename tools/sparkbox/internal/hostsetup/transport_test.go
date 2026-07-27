package hostsetup

import (
	"strings"
	"testing"
)

func TestValidateTransportConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "zero value inherits auto defaults", cfg: Config{}},
		{name: "gateway auto", cfg: DefaultConfig()},
		{
			name: "node grpc and routed",
			cfg: Config{
				Gateway: "gateway.example:2222", NodeControlTransport: "grpc",
				NodeGRPCAddr: "100.64.0.20:9443", GuestDataTransport: "routed",
			},
		},
		{
			name: "forced node grpc needs listener",
			cfg: Config{
				Gateway: "gateway.example:2222", NodeControlTransport: "grpc",
				GuestDataTransport: "auto",
			},
			wantErr: "requires --node-grpc-addr",
		},
		{
			name: "routed cannot use ssh control",
			cfg: Config{
				NodeControlTransport: "ssh", GuestDataTransport: "routed",
			},
			wantErr: "requires gRPC node control",
		},
		{
			name: "gateway cannot bind node control",
			cfg: Config{
				NodeControlTransport: "auto", NodeGRPCAddr: "100.64.0.20:9443",
				GuestDataTransport: "auto",
			},
			wantErr: "has no effect on a gateway",
		},
		{
			name: "node cannot choose cluster identity",
			cfg: Config{
				Gateway: "gateway.example:2222", NodeControlTransport: "auto",
				GuestDataTransport: "auto", ClusterID: "prod-a",
			},
			wantErr: "has no effect on a fleet node",
		},
		{
			name: "gateway identity listener",
			cfg: Config{
				NodeControlTransport: "auto", GuestDataTransport: "auto",
				GatewayGRPCAddr: "100.64.0.10:9444",
			},
		},
		{
			name: "node cannot bind gateway identity",
			cfg: Config{
				Gateway: "gateway.example:2222", NodeControlTransport: "auto",
				NodeGRPCAddr: "100.64.0.20:9443", GatewayGRPCAddr: "100.64.0.10:9444",
				GuestDataTransport: "auto",
			},
			wantErr: "has no effect on a fleet node",
		},
		{
			name: "ssh disables gateway identity",
			cfg: Config{
				NodeControlTransport: "ssh", GuestDataTransport: "auto",
				GatewayGRPCAddr: "100.64.0.10:9444",
			},
			wantErr: "has no effect with --node-control-transport=ssh",
		},
		{
			name: "invalid listener",
			cfg: Config{
				Gateway: "gateway.example:2222", NodeControlTransport: "auto",
				NodeGRPCAddr: ":9443", GuestDataTransport: "auto",
			},
			wantErr: "concrete tailnet host:port",
		},
		{
			name: "unknown control transport",
			cfg: Config{
				NodeControlTransport: "magic", GuestDataTransport: "auto",
			},
			wantErr: "expected auto|ssh|grpc",
		},
		{
			name: "gateway staged control rollout",
			cfg: Config{
				NodeControlTransport: "auto", NodeControlRollout: "read-only",
				GuestDataTransport: "auto",
			},
		},
		{
			name: "node cannot own control rollout",
			cfg: Config{
				Gateway: "gateway.example:2222", NodeControlTransport: "auto",
				NodeControlRollout: "shadow", GuestDataTransport: "auto",
			},
			wantErr: "gateway owns transport selection",
		},
		{
			name: "staged control rollout requires grpc infrastructure",
			cfg: Config{
				NodeControlTransport: "ssh", NodeControlRollout: "shadow",
				GuestDataTransport: "auto",
			},
			wantErr: "requires --node-control-transport=auto|grpc",
		},
		{
			name: "gateway routed canary",
			cfg: Config{
				NodeControlTransport: "auto", GuestDataTransport: "auto",
				RoutedGuestCanaryPercent: 25,
			},
		},
		{
			name: "routed canary bounds",
			cfg: Config{
				NodeControlTransport: "auto", GuestDataTransport: "auto",
				RoutedGuestCanaryPercent: 101,
			},
			wantErr: "expected 0..100",
		},
		{
			name: "routed canary only applies to auto data",
			cfg: Config{
				NodeControlTransport: "auto", GuestDataTransport: "routed",
				RoutedGuestCanaryPercent: 25,
			},
			wantErr: "requires --guest-data-transport=auto",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTransportConfig(tc.cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestTransportFlagsPersistWithoutDisturbingLegacyUpgrades(t *testing.T) {
	cfg := DefaultConfig()
	e := &Env{Cfg: cfg}
	fresh := e.renderEnvFile()
	if !strings.Contains(fresh,
		"TRANSPORT_FLAGS=--node-control-transport=auto --node-control-rollout=inherit "+
			"--guest-data-transport=auto --routed-guest-canary-percent=100\n") {
		t.Fatalf("fresh env does not persist explicit transport defaults:\n%s", fresh)
	}
	if !strings.Contains(fresh, routedGuestCanaryExplicitEnv+"=0\n") {
		t.Fatalf("fresh env does not mark the default canary as implicit:\n%s", fresh)
	}

	legacy := map[string]string{"PROXY_DOMAIN": "example.test"}
	if _, managed := lookupSetting(e.managedEnv(legacy), transportFlagsEnv); managed {
		t.Fatal("an unasked upgrade should not add TRANSPORT_FLAGS and restart a legacy host")
	}
	if _, managed := lookupSetting(e.managedEnv(legacy), routedGuestCanaryExplicitEnv); managed {
		t.Fatal("an unasked upgrade should not add the routed-canary intent marker")
	}

	cfg.FlagsGiven = map[string]bool{"node-control-transport": true}
	e.Cfg = cfg
	value, managed := lookupSetting(e.managedEnv(legacy), transportFlagsEnv)
	if !managed || value != "--node-control-transport=auto --node-control-rollout=inherit "+
		"--guest-data-transport=auto --routed-guest-canary-percent=100" {
		t.Fatalf("explicit default was not persisted: managed=%v value=%q", managed, value)
	}
	if value, managed = lookupSetting(e.managedEnv(legacy), routedGuestCanaryExplicitEnv); !managed || value != "0" {
		t.Fatalf("implicit canary marker = %q, %v; want 0, true", value, managed)
	}

	cfg = DefaultConfig()
	cfg.GatewayGRPCAddr = "100.64.0.10:9444"
	e.Cfg = cfg
	value, managed = lookupSetting(e.managedEnv(legacy), transportFlagsEnv)
	if !managed || !strings.Contains(value, "--gateway-grpc-addr=100.64.0.10:9444") {
		t.Fatalf("gateway identity listener was not persisted: managed=%v value=%q", managed, value)
	}
}

func TestGeneratedTransportDefaultsRemainStandaloneAfterEnvRecovery(t *testing.T) {
	cfg := DefaultConfig()
	raw := (&Env{Cfg: cfg}).renderEnvFile()
	kv, err := parseEnv(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	effective := effectiveTransportConfig(DefaultConfig(), kv)
	if effective.RoutedGuestCanaryPercent != 100 || effective.RoutedGuestCanaryExplicit {
		t.Fatalf("generated defaults became an explicit rollout: %+v", effective)
	}
	if fleetRoutingExpected(effective) {
		t.Fatalf("generated standalone defaults unexpectedly require fleet routing: %+v", effective)
	}
}

func TestExplicitFullCanarySurvivesEnvRecovery(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FlagsGiven = map[string]bool{"routed-guest-canary-percent": true}
	raw := (&Env{Cfg: cfg}).renderEnvFile()
	if !strings.Contains(raw, routedGuestCanaryExplicitEnv+"=1\n") {
		t.Fatalf("explicit full canary was not marked:\n%s", raw)
	}
	kv, err := parseEnv(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	effective := effectiveTransportConfig(DefaultConfig(), kv)
	if effective.RoutedGuestCanaryPercent != 100 || !effective.RoutedGuestCanaryExplicit {
		t.Fatalf("explicit full canary intent was lost: %+v", effective)
	}
	if !fleetRoutingExpected(effective) {
		t.Fatalf("explicit full canary no longer requires fleet routing: %+v", effective)
	}
}

func TestOperatorTransportOverrideMakesFullCanaryExplicit(t *testing.T) {
	effective := effectiveTransportConfig(DefaultConfig(), map[string]string{
		transportFlagsEnv:            "--guest-data-transport=auto --routed-guest-canary-percent=100",
		routedGuestCanaryExplicitEnv: "0",
		"EXTRA_FLAGS":                "--routed-guest-canary-percent=100",
	})
	if !effective.RoutedGuestCanaryExplicit {
		t.Fatalf("operator-owned full canary override was treated as a default: %+v", effective)
	}
}

func TestEffectiveTransportConfigPreservesAndOverridesInUnitOrder(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Gateway = "gateway.example:2222"
	cfg.FlagsGiven = map[string]bool{"guest-data-transport": true}
	cfg.GuestDataTransport = "routed"
	kv := map[string]string{
		transportFlagsEnv: "--node-control-transport=grpc --node-control-rollout=shadow " +
			"--node-grpc-addr=100.64.0.20:9443 --guest-data-transport=ssh " +
			"--routed-guest-canary-percent=25",
		"OVERCOMMIT_FLAGS": "--node-control-transport=ssh",
		"TLS_FLAGS":        "--node-control-transport=auto",
		"EXTRA_FLAGS": "--node-control-transport=grpc --node-control-rollout=read-only " +
			"--node-grpc-addr=100.64.0.21:9443 --routed-guest-canary-percent=5",
	}
	effective := effectiveTransportConfig(cfg, kv)
	if effective.NodeControlTransport != "grpc" ||
		effective.NodeControlRollout != "read-only" ||
		effective.NodeGRPCAddr != "100.64.0.21:9443" ||
		effective.GuestDataTransport != "routed" ||
		effective.RoutedGuestCanaryPercent != 5 {
		t.Fatalf("effective config = %+v", effective)
	}

	probe := fakeProbe{files: map[string]string{
		cfg.envPath(): strings.Join([]string{
			"TRANSPORT_FLAGS=" + kv[transportFlagsEnv],
			"OVERCOMMIT_FLAGS=" + kv["OVERCOMMIT_FLAGS"],
			"TLS_FLAGS=" + kv["TLS_FLAGS"],
			"EXTRA_FLAGS=" + kv["EXTRA_FLAGS"],
			"",
		}, "\n"),
	}}
	fromDoctor := EffectiveTransportConfig(probe, cfg)
	if fromDoctor.NodeControlTransport != effective.NodeControlTransport ||
		fromDoctor.NodeControlRollout != effective.NodeControlRollout ||
		fromDoctor.NodeGRPCAddr != effective.NodeGRPCAddr ||
		fromDoctor.GuestDataTransport != effective.GuestDataTransport ||
		fromDoctor.RoutedGuestCanaryPercent != effective.RoutedGuestCanaryPercent {
		t.Fatalf("doctor effective config = %+v, want %+v", fromDoctor, effective)
	}
}

func TestEffectiveTransportConfigPreservesZeroPercentCanary(t *testing.T) {
	cfg := DefaultConfig()
	effective := effectiveTransportConfig(cfg, map[string]string{
		transportFlagsEnv: "--node-control-transport=auto --guest-data-transport=auto " +
			"--routed-guest-canary-percent=0",
	})
	if effective.RoutedGuestCanaryPercent != 0 || !effective.RoutedGuestCanaryExplicit {
		t.Fatalf("effective zero-percent canary was lost: %+v", effective)
	}
}

func TestEffectiveFleetConfigRecoversPersistedNodeIdentity(t *testing.T) {
	cfg := DefaultConfig()
	probe := fakeProbe{files: map[string]string{
		cfg.envPath(): strings.Join([]string{
			"GATEWAY_FLAG=--gateway gateway.example:2222 --node-name gpu-a",
			"TRANSPORT_FLAGS=--node-control-transport=grpc --node-grpc-addr=100.64.0.20:9443",
			"",
		}, "\n"),
	}}
	effective := EffectiveFleetConfig(probe, cfg)
	if effective.Gateway != "gateway.example:2222" ||
		effective.NodeName != "gpu-a" ||
		effective.NodeGRPCAddr != "100.64.0.20:9443" {
		t.Fatalf("effective fleet config = %+v", effective)
	}

	// An explicit doctor assertion remains authoritative over persisted role
	// metadata; the env is a default, not a way to ignore the operator.
	cfg.Gateway = "other.example:2222"
	cfg.NodeName = "gpu-b"
	cfg.FlagsGiven = map[string]bool{"gateway": true, "node-name": true}
	effective = EffectiveFleetConfig(probe, cfg)
	if effective.Gateway != cfg.Gateway || effective.NodeName != cfg.NodeName {
		t.Fatalf("explicit role assertion was overwritten: %+v", effective)
	}
}

func TestNormalizeTransportConfigCanonicalizesListeners(t *testing.T) {
	node := DefaultConfig()
	node.Gateway = "gateway.example:2222"
	node.NodeGRPCAddr = "100.64.0.20:09443"
	normalized, err := NormalizeTransportConfig(node)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.NodeGRPCAddr != "100.64.0.20:9443" {
		t.Fatalf("node address = %q", normalized.NodeGRPCAddr)
	}

	gateway := DefaultConfig()
	gateway.GatewayGRPCAddr = "[fd7a:115c:a1e0::10]:09444"
	normalized, err = NormalizeTransportConfig(gateway)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.GatewayGRPCAddr != "[fd7a:115c:a1e0::10]:9444" {
		t.Fatalf("gateway address = %q", normalized.GatewayGRPCAddr)
	}
}

func TestNodeGRPCAddressParticipatesInPortPreflight(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Cfg.Gateway = "gateway.example:2222"
	e.Cfg.NodeGRPCAddr = "100.64.0.20:9443"
	listener := &fakeListener{}
	e.Listen = listener
	if err := preflightPorts(e); err != nil {
		t.Fatal(err)
	}
	if len(listener.calls) != 1 || listener.calls[0] != "tcp/100.64.0.20:9443" {
		t.Fatalf("node port probes = %v", listener.calls)
	}

	// A later SSH-mode override disables the listener even if a preserved
	// TRANSPORT_FLAGS value still carries its old address.
	addrs, _ := effectiveAddrs(e.Cfg, map[string]string{
		transportFlagsEnv: "--node-control-transport=grpc --node-grpc-addr=100.64.0.20:9443",
		"EXTRA_FLAGS":     "--node-control-transport=ssh",
	})
	if got := addrs["--node-grpc-addr"]; got != "" {
		t.Fatalf("SSH mode retained node gRPC listener %q", got)
	}
}

func TestGatewayGRPCAddressParticipatesInPortPreflight(t *testing.T) {
	e, _ := testEnv(t, false)
	e.Cfg.GatewayGRPCAddr = "100.64.0.10:9444"
	listener := &fakeListener{}
	e.Listen = listener
	if err := preflightPorts(e); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range listener.calls {
		if call == "tcp/100.64.0.10:9444" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gateway identity listener missing from port probes: %v", listener.calls)
	}

	addrs, _ := effectiveAddrs(e.Cfg, map[string]string{
		transportFlagsEnv: "--node-control-transport=auto --gateway-grpc-addr=100.64.0.10:9444",
		"EXTRA_FLAGS":     "--node-control-transport=ssh",
	})
	if got := addrs["--gateway-grpc-addr"]; got != "" {
		t.Fatalf("SSH mode retained gateway gRPC listener %q", got)
	}
}

func TestServiceUnitIncludesTransportBundle(t *testing.T) {
	unit, err := renderService(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "$GUEST_SUBNET_FLAG $TRANSPORT_FLAGS $SUBNET6_FLAG") {
		t.Fatalf("service unit does not place TRANSPORT_FLAGS in precedence order:\n%s", unit)
	}
}
