package restapi

// Repo attachments: the GitHub repositories a tag carries into every sandbox
// wearing it. There is deliberately no per-sandbox attach here, and that is the
// whole shape of the feature rather than an omission — the clone happens at
// boot, so an attachment has to exist BEFORE the sandbox does, and a tag is the
// one handle the platform already has for "the things my next box gets". It is
// the same object the secrets and network panels are built on, viewed a third
// way.
//
// Nothing in this file reaches github.com except `repo.check`, which says so on
// its handler. Attaching is a row in SQLite; the credential that eventually
// clones the repo is minted per sandbox, in the metadata service, and never
// travels through this API.

import (
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
)

// repoList is an object rather than a bare array, for the reason nodeList
// documents: a collection needs somewhere to grow a cursor without breaking
// every client's parser. Its element is the store's own row, which ctlops hands
// back unchanged — the same choice ListSecrets makes with secrets.SecretMeta,
// and it keeps one description of a repo in the tree instead of two that drift.
type repoList struct {
	Repos []repos.Repo `json:"repos"`
}

// repoCheckList wraps the reachability report in the same field name, so a
// client that renders the listing renders the check with the same code path:
// the rows are the same repos, each carrying the answer to "can the App
// actually get at it".
type repoCheckList struct {
	Repos []ctlops.RepoCheck `json:"repos"`
}

type repoRequest struct {
	Slug string `json:"slug"`           // "wandb/hivemind"
	Host string `json:"host,omitempty"` // "" means github.com
	// Tags is the whole set this attachment answers to. An empty list takes the
	// `default` tag — which is also stamped on every untagged sandbox, so an
	// untagged attachment clones into every box the caller creates from then on.
	// That is a legitimate thing to want and a surprising thing to get by
	// accident, so the answer's `defaulted` says when it happened.
	Tags []string `json:"tags"`
	Ref  string   `json:"ref,omitempty"`  // "" = the repo's default branch
	Path string   `json:"path,omitempty"` // "" = the default clone layout
	// Write asks for a token that can push. It is a switch rather than the
	// `access` string the stored row reports back, and deliberately so: mapping
	// a free-text permission onto the two the platform has would be validation,
	// and validation that lives here is validation the SSH door does not get.
	Write bool `json:"write,omitempty"`
}

func (h *Handler) listRepos(w http.ResponseWriter, r *http.Request) {
	const op = "repo.list"
	list, err := h.ops.ListRepos(caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, repoList{Repos: list})
}

// addRepo is synchronous. It writes one row and, when a GitHub App is
// configured, asks github.com whether the App can actually reach the
// repository — which is worth the round trip, because the failure mode of this
// whole feature is an attachment that looks fine here and produces a clone
// failure in a boot log two days later. That probe is advisory and rides in the
// answer's `check`: a repository the App cannot see is still attached, because
// refusing the write would leave the caller with nothing recorded and an errand
// to run before they could type it again.
//
// It is emphatically not job-backed. runJob exists for the operations that
// outlive a request — pause, archive, fork — and buys a 202/Prefer/Location
// contract that would be pure ceremony over a single SQLite write.
func (h *Handler) addRepo(w http.ResponseWriter, r *http.Request) {
	const op = "repo.add"
	var req repoRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	res, err := h.ops.AttachRepo(r.Context(), caller(r), ctlops.RepoArgs{
		Slug: req.Slug, Host: req.Host, Tags: req.Tags,
		Ref: req.Ref, Path: req.Path, Write: req.Write,
	})
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// removeRepo addresses the attachment by three path segments rather than one.
// A slug contains a '/' and a Go 1.22 mux wildcard matches exactly one segment,
// so "wandb/hivemind" cannot be a single parameter; percent-encoding would not
// rescue it either, because the proxies in front of this API normalize %2F
// straight back into a separator — the same reason removeKey takes an SSH
// fingerprint as a query parameter. Spelling the host out as its own segment
// costs nothing today and is the seam GHES would arrive through.
//
// The answer says the attachment is gone and nothing else. ctlops knows which
// sandboxes carried it, and naming them here would imply their checkouts were
// cleaned up: they are not. Detaching removes a manifest entry, never a
// directory somebody may be working in.
func (h *Handler) removeRepo(w http.ResponseWriter, r *http.Request) {
	const op = "repo.rm"
	host, slug := repoTarget(r)
	if _, err := h.ops.DetachRepo(r.Context(), caller(r), host, slug); err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, deleted{Name: slug, Deleted: true})
}

// checkRepos asks github.com, for each attachment, whether the App is installed
// on it and holds the access the attachment claims. It is the answer to the one
// question this design cannot answer locally, and the one users need before
// they trust a boot-time clone.
//
// It is a POST that changes nothing, which looks wrong and is not. A GET is
// something browsers, link previewers and caches feel free to issue on their
// own, and every one of those issues here would spend the installation's
// upstream rate limit on a request nobody asked for. The gate stays authRead
// because there is no state to protect with a CSRF proof — the worst a
// cross-site POST achieves is making the caller's own host talk to github.com.
func (h *Handler) checkRepos(w http.ResponseWriter, r *http.Request) {
	const op = "repo.check"
	checks, err := h.ops.CheckRepos(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, repoCheckList{Repos: checks})
}

// repoTarget puts the addressed repo back together from the path. The wildcard
// names here are the ones the route pattern declares and the ones openapi.json
// documents as parameters; a rename in any of the three that is not made in all
// three produces an empty slug and a masked 404, which is why they are read in
// exactly one place.
func repoTarget(r *http.Request) (host, slug string) {
	return r.PathValue("host"), r.PathValue("owner") + "/" + r.PathValue("name")
}
