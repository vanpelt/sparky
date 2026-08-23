package guestdocs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
