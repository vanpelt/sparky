// Package webui holds the design system shared by sparkbox's two embedded
// single-page consoles (internal/console, internal/userconsole) and the
// build-and-serve pipeline for them: compose a page template against the
// shared CSS/JS, minify the result, and pre-gzip it — all at package init,
// so it "just happens" whenever the binary is built and started, with no
// separate generate step to keep in sync or forget in CI.
package webui

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
	"github.com/tdewolff/minify/v2/html"
	"github.com/tdewolff/minify/v2/js"
)

//go:embed shared.css
var sharedCSS string

//go:embed shared.js
var sharedJS string

// Markers a page template embeds inside its own <style>/<script> blocks,
// replaced with the shared design system before minification.
const (
	cssMarker = "/*SHARED_CSS*/"
	jsMarker  = "/*SHARED_JS*/"
)

var minifier = newMinifier()

func newMinifier() *minify.M {
	m := minify.New()
	// KeepQuotes/KeepDocumentTags/KeepEndTags: this is a hand-authored SPA,
	// not throwaway markup — tests and tooling (hack/preview-console.py)
	// depend on attribute quoting and on <head>/<body> tags surviving intact,
	// so only whitespace/comments get squeezed out, not structure.
	m.Add("text/html", &html.Minifier{KeepQuotes: true, KeepDocumentTags: true, KeepEndTags: true})
	m.AddFunc("text/css", css.Minify)
	// KeepVarNames: renaming locals saves a few hundred bytes but makes
	// view-source debugging on these admin/operator consoles useless; we only
	// want whitespace/comment stripping here, not a full mangler.
	m.Add("application/javascript", &js.Minifier{KeepVarNames: true})
	return m
}

// Page is a fully composed console SPA: minified HTML plus a pre-gzipped
// copy, computed once so every request just picks whichever the client
// accepts.
type Page struct {
	raw  []byte
	gzip []byte
}

// Build composes a page template — an index.html embedding the
// /*SHARED_CSS*/ and /*SHARED_JS*/ markers inside its own <style> and
// <script> blocks — against the shared design system, minifies it, and
// pre-gzips the result. Intended for a package-level var initialized from
// embedded, developer-controlled HTML, so a malformed template panics
// immediately rather than surfacing as a runtime error.
func Build(tmpl []byte) *Page {
	out := bytes.Replace(tmpl, []byte(cssMarker), []byte(sharedCSS), 1)
	out = bytes.Replace(out, []byte(jsMarker), []byte(sharedJS), 1)

	var buf bytes.Buffer
	if err := minifier.Minify("text/html", &buf, bytes.NewReader(out)); err != nil {
		panic(fmt.Sprintf("webui: minify failed: %v", err))
	}
	raw := buf.Bytes()

	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		panic(fmt.Sprintf("webui: gzip writer: %v", err))
	}
	if _, err := zw.Write(raw); err != nil {
		panic(fmt.Sprintf("webui: gzip write: %v", err))
	}
	if err := zw.Close(); err != nil {
		panic(fmt.Sprintf("webui: gzip close: %v", err))
	}

	return &Page{raw: raw, gzip: gz.Bytes()}
}

// ServeHTTP writes the page, gzip-compressed whenever the client advertises
// support for it (every browser, and Go's own http.Client with
// DisableCompression left at its default false).
func (p *Page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Vary", "Accept-Encoding")
	body := p.raw
	if acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		body = p.gzip
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body) //nolint:errcheck
}

func acceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			return true
		}
	}
	return false
}
