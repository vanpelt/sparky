package launch

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
)

// getBadge drives the real mux, not h.badge directly, because half of what is
// being asserted is that the route is mounted OUTSIDE the auth wrapper. Calling
// the method would prove the handler is correct and say nothing about whether
// anybody can reach it.
func getBadge(t *testing.T, h *Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://go.example.test/badge.svg", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec
}

// TestBadgeNeedsNoSession is the test that keeps the button visible.
//
// GitHub proxies every image in a comment through camo, which sends no cookies
// and carries no identity. If this route ever slipped behind edgeauth.Require —
// by someone wrapping the whole mux instead of the routes that need it — camo
// would get a 401 or a login redirect, and every badge in every comment ever
// written would render as a broken image with no error anybody would see.
func TestBadgeNeedsNoSession(t *testing.T) {
	h := newHandler(t, &fakeOps{t: t}, &fakeRepos{})

	rec := getBadge(t, h, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a badge behind a gate is a broken image)", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", got)
	}
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "public") || !strings.Contains(cc, "max-age=3600") {
		t.Errorf("Cache-Control = %q, want a public max-age camo can honour", cc)
	}
	// The specific poison: edgeauth.Require stamps this on every response it
	// produces, so its presence here would mean the gate closed over the route.
	if strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q — no-store defeats camo's cache entirely", cc)
	}
	if got := rec.Header().Get("ETag"); got != badgeETag || got == "" {
		t.Errorf("ETag = %q, want the compile-time %q", got, badgeETag)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	// A public, hour-cacheable response that minted or refreshed a session
	// would put one visitor's cookie in a shared cache.
	if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("Set-Cookie = %v, want none on a publicly cached route", got)
	}
	if body := rec.Body.Bytes(); string(body) != string(badgeSVG) {
		t.Errorf("body is not the embedded badge (%d bytes vs %d)", len(body), len(badgeSVG))
	}

	// One image for everyone is the whole premise of the static badge, so a
	// credentialed fetch must be byte-identical to an anonymous one.
	withSession := getBadge(t, h, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "spk_v1", Value: "not-a-real-token"})
	})
	if withSession.Code != http.StatusOK {
		t.Fatalf("status with a cookie = %d, want 200", withSession.Code)
	}
	if withSession.Body.String() != rec.Body.String() {
		t.Error("the badge differs for a signed-in viewer; it must not vary by viewer at all")
	}
}

// TestBadgeHonoursIfNoneMatch pins the revalidation camo does once an hour: a
// 304 with no body rather than a second full download of the same bytes.
func TestBadgeHonoursIfNoneMatch(t *testing.T) {
	h := newHandler(t, &fakeOps{t: t}, &fakeRepos{})

	first := getBadge(t, h, nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag to revalidate with")
	}
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Errorf("ETag = %s, want it quoted — an unquoted validator is malformed and may be dropped", etag)
	}

	second := getBadge(t, h, func(r *http.Request) { r.Header.Set("If-None-Match", etag) })
	if second.Code != http.StatusNotModified {
		t.Fatalf("revalidation status = %d, want 304", second.Code)
	}
	if n := second.Body.Len(); n != 0 {
		t.Errorf("304 carried %d bytes of body, want none", n)
	}
}

