package main

import (
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/hostsetup"
)

// TestDoctorMacFlagsAreClassified: `sparkbox doctor` declares the same machine
// flags `sparkbox setup` does, and for the same reason — a Mac whose gateway
// lives in a non-default machine must be diagnosable. They therefore have to be
// the SAME flags, in the same bucket, or the two commands would disagree about
// which VM they are talking about.
func TestDoctorMacFlagsAreClassified(t *testing.T) {
	for _, name := range doctorMacFlags {
		if fate := hostsetup.FlagFate(name); fate != "darwin-only" {
			t.Errorf("doctor's --%s is classified %q, want darwin-only "+
				"(if it moved buckets, doctor and setup no longer mean the same thing by it)", name, fate)
		}
	}
}

// doctorMacFlags mirrors the macOS-only flags declared in doctor(). Kept beside
// the test rather than exported from doctor(): the point is to notice when the
// two lists drift, which a shared variable would hide.
var doctorMacFlags = []string{"machine-name", "machine-image", "outer-kernel", "container-bin"}

func TestRejectMacOnlyFlags(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		given   []string
		wantErr string
	}{
		{name: "linux, ordinary doctor flags", goos: "linux", given: []string{"root", "gateway", "release"}},
		{
			// Not merely ignored: an operator who asked to diagnose a machine on
			// a host that has none is asking about something that cannot exist,
			// and a report that answered anyway would be answering about the
			// wrong host.
			name: "linux rejects --machine-name", goos: "linux", given: []string{"machine-name"},
			wantErr: "--machine-name",
		},
		{
			name: "linux names every offender, sorted", goos: "linux",
			given:   []string{"container-bin", "machine-name", "root"},
			wantErr: "--container-bin, --machine-name",
		},
		{name: "darwin accepts them", goos: "darwin", given: []string{"machine-name", "outer-kernel"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			given := map[string]bool{}
			for _, g := range tc.given {
				given[g] = true
			}
			err := rejectMacOnlyFlags(tc.goos, given)
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
