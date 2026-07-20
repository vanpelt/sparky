package edgeauth

import (
	_ "embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

//go:embed login.html
var loginHTML string

var loginTpl = template.Must(template.New("login").Parse(loginHTML))

//go:embed enroll.html
var enrollHTML string

var enrollTpl = template.Must(template.New("enroll").Parse(enrollHTML))

// LoginConfig parameterises the login handler served at login.<domain>.
type LoginConfig struct {
	Signer *Signer
	// Domain is the base zone, e.g. "hivemind.tools". The session cookie is set
	// for ".<Domain>" so one login covers every subdomain and port, and return
	// URLs are constrained to it so the login page can't be an open redirect.
	Domain string
	// Secure sets the cookie's Secure flag (true whenever the edge terminates
	// TLS). SameSite is always Lax: these are first-party navigations.
	Secure  bool
	TTL     time.Duration
	Logger  *slog.Logger
	Gateway string // SSH host shown in the mint instructions, e.g. "hivemind.tools"
	// GatewayPort is the gateway's SSH listen port. When it isn't 22 the mint
	// instructions include -p<port>, so the shown command is copy-pasteable
	// (port 22 on the edge address is typically the host's own sshd, not us).
	GatewayPort int
	// Passkeys enables WebAuthn sign-in and enrollment when non-nil.
	Passkeys PasskeyStore
	// Subdomain is the login handler's own subdomain ("login") — with Domain,
	// Secure and Port it fixes the WebAuthn origin.
	Subdomain string
	// Port is the edge's public listen port; non-standard ports appear in the
	// WebAuthn origin. 0 means the scheme default.
	Port int
	// HomeSub, when set, names the subdomain a sign-in lands on if no return
	// URL was carried (someone opened login.<domain> directly). The natural
	// value is the user console ("my") — the zone apex serves nothing.
	HomeSub string
}

// LoginHandler serves the browser login: passkey sign-in when enabled, and the
// token path — a page that takes a token minted via `ssh ctl@<gateway>
// session-token`, and a POST that verifies it and drops the session cookie
// before bouncing back to the originally-requested URL.
type LoginHandler struct {
	cfg        LoginConfig
	cookieDom  string
	returnHost string // "." + Domain suffix that a return URL's host must match

	// wa/origin/pending drive the WebAuthn ceremonies; wa is nil when
	// cfg.Passkeys is unset and the page falls back to token-only.
	wa      *webauthn.WebAuthn
	origin  string
	pending ceremonies
}

func NewLoginHandler(cfg LoginConfig) (*LoginHandler, error) {
	if cfg.TTL <= 0 {
		cfg.TTL = 12 * time.Hour
	}
	if cfg.Subdomain == "" {
		cfg.Subdomain = "login"
	}
	h := &LoginHandler{
		cfg:        cfg,
		cookieDom:  "." + strings.TrimPrefix(cfg.Domain, "."),
		returnHost: "." + strings.TrimPrefix(cfg.Domain, "."),
	}
	if cfg.Passkeys != nil {
		wa, origin, err := newRelyingParty(cfg)
		if err != nil {
			return nil, err
		}
		h.wa, h.origin = wa, origin
	}
	return h, nil
}

func (h *LoginHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.page)
	mux.HandleFunc("POST /session", h.session)
	mux.HandleFunc("POST /logout", h.logout)
	if h.wa != nil {
		mux.HandleFunc("GET /enroll", h.enrollPage)
		mux.HandleFunc("POST /webauthn/login/begin", h.loginBegin)
		mux.HandleFunc("POST /webauthn/login/finish", h.loginFinish)
		mux.HandleFunc("POST /webauthn/register/begin", h.registerBegin)
		mux.HandleFunc("POST /webauthn/register/finish", h.registerFinish)
	}
	return mux
}

