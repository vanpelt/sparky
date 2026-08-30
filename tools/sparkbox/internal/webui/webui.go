// Package webui holds what sparkbox's two embedded single-page consoles
// (internal/console, internal/userconsole) share. Mostly that is the design
// system and the build-and-serve pipeline for it: compose a page template
// against the shared CSS/JS, minify the result, and pre-gzip it — all at
// package init, so it "just happens" whenever the binary is built and started,
// with no separate generate step to keep in sync or forget in CI. It also
// holds the server-side policy both dashboards answer a listing with — see
// dashboard.go.
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
// replaced with the shared design system by Compose — and so by Build, which
// minifies and pre-gzips what Compose returns.
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
	//
	// KeepDefaultAttrVals for a subtler reason: the minifier is entitled to drop
	// an attribute that merely restates the HTML default, and type="text" is
	// exactly that. Dropping it is harmless to the DOM and fatal to CSS — an
	// `input[type=text]` selector stops matching, and every text field silently
	// falls back to the browser's own white-on-black-page styling while the
	// password and number fields beside it (whose types are not defaults, so
	// they survive) still look right. That shipped once; keep the attributes.
	m.Add("text/html", &html.Minifier{
		KeepQuotes: true, KeepDocumentTags: true, KeepEndTags: true, KeepDefaultAttrVals: true,
	})
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

// Compose substitutes the shared design system into a page template and hands
// back the result with nothing else done to it — no minification, no gzip.
// It exists for the one kind of page Build cannot serve: HTML that is also a
// Go text/template or html/template source, parsed at startup and executed
// per request.
//
// The minifier is why those two cases have to be separated. tdewolff/minify
// tokenises HTML, CSS and JS; it has no knowledge whatsoever of {{...}}
// template actions, so it is free to move, requote or delete the bytes around
// one. An action that spans a tag boundary, or that sits in an attribute the
// minifier decides to unquote, comes back out as markup the template parser
// then refuses — which means a panic inside template.Must at package init, on
// every host, at startup, for a page nobody has even requested yet — or, worse
// because it is silent, as a template that still parses but renders the wrong
// thing. internal/edgeauth/login.html is the standing counter-example in this
// tree: it is a template, so it could not go through Build, and it ended up
// pasting its own copy of the design tokens rather than run a minifier over
// template syntax. Compose is the way out of that trade — a template page gets
// 100% of the design-system reuse and none of the minifier risk, at the cost
// of shipping a few unminified kilobytes on a page nobody loads in a loop.
//
// Callers that are plain HTML with no template actions should keep using
// Build, which is Compose plus the minify-and-pre-gzip pipeline.
//
// Note that substitution is single-shot, by bytes.Replace(..., 1): a template
// carrying a SECOND /*SHARED_CSS*/ keeps that one verbatim in the shipped
// page, where it reads as a stray CSS comment. One marker each is the contract.
func Compose(tmpl []byte) []byte {
	out := bytes.Replace(tmpl, []byte(cssMarker), []byte(sharedCSS), 1)
	return bytes.Replace(out, []byte(jsMarker), []byte(sharedJS), 1)
}

// Build composes a page template — an index.html embedding the
// /*SHARED_CSS*/ and /*SHARED_JS*/ markers inside its own <style> and
// <script> blocks — against the shared design system, minifies it, and
// pre-gzips the result. Intended for a package-level var initialized from
// embedded, developer-controlled HTML, so a malformed template panics
// immediately rather than surfacing as a runtime error.
//
// The substitution half is Compose's, deliberately not a second copy of those
// two Replace calls: two implementations of the same marker contract are two
// places to add a third marker and one place to forget it.
func Build(tmpl []byte) *Page {
	out := Compose(tmpl)

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
