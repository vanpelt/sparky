package edgeauth

import (
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

//go:embed login.html
var loginHTML string

var loginTpl = template.Must(template.New("login").Parse(loginHTML))

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
}

// LoginHandler serves the browser login: a page that takes a token minted via
// `ssh ctl@<gateway> session-token`, and a POST that verifies it and drops the
// session cookie before bouncing back to the originally-requested URL.
type LoginHandler struct {
	cfg        LoginConfig
	cookieDom  string
	returnHost string // "." + Domain suffix that a return URL's host must match
}

func NewLoginHandler(cfg LoginConfig) *LoginHandler {
	if cfg.TTL <= 0 {
		cfg.TTL = 12 * time.Hour
	}
	return &LoginHandler{
		cfg:        cfg,
		cookieDom:  "." + strings.TrimPrefix(cfg.Domain, "."),
		returnHost: "." + strings.TrimPrefix(cfg.Domain, "."),
	}
}

func (h *LoginHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.page)
	mux.HandleFunc("POST /session", h.session)
	mux.HandleFunc("POST /logout", h.logout)
	return mux
}

// page renders the login form, carrying the sanitised return target through as
// a hidden field.
func (h *LoginHandler) page(w http.ResponseWriter, r *http.Request) {
	ret := h.safeReturn(r.URL.Query().Get("return"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	err := loginTpl.Execute(w, struct {
		Return  string
		Gateway string
	}{ret, h.cfg.Gateway})
	if err != nil && h.cfg.Logger != nil {
		h.cfg.Logger.Error("login page render failed", "err", err)
	}
}

// session verifies a pasted token and, on success, sets the cookie and
// redirects to the return target (or a plain OK for non-browser posts).
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
	if h.cfg.Logger != nil {
		h.cfg.Logger.Info("session established", "handle", id.Handle)
	}
	http.Redirect(w, r, h.safeReturn(r.PostForm.Get("return")), http.StatusSeeOther)
}

func (h *LoginHandler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", Domain: h.cookieDom, MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, h.safeReturn(r.PostForm.Get("return")), http.StatusSeeOther)
}

// safeReturn constrains a return URL to https on this zone, defeating an
// open-redirect (`?return=https://evil.com`). Anything off-zone or unparseable
// collapses to the zone root.
func (h *LoginHandler) safeReturn(raw string) string {
	fallback := "https://" + strings.TrimPrefix(h.cfg.Domain, ".") + "/"
	if raw == "" {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return fallback
	}
	host := u.Hostname()
	if host != strings.TrimPrefix(h.cfg.Domain, ".") && !strings.HasSuffix(host, h.returnHost) {
		return fallback
	}
	return u.String()
}
