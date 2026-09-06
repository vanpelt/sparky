package edgeauth

// Passkey (WebAuthn) sign-in for the login subdomain. The ceremony endpoints
// live beside the token form and produce the exact same artefact: a minted
// spk_v1 session cookie. Passkeys are therefore a convenience layer over the
// existing session machinery, not a second auth system — everything downstream
// (middleware, forwarded identity, consoles) is unchanged.
//
// Registration is gated behind an existing session: the first sign-in is
// always token-based (proving SSH key possession), after which the browser
// offers to enroll a passkey for that handle. Login uses discoverable
// credentials, so the page never asks who you are — the authenticator answers
// with the user handle it stored at enrollment.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// PasskeyStore is the slice of the user store the passkey flow needs.
// *users.Store satisfies it.
type PasskeyStore interface {
	Get(handle string) (users.User, error)
	Passkeys(handle string) ([]users.Passkey, error)
	HasPasskeys(handle string) (bool, error)
	AddPasskey(handle, label string, cred webauthn.Credential) error
	UpdatePasskey(handle string, cred webauthn.Credential) error
}

// ceremonyCookie carries the server-side ceremony id between begin and finish.
// Short-lived and HttpOnly; the ceremony itself (challenge, expiry) stays in
// process memory.
const ceremonyCookie = "spark_webauthn"

// ceremonyTTL bounds how long a begun ceremony may be finished. Generous —
// picking a phone off the desk for a cross-device passkey takes a while.
const ceremonyTTL = 5 * time.Minute

// ceremonies is the in-memory begin→finish state. A single edge process serves
// the login subdomain, so process memory is the natural home; a restart merely
// aborts in-flight ceremonies, which retry harmlessly.
type ceremonies struct {
	mu sync.Mutex
	m  map[string]ceremony
}

type ceremony struct {
	data webauthn.SessionData
	exp  time.Time
}

func (c *ceremonies) put(data webauthn.SessionData) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(buf)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]ceremony)
	}
	// Lazy sweep: ceremonies are rare enough that begin-time is a fine broom.
	for k, v := range c.m {
		if now.After(v.exp) {
			delete(c.m, k)
		}
	}
	c.m[id] = ceremony{data: data, exp: now.Add(ceremonyTTL)}
	return id, nil
}

func (c *ceremonies) take(id string) (webauthn.SessionData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[id]
	delete(c.m, id) // single-use either way: a failed finish must re-begin
	if !ok || time.Now().After(v.exp) {
		return webauthn.SessionData{}, false
	}
	return v.data, true
}

// passkeyUser adapts a sparkbox account to the library's User. The WebAuthn
// user handle is the account handle's bytes: handles are immutable and ≤32
// chars (well under the 64-byte cap), and an opaque random id would only add a
// mapping table for the same guarantee. name is the cosmetic label a passkey
// manager displays; it is chosen at enrollment, never stored server-side, and
// never part of an authorization decision.
type passkeyUser struct {
	handle string
	name   string
	creds  []users.Passkey
}

func (u passkeyUser) WebAuthnID() []byte { return []byte(u.handle) }
func (u passkeyUser) WebAuthnName() string {
	if u.name != "" {
		return u.name
	}
	return u.handle
}
func (u passkeyUser) WebAuthnDisplayName() string { return u.WebAuthnName() }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, len(u.creds))
	for i, p := range u.creds {
		out[i] = p.Credential
	}
	return out
}

// newRelyingParty builds the WebAuthn config for this edge. The RP ID is the
// bare zone (not login.<zone>) so a credential registered here stays valid if
// the ceremony ever moves to another subdomain.
func newRelyingParty(cfg LoginConfig) (*webauthn.WebAuthn, string, error) {
	domain := strings.TrimPrefix(cfg.Domain, ".")
	origin := Origin(cfg.Subdomain, domain, cfg.Secure, cfg.Port)
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          domain,
		RPDisplayName: "sparkbox",
		RPOrigins:     []string{origin},
	})
	if err != nil {
		return nil, "", fmt.Errorf("webauthn config: %w", err)
	}
	return wa, origin, nil
}

