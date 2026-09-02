package main

import (
	"slices"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
)

// The issuer's allowlist and the audience guests are told to ask for are two
// statements of one fact, and an operator who has to repeat it in
// --oidc-audiences will one day not. The failure when they disagree is remote
// from its cause: everything configures cleanly, every log line is green, and
// guests take a 400 from a mint nobody is watching.
func TestOpenAIAudienceJoinsTheIssuerAllowlist(t *testing.T) {
	configured := metadata.OpenAI{FederationRule: "idpm_ghi789"}

	t.Run("it is added when federation is on", func(t *testing.T) {
		got := withOpenAIAudience([]string{defaultAudience}, configured)
		if !slices.Contains(got, metadata.DefaultOpenAIAudience) {
			t.Errorf("allowlist = %v, want the OpenAI audience in it", got)
		}
		if !slices.Contains(got, defaultAudience) {
			t.Errorf("allowlist = %v, want HiveMind's audience still in it", got)
		}
	})

	t.Run("an operator's own audience is the one allowlisted", func(t *testing.T) {
		got := withOpenAIAudience([]string{defaultAudience},
			metadata.OpenAI{Audience: "api://sparkbox-codex", FederationRule: "idpm_ghi789"})
		if !slices.Contains(got, "api://sparkbox-codex") {
			t.Errorf("allowlist = %v, want the configured audience", got)
		}
		if slices.Contains(got, metadata.DefaultOpenAIAudience) {
			t.Errorf("allowlist = %v, want no default beside the override", got)
		}
	})

	t.Run("it is not added twice", func(t *testing.T) {
		got := withOpenAIAudience(
			[]string{defaultAudience, metadata.DefaultOpenAIAudience}, configured)
		if n := len(got); n != 2 {
			t.Errorf("allowlist = %v, want the audience listed once", got)
		}
	})

	t.Run("an unconfigured fleet is left alone", func(t *testing.T) {
		got := withOpenAIAudience([]string{defaultAudience}, metadata.OpenAI{})
		if len(got) != 1 || got[0] != defaultAudience {
			t.Errorf("allowlist = %v, want it untouched", got)
		}
	})

	// Empty means "any audience". Appending to it would silently NARROW the
	// issuer to one value, turning a permissive development host into one that
	// refuses the mint it was performing yesterday.
	t.Run("an empty allowlist still means any audience", func(t *testing.T) {
		if got := withOpenAIAudience(nil, configured); len(got) != 0 {
			t.Errorf("allowlist = %v, want it left empty rather than narrowed to one value", got)
		}
	})
}
