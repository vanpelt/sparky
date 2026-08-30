package webui

import (
	"bytes"
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

// Default attribute values must survive minification: the consoles style their
// form fields with attribute selectors, so a dropped type="text" leaves every
// text input matching nothing and rendering in the browser's default white,
// which is what shipped before KeepDefaultAttrVals was set.
func TestBuildKeepsDefaultAttributeValues(t *testing.T) {
	tmpl := []byte(`<!doctype html><html lang="en"><head><title>t</title>
<style>/*SHARED_CSS*/
input[type=text] { background: hsl(var(--background)); }</style></head>
<body><input id="a" type="text" /><script>/*SHARED_JS*/</script></body></html>`)

	body := string(Build(tmpl).raw)
	if !strings.Contains(body, `type="text"`) {
		t.Errorf(`type="text" was minified away — input[type=text] selectors will not match; got: %s`, body)
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

// Compose is the path a server-rendered html/template page takes into the
// design system, so what it must prove is a negative: that it substituted both
// markers and then left the bytes alone. The template actions below are the
// point — the same source run through Build's minifier is what internal/edgeauth
// declined to do, because a tokeniser that does not know {{...}} can requote or
// reflow an action into something template.Parse then rejects at startup.
func TestComposeSubstitutesBothMarkers(t *testing.T) {
	tmpl := []byte(`<!doctype html>
<html lang="en"><head><title>{{.Title}}</title>
<style>
  /*SHARED_CSS*/
  .wrap { max-width: 900px; }
</style></head>
<body>
<p class="{{if .Bad}}err{{end}}">{{.Field}}</p>
<script>/*SHARED_JS*/</script>
</body></html>`)

	out := string(Compose(tmpl))

	// Both markers are gone, and the assets they stood in for are here.
	for _, marker := range []string{cssMarker, jsMarker} {
		if strings.Contains(out, marker) {
			t.Errorf("marker %q survived Compose", marker)
		}
	}
	if !strings.Contains(out, "--muted-foreground") {
		t.Error("shared CSS token --muted-foreground missing — the CSS marker was not substituted")
	}
	if !strings.Contains(out, "function esc") {
		t.Error("shared JS helper esc missing — the JS marker was not substituted")
	}

	// Template syntax comes through byte for byte. A minifier in this path
	// would be entitled to rewrite any of these three.
	for _, action := range []string{"{{.Title}}", `class="{{if .Bad}}err{{end}}"`, "{{.Field}}"} {
		if !strings.Contains(out, action) {
			t.Errorf("template action %s did not survive Compose — something rewrote the markup", action)
		}
	}

	// Nothing but the two markers moved: the composed length is exactly the
	// template plus the two assets, minus the markers they replaced.
	want := len(tmpl) + len(sharedCSS) + len(sharedJS) - len(cssMarker) - len(jsMarker)
	if len(out) != want {
		t.Errorf("composed length = %d, want %d — Compose did more than substitute", len(out), want)
	}
}

// Build must stay expressed in terms of Compose rather than carrying its own
// copy of the substitution, so a marker added to one is never missing from the
// other. Composing first and minifying the result is exactly what Build does,
// so the two byte streams have to agree.
func TestBuildComposesThroughCompose(t *testing.T) {
	var buf bytes.Buffer
	if err := minifier.Minify("text/html", &buf, bytes.NewReader(Compose(testTmpl))); err != nil {
		t.Fatal(err)
	}
	if got, want := string(Build(testTmpl).raw), buf.String(); got != want {
		t.Errorf("Build output differs from minify(Compose(tmpl)); Build has drifted into a second substitution implementation\n got: %s\nwant: %s", got, want)
	}
}
