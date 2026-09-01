package main

// The node's half of repo attachment, and the gateway's adapter onto it.
//
// A node holds no tag table, no attachment store and no GitHub App key, so
// there is nothing here to answer a guest with and nothing to fall back on when
// the link is down. That is the intended shape rather than a gap: a cached,
// node-resolved repo list would be a machine telling the gateway which
// repositories its guests are entitled to, which is the one thing the whole
// design is arranged to prevent. Both methods therefore relay, and a node
// between links answers 503 and lets the guest's own retry be the repair.
//
// Two transports, for the reason the identity relay has two. A node enrolled
// against a gateway started without --gateway-grpc-addr never gets a certificate
// to dial with and runs entirely on the SSH uplink; a node that did negotiate
// mTLS prefers it and falls back only when that transport is down. Implementing
// either alone would leave half the fleet with an unregistered-message-type
// error, which reads as a protocol mismatch and sends an operator looking for
// the wrong thing.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/grpcidentity"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// gatewayReposClient is the gRPC half, named as its own interface rather than
// widened onto gatewayIdentityClient: the identity client is what the
// hivemindpresence monitor and the token path are handed, and those must keep
// working on a build whose gateway serves no repositories at all.
type gatewayReposClient interface {
	Manifest(ctx context.Context, box *host.Sandbox) (metadata.Manifest, error)
	Credential(ctx context.Context, box *host.Sandbox, slug string) (metadata.Credential, error)
}

// relayRepos is a node's metadata.RepoAccess. It names the sandbox — and, for a
// credential, the repository — and lets the gateway decide everything else.
//
// It borrows the identity relay's gRPC client rather than dialing one of its
// own, because there is exactly one control connection to the gateway and its
// lifecycle (enrollment, redial, supersession) is already owned over there.
// current is that relay's accessor; it returns nil whenever the mTLS transport
// is unconfigured or torn down, which is the same condition the SSH fallback
// exists for.
type relayRepos struct {
	up      *nodelink.Uplink
	current func() gatewayIdentityClient
	log     *slog.Logger
}

func newRelayRepos(up *nodelink.Uplink, current func() gatewayIdentityClient, log *slog.Logger) *relayRepos {
	return &relayRepos{up: up, current: current, log: log}
}

// grpc returns the live gRPC client if it can also speak repositories.
//
// The type assertion is what keeps this honest across a mixed fleet: the
// identity relay's client is an interface, a build could install one that
// predates these RPCs, and asserting is cheaper than threading a second
// capability flag through the relay's configuration path.
func (r *relayRepos) grpc() gatewayReposClient {
	if r == nil || r.current == nil {
		return nil
	}
	client, ok := r.current().(gatewayReposClient)
	if !ok {
		return nil
	}
	return client
}

func (r *relayRepos) Manifest(ctx context.Context, box *host.Sandbox) (metadata.Manifest, error) {
	if box == nil || box.Name == "" {
		return metadata.Manifest{}, errors.New("sparkbox: a repo manifest needs a sandbox")
	}
	if client := r.grpc(); client != nil {
		manifest, err := client.Manifest(ctx, box)
		if err == nil || !errors.Is(err, grpcidentity.ErrUnavailable) {
			return manifest, err
		}
		r.log.Debug("gateway repo gRPC unavailable; falling back to SSH", "err", err)
	}
	var resp nodelink.SelfReposResp
	req := nodelink.SelfReposReq{Sandbox: box.Name}
	if err := r.up.Request(ctx, nodelink.TypeSelfRepos, req, &resp); err != nil {
		return metadata.Manifest{}, relayReposError(err)
	}
	// Never nil. The guest's clone script tells "this sandbox has no
	// attachments" from "this failed" by an empty list, and a JSON null would
	// be neither.
	manifest := metadata.Manifest{Repos: make([]metadata.RepoEntry, 0, len(resp.Repos))}
	for _, repo := range resp.Repos {
		manifest.Repos = append(manifest.Repos, metadata.RepoEntry{
			Host: repo.Host, Slug: repo.Slug, Ref: repo.Ref,
			Path: repo.Path, Access: repo.Access, Instance: repo.Instance,
		})
	}
	return manifest, nil
}

