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
		{"/docs.md", http.StatusOK, "## Pinning this VM"},
		{"/proxy.md", http.StatusOK, "## Wake on request"},
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
