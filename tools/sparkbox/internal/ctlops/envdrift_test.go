package ctlops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
)

// TestClassifyDriftIsAPureRule pins the verdict table itself, without a store,
// because the rule is the feature: everything above it is caching and
// rendering, and every one of these rows is a sentence somebody reads on a card.
func TestClassifyDriftIsAPureRule(t *testing.T) {
	const stored = "#!/bin/sh\necho one\n"
	const other = "#!/bin/sh\necho two\n"
	seed := envs.ScriptSHA(stored)

	for _, tc := range []struct {
		name              string
		stored, seed, rep string
		want              string
	}{
		{
			// Nothing to compare against. Said as "no answer" and never as
			// "match", because a surface must not reassure on a question it
			// could not ask.
			name: "no repository file", stored: stored, rep: "", want: "",
		},
		{
			name: "same bytes", stored: stored, seed: seed, rep: envs.ScriptSHA(stored),
			want: ScriptDriftMatch,
		},
		{
			// The committed-back case: identical content, no seed yet. The
			// content comparison has to win or the platform's own advice reads
			// as an error.
			name: "same bytes, never seeded", stored: stored, seed: "", rep: envs.ScriptSHA(stored),
			want: ScriptDriftMatch,
		},
		{
			// The row is untouched since seeding and the repository moved.
			name: "repository moved ahead", stored: stored, seed: seed, rep: envs.ScriptSHA(other),
			want: ScriptDriftRepoAhead,
		},
		{
			// The row was changed after seeding — a repair pass, or a person.
			name: "row changed since seeding", stored: other, seed: seed, rep: envs.ScriptSHA(stored),
			want: ScriptDriftDiverged,
		},
		{
			// No seed at all and the bytes differ: history unknown, so nothing
			// may be overwritten and a person is asked.
			name: "never seeded and different", stored: other, seed: "", rep: envs.ScriptSHA(stored),
			want: ScriptDriftDiverged,
		},
		{
			name: "no script yet", stored: "", rep: envs.ScriptSHA(stored),
			want: ScriptDriftRepoOnly,
		},
		{
			name: "no script and no repository file", stored: "", rep: "", want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDrift(tc.stored, tc.seed, tc.rep); got != tc.want {
				t.Errorf("classifyDrift = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestListEnvironmentsReportsScriptDrift is the rule reaching a surface, which
// is the only place it does anybody any good.
func TestListEnvironmentsReportsScriptDrift(t *testing.T) {
	const committed = "#!/bin/sh\necho what the repository says\n"
	const repaired = "#!/bin/sh\necho what the builder fixed\n"

	b := newBuildRig(t)
	b.env("alice", "current", withSeededScript(committed))
	b.env("alice", "behind", withSeededScript("#!/bin/sh\necho the old one\n"))
	b.env("alice", "mine", func(e *envs.Environment) {
		e.SetupScript, e.SetupFrom = repaired, envs.SetupFromRepo
		e.SetupSeedSHA = envs.ScriptSHA("#!/bin/sh\necho the old one\n")
	})
	b.env("alice", "empty")
	b.env("alice", "unattached", withSeededScript(committed))
	// One attachment carrying four tags, which is how a repository reaches
	// several environments — `unattached` is deliberately not among them.
	b.attach("alice", "wandb/hivemind", "main", "current", "behind", "mine", "empty")
	b.file("wandb/hivemind", "main", SetupScriptPath, committed)

	list, err := b.ops.ListEnvironments(alice())
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	got := make(map[string]EnvironmentInfo, len(list))
	for _, e := range list {
		got[e.Name] = e
	}
	for _, tc := range []struct{ env, want, wantRepo string }{
		{"current", ScriptDriftMatch, "wandb/hivemind"},
		{"behind", ScriptDriftRepoAhead, "wandb/hivemind"},
		{"mine", ScriptDriftDiverged, "wandb/hivemind"},
		{"empty", ScriptDriftRepoOnly, "wandb/hivemind"},
		// An environment whose tag no repository carries has nothing to be
		// compared with, and says nothing rather than "diverged".
		{"unattached", "", ""},
	} {
		e, ok := got[tc.env]
		if !ok {
			t.Errorf("%s is missing from the listing", tc.env)
			continue
		}
		if e.ScriptDrift != tc.want {
			t.Errorf("%s: drift = %q, want %q", tc.env, e.ScriptDrift, tc.want)
		}
		if e.ScriptDriftRepo != tc.wantRepo {
			t.Errorf("%s: drift repo = %q, want %q", tc.env, e.ScriptDriftRepo, tc.wantRepo)
		}
	}
}

// TestScriptDriftIsCachedAcrossListings keeps a console that polls from being a
// console that hammers github. The second listing must answer from the cache
// and ask nothing.
func TestScriptDriftIsCachedAcrossListings(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web", withSeededScript("#!/bin/sh\necho one\n"))
	b.attach("alice", "wandb/hivemind", "main", "web")
	b.file("wandb/hivemind", "main", SetupScriptPath, "#!/bin/sh\necho two\n")

	if _, err := b.ops.ListEnvironments(alice()); err != nil {
		t.Fatalf("first ListEnvironments: %v", err)
	}
	asked := len(b.files.asked)
	if asked == 0 {
		t.Fatal("the first listing never read the repository")
	}
	if _, err := b.ops.ListEnvironments(alice()); err != nil {
		t.Fatalf("second ListEnvironments: %v", err)
	}
	if len(b.files.asked) != asked {
		t.Errorf("the second listing read the repository again: %v", b.files.asked)
	}
}

// TestBuildingForgetsTheCachedDrift. A build that takes the repository's newer
// script has just made the cached verdict wrong, and a card that went on saying
// "your repository has moved ahead" for the next ten minutes — about a build
// that just picked it up — would be worse than saying nothing.
func TestBuildingForgetsTheCachedDrift(t *testing.T) {
	const updated = "#!/bin/sh\necho the newer one\n"
	b := newBuildRig(t)
	b.env("alice", "web", withSeededScript("#!/bin/sh\necho the old one\n"))
	b.attach("alice", "wandb/hivemind", "main", "web")
	b.file("wandb/hivemind", "main", SetupScriptPath, updated)

	list, err := b.ops.ListEnvironments(alice())
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if list[0].ScriptDrift != ScriptDriftRepoAhead {
		t.Fatalf("drift = %q, want %q before the build", list[0].ScriptDrift, ScriptDriftRepoAhead)
	}

	if _, err := b.ops.BuildEnvironment(context.Background(), alice(), "web"); err != nil {
		t.Fatalf("BuildEnvironment: %v", err)
	}
	list, err = b.ops.ListEnvironments(alice())
	if err != nil {
		t.Fatalf("ListEnvironments after the build: %v", err)
	}
	if list[0].ScriptDrift != ScriptDriftMatch {
		t.Errorf("drift = %q, want %q — the build took the repository's script",
			list[0].ScriptDrift, ScriptDriftMatch)
	}
}

// TestAdoptRepoScriptResolvesADivergence is the escape hatch, and the only path
// in this package that deliberately discards a setup script.
func TestAdoptRepoScriptResolvesADivergence(t *testing.T) {
	const committed = "#!/bin/sh\necho what the repository says\n"
	const mine = "#!/bin/sh\necho what the builder fixed\n"

	b := newBuildRig(t)
	b.env("alice", "web", func(e *envs.Environment) {
		e.SetupScript, e.SetupFrom = mine, envs.SetupFromRepo
		e.SetupSeedSHA = envs.ScriptSHA("#!/bin/sh\necho the original\n")
	})
	b.attach("alice", "wandb/hivemind", "main", "web")
	b.file("wandb/hivemind", "main", SetupScriptPath, committed)

	before, err := b.ops.ListEnvironments(alice())
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if before[0].ScriptDrift != ScriptDriftDiverged {
		t.Fatalf("drift = %q, want %q to begin with", before[0].ScriptDrift, ScriptDriftDiverged)
	}

	info, err := b.ops.AdoptRepoScript(context.Background(), alice(), "web")
	if err != nil {
		t.Fatalf("AdoptRepoScript: %v", err)
	}
	row := b.row(t, "alice", "web")
	if row.SetupScript != committed {
		t.Errorf("script = %q, want the repository's", row.SetupScript)
	}
	// The seed moves too, which is what re-enrols this environment in taking
	// later commits automatically — the point of adopting rather than pasting.
	if row.SetupSeedSHA != envs.ScriptSHA(committed) {
		t.Errorf("seed = %q, want the adopted script's", row.SetupSeedSHA)
	}
	if row.SetupFrom != envs.SetupFromRepo {
		t.Errorf("origin = %q, want %q", row.SetupFrom, envs.SetupFromRepo)
	}
	// And the answer it hands back is already re-rendered, so a console does
	// not show the divergence it just resolved.
	if info.ScriptDrift != ScriptDriftMatch {
		t.Errorf("returned drift = %q, want %q", info.ScriptDrift, ScriptDriftMatch)
	}
}

// TestAdoptRepoScriptRefusesWithNothingToTake. The refusal has to name the way
// out, because the state it describes — an environment whose repositories have
// no setup script — is one somebody reaches by clicking a button this package
// offered them.
func TestAdoptRepoScriptRefusesWithNothingToTake(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web", withScript("#!/bin/sh\necho mine\n"))
	b.attach("alice", "wandb/hivemind", "main", "web")

	_, err := b.ops.AdoptRepoScript(context.Background(), alice(), "web")
	if err == nil {
		t.Fatal("AdoptRepoScript accepted an environment with no repository script")
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != "no_repo_script" {
		t.Fatalf("err = %v, want a no_repo_script refusal", err)
	}
	if row := b.row(t, "alice", "web"); !strings.Contains(row.SetupScript, "mine") {
		t.Errorf("the refusal still changed the script: %q", row.SetupScript)
	}
}

// TestAdoptRepoScriptMasksAnotherOwnersEnvironment. The environment gate runs
// before the repository walk, so mallory naming alice's environment gets the
// same answer as naming one nobody has — and reads none of alice's files on the
// way to it.
func TestAdoptRepoScriptMasksAnotherOwnersEnvironment(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web", withSeededScript("#!/bin/sh\necho alice\n"))
	b.attach("alice", "wandb/hivemind", "main", "web")
	b.file("wandb/hivemind", "main", SetupScriptPath, "#!/bin/sh\necho secret\n")
	b.calls.reset()

	_, err := b.ops.AdoptRepoScript(context.Background(), mallory(), "web")
	if err == nil {
		t.Fatal("mallory adopted into alice's environment")
	}
	if len(b.files.asked) != 0 {
		t.Errorf("a cross-owner call read repository files: %v", b.files.asked)
	}
}
