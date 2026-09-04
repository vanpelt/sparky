package ghapp

import (
	"maps"
	"slices"
	"testing"
)

// The core tier follows the attachment; the read tier never does. A write
// attachment means "this agent may push code", not "this agent may dismiss a
// security alert", and the token is handed to a model.
func TestMintPermissionsRaisesOnlyTheCoreTier(t *testing.T) {
	write := MintPermissions(PermWrite)
	for _, name := range []string{"contents", "pull_requests", "issues"} {
		if write[name] != PermWrite {
			t.Errorf("%s = %q at a write attachment, want write", name, write[name])
		}
	}
	for name, level := range write {
		if slices.Contains(coreScope, name) {
			continue
		}
		if level != PermRead {
			t.Errorf("%s = %q, want read: nothing outside the core tier may be raised", name, level)
		}
	}
	read := MintPermissions(PermRead)
	for name, level := range read {
		if level != PermRead {
			t.Errorf("a read attachment asked for %s=%q", name, level)
		}
	}
	// Same names either way; only the levels move.
	if !slices.Equal(slices.Sorted(maps.Keys(read)), slices.Sorted(maps.Keys(write))) {
		t.Errorf("read and write attachments ask for different permissions:\n read  %v\n write %v",
			slices.Sorted(maps.Keys(read)), slices.Sorted(maps.Keys(write)))
	}
}

// Dependabot alerts are the reason this tier exists: an agent asked "what is
// vulnerable in here" reaches GET /repos/{o}/{r}/dependabot/alerts, which is
// gated on exactly this permission and on nothing the old set carried.
func TestMintPermissionsCarriesTheDependabotRead(t *testing.T) {
	if got := MintPermissions(PermWrite)["vulnerability_alerts"]; got != PermRead {
		t.Fatalf("vulnerability_alerts = %q, want read", got)
	}
}

func TestCoreMintPermissionsIsTheOldSet(t *testing.T) {
	core := CoreMintPermissions(PermWrite)
	want := map[string]string{"contents": PermWrite, "pull_requests": PermWrite, "issues": PermWrite}
	if !maps.Equal(core, want) {
		t.Fatalf("core set = %v, want %v", core, want)
	}
	if !IsCoreOnly(core) {
		t.Error("the core set does not report itself as core-only")
	}
	if IsCoreOnly(MintPermissions(PermWrite)) {
		t.Error("the full set reported itself as core-only; a retry would repeat the identical request")
	}
}

func TestCoversComparesNamesAndLevels(t *testing.T) {
	for _, tc := range []struct {
		name string
		have map[string]string
		want map[string]string
		ok   bool
	}{
		{"identical", map[string]string{"contents": "write"}, map[string]string{"contents": "write"}, true},
		{"missing name", map[string]string{"contents": "write"}, map[string]string{"contents": "write", "issues": "read"}, false},
		{"read does not cover write", map[string]string{"contents": "read"}, map[string]string{"contents": "write"}, false},
		{"write covers read", map[string]string{"contents": "write"}, map[string]string{"contents": "read"}, true},
		{"admin covers write", map[string]string{"contents": "admin"}, map[string]string{"contents": "write"}, true},
		// One-directional on purpose: an attachment downgraded from write to
		// read is not a reason to nag anybody to re-authorize.
		{"wider than asked", map[string]string{"contents": "write", "issues": "write"}, map[string]string{"contents": "read"}, true},
		{"nothing wanted", nil, nil, true},
		{"nothing held", nil, map[string]string{"contents": "read"}, false},
	} {
		if got := Covers(tc.have, tc.want); got != tc.ok {
			t.Errorf("%s: Covers(%v, %v) = %v, want %v", tc.name, tc.have, tc.want, got, tc.ok)
		}
	}
}

// Missing is Narrow's complement, and the only way to tell a permission that
// was never granted apart from one whose name is a typo — Narrow drops both in
// silence and mints a working, narrower token either way.
func TestMissingIsNarrowsComplement(t *testing.T) {
	inst := Installation{Permissions: map[string]string{"contents": "write", "metadata": "read"}}
	want := MintPermissions(PermWrite)

	narrowed := inst.Narrow(want)
	missing := Missing(inst.Permissions, want)
	if len(narrowed)+len(missing) != len(want) {
		t.Fatalf("narrow (%v) and missing (%v) do not partition the request (%d)", narrowed, missing, len(want))
	}
	for _, name := range missing {
		if _, ok := narrowed[name]; ok {
			t.Errorf("%s is reported both granted and missing", name)
		}
	}
	if !slices.Contains(missing, "vulnerability_alerts") {
		t.Errorf("missing = %v, want it to name the ungranted Dependabot permission", missing)
	}
	if !slices.IsSorted(missing) {
		t.Errorf("missing = %v, want sorted so a UI does not reorder between polls", missing)
	}
}

// `metadata` is mandatory on every GitHub App and always comes back, so an
// installation reporting nothing is an answer this code did not learn — not an
// App that holds nothing. Reporting the whole request as missing there would
// put a wall of names in front of every operator on no evidence.
func TestMissingSaysNothingWhenTheInstallationReportedNothing(t *testing.T) {
	if got := Missing(nil, MintPermissions(PermWrite)); got != nil {
		t.Fatalf("missing = %v against an unknown installation, want none", got)
	}
	if got := Missing(map[string]string{}, MintPermissions(PermWrite)); got != nil {
		t.Fatalf("missing = %v against an empty permission map, want none", got)
	}
}

func TestNarrowToMatchesTheMethod(t *testing.T) {
	inst := Installation{Permissions: map[string]string{"contents": "read", "actions": "read"}}
	want := MintPermissions(PermWrite)
	if !maps.Equal(NarrowTo(inst.Permissions, want), inst.Narrow(want)) {
		t.Fatal("NarrowTo and Installation.Narrow disagree")
	}
	// A write request against a read grant is downgraded, not dropped.
	if got := NarrowTo(inst.Permissions, want)["contents"]; got != PermRead {
		t.Fatalf("contents = %q, want read", got)
	}
}
