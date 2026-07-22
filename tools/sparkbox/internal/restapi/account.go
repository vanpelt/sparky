package restapi

import (
	"net/http"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

type keyList struct {
	Keys []ctlops.KeyInfo `json:"keys"`
}

type passkeyList struct {
	Passkeys []ctlops.PasskeyInfo `json:"passkeys"`
}

type addKeyRequest struct {
	Key string `json:"key"` // one authorized_keys line
}

type githubRequest struct {
	Login       string `json:"login"`       // "" falls back to the linked login
	Fingerprint string `json:"fingerprint"` // which of YOUR keys proves the link
}

type emailRequest struct {
	Email string `json:"email"` // "" clears the address
}

type emailResponse struct {
	Email string `json:"email"`
}

type tokenRequest struct {
	TTL string `json:"ttl"` // a Go duration: "12h", "30m"; absent takes the default
}

func (h *Handler) capabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.ops.Capabilities())
}

func (h *Handler) whoami(w http.ResponseWriter, r *http.Request) {
	const op = "whoami"
	me, err := h.ops.Whoami(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, me)
}

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
	const op = "keys.list"
	keys, err := h.ops.ListKeys(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, keyList{Keys: keys})
}

func (h *Handler) addKey(w http.ResponseWriter, r *http.Request) {
	const op = "keys.add"
	var req addKeyRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	k, err := h.ops.AddKey(r.Context(), caller(r), req.Key)
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusCreated, k)
}

// removeKey takes the fingerprint as a query parameter rather than a path
// segment. An SSH fingerprint is `SHA256:` plus unpadded base64, whose alphabet
// includes '/' — so as a path segment it would have to be percent-encoded, and
// the proxies on this path normalize %2F back into a separator. The alphabet
// also includes '+', which a query decoder reads as a space, so the value has
// to be percent-encoded either way; the parameter's description says so.
func (h *Handler) removeKey(w http.ResponseWriter, r *http.Request) {
	const op = "keys.rm"
	fp := r.URL.Query().Get("fingerprint")
	if err := h.ops.RemoveKey(r.Context(), caller(r), fp); err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, deleted{Name: fp, Deleted: true})
}

func (h *Handler) importGitHubKeys(w http.ResponseWriter, r *http.Request) {
	const op = "keys.import-github"
	res, err := h.ops.ImportGitHubKeys(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// verifyGitHub links a GitHub login by finding one of the caller's already
// registered keys on github.com/<login>.keys. The SSH path implies which key
// (the one authenticating the session); an HTTP caller has no session key, so
// the fingerprint is a required field. ctlops checks that the fingerprint
// belongs to the caller before using it — otherwise nominating a stranger's key
// would claim that stranger's login.
func (h *Handler) verifyGitHub(w http.ResponseWriter, r *http.Request) {
	const op = "keys.verify-github"
	var req githubRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	me, err := h.ops.VerifyGitHub(r.Context(), caller(r), req.Login, req.Fingerprint)
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, me)
}

func (h *Handler) listPasskeys(w http.ResponseWriter, r *http.Request) {
	const op = "passkey.list"
	pks, err := h.ops.ListPasskeys(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, passkeyList{Passkeys: pks})
}

// removePasskey accepts any unique prefix of the id, as `ctl passkey rm` does.
// An ambiguous prefix is a 409 carrying the matching ids in details.matches —
// they are all the caller's own, so listing them leaks nothing and saves a
// round trip.
func (h *Handler) removePasskey(w http.ResponseWriter, r *http.Request) {
	const op = "passkey.rm"
	id := r.PathValue("id")
	if err := h.ops.RemovePasskey(r.Context(), caller(r), id); err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, deleted{Name: id, Deleted: true})
}

func (h *Handler) getEmail(w http.ResponseWriter, r *http.Request) {
	const op = "email.get"
	addr, err := h.ops.Email(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, emailResponse{Email: addr})
}

// setEmail records the address the edge forwards to private apps as
// X-Forwarded-Email. An empty string clears it, which is why this is a PUT with
// a body rather than a DELETE: "no address" is a value the account can hold,
// not the absence of the resource.
func (h *Handler) setEmail(w http.ResponseWriter, r *http.Request) {
	const op = "email.set"
	var req emailRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	addr, err := h.ops.SetEmail(r.Context(), caller(r), req.Email)
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, emailResponse{Email: addr})
}

// mintToken issues an edge session token — the same credential `ssh ctl@<host>
// session-token` prints, and the one this API authenticates with. Minting a new
// token from an existing session is deliberate: it is how a browser session
// becomes a scriptable one, and how a script rotates its own credential before
// expiry without touching SSH.
func (h *Handler) mintToken(w http.ResponseWriter, r *http.Request) {
	const op = "session-token"
	var req tokenRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	var ttl time.Duration
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil {
			h.fail(w, r, op, ctlops.Invalid(op, "bad_ttl",
				"ttl must be a duration like \"12h\" or \"30m\": %v", err))
			return
		}
		ttl = d
	}
	// A ttl over the ceiling is clamped rather than refused, exactly as ctl
	// does: asking for a week and a day is a rounding error, not a mistake.
	res, err := h.ops.MintSessionToken(r.Context(), caller(r), ttl)
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// invite is the only operator-gated operation in the API. Operator status is
// resolved inside ctlops from the account store, never from the session — the
// middleware resolves it too, for the console's benefit, but nothing here
// passes that value along, so a transport cannot widen anyone's authority.
func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	const op = "invite"
	res, err := h.ops.Invite(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}
