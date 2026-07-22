package ctlops

import (
	"slices"
	"strings"
	"testing"
)

// TestParseSize is the shipped `ctl resize` behaviour, moved here verbatim: a
// bare number means GB because that is the unit anyone sizing a sandbox disk is
// thinking in, and the error strings are part of the CLI contract.
func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr string // substring; "" means success
	}{
		{"25G", 25 * 1024, ""},
		{"25GB", 25 * 1024, ""},
		{"25g", 25 * 1024, ""},
		{"512M", 512, ""},
		{"512MB", 512, ""},
		{"512m", 512, ""},
		{" 64 G ", 64 * 1024, ""},
		{"25", 25 * 1024, ""}, // bare number is GB
		{"", 0, "bad size"},
		{"abc", 0, "bad size"},
		{"25T", 0, "bad size"}, // only G and M suffixes are understood
		{"0", 0, "must be positive"},
		{"-5G", 0, "must be positive"},
		{"2000G", 0, "exceeds the 1024 GB per-sandbox limit"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseSize(tc.in)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseSize(%q) = %v", tc.in, err)
				}
				if got != tc.want {
					t.Errorf("ParseSize(%q) = %d MB, want %d", tc.in, got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseSize(%q) = %d, want an error", tc.in, got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseSize(%q) error = %q, want it to mention %q", tc.in, err, tc.wantErr)
			}
		})
	}
}

// TestNormalizeTags: every write path runs through this, so the cap cannot be
// bypassed by a transport that never parses a flag. It must also be idempotent,
// because sshgw normalizes first and ctlops normalizes again.
func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{"empty", nil, nil, false},
		{"blank entries", []string{"", "  ", ","}, nil, false},
		{"lowercased", []string{"ML", "Prod"}, []string{"ml", "prod"}, false},
		{"comma split", []string{"ml,prod"}, []string{"ml", "prod"}, false},
		{"deduped and sorted", []string{"prod", "ml", "prod"}, []string{"ml", "prod"}, false},
		{"trimmed", []string{" ml , prod "}, []string{"ml", "prod"}, false},
		{"at the cap", tagsN(MaxTagsPerSandbox), nil, false},
		{"over the cap", tagsN(MaxTagsPerSandbox + 1), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTags(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeTags(%d tags) = %v, want an error", len(tc.in), got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeTags: %v", err)
			}
			if tc.want != nil && !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			again, err := NormalizeTags(got)
			if err != nil || !slices.Equal(again, got) {
				t.Errorf("not idempotent: %v -> %v (%v)", got, again, err)
			}
		})
	}
}

func tagsN(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "t" + itoaTest(i)
	}
	return out
}

// TestGenerateNameAvoidsCollisions exercises the built-in generator (the rig
// injects a fixed one, so this builds its own Ops).
func TestGenerateNameAvoidsCollisions(t *testing.T) {
	r := newRig(t)
	r.ops.newName = nil
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		n := r.ops.GenerateName()
		if n == "" || strings.ContainsAny(n, " _./") {
			t.Fatalf("generated name %q is not DNS-safe", n)
		}
		seen[n] = true
	}
	if len(seen) < 10 {
		t.Errorf("generator produced only %d distinct names in 50 draws", len(seen))
	}
	// A name already taken is never handed out.
	r.boxes.boxes["taken"] = r.boxes.boxes["alicebox"]
	for i := 0; i < 20; i++ {
		if r.ops.GenerateName() == "taken" {
			t.Fatal("generated a name that already exists")
		}
	}
}

// TestCapabilities is what a client reads to avoid provoking a KindDisabled.
func TestCapabilities(t *testing.T) {
	r := newRig(t)
	got := r.ops.Capabilities()
	want := Capabilities{
		Archiving: true, Snapshots: true, Scheduling: true, Tags: true,
		Routes: true, SessionTokens: true, Terminal: true,
	}
	if got != want {
		t.Errorf("Capabilities() = %+v, want %+v", got, want)
	}

	r2 := newRig(t)
	r2.ops.tags, r2.ops.schedules, r2.ops.routes, r2.ops.sessions = nil, nil, nil, nil
	r2.ops.xtermSubdomain = ""
	r2.boxes.archiving, r2.tmpl.on = false, false
	if got := r2.ops.Capabilities(); got != (Capabilities{}) {
		t.Errorf("Capabilities() on a bare host = %+v, want all false", got)
	}
}
