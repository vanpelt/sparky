package webui

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A minimal page template exercising both markers inside real style/script
// blocks, so the test covers the substitution + minify path end to end.
var testTmpl = []byte(`<!doctype html>
<html lang="en"><head><title>t</title>
<style>
  /*SHARED_CSS*/
  .wrap { max-width: 900px; }
</style></head>
<body>
<div id="toast"></div>
<script>
(function () {
  /*SHARED_JS*/
  function localHelper() { return esc("hi"); }
  localHelper();
})();
</script>
</body></html>`)

func TestBuildSubstitutesAndMinifies(t *testing.T) {
	p := Build(testTmpl)
	body := string(p.raw)

	// Markers are gone; the shared assets they stood in for are present.
	for _, marker := range []string{cssMarker, jsMarker} {
		if strings.Contains(body, marker) {
			t.Errorf("marker %q survived into output", marker)
		}
	}
	// A token from shared.css and one from shared.js each made it in.
	if !strings.Contains(body, "--background") {
		t.Error("shared CSS token --background missing from output")
	}
	if !strings.Contains(body, "function esc") {
		t.Error("shared JS helper esc missing from output")
	}
	// KeepVarNames: page-local identifiers are not mangled away.
	if !strings.Contains(body, "localHelper") {
		t.Error("local identifier localHelper was mangled — KeepVarNames not in effect")
	}
	// Minification actually shrank it.
	if len(p.raw) >= len(testTmpl)+len(sharedCSS)+len(sharedJS) {
		t.Error("output not smaller than the composed input — minify did nothing")
	}
}

func TestServeHTTPContentNegotiation(t *testing.T) {
	p := Build(testTmpl)

	// With gzip advertised, the body is the compressed copy and decompresses
	// back to the exact minified bytes.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := io.ReadAll(zr)
	if string(decoded) != string(p.raw) {
		t.Error("gzip body did not decompress to the minified page")
	}

	// Without gzip, the raw minified bytes are served uncompressed.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if got := rec2.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if rec2.Body.String() != string(p.raw) {
		t.Error("uncompressed body did not match the minified page")
	}
}