// requireSameOrigin is the CSRF gate for the ceremony endpoints: they are only
// ever called by fetch() from our own page, and a browser always attaches
// Origin to cross-origin POSTs, so anything not first-party is refused.
func (h *LoginHandler) requireSameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != h.origin {
		h.jsonErr(w, http.StatusForbidden, "cross-origin request refused", nil)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// jsonErr answers a ceremony endpoint with a terse, user-displayable message.
// Library errors are logged, not echoed — they name protocol internals that
// help nobody at the sign-in screen.
func (h *LoginHandler) jsonErr(w http.ResponseWriter, status int, msg string, err error) {
	if err != nil && h.cfg.Logger != nil {
		h.cfg.Logger.Warn("webauthn ceremony failed", "msg", msg, "err", err)
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

func (h *LoginHandler) setCeremonyCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name: ceremonyCookie, Value: id, Path: "/",
		MaxAge: int(ceremonyTTL.Seconds()), HttpOnly: true,
		Secure: h.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
}

func (h *LoginHandler) takeCeremony(w http.ResponseWriter, r *http.Request) (webauthn.SessionData, bool) {
	c, err := r.Cookie(ceremonyCookie)
	if err != nil {
		return webauthn.SessionData{}, false
	}
	// Clear it: ceremonies are single-use.
	http.SetCookie(w, &http.Cookie{
		Name: ceremonyCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
	return h.pending.take(c.Value)
}

// loginBegin starts a discoverable-credential assertion: no username asked,
// the authenticator offers whichever catnip passkeys it holds.
func (h *LoginHandler) loginBegin(w http.ResponseWriter, r *http.Request) {
	if !h.requireSameOrigin(w, r) {
		return
	}
	assertion, session, err := h.wa.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		h.jsonErr(w, http.StatusInternalServerError, "could not start passkey sign-in", err)
		return
	}
	id, err := h.pending.put(*session)
	if err != nil {
		h.jsonErr(w, http.StatusInternalServerError, "could not start passkey sign-in", err)
		return
	}
	h.setCeremonyCookie(w, id)
	writeJSON(w, http.StatusOK, assertion)
}

// loginFinish verifies the assertion, resolves the asserted user handle to an
// active account, and mints the same session cookie the token form would.
func (h *LoginHandler) loginFinish(w http.ResponseWriter, r *http.Request) {
	if !h.requireSameOrigin(w, r) {
		return
	}
	session, ok := h.takeCeremony(w, r)
	if !ok {
		h.jsonErr(w, http.StatusBadRequest, "sign-in attempt expired — try again", nil)
		return
	}
	var handle string
	user, cred, err := h.wa.FinishPasskeyLogin(func(_, userHandle []byte) (webauthn.User, error) {
		handle = string(userHandle)
		u, err := h.cfg.Passkeys.Get(handle)
		if err != nil {
			return nil, err
		}
		if u.Status != "active" {
			return nil, fmt.Errorf("account %s is not active", handle)
		}
		pks, err := h.cfg.Passkeys.Passkeys(handle)
		if err != nil {
			return nil, err
		}
		return passkeyUser{handle: handle, creds: pks}, nil
	}, session, r)
	if err != nil {
		h.jsonErr(w, http.StatusUnauthorized, "that passkey wasn't recognised", err)
		return
	}
	handle = string(user.WebAuthnID())
	// Persist the moved sign counter; a failure here is log-worthy but must not
	// fail a sign-in that already verified.
	if err := h.cfg.Passkeys.UpdatePasskey(handle, *cred); err != nil && h.cfg.Logger != nil {
		h.cfg.Logger.Warn("passkey counter update failed", "handle", handle, "err", err)
	}
	acct, err := h.cfg.Passkeys.Get(handle)
	if err != nil {
		h.jsonErr(w, http.StatusUnauthorized, "that passkey wasn't recognised", err)
		return
	}
	token, _, err := h.cfg.Signer.Mint(Identity{Handle: handle, Email: acct.Email}, h.cfg.TTL)
	if err != nil {
		h.jsonErr(w, http.StatusInternalServerError, "could not establish a session", err)
		return
	}
	h.setSessionCookie(w, token)
	if h.cfg.Logger != nil {
		h.cfg.Logger.Info("session established via passkey", "handle", handle)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "return": h.safeReturn(r.URL.Query().Get("return")),
	})
}

// registerBegin starts enrollment for the signed-in visitor. Resident keys are
// required — that is what makes the later sign-in discoverable — and already-
// enrolled credentials are excluded so a re-run can't double-register one
// authenticator.
func (h *LoginHandler) registerBegin(w http.ResponseWriter, r *http.Request) {
	if !h.requireSameOrigin(w, r) {
		return
	}
	id, ok := h.cfg.Signer.IdentityFrom(r)
	if !ok {
		h.jsonErr(w, http.StatusUnauthorized, "sign in first, then add a passkey", nil)
		return
	}
	pks, err := h.cfg.Passkeys.Passkeys(id.Handle)
	if err != nil {
		h.jsonErr(w, http.StatusInternalServerError, "could not start passkey setup", err)
		return
	}
	// The visitor may pick the name their passkey manager will display; the
	// account handle underneath is not theirs to choose (it is the OIDC
	// subject), so only the cosmetic field is client input.
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // absent/empty body → handle
	name := strings.TrimSpace(req.Name)
	if len(name) > 64 {
		name = name[:64]
	}
	user := passkeyUser{handle: id.Handle, name: name, creds: pks}
	exclude := make([]protocol.CredentialDescriptor, len(pks))
	for i, p := range pks {
		exclude[i] = p.Credential.Descriptor()
	}
	creation, session, err := h.wa.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclude))
	if err != nil {
		h.jsonErr(w, http.StatusInternalServerError, "could not start passkey setup", err)
		return
	}
	cid, err := h.pending.put(*session)
	if err != nil {
		h.jsonErr(w, http.StatusInternalServerError, "could not start passkey setup", err)
		return
	}
	h.setCeremonyCookie(w, cid)
	writeJSON(w, http.StatusOK, creation)
}

