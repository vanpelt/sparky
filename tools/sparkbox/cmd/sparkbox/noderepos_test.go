package main

// The node's repo relay: which transport it uses, and what a guest is told when
// the answer is no.
//
// The classification is the whole subject. It crosses four switches — the
// gateway's ctlops code, the gRPC status, the node's mapping back onto a
// metadata sentinel, and metadata's mapping onto a status — and a miss in any
// one of them does not fail loudly. It silently becomes the wrong number in
// front of a guest, and the wrong number is usually 503, which the guest's timer
// retries forever.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/grpcidentity"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"google.golang.org/grpc/codes"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The fallback rule, and it is narrow on purpose: only a dead TRANSPORT may be
// retried over the uplink. Replaying a refusal there spends the rest of a
// guest's ten-second budget arriving at the same answer from the same ledger,
// and replaying a credential mint spends a GitHub round trip doing it.
func TestRepoRelayFallsBackToSSHOnlyForGRPCTransportFailure(t *testing.T) {
	box := &host.Sandbox{Name: "alpha"}
	for _, test := range []struct {
		name      string
		code      codes.Code
		wantSSH   bool
		wantClass error
	}{
		{name: "transport unavailable", code: codes.Unavailable, wantSSH: true, wantClass: metadata.ErrNoIssuer},
		{name: "unattached repository", code: codes.NotFound, wantClass: metadata.ErrNoSuchRepo},
		{name: "refused", code: codes.PermissionDenied, wantClass: metadata.ErrRepoDenied},
		{name: "not enabled", code: codes.Unimplemented, wantClass: metadata.ErrNotEnabled},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := &relayGRPCFailure{code: test.code}
			client, err := grpcidentity.WrapClient(raw)
			if err != nil {
				t.Fatal(err)
			}
			identity := newRelayIdentity(nodelink.NewUplink(), "auto", discardLogger())
			identity.setGRPC(client)
			defer identity.Close()
			relay := newRelayRepos(nodelink.NewUplink(), identity.currentGRPC, discardLogger())

			_, err = relay.Credential(context.Background(), box, "wandb/hivemind")
			if raw.calls != 1 {
				t.Fatalf("gRPC calls = %d, want 1", raw.calls)
			}
			// An unlinked uplink is what the SSH attempt reports, so its error is
			// the evidence that the fallback was taken at all.
			if got := errors.Is(err, nodelink.ErrNoLink); got != test.wantSSH {
				t.Fatalf("SSH fallback = %t, want %t (error %v)", got, test.wantSSH, err)
			}
			if test.wantClass != nil && !errors.Is(err, test.wantClass) {
				t.Fatalf("error = %v, want class %v", err, test.wantClass)
			}
		})
	}
}

// A gRPC client that predates these RPCs is not a repo transport, and asking it
// would produce an error about a method rather than about a repository. The
// relay must ignore it and use the uplink, which every node has.
func TestRepoRelayIgnoresAGRPCClientThatCannotSpeakRepos(t *testing.T) {
	identity := newRelayIdentity(nodelink.NewUplink(), "auto", discardLogger())
	identity.setGRPC(&trackedIdentityClient{token: "jwt"})
	defer identity.Close()
	relay := newRelayRepos(nodelink.NewUplink(), identity.currentGRPC, discardLogger())

	if relay.grpc() != nil {
		t.Fatal("an identity-only client was accepted as a repo transport")
	}
	_, err := relay.Manifest(context.Background(), &host.Sandbox{Name: "alpha"})
	if !errors.Is(err, nodelink.ErrNoLink) {
		t.Fatalf("err = %v, want the uplink to have been used", err)
	}
}

// With no client at all — a node enrolled against a gateway that was started
// without --gateway-grpc-addr, which is a supported deployment and not a
// degraded one — the uplink is the only path and must simply work.
func TestRepoRelayUsesTheUplinkWithNoGRPCAtAll(t *testing.T) {
	relay := newRelayRepos(nodelink.NewUplink(), nil, discardLogger())
	if _, err := relay.Manifest(context.Background(), &host.Sandbox{Name: "alpha"}); !errors.Is(err, nodelink.ErrNoLink) {
		t.Fatalf("err = %v, want the uplink to have been used", err)
	}
}

