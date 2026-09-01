package ghapp

// The HTTP half: the App assertion every call carries, the one function that
// talks to api.github.com, and the translation from status codes to this
// package's four sentinels.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// The endpoints. Constants rather than the test-injectable vars the rest of the
// tree uses for github URLs (see users/seed.go), because Config.BaseURL already
// gives a test somewhere to point — the failure that convention was invented to
// catch, a URL nothing could exercise without reaching github.com, cannot
// happen here.
const (
	installationPath = "/repos/%s/%s/installation"
	repositoryPath   = "/repos/%s/%s"
	accessTokensPath = "/app/installations/%d/access_tokens"
	membershipPath   = "/orgs/%s/memberships/%s"
	appPath          = "/app"
)

const (
	// requestTimeout bounds one request. The caller's budget is smaller than it
	// looks: a guest asks for a credential under a 10s HTTP write timeout, and
	// on a fleet the path is guest -> node -> gateway -> github, so a hung
	// request spends somebody else's deadline too.
	requestTimeout = 10 * time.Second

	// jwtTTL is github's maximum for an App assertion. Anything longer is
	// rejected outright.
	jwtTTL = 10 * time.Minute

	// jwtBackdate is the one non-obvious line in the whole JWS. GitHub refuses
	// an assertion whose `iat` is in the future and says only "'Issued at'
	// claim ('iat') is in the future", which reads like a broken key. A gateway
	// whose clock is a second fast produces it on every mint, so the claim is
	// backdated by a minute and the whole class of failure goes away.
	jwtBackdate = 60 * time.Second

	// jwtReuse keeps one assertion for eight of its ten minutes. The two
	// minutes of slack absorb clock skew in the other direction: an assertion
	// still held here after github considers it expired would fail a mint that
	// has no reason to fail.
	jwtReuse = 8 * time.Minute

	// installTTL caches a slug -> installation lookup. Installations change
	// when a human clicks something, which is rare, and this lookup is in front
	// of every mint.
	installTTL = 10 * time.Minute

	// membershipTTL caches an org membership answer. Ten minutes is the
	// compromise the design names: leaving the org revokes repo access at the
	// next mint after this window, not at the next operator-run sync.
	membershipTTL = 10 * time.Minute

	// tokenRefreshLead retires a cached token five minutes before github does,
	// so a token handed to a guest is always good for long enough to finish the
	// clone it was asked for.
	tokenRefreshLead = 5 * time.Minute

	// maxCacheEntries bounds each cache. A gateway's real working set is a
	// handful of installations and one token per (sandbox tag set); the cap is
	// there so that a caller looping over generated names cannot grow the map
	// without limit.
	maxCacheEntries = 512
)

// appAuth returns the Authorization header value for an app-level call, minting
// a fresh assertion when the held one is past jwtReuse.
func (a *App) appAuth() (string, error) {
	a.jwtMu.Lock()
	defer a.jwtMu.Unlock()
	now := a.now()
	if a.jwt != "" && now.Before(a.jwtGood) {
		return "Bearer " + a.jwt, nil
	}
	token, err := a.signJWT(now)
	if err != nil {
		return "", err
	}
	a.jwt, a.jwtGood = token, now.Add(jwtReuse)
	return "Bearer " + token, nil
}

// signJWT renders the App assertion as a compact JWS, the same shape and the
// same twenty lines as oidc.Issuer.sign, with RS256 in place of ES256 because
// that is what github verifies an App key with.
func (a *App) signJWT(now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtTTL).Unix(),
		"iss": a.clientID,
	})
	if err != nil {
		return "", err
	}
	signingInput := b64(header) + "." + b64(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// do issues one request and decodes one answer.
//
// auth is the complete Authorization header value, passed in rather than
// derived, so that this function never chooses a credential and never has one
// worth naming in an error: the whole "no token in a log line" rule is enforced
// by there being nothing here to print.
func (a *App) do(ctx context.Context, method, path, auth, op string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		// A *url.Error prints the method and the URL and nothing else — no
		// headers — so a transport failure cannot carry the assertion or a
		// token into a log line.
		return fmt.Errorf("%w: %s: %v", ErrUpstream, op, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 != 2 {
		return statusError(resp, op)
	}
	if out == nil {
		return nil
	}
	// The cap is the usual guard against an upstream that streams forever; the
	// real bodies here are a few hundred bytes.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("%w: %s: could not read github's answer: %v", ErrUpstream, op, err)
	}
	return nil
}

// apiError is a non-2xx answer. It keeps the status because the same code means
// different things at different endpoints — a 404 resolving an installation is
// ErrNotInstalled, a 404 on a membership is "not a member" — so the mapping to
// a sentinel is finished by the caller, and it unwraps to the sentinel for the
// classes that mean the same thing everywhere.
type apiError struct {
	status   int
	op       string // "minting a token for installation 42"
	message  string // github's own "message" field
	hint     string // ours, when the status needs a fix naming
	sentinel error
}

func (e *apiError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "github answered %d %s", e.status, e.op)
	if e.message != "" {
		fmt.Fprintf(&b, ": %s", e.message)
	}
	if e.hint != "" {
		fmt.Fprintf(&b, " — %s", e.hint)
	}
	return b.String()
}

