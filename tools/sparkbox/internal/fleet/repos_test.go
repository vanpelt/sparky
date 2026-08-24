package fleet_test

// The repo relay's authorization. It is the identity relay's check in the same
// position, and these tests are its tests with the stakes changed: a hole here
// hands one user's source code to another user's agent for an hour.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// recordingRepos is a resolver that answers from a fixed per-owner table and
// remembers what it was asked. What it records is the point: the owner it is
// handed must be the LEDGER's, never anything a node could have reported.
type recordingRepos struct {
	byOwner map[string][]fleet.RepoAttachment
	box     *host.Sandbox
	slug    string
	err     error
}

func (r *recordingRepos) Manifest(_ context.Context, box *host.Sandbox) ([]fleet.RepoAttachment, error) {
	r.box = box
	if r.err != nil {
		return nil, r.err
	}
	return r.byOwner[box.Owner], nil
}

func (r *recordingRepos) Credential(_ context.Context, box *host.Sandbox, slug string) (fleet.RepoCredential, error) {
	r.box, r.slug = box, slug
	if r.err != nil {
		return fleet.RepoCredential{}, r.err
	}
	return fleet.RepoCredential{
		Username: "x-access-token", Password: "ghs_" + slug,
		ExpiresAt: time.Unix(1700000000, 0).UTC(),
	}, nil
}

// reposFleet builds a gateway with one node attached and alice's sandbox placed
// on it, which is the shape every test below varies one thing about.
func reposFleet(t *testing.T) (*fleet.Fleet, *recordingRepos) {
	t.Helper()
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	source := &recordingRepos{byOwner: map[string][]fleet.RepoAttachment{
		"alice": {
			{Host: "github.com", Slug: "wandb/hivemind", Ref: "main", Access: "write"},
			{Host: "github.com", Slug: "alice/notes", Access: "read"},
		},
		"bob": {{Host: "github.com", Slug: "bob/secret-plans", Access: "write"}},
	}}
	f.SetRepos(source)

	n := newFakeNode("laptop")
	attach(t, f, n, &host.Sandbox{Name: "alices-box", Owner: "alice", Image: "universal"})
	place(t, index, "alices-box", "alice", "laptop")
	return f, source
}

func TestSelfReposAnswersForASandboxOnTheAskingNode(t *testing.T) {
	f, source := reposFleet(t)
	resp, err := f.SelfRepos(context.Background(), "laptop", nodelink.SelfReposReq{Sandbox: "alices-box"})
	if err != nil {
		t.Fatalf("SelfRepos: %v", err)
	}
	if len(resp.Repos) != 2 || resp.Repos[0].Slug != "wandb/hivemind" ||
		resp.Repos[0].Ref != "main" || resp.Repos[0].Access != "write" {
		t.Errorf("repos = %+v", resp.Repos)
	}
	// The owner is the ledger's. A node that could choose it would be choosing
	// whose repositories its guests receive.
	if source.box.Owner != "alice" {
		t.Errorf("owner = %q, want alice", source.box.Owner)
	}
}

// The check the whole relay rests on. A second machine asking about a sandbox
// the ledger places on the first must be refused, and the resolver must not be
// reached at all — reaching it would mean the refusal was a matter of what it
// happened to return.
func TestSelfReposRefusesASandboxOnAnotherNode(t *testing.T) {
	f, source := reposFleet(t)
	attach(t, f, newFakeNode("intruder"))

	if _, err := f.SelfRepos(context.Background(), "intruder",
		nodelink.SelfReposReq{Sandbox: "alices-box"}); codeOf(err) != nodelink.CodeNotYours {
		t.Fatalf("cross-node manifest = %v (code %q), want %s", err, codeOf(err), nodelink.CodeNotYours)
	}
	if _, err := f.SelfRepoCredential(context.Background(), "intruder",
		nodelink.SelfRepoCredReq{Sandbox: "alices-box", Slug: "wandb/hivemind"}); codeOf(err) != nodelink.CodeNotYours {
		t.Fatalf("cross-node credential = %v (code %q), want %s", err, codeOf(err), nodelink.CodeNotYours)
	}
	if source.box != nil {
		t.Error("the resolver was reached for a refused request")
	}
}

