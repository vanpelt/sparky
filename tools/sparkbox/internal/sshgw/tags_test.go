package sshgw

// Tests for --tag parsing at sandbox creation.

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseTags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantTags []string
		wantRest []string
	}{
		{"none", []string{"snap", "box"}, nil, []string{"snap", "box"}},
		{"repeated flag", []string{"--tag", "ml", "--tag", "prod"}, []string{"ml", "prod"}, nil},
		{"comma separated", []string{"--tag", "ml,prod"}, []string{"ml", "prod"}, nil},
		{"equals form", []string{"--tag=ml", "--tag=prod"}, []string{"ml", "prod"}, nil},
		{"short flag", []string{"-t", "ml"}, []string{"ml"}, nil},
		{"mixed with positionals", []string{"snap", "--tag", "ml", "box"}, []string{"ml"}, []string{"snap", "box"}},
		// Tags are matched against secret tags, so case must not create two
		// distinct universes of meaning.
		{"lowercased", []string{"--tag", "ML"}, []string{"ml"}, nil},
		{"deduped and sorted", []string{"--tag", "prod,ml,prod"}, []string{"ml", "prod"}, nil},
		{"blanks dropped", []string{"--tag", "ml,,  ,prod"}, []string{"ml", "prod"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tags, rest, err := parseTags(tc.args)
			if err != nil {
				t.Fatalf("parseTags(%q) errored: %v", tc.args, err)
			}
			if !reflect.DeepEqual(tags, tc.wantTags) {
				t.Errorf("tags = %#v, want %#v", tags, tc.wantTags)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %#v, want %#v", rest, tc.wantRest)
			}
		})
	}
}

func TestParseTagsRejects(t *testing.T) {
	if _, _, err := parseTags([]string{"--tag"}); err == nil {
		t.Error("--tag with no value was accepted")
	}
	if _, _, err := parseTags([]string{"-t"}); err == nil {
		t.Error("-t with no value was accepted")
	}
	many := make([]string, 0, 2*(maxTagsPerSandbox+1))
	for i := 0; i <= maxTagsPerSandbox; i++ {
		many = append(many, "--tag", string(rune('a'+i%26))+strings.Repeat("x", i))
	}
	if _, _, err := parseTags(many); err == nil {
		t.Error("an over-long tag list was accepted")
	}
}

// TestApplyTagsWithoutStoreErrors: a host with no secrets store must say so
// rather than silently dropping tags the user asked for.
func TestApplyTagsWithoutStoreErrors(t *testing.T) {
	gw, _, _ := newDoorGateway(t) // built without Tags
	if err := gw.applyTags("box", "alice", []string{"ml"}); err == nil {
		t.Fatal("tagging without a store silently succeeded")
	}
	// No tags requested is not an error — the common path must stay quiet.
	if err := gw.applyTags("box", "alice", nil); err != nil {
		t.Fatalf("untagged create errored: %v", err)
	}
}
