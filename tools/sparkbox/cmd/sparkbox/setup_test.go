package main

import (
	"flag"
	"sort"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/hostsetup"
)

// TestEverySetupFlagHasAFate walks the REAL FlagSet and demands that every flag
// is classified at the darwin boundary.
//
// This is the guard on the one failure this whole layer can have quietly: on
// macOS the gateway runs inside a nested machine, so a flag that is neither
// forwarded, always emitted, refused nor Mac-only is a flag the operator typed
// and nothing ever acted on. The gateway then runs a configuration nobody asked
// for, and there is no error anywhere — F2's shape. Adding a flag without
// deciding its fate fails here rather than in production six months later.
func TestEverySetupFlagHasAFate(t *testing.T) {
	fs, _ := newSetupFlags(hostsetup.DefaultConfig())
	byFate := map[string][]string{}
	var orphans []string
	fs.VisitAll(func(f *flag.Flag) {
		fate := hostsetup.FlagFate(f.Name)
		if fate == "" {
			orphans = append(orphans, f.Name)
			return
		}
		byFate[fate] = append(byFate[fate], f.Name)
	})
	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Errorf("these `sparkbox setup` flags have no defined fate on macOS: %s\n"+
			"  Classify each one in internal/hostsetup/darwinargs.go: add it to forwardableFlags() "+
			"(it means something inside the machine), alwaysEmittedFlags, refusedOnDarwin (with the reason), "+
			"or darwinOnlyFlags.", strings.Join(orphans, ", "))
	}
	// Sanity: each bucket is non-empty, so a refactor that emptied one shows up.
	for _, fate := range []string{"always", "forwardable", "refused", "darwin-only"} {
		if len(byFate[fate]) == 0 {
			t.Errorf("no flag is classified %q — did the tables get lost?", fate)
		}
	}
}

func TestValidatePlatformFlags(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		given   []string
		wantErr string
	}{
		{name: "linux, ordinary flags", goos: "linux", given: []string{"proxy-domain", "sluice"}},
		{
			name: "linux rejects a machine flag", goos: "linux", given: []string{"machine-name"},
			wantErr: "--machine-name",
		},
		{name: "darwin, ordinary flags", goos: "darwin", given: []string{"proxy-domain", "gateway", "node-name"}},
		{
			// Refused loudly: an operator who typed this and got a gateway that
			// relocated no sshd would have no way to find out.
			name: "darwin rejects --move-admin-ssh", goos: "darwin", given: []string{"move-admin-ssh"},
			wantErr: "--move-admin-ssh cannot be used on macOS",
		},
		{
			name: "darwin rejects --bin-path", goos: "darwin", given: []string{"bin-path"},
			wantErr: "--bin-path cannot be used on macOS",
		},
		{name: "--dry-run works on both", goos: "linux", given: []string{"dry-run"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			given := map[string]bool{}
			for _, g := range tc.given {
				given[g] = true
			}
			err := validatePlatformFlags(tc.goos, given)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestSetupFlagsParse is a smoke test that the extracted declaration still
// wires every value through — a mis-assigned pointer in newSetupFlags would
// otherwise be invisible until someone provisioned a host with it.
func TestSetupFlagsParse(t *testing.T) {
	fs, o := newSetupFlags(hostsetup.DefaultConfig())
	if err := fs.Parse([]string{
		"--proxy-domain", "example.test",
		"--gateway", "gw.example.com:2222",
		"--node-name", "laptop",
		"--machine-name", "sparkbox-two",
		"--machine-cpus", "4",
		"--machine-memory-gb", "12",
		"--outer-kernel", "/tmp/vmlinux",
		"--sluice",
		"--dry-run",
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"proxy-domain", *o.domain, "example.test"},
		{"gateway", *o.gateway, "gw.example.com:2222"},
		{"node-name", *o.nodeName, "laptop"},
		{"machine-name", *o.machineName, "sparkbox-two"},
		{"machine-cpus", *o.machineCPUs, 4},
		{"machine-memory-gb", *o.machineMemGB, 12},
		{"outer-kernel", *o.outerKernel, "/tmp/vmlinux"},
		{"sluice", *o.sluice, true},
		{"dry-run", *o.dryRun, true},
	} {
		if c.got != c.want {
			t.Errorf("--%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}