func (e *apiError) Unwrap() error { return e.sentinel }

func statusError(resp *http.Response, op string) *apiError {
	e := &apiError{status: resp.StatusCode, op: op, message: githubMessage(resp.Body)}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// 401 is the assertion being refused, never the installation: either
		// the key does not belong to this client id, or this host's clock is
		// far enough off that github will not accept a JWT it signs. Both are
		// operator fixes, so it refuses rather than inviting a retry.
		e.sentinel = ErrForbidden
		e.hint = "github rejected this host's app assertion — the private key may not belong to this client id, or this host's clock may be wrong"
	case resp.StatusCode == http.StatusForbidden:
		e.sentinel = ErrForbidden
	case resp.StatusCode == http.StatusTooManyRequests:
		// Rate limiting is upstream saying "later", which is exactly the class
		// a caller is allowed to retry.
		e.sentinel = ErrUpstream
	case resp.StatusCode >= 500:
		e.sentinel = ErrUpstream
	}
	return e
}

// githubMessage lifts the "message" field out of an error body. It is safe to
// repeat: it is github's own prose about our request, it names the fix more
// often than not ("Resource not accessible by integration"), and it cannot
// contain a credential because github never echoes the Authorization header
// back. It is still clipped and flattened — an error that spans forty lines of
// somebody else's HTML is not an error anybody reads.
func githubMessage(body io.Reader) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 1<<16)).Decode(&out); err != nil {
		return ""
	}
	msg := strings.Join(strings.Fields(out.Message), " ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

// learnSlug records the App's own slug the first time github mentions it. See
// InstallURL for why this is learned rather than configured.
func (a *App) learnSlug(slug string) {
	if !validAppSlug(slug) {
		return
	}
	a.slugMu.Lock()
	defer a.slugMu.Unlock()
	a.appSlug = slug
}

// learnSlugFromAPI asks GET /app for the slug, best effort. It runs on the
// 404 path only — the moment an install URL is about to be printed and no
// installation has ever answered to supply one — and it is rate limited so a
// guest looping on a repository nobody installed cannot turn one failure into a
// second request every time.
func (a *App) learnSlugFromAPI(ctx context.Context) {
	a.slugMu.Lock()
	if a.appSlug != "" || a.now().Before(a.slugAfter) {
		a.slugMu.Unlock()
		return
	}
	a.slugAfter = a.now().Add(installTTL)
	a.slugMu.Unlock()

	auth, err := a.appAuth()
	if err != nil {
		return
	}
	var out struct {
		Slug string `json:"slug"`
	}
	if err := a.do(ctx, http.MethodGet, appPath, auth, "reading the app's own record", nil, &out); err != nil {
		// Nothing to report: the caller already has a real error to return, and
		// the only consequence of failing here is a less direct install URL.
		return
	}
	a.learnSlug(out.Slug)
}

// tokenKey identifies a cached token by everything that changes its scope. The
// lists are sorted so that two callers asking for the same access in different
// order share one token rather than minting two.
func tokenKey(instID int64, sortedNames []string, perms map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\x00%s\x00", instID, strings.Join(sortedNames, ","))
	for _, k := range slices.Sorted(maps.Keys(perms)) {
		fmt.Fprintf(&b, "%s=%s,", k, perms[k])
	}
	return b.String()
}

// checkSlug validates the two halves of "owner/name" before either becomes a
// URL path segment. GitHub handed us most of these strings itself, and they are
// checked anyway for the reason users.ListMembers states: "GitHub would never"
// is not a property this process can check.
func checkSlug(owner, name string) (string, error) {
	if !validLogin(owner) {
		return "", fmt.Errorf("%q is not a github account name", owner)
	}
	if !validRepoName(name) {
		return "", fmt.Errorf("%q is not a github repository name", name)
	}
	return owner + "/" + name, nil
}

// validLogin is users.githubLoginOK, which is unexported there and whose
// exported wrappers (ValidGitHubOrg, ValidGitHubTeam) are aliases of it. Copied
// rather than imported so that an HTTP client does not pull in the account
// store to spell a grammar in eight lines.
func validLogin(login string) bool {
	if len(login) == 0 || len(login) > 39 || strings.HasPrefix(login, "-") || strings.HasSuffix(login, "-") {
		return false
	}
	for i := 0; i < len(login); i++ {
		c := login[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alnum && !(c == '-' && login[i-1] != '-') {
			return false
		}
	}
	return true
}

// validRepoName is a deliberately different grammar from validLogin: repository
// names permit '.' and '_' and may start with a digit, none of which a login
// may. "." and ".." are refused separately — they are legal-looking names that
// would climb out of the URL path they are pasted into.
func validRepoName(name string) bool {
	if len(name) == 0 || len(name) > 100 || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

// validAppSlug bounds the string that gets pasted into the install URL. App
// slugs are github's own lower-cased dashed form of the app's name, which is
// the login grammar plus a little more length.
func validAppSlug(slug string) bool {
	if len(slug) == 0 || len(slug) > 64 || strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") {
		return false
	}
	for i := 0; i < len(slug); i++ {
		c := slug[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}
