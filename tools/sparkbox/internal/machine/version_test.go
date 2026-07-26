package machine

import "testing"

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		name        string
		have, floor string
		want        bool
	}{
		// THE row this function exists for: macOS went 15 -> 26, so any lexical
		// or prefix comparison answers "no" here and refuses a perfectly good
		// Mac. (And uname -r on that same machine reports 25.5.0, the Darwin
		// version, which is a different number entirely.)
		{"macOS 26 clears a floor of 15", "26.5.2", "15.0", true},
		{"macOS 26 clears a bare 15", "26.5.2", "15", true},
		{"macOS 15.0 exactly clears its own floor", "15.0", "15.0", true},
		{"macOS 14.7 does not", "14.7", "15.0", false},
		{"container 1.1.0 clears its own floor", "1.1.0", "1.1.0", true},
		{"container 1.0.9 does not", "1.0.9", "1.1.0", false},
		{"container 1.2 clears 1.1.0", "1.2", "1.1.0", true},
		{"missing components read as zero", "2", "2.0.0", true},
		{"a suffix on the last component is tolerated", "1.1.0-beta", "1.1.0", true},
		{"unparseable have", "unknown", "1.1.0", false},
		{"empty have", "", "1.1.0", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VersionAtLeast(tc.have, tc.floor); got != tc.want {
				t.Errorf("VersionAtLeast(%q, %q) = %v, want %v", tc.have, tc.floor, got, tc.want)
			}
		})
	}
}

func TestAppleGeneration(t *testing.T) {
	tests := []struct {
		brand string
		gen   int
		ok    bool
	}{
		{"Apple M4 Max", 4, true},
		{"Apple M3", 3, true},
		{"Apple M1 Pro", 1, true},
		{"Apple M10 Ultra", 10, true},
		// An Intel Mac has no nested virtualization at all, and neither has
		// anything whose brand string we do not recognise: both must read as a
		// refusal rather than as a pass.
		{"Intel(R) Core(TM) i9-9880H CPU @ 2.30GHz", 0, false},
		{"", 0, false},
		{"Apple Silicon", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.brand, func(t *testing.T) {
			gen, ok := AppleGeneration(tc.brand)
			if gen != tc.gen || ok != tc.ok {
				t.Errorf("AppleGeneration(%q) = (%d, %v), want (%d, %v)", tc.brand, gen, ok, tc.gen, tc.ok)
			}
		})
	}
}