// page renders the login form, carrying the sanitised return target through as
// a hidden field.
func (h *LoginHandler) page(w http.ResponseWriter, r *http.Request) {
	ret := h.safeReturn(r.URL.Query().Get("return"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	portFlag := ""
	if h.cfg.GatewayPort != 0 && h.cfg.GatewayPort != 22 {
		portFlag = fmt.Sprintf("-p%d ", h.cfg.GatewayPort)
	}
	err := loginTpl.Execute(w, struct {
		Return   string
		Gateway  string
		PortFlag string
		Passkeys bool
	}{ret, h.cfg.Gateway, portFlag, h.wa != nil})
	if err != nil && h.cfg.Logger != nil {
		h.cfg.Logger.Error("login page render failed", "err", err)
	}
}

// enrollPage offers the signed-in visitor a one-time "add a passkey?" step.
// Unauthenticated hits bounce to the login page rather than 401 — the only way
// here is a stale bookmark or an expired session.
func (h *LoginHandler) enrollPage(w http.ResponseWriter, r *http.Request) {
	ret := h.safeReturn(r.URL.Query().Get("return"))
	id, ok := h.cfg.Signer.IdentityFrom(r)
	if !ok {
		http.Redirect(w, r, "/?return="+url.QueryEscape(ret), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	err := enrollTpl.Execute(w, struct {
		Handle string
		Return string
	}{id.Handle, ret})
	if err != nil && h.cfg.Logger != nil {
		h.cfg.Logger.Error("enroll page render failed", "err", err)
	}
}

// setSessionCookie drops the zone-wide session cookie — the one artefact every
// sign-in path (token form, passkey assertion) converges on.
func (h *LoginHandler) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Domain:   h.cookieDom,
		MaxAge:   int(h.cfg.TTL.Seconds()),
		HttpOnly: true,
		Secure:   h.cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// session verifies a pasted token and, on success, sets the cookie and
// redirects to the return target — via a one-time passkey-enrollment offer
// when the account hasn't got one yet.
func (h *LoginHandler) session(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(r.PostForm.Get("token"))
	id, ok := h.cfg.Signer.Verify(token)
	if !ok {
		http.Redirect(w, r, "/?err=1&return="+url.QueryEscape(h.safeReturn(r.PostForm.Get("return"))), http.StatusSeeOther)
		return
	}
	h.setSessionCookie(w, token)
	if h.cfg.Logger != nil {
		h.cfg.Logger.Info("session established", "handle", id.Handle)
	}
	ret := h.safeReturn(r.PostForm.Get("return"))
	if h.wa != nil {
		if has, err := h.cfg.Passkeys.HasPasskeys(id.Handle); err == nil && !has {
			http.Redirect(w, r, "/enroll?return="+url.QueryEscape(ret), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, ret, http.StatusSeeOther)
}

func (h *LoginHandler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", Domain: h.cookieDom, MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, h.safeReturn(r.PostForm.Get("return")), http.StatusSeeOther)
}

// safeReturn constrains a return URL to this zone, defeating an open-redirect
// (`?return=https://evil.com`). Anything off-zone or unparseable collapses to
// the zone root. The scheme rule follows the edge: https-only when the edge
// terminates TLS, with http also allowed on a non-TLS edge (the mock-driver
// dev loop serves the whole zone over http, and its consoles hand out http
// return URLs).
func (h *LoginHandler) safeReturn(raw string) string {
	scheme := "https"
	if !h.cfg.Secure {
		scheme = "http"
	}
	home := strings.TrimPrefix(h.cfg.Domain, ".")
	if h.cfg.HomeSub != "" {
		home = h.cfg.HomeSub + "." + home
	}
	fallback := scheme + "://" + home + "/"
	if raw == "" {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && !h.cfg.Secure)) {
		return fallback
	}
	host := u.Hostname()
	if host != strings.TrimPrefix(h.cfg.Domain, ".") && !strings.HasSuffix(host, h.returnHost) {
		return fallback
	}
	return u.String()
}
