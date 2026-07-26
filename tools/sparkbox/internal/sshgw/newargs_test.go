package sshgw

// Tests for the create-argument grammar: --node alongside --tag, and the bare
// words that are neither.
//
// These live apart from tags_test.go on purpose. That file pins the TAG
// grammar, and it is a tripwire: the new@ door folds every leftover word into
// the tag list, so a change to what parseTags consumes silently changes what a
// sandbox comes up tagged with. Adding --node must not move a single one of its
// expectations, and keeping the two files separate is what makes that visible
// in a diff rather than a matter of trust.

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseCreateArgs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantTags []string
		wantNode string
		wantRest []string
	}{
		// The tripwire case, restated here in the function the door actually
		// calls: a bare word is still a tag, and --node has not eaten it.
		{"bare words are still tags", []string{"snap", "box"}, nil, "", []string{"snap", "box"}},
		{"tags alone", []string{"--tag", "ml,prod"}, []string{"ml", "prod"}, "", nil},

		{"node with a value", []string{"--node", "dgx"}, nil, "dgx", nil},
		{"node in the equals form", []string{"--node=dgx"}, nil, "dgx", nil},
		// The whole reason the flag is parsed here: without it the door sees the
		// three bare words `--node`, `dgx` and `ml`, folds all of them into the
		// tag list, and builds the sandbox on the gateway. `ml` stays in rest
		// because a bare word is the door's business, not this grammar's.
		{"node and a bare tag", []string{"--node", "dgx", "ml"}, nil, "dgx", []string{"ml"}},
		{"node between tags", []string{"--tag", "ml", "--node", "dgx", "--tag", "prod"},
			[]string{"ml", "prod"}, "dgx", nil},
		{"node after positionals", []string{"snap", "box", "--node=dgx"}, nil, "dgx", []string{"snap", "box"}},
		// Naming one machine twice is a correction, not a list — unlike --tag,
		// which accumulates.
		{"the last node wins", []string{"--node", "a", "--node=b"}, nil, "b", nil},
		// A value that looks like a flag is still a value: consuming the next
		// argument is what every flag in this grammar does, and second-guessing
		// it would make `--node --tag` mean two different things depending on
		// which flag ran first.
		{"a flag-shaped value is a value", []string{"--node", "--tag"}, nil, "--tag", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tags, node, rest, err := parseCreateArgs(tc.args)
			if err != nil {
				t.Fatalf("parseCreateArgs(%q) errored: %v", tc.args, err)
			}
			if !reflect.DeepEqual(tags, tc.wantTags) {
				t.Errorf("tags = %#v, want %#v", tags, tc.wantTags)
			}
			if node != tc.wantNode {
				t.Errorf("node = %q, want %q", node, tc.wantNode)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %#v, want %#v", rest, tc.wantRest)
			}
		})
	}
}

// A --node the user meant to fill in is refused rather than read as "wherever
// you like": building on the gateway while they wait to be told which machine
// it went to is the silent-swallow failure with extra steps.
func TestParseCreateArgsRejects(t *testing.T) {
	for _, args := range [][]string{
		{"--node"},
		{"ml", "--node"},
		{"--node="},
		{"--node", "   "},
	} {
		if _, _, _, err := parseCreateArgs(args); err == nil {
			t.Errorf("parseCreateArgs(%q) was accepted", args)
		} else if !strings.Contains(err.Error(), "--node needs a value") {
			t.Errorf("parseCreateArgs(%q) = %v, want a sentence naming the flag", args, err)
		}
	}
	// And the tag grammar's own refusals still arrive through this door.
	if _, _, _, err := parseCreateArgs([]string{"--node", "dgx", "--tag"}); err == nil {
		t.Error("--tag with no value was accepted")
	}
}
