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

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

func TestParseCreateArgs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantTags []string
		wantNode string
		wantRest []string
		wantRefs []ctlops.RepoRef
	}{
		// The tripwire case, restated here in the function the door actually
		// calls: a bare word is still a tag, and --node has not eaten it.
		{name: "bare words are still tags", args: []string{"snap", "box"}, wantRest: []string{"snap", "box"}},
		{name: "tags alone", args: []string{"--tag", "ml,prod"}, wantTags: []string{"ml", "prod"}},

		{name: "node with a value", args: []string{"--node", "dgx"}, wantNode: "dgx"},
		{name: "node in the equals form", args: []string{"--node=dgx"}, wantNode: "dgx"},
		// The whole reason the flag is parsed here: without it the door sees the
		// three bare words `--node`, `dgx` and `ml`, folds all of them into the
		// tag list, and builds the sandbox on the gateway. `ml` stays in rest
		// because a bare word is the door's business, not this grammar's.
		{name: "node and a bare tag", args: []string{"--node", "dgx", "ml"}, wantNode: "dgx", wantRest: []string{"ml"}},
		{name: "node between tags", args: []string{"--tag", "ml", "--node", "dgx", "--tag", "prod"},
			wantTags: []string{"ml", "prod"}, wantNode: "dgx"},
		{name: "node after positionals", args: []string{"snap", "box", "--node=dgx"}, wantNode: "dgx", wantRest: []string{"snap", "box"}},
		// Naming one machine twice is a correction, not a list — unlike --tag,
		// which accumulates.
		{name: "the last node wins", args: []string{"--node", "a", "--node=b"}, wantNode: "b"},
		// A value that looks like a flag is still a value: consuming the next
		// argument is what every flag in this grammar does, and second-guessing
		// it would make `--node --tag` mean two different things depending on
		// which flag ran first.
		{name: "a flag-shaped value is a value", args: []string{"--node", "--tag"}, wantNode: "--tag"},

		// --ref, in both spellings and both forms. The bare one names no
		// repository on purpose: which one it means depends on what the tags
		// select, which this grammar cannot see. ctlops refuses the ambiguity.
		{name: "a bare ref", args: []string{"--ref", "feat/x"},
			wantRefs: []ctlops.RepoRef{{Ref: "feat/x"}}},
		{name: "a bare ref in the equals form", args: []string{"--ref=feat/x"},
			wantRefs: []ctlops.RepoRef{{Ref: "feat/x"}}},
		{name: "a scoped ref", args: []string{"--ref", "wandb/hivemind=feat/x"},
			wantRefs: []ctlops.RepoRef{{Slug: "wandb/hivemind", Ref: "feat/x"}}},
		// Unlike --node, --ref accumulates: one branch per attached repository.
		{name: "refs accumulate", args: []string{"--ref", "a/b=main", "--ref=c/d=dev"},
			wantRefs: []ctlops.RepoRef{{Slug: "a/b", Ref: "main"}, {Slug: "c/d", Ref: "dev"}}},
		// A branch name may legally contain '=', so the scope separator is a
		// '=' whose left side holds a '/'. Without that rule this would become
		// a request for a repository called "weird" that nobody attached.
		{name: "an equals in a branch name is not a scope", args: []string{"--ref", "weird=name"},
			wantRefs: []ctlops.RepoRef{{Ref: "weird=name"}}},
		// The flags coexist, and none of them leaks into the tag list.
		{name: "everything at once", args: []string{"--tag", "ml", "--node", "dgx", "--ref", "a/b=main", "box"},
			wantTags: []string{"ml"}, wantNode: "dgx", wantRest: []string{"box"},
			wantRefs: []ctlops.RepoRef{{Slug: "a/b", Ref: "main"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCreateArgs(tc.args)
			if err != nil {
				t.Fatalf("parseCreateArgs(%q) errored: %v", tc.args, err)
			}
			if !reflect.DeepEqual(got.Tags, tc.wantTags) {
				t.Errorf("tags = %#v, want %#v", got.Tags, tc.wantTags)
			}
			if got.Node != tc.wantNode {
				t.Errorf("node = %q, want %q", got.Node, tc.wantNode)
			}
			if !reflect.DeepEqual(got.Rest, tc.wantRest) {
				t.Errorf("rest = %#v, want %#v", got.Rest, tc.wantRest)
			}
			if !reflect.DeepEqual(got.Refs, tc.wantRefs) {
				t.Errorf("refs = %#v, want %#v", got.Refs, tc.wantRefs)
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
		if _, err := parseCreateArgs(args); err == nil {
			t.Errorf("parseCreateArgs(%q) was accepted", args)
		} else if !strings.Contains(err.Error(), "--node needs a value") {
			t.Errorf("parseCreateArgs(%q) = %v, want a sentence naming the flag", args, err)
		}
	}
	// And the tag grammar's own refusals still arrive through this door.
	if _, err := parseCreateArgs([]string{"--node", "dgx", "--tag"}); err == nil {
		t.Error("--tag with no value was accepted")
	}
	// --ref refuses on the same terms, including the scoped form with an empty
	// right-hand side: `--ref owner/repo=` names a repository and no branch,
	// which is a typo, not a request to use the default.
	for _, args := range [][]string{{"--ref"}, {"--ref="}, {"--ref", "  "}, {"--ref", "a/b="}} {
		if _, err := parseCreateArgs(args); err == nil {
			t.Errorf("parseCreateArgs(%q) was accepted", args)
		} else if !strings.Contains(err.Error(), "--ref") {
			t.Errorf("parseCreateArgs(%q) = %v, want a sentence naming the flag", args, err)
		}
	}
}