// registerFinish verifies the attestation and stores the new credential under
// the signed-in handle. The label is a client-supplied display hint only.
func (h *LoginHandler) registerFinish(w http.ResponseWriter, r *http.Request) {
	if !h.requireSameOrigin(w, r) {
		return
	}
	id, ok := h.cfg.Signer.IdentityFrom(r)
	if !ok {
		h.jsonErr(w, http.StatusUnauthorized, "sign in first, then add a passkey", nil)
		return
	}
	session, ok := h.takeCeremony(w, r)
	if !ok {
		h.jsonErr(w, http.StatusBadRequest, "passkey setup expired — try again", nil)
		return
	}
	pks, err := h.cfg.Passkeys.Passkeys(id.Handle)
	if err != nil {
		h.jsonErr(w, http.StatusInternalServerError, "could not save the passkey", err)
		return
	}
	cred, err := h.wa.FinishRegistration(passkeyUser{handle: id.Handle, creds: pks}, session, r)
	if err != nil {
		h.jsonErr(w, http.StatusBadRequest, "the browser's response didn't verify", err)
		return
	}
	label := strings.TrimSpace(r.URL.Query().Get("label"))
	if len(label) > 64 {
		label = label[:64]
	}
	if err := h.cfg.Passkeys.AddPasskey(id.Handle, label, *cred); err != nil {
		h.jsonErr(w, http.StatusInternalServerError, "could not save the passkey", err)
		return
	}
	if h.cfg.Logger != nil {
		h.cfg.Logger.Info("passkey enrolled", "handle", id.Handle, "label", label)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