// A name nothing places anywhere must be refused with the SAME error as one
// placed on somebody else, or this endpoint becomes an oracle for which sandbox
// names — and therefore which people — exist elsewhere in the fleet.
func TestSelfReposRefusesAnUnknownSandboxIndistinguishably(t *testing.T) {
	f, _ := reposFleet(t)
	attach(t, f, newFakeNode("intruder"))
	_, unknown := f.SelfRepos(context.Background(), "intruder", nodelink.SelfReposReq{Sandbox: "no-such-box"})
	_, elsewhere := f.SelfRepos(context.Background(), "intruder", nodelink.SelfReposReq{Sandbox: "alices-box"})
	if unknown == nil || elsewhere == nil {
		t.Fatal("a refusal was expected for both")
	}
	if codeOf(unknown) != codeOf(elsewhere) {
		t.Errorf("refusal codes differ: unknown=%s placed-elsewhere=%s", codeOf(unknown), codeOf(elsewhere))
	}
	if unknown.Error() != elsewhere.Error() {
		t.Errorf("refusal sentences differ:\n unknown   = %v\n elsewhere = %v", unknown, elsewhere)
	}
}

// A gateway with no resolver must answer in a sentence the node turns into the
// same 501 a gateway-local guest would be given, rather than leaving the node to
// infer it from the unregistered-type error a version skew also produces.
func TestSelfReposRefusesWhenTheGatewayAttachesNoRepos(t *testing.T) {
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	n := newFakeNode("laptop")
	attach(t, f, n, &host.Sandbox{Name: "alices-box", Owner: "alice"})
	place(t, index, "alices-box", "alice", "laptop")

	if _, err := f.SelfRepos(context.Background(), "laptop",
		nodelink.SelfReposReq{Sandbox: "alices-box"}); codeOf(err) != nodelink.CodeNoRepos {
		t.Fatalf("err = %v (code %q), want %s", err, codeOf(err), nodelink.CodeNoRepos)
	}
	if _, err := f.SelfRepoCredential(context.Background(), "laptop",
		nodelink.SelfRepoCredReq{Sandbox: "alices-box", Slug: "wandb/hivemind"}); codeOf(err) != nodelink.CodeNoRepos {
		t.Fatalf("err = %v (code %q), want %s", err, codeOf(err), nodelink.CodeNoRepos)
	}
}

func TestSelfRepoCredentialMintsForAnAttachedRepository(t *testing.T) {
	f, source := reposFleet(t)
	resp, err := f.SelfRepoCredential(context.Background(), "laptop",
		nodelink.SelfRepoCredReq{Sandbox: "alices-box", Slug: "wandb/hivemind"})
	if err != nil {
		t.Fatalf("SelfRepoCredential: %v", err)
	}
	if resp.Password != "ghs_wandb/hivemind" || resp.Username != "x-access-token" {
		t.Errorf("credential = %+v", resp)
	}
	if source.slug != "wandb/hivemind" {
		t.Errorf("minted for %q", source.slug)
	}
}

// The slug narrows a token; it must never widen one. A repository this sandbox
// has no attachment to is a 404-shaped miss, and the minter must not be reached
// with it — a minter that would have happily produced a token for a repository
// the App can see is exactly the failure this check exists for.
func TestSelfRepoCredentialRefusesAnUnattachedRepository(t *testing.T) {
	f, source := reposFleet(t)
	_, err := f.SelfRepoCredential(context.Background(), "laptop",
		nodelink.SelfRepoCredReq{Sandbox: "alices-box", Slug: "bob/secret-plans"})
	if codeOf(err) != nodelink.CodeNoSuchRepo {
		t.Fatalf("err = %v (code %q), want %s", err, codeOf(err), nodelink.CodeNoSuchRepo)
	}
	if source.slug != "" {
		t.Errorf("the minter was reached for %q", source.slug)
	}
}

