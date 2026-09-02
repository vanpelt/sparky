package guestdocs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/publicports"
)

func TestHandler(t *testing.T) {
	for _, tc := range []struct {
		path string
		want int
		text string
	}{
		{"/", http.StatusOK, "Shared resources"},
		{"/proxy", http.StatusOK, "Bind to"},
		{"/dev-environment", http.StatusOK, "Allow the proxy's Host header"},
		{"/docs.md", http.StatusOK, "## Pinning this VM"},
		{"/proxy.md", http.StatusOK, "## Wake on request"},
		{"/dev-environment.md", http.StatusOK, "## Allow the proxy's Host header"},
		{"/missing", http.StatusNotFound, "404 page not found"},
	} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.text) {
			t.Errorf("GET %s = %d %q, want %d containing %q", tc.path, rec.Code, rec.Body.String(), tc.want, tc.text)
		}
		if tc.want == http.StatusOK {
			wantType := "text/html"
			if strings.HasSuffix(tc.path, ".md") {
				wantType = "text/markdown"
			}
			if !strings.Contains(rec.Header().Get("Content-Type"), wantType) {
				t.Errorf("GET %s content type = %q, want %s", tc.path, rec.Header().Get("Content-Type"), wantType)
			}
		}
	}
}

// TestSetupShExampleIsEmbeddedNotRetyped guards the one fact that makes the
// example in the docs trustworthy: it is the literal file at
// examples/.sparkbox/setup.sh, not a second copy somebody can let drift.
func TestSetupShExampleIsEmbeddedNotRetyped(t *testing.T) {
	for _, path := range []string{"/dev-environment", "/dev-environment.md"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		body := rec.Body.String()
		if !strings.Contains(body, "sparkbox set-port 5173") {
			t.Errorf("GET %s does not contain the setup.sh example", path)
		}
		if strings.Contains(body, setupExampleMarker) {
			t.Errorf("GET %s still contains the unexpanded setup.sh marker", path)
		}
	}
}

// TestNoPageHardcodesADomain: every page here is baked into every
// deployment's binary, not just the flagship catnip.sh one, and is read by
// agents (via `sparkbox docs`) as well as browsers, so a literal domain in an
// example URL or framework config would be silently wrong everywhere except
// the one deployment it was written against.
func TestNoPageHardcodesADomain(t *testing.T) {
	for _, path := range []string{
		"/", "/proxy", "/dev-environment",
		"/docs.md", "/proxy.md", "/dev-environment.md",
	} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(rec.Body.String(), "catnip.sh") {
			t.Errorf("GET %s hardcodes a domain instead of deriving one from `sparkbox whoami`", path)
		}
	}
}

func TestPublicPortListComesFromSourceOfTruth(t *testing.T) {
	for _, path := range []string{"/", "/proxy", "/docs.md", "/proxy.md"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		body := rec.Body.String()
		if !strings.Contains(body, publicports.HumanList()) {
			t.Errorf("GET %s does not contain the common HTTPS ports", path)
		}
		if strings.Contains(body, portsMarker) {
			t.Errorf("GET %s still contains the unexpanded port marker", path)
		}
	}
}

// TestTheLifecycleVerbsAreDocumentedInBothRenderings. This page is what the
// agent inside a box reads, and the two verbs it documents can end that box's
// session — so a verb missing from here is a verb somebody discovers by
// accident, on a VM that then stops.
func TestTheLifecycleVerbsAreDocumentedInBothRenderings(t *testing.T) {
	for _, page := range []struct{ path, sample string }{
		{"/docs.md", "## Saving this VM as your tag's template"},
		{"/", "Saving a VM"},
	} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, page.path, nil))
		body := rec.Body.String()
		if !strings.Contains(body, page.sample) {
			t.Errorf("GET %s does not document the capture verb", page.path)
		}
		for _, want := range []string{
			"sparkbox pause",
			"snapshot",
			// The three facts somebody has to know BEFORE they run it, because
			// afterwards their session is gone.
			"pause",
			"already carries",
			"snapshot ls",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s does not mention %q", page.path, want)
			}
		}
	}
}