func (r *relayRepos) Credential(ctx context.Context, box *host.Sandbox, slug string) (metadata.Credential, error) {
	if box == nil || box.Name == "" {
		return metadata.Credential{}, errors.New("sparkbox: a git credential needs a sandbox")
	}
	if slug == "" {
		return metadata.Credential{}, errors.New("sparkbox: a git credential needs a repository")
	}
	if client := r.grpc(); client != nil {
		cred, err := client.Credential(ctx, box, slug)
		if err == nil || !errors.Is(err, grpcidentity.ErrUnavailable) {
			return cred, err
		}
		r.log.Debug("gateway repo gRPC unavailable; falling back to SSH", "err", err)
	}
	var resp nodelink.SelfRepoCredResp
	req := nodelink.SelfRepoCredReq{Sandbox: box.Name, Slug: slug}
	if err := r.up.Request(ctx, nodelink.TypeSelfRepoCred, req, &resp); err != nil {
		return metadata.Credential{}, relayReposError(err)
	}
	// Logged with the repository and the expiry and never the token, exactly as
	// the gateway logs the mint at the other end.
	r.log.Debug("relayed a git credential to a guest", "sandbox", box.Name, "slug", slug, "exp", resp.ExpiresAt)
	return metadata.Credential{
		Username: resp.Username, Password: resp.Password, ExpiresAt: resp.ExpiresAt,
	}, nil
}

func (r *relayRepos) StartAuthorization(ctx context.Context, box *host.Sandbox, slug string) (metadata.AuthorizationStart, error) {
	if box == nil || box.Name == "" || slug == "" {
		return metadata.AuthorizationStart{}, errors.New("sparkbox: a repo authorization needs a sandbox and repository")
	}
	var resp nodelink.SelfRepoAuthStartResp
	err := r.up.Request(ctx, nodelink.TypeSelfRepoAuthStart,
		nodelink.SelfRepoAuthStartReq{Sandbox: box.Name, Slug: slug}, &resp)
	if err != nil {
		return metadata.AuthorizationStart{}, relayReposError(err)
	}
	return metadata.AuthorizationStart{
		ID: resp.ID, UserCode: resp.UserCode, VerificationURI: resp.VerificationURI,
		IntervalSeconds: resp.IntervalSeconds, ExpiresAt: resp.ExpiresAt,
	}, nil
}

func (r *relayRepos) PollAuthorization(ctx context.Context, box *host.Sandbox, id string) (metadata.AuthorizationStatus, error) {
	if box == nil || box.Name == "" || id == "" {
		return metadata.AuthorizationStatus{}, errors.New("sparkbox: a repo authorization poll needs a sandbox and flow id")
	}
	var resp nodelink.SelfRepoAuthPollResp
	err := r.up.Request(ctx, nodelink.TypeSelfRepoAuthPoll,
		nodelink.SelfRepoAuthPollReq{Sandbox: box.Name, ID: id}, &resp)
	if err != nil {
		return metadata.AuthorizationStatus{}, relayReposError(err)
	}
	return metadata.AuthorizationStatus{State: resp.State, Slug: resp.Slug}, nil
}

// relayReposError is relayError plus the one case only this path can see.
//
// A gateway that predates these message types answers with the framing layer's
// unknown_request, which relayError would classify as an internal fault and a
// guest would be told 500 about. It is not a fault: from a guest's point of
// view a gateway that cannot speak repositories and a gateway that has none
// configured are the same 501, and neither is repaired by retrying.
func relayReposError(err error) error {
	var typed *ctlops.Error
	if errors.As(err, &typed) && typed.Code == "unknown_request" {
		return fmt.Errorf("%w: this gateway does not serve repo attachments", metadata.ErrNotEnabled)
	}
	return relayError(err)
}

