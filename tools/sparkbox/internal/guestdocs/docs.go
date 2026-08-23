// Package guestdocs serves the public Sparkbox environment documentation from
// the gateway itself. Keeping this tiny surface in the gateway means every
// deployment already has a version-matched docs.catnip.sh; no separate docs
// workload or user sandbox is involved.
package guestdocs

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

//go:embed proxy.html
var proxyHTML []byte

//go:embed docs.md
var docsMarkdown []byte

//go:embed proxy.md
var proxyMarkdown []byte

// Handler returns the public documentation handler.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveHTML(w, indexHTML)
	})
	mux.HandleFunc("GET /proxy", func(w http.ResponseWriter, _ *http.Request) {
		serveHTML(w, proxyHTML)
	})
	mux.HandleFunc("GET /docs.md", func(w http.ResponseWriter, _ *http.Request) {
		serveMarkdown(w, docsMarkdown)
	})
	mux.HandleFunc("GET /proxy.md", func(w http.ResponseWriter, _ *http.Request) {
		serveMarkdown(w, proxyMarkdown)
	})
	return mux
}

func serveMarkdown(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(body) //nolint:errcheck
}

func serveHTML(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(body) //nolint:errcheck
}