// The uplink's four refusal codes, mapped onto what metadata answers with. This
// is the SSH half of the classification and it is the half every node has: the
// gRPC half above is an optimisation on top of it.
func TestRelayReposErrorClassifiesEveryUplinkRefusal(t *testing.T) {
	for _, test := range []struct {
		code string
		want error
	}{
		{code: nodelink.CodeNoRepos, want: metadata.ErrNotEnabled},
		{code: nodelink.CodeNoSuchRepo, want: metadata.ErrNoSuchRepo},
		{code: nodelink.CodeRepoDenied, want: metadata.ErrRepoDenied},
		{code: nodelink.CodeRepoUpstream, want: metadata.ErrUpstream},
		// A gateway too old to speak repos answers with the framing layer's own
		// refusal. To a guest that is the same 501 as a gateway with no App key,
		// and it must not become the 500 an unclassified error would be.
		{code: "unknown_request", want: metadata.ErrNotEnabled},
	} {
		err := relayReposError(&ctlops.Error{Code: test.code, Msg: "refused"})
		if !errors.Is(err, test.want) {
			t.Errorf("code %q classified as %v, want %v", test.code, err, test.want)
		}
	}
}

// The gateway-side adapter. It translates rather than decides, and it is the
// place a drift between metadata's sentinels and fleet's would go unnoticed —
// so every class is asserted, including the fault that must stay a fault.
func TestFleetReposTranslatesEveryClass(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "unattached", err: metadata.ErrNoSuchRepo, want: fleet.ErrNoSuchRepo},
		{name: "denied", err: metadata.ErrRepoDenied, want: fleet.ErrRepoDenied},
		{name: "disabled", err: metadata.ErrNotEnabled, want: fleet.ErrReposDisabled},
		{name: "upstream", err: metadata.ErrUpstream, want: fleet.ErrRepoUpstream},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := stubRepoAccess{err: test.err}
			if _, err := newFleetRepos(source).Manifest(context.Background(), nil); !errors.Is(err, test.want) {
				t.Fatalf("manifest error = %v, want %v", err, test.want)
			}
			if _, err := newFleetRepos(source).Credential(context.Background(), nil, "o/n"); !errors.Is(err, test.want) {
				t.Fatalf("credential error = %v, want %v", err, test.want)
			}
		})
	}
	// Anything else stays unclassified, so that fleet reports it as the internal
	// fault it is instead of dressing a store failure up as a missing repo.
	fault := errors.New("attachments.db is on fire")
	_, err := newFleetRepos(stubRepoAccess{err: fault}).Manifest(context.Background(), nil)
	if !errors.Is(err, fault) {
		t.Fatalf("fault = %v, want it carried through", err)
	}
	for _, sentinel := range []error{fleet.ErrNoSuchRepo, fleet.ErrRepoDenied, fleet.ErrReposDisabled, fleet.ErrRepoUpstream} {
		if errors.Is(err, sentinel) {
			t.Errorf("a store fault was classified as %v", sentinel)
		}
	}
}

// And the happy path through the adapter, which is where a dropped field would
// show up as a repository the guest clones at the wrong ref.
func TestFleetReposCarriesEveryColumn(t *testing.T) {
	source := stubRepoAccess{
		manifest: metadata.Manifest{Repos: []metadata.RepoEntry{
			{Host: "github.com", Slug: "wandb/hivemind", Ref: "main", Path: "src/hivemind", Access: "write"},
		}},
		credential: metadata.Credential{
			Username: "x-access-token", Password: "ghs_token",
			ExpiresAt: time.Unix(1700000000, 0).UTC(),
		},
	}
	attachments, err := newFleetRepos(source).Manifest(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := fleet.RepoAttachment{
		Host: "github.com", Slug: "wandb/hivemind", Ref: "main",
		Path: "src/hivemind", Access: "write",
	}
	if len(attachments) != 1 || attachments[0] != want {
		t.Fatalf("attachments = %+v, want %+v", attachments, want)
	}
	cred, err := newFleetRepos(source).Credential(context.Background(), nil, "wandb/hivemind")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "x-access-token" || cred.Password != "ghs_token" ||
		!cred.ExpiresAt.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Fatalf("credential = %+v", cred)
	}
}

type stubRepoAccess struct {
	manifest   metadata.Manifest
	credential metadata.Credential
	err        error
}

func (s stubRepoAccess) Manifest(context.Context, *host.Sandbox) (metadata.Manifest, error) {
	return s.manifest, s.err
}

func (s stubRepoAccess) Credential(context.Context, *host.Sandbox, string) (metadata.Credential, error) {
	return s.credential, s.err
}