// fleetRepos is the GATEWAY-side adapter: it lets the same resolver that serves
// this machine's own guests answer for the guests on every node in the fleet.
//
// One resolver for both is the point. A sandbox must get the same manifest and
// the same credential whichever machine it happens to have been placed on, and
// the placement check that makes the relay safe lives in internal/fleet, not
// here — this type only translates vocabularies.
type fleetRepos struct{ local metadata.RepoAccess }

func newFleetRepos(local metadata.RepoAccess) fleet.Repos { return fleetRepos{local: local} }

func (a fleetRepos) Manifest(ctx context.Context, box *host.Sandbox) ([]fleet.RepoAttachment, error) {
	manifest, err := a.local.Manifest(ctx, box)
	if err != nil {
		return nil, reposFleetError(err)
	}
	out := make([]fleet.RepoAttachment, 0, len(manifest.Repos))
	for _, repo := range manifest.Repos {
		out = append(out, fleet.RepoAttachment{
			Host: repo.Host, Slug: repo.Slug, Ref: repo.Ref,
			Path: repo.Path, Access: repo.Access, Instance: repo.Instance,
		})
	}
	return out, nil
}

func (a fleetRepos) Credential(ctx context.Context, box *host.Sandbox, slug string) (fleet.RepoCredential, error) {
	cred, err := a.local.Credential(ctx, box, slug)
	if err != nil {
		return fleet.RepoCredential{}, reposFleetError(err)
	}
	return fleet.RepoCredential{
		Username: cred.Username, Password: cred.Password, ExpiresAt: cred.ExpiresAt,
	}, nil
}

func (a fleetRepos) StartAuthorization(ctx context.Context, box *host.Sandbox, slug string) (fleet.RepoAuthorizationStart, error) {
	authorizer, ok := a.local.(metadata.RepoAuthorizer)
	if !ok {
		return fleet.RepoAuthorizationStart{}, fleet.ErrReposDisabled
	}
	started, err := authorizer.StartAuthorization(ctx, box, slug)
	if err != nil {
		return fleet.RepoAuthorizationStart{}, reposFleetError(err)
	}
	return fleet.RepoAuthorizationStart{
		ID: started.ID, UserCode: started.UserCode, VerificationURI: started.VerificationURI,
		IntervalSeconds: started.IntervalSeconds, ExpiresAt: started.ExpiresAt,
	}, nil
}

func (a fleetRepos) PollAuthorization(ctx context.Context, box *host.Sandbox, id string) (fleet.RepoAuthorizationStatus, error) {
	authorizer, ok := a.local.(metadata.RepoAuthorizer)
	if !ok {
		return fleet.RepoAuthorizationStatus{}, fleet.ErrReposDisabled
	}
	status, err := authorizer.PollAuthorization(ctx, box, id)
	if err != nil {
		return fleet.RepoAuthorizationStatus{}, reposFleetError(err)
	}
	return fleet.RepoAuthorizationStatus{State: status.State, Slug: status.Slug}, nil
}

// reposFleetError restates the resolver's four sentinels as the four
// internal/fleet classifies on, so that a guest on a node is told exactly what
// its gateway-local sibling would be told.
//
// This is a translation and not a decision: metadata.LocalRepos has already
// done the classifying, including folding ghapp's own sentinels in
// (metadata.githubError). Restating them is the price of fleet not importing
// the metadata package for four variables — and it is a translation that fails
// SILENTLY if it drifts, turning a 404 into the 500 a guest gives up on, so it
// is written next to the fleet sentinels it feeds and must move with them.
func reposFleetError(err error) error {
	switch {
	case errors.Is(err, metadata.ErrNoSuchRepo):
		return fmt.Errorf("%w: %w", fleet.ErrNoSuchRepo, err)
	case errors.Is(err, metadata.ErrRepoDenied):
		return fmt.Errorf("%w: %w", fleet.ErrRepoDenied, err)
	case errors.Is(err, metadata.ErrNotEnabled):
		return fmt.Errorf("%w: %w", fleet.ErrReposDisabled, err)
	case errors.Is(err, metadata.ErrUpstream):
		return fmt.Errorf("%w: %w", fleet.ErrRepoUpstream, err)
	}
	return err
}