// github.com is case-insensitive on both halves of a slug and the attachment
// store folds case in SQL; Go does not. A guest whose remote URL merely differs
// in capitalisation from what its owner typed must still get a credential.
func TestSelfRepoCredentialFoldsCaseAndMintsForTheStoredSpelling(t *testing.T) {
	f, source := reposFleet(t)
	resp, err := f.SelfRepoCredential(context.Background(), "laptop",
		nodelink.SelfRepoCredReq{Sandbox: "alices-box", Slug: "WandB/HiveMind"})
	if err != nil {
		t.Fatalf("SelfRepoCredential: %v", err)
	}
	if source.slug != "wandb/hivemind" {
		t.Errorf("minted for %q, want the stored spelling wandb/hivemind", source.slug)
	}
	if resp.Password == "" {
		t.Error("no credential was returned")
	}
}

// The four classes a resolver states must survive as the codes three other
// layers switch on. A miss here does not fail loudly: it silently becomes the
// wrong HTTP status, and a 503 is one the guest's timer retries forever.
func TestRepoFailuresCarryTheirClassAcrossTheLink(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "unattached", err: fleet.ErrNoSuchRepo, want: nodelink.CodeNoSuchRepo},
		{name: "disabled", err: fleet.ErrReposDisabled, want: nodelink.CodeNoRepos},
		{name: "denied", err: fleet.ErrRepoDenied, want: nodelink.CodeRepoDenied},
		{name: "upstream", err: fleet.ErrRepoUpstream, want: nodelink.CodeRepoUpstream},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, source := reposFleet(t)
			source.err = test.err
			_, err := f.SelfRepos(context.Background(), "laptop", nodelink.SelfReposReq{Sandbox: "alices-box"})
			if codeOf(err) != test.want {
				t.Fatalf("err = %v (code %q), want %s", err, codeOf(err), test.want)
			}
		})
	}
}

// Anything else is a fault, and a fault must cross the link as a typed one with
// nothing of the cause in it: the resolver's errors quote the store or quote
// github.com, and neither was written for whatever is running in the VM.
func TestRepoFaultsAreTypedAndSayNothing(t *testing.T) {
	f, source := reposFleet(t)
	source.err = errors.New("attachments.db is on fire")
	_, err := f.SelfRepos(context.Background(), "laptop", nodelink.SelfReposReq{Sandbox: "alices-box"})
	var typed *ctlops.Error
	if !errors.As(err, &typed) {
		t.Fatalf("err = %v, want a typed error", err)
	}
	if strings.Contains(typed.Msg, "on fire") {
		t.Errorf("the internal cause leaked to the node: %q", typed.Msg)
	}
}

// The one class whose sentence IS carried, because each of its causes has a
// different fix and the guest's log is where somebody will read it.
func TestRepoDenialCarriesItsSentence(t *testing.T) {
	f, source := reposFleet(t)
	source.err = errors.Join(fleet.ErrRepoDenied, errors.New("owner has no verified github link"))
	_, err := f.SelfRepoCredential(context.Background(), "laptop",
		nodelink.SelfRepoCredReq{Sandbox: "alices-box", Slug: "wandb/hivemind"})
	var typed *ctlops.Error
	if !errors.As(err, &typed) || typed.Code != nodelink.CodeRepoDenied {
		t.Fatalf("err = %v (code %q), want %s", err, codeOf(err), nodelink.CodeRepoDenied)
	}
	if !strings.Contains(typed.Msg, "verified github link") {
		t.Errorf("the fix was not carried to the guest: %q", typed.Msg)
	}
}