// TestBadgeIsSelfContained is the art's safety net.
//
// An SVG loaded through <img> is an isolated document that camo serves from its
// own hostname, so a relative URL has no meaning and an absolute one is a
// third-party fetch from inside a GitHub page. A webfont reference would make
// the label render in whatever the fallback is — or not at all — on the
// machines that matter most. The logo is an embedded PNG data URI, not a
// second network hop.
func TestBadgeIsSelfContained(t *testing.T) {
	svg := string(badgeSVG)

	if strings.Contains(svg, "https://") {
		t.Error("the badge references an absolute https URL; it must fetch nothing")
	}
	// The one http:// allowed is the SVG namespace declaration, which is an
	// identifier and not a fetch.
	if n := strings.Count(svg, "http://"); n != 1 || !strings.Contains(svg, `xmlns="http://www.w3.org/2000/svg"`) {
		t.Errorf("found %d http:// references; the only legal one is the xmlns", n)
	}
	for _, banned := range []string{"@font-face", "@import", "xlink:href", "<script", "<foreignObject"} {
		if strings.Contains(svg, banned) {
			t.Errorf("the badge contains %q, which either fetches something or executes something", banned)
		}
	}
	if !strings.Contains(svg, `<image`) || !strings.Contains(svg, `href="data:image/png;base64,`) {
		t.Error("the badge logo is not an embedded PNG data URI")
	}
	if strings.Contains(svg, badgeLogoMarker) {
		t.Error("the badge still contains the unexpanded logo marker")
	}
	// No emoji glyph: the supplied logo must render identically everywhere.
	if strings.ContainsAny(svg, "\U0001F680⚡") {
		t.Error("the badge draws an emoji glyph instead of the Sparkbox logo")
	}
	// textLength plus lengthAdjust is what stops the label spilling out of the
	// pill on a machine whose first font in the stack is missing — shields.io's
	// trick, and the reason the badge has a fixed width at all.
	if !strings.Contains(svg, "textLength=") || !strings.Contains(svg, `lengthAdjust="spacingAndGlyphs"`) {
		t.Error("the label has no textLength/lengthAdjust; on a machine without the first font it will overflow the pill")
	}
	// The alt text degrades to a sentence when the image is blocked, and the
	// title is what a screen reader announces.
	if !strings.Contains(svg, "<title>Open in Sparkbox</title>") || !strings.Contains(svg, `role="img"`) {
		t.Error("the badge is missing its <title>/role=img accessible name")
	}
	// Its own opaque dark ground: GitHub serves one image to light and dark
	// readers and a bare <img> cannot switch themes, so the badge brings the
	// contrast with it rather than borrowing the page's.
	if !strings.Contains(svg, "#18181B") || !strings.Contains(svg, "#FAFAFA") {
		t.Error("the badge does not carry the pinned dark ground / light text")
	}
}

// TestNewRefusesAMissingStore pins the typed-nil guard.
//
// h.ops is an interface and cfg.Ops is a concrete pointer, so assigning a nil
// *ctlops.Ops would produce an interface value that is NOT nil: every later
// `if h.ops == nil` check would pass and the first click would nil-dereference
// inside a request goroutine. The check therefore has to happen on the concrete
// value, before the assignment, and it has to be loud.
func TestNewRefusesAMissingStore(t *testing.T) {
	signer := edgeauth.NewSigner([]byte("test-key-material"))
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no ops", Config{Repos: &repos.Store{}, Signer: signer}},
		{"no repo store", Config{Ops: &ctlops.Ops{}, Signer: signer}},
		{"no signer", Config{Ops: &ctlops.Ops{}, Repos: &repos.Store{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New returned instead of panicking; the failure would surface as a nil deref on the first click")
				}
			}()
			New(tc.cfg)
		})
	}
}

// TestNewNormalizesTheZone: a leading-dot --proxy-domain is tolerated
// everywhere else in the tree, and the origin this door compares a form POST's
// Origin against has to match what a browser will actually send — a dot in it
// would refuse every create as cross-origin.
func TestNewNormalizesTheZone(t *testing.T) {
	h := New(Config{
		Ops: &ctlops.Ops{}, Repos: &repos.Store{},
		Signer: edgeauth.NewSigner([]byte("test-key-material")),
		Domain: ".Catnip.SH.",
	})
	if h.Subdomain() != DefaultSubdomain {
		t.Errorf("Subdomain() = %q, want the package default %q", h.Subdomain(), DefaultSubdomain)
	}
	if want := "https://go.catnip.sh"; h.origin != want {
		t.Errorf("origin = %q, want %q with no dot and no trailing slash", h.origin, want)
	}
}
