// Package guestdocs serves the public Sparkbox environment documentation from
// the gateway itself. Keeping this tiny surface in the gateway means every
// deployment already has a version-matched docs.catnip.sh; no separate docs
// workload or user sandbox is involved.
package guestdocs

import (
	"bytes"
	_ "embed"
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/publicports"
)

//go:embed index.html
var indexHTML []byte

//go:embed proxy.html
var proxyHTML []byte

//go:embed dev-environment.html
var devEnvironmentHTML []byte

//go:embed docs.md
var docsMarkdown []byte

//go:embed proxy.md
var proxyMarkdown []byte

//go:embed dev-environment.md
var devEnvironmentMarkdown []byte

//go:embed examples/.sparkbox/setup.sh
var setupShExample []byte

const portsMarker = "{{COMMON_HTTPS_PORTS}}"
const setupExampleMarker = "{{SETUP_SH_EXAMPLE}}"

// The public port list and the setup.sh example are process data rather than
// hand-maintained prose. The embedded files retain an obvious marker for
// editors, and every rendering is expanded once at startup: the ports from
// publicports' source of truth, the example from the one file that is also a
// real, lintable shell script.
var (
	indexPage            = expandAll(indexHTML)
	proxyPage            = expandAll(proxyHTML)
	devEnvironmentPage   = expandAll(devEnvironmentHTML)
	docsMarkdownPage     = expandAll(docsMarkdown)
	proxyMarkdownPage    = expandAll(proxyMarkdown)
	devEnvironmentMdPage = expandAll(devEnvironmentMarkdown)
)

func expandAll(body []byte) []byte {
	body = bytes.ReplaceAll(body, []byte(portsMarker), []byte(publicports.HumanList()))
	body = bytes.ReplaceAll(body, []byte(setupExampleMarker), bytes.TrimRight(setupShExample, "\n"))
	return body
}

// Handler returns the public documentation handler.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveHTML(w, indexPage)
	})
	mux.HandleFunc("GET /proxy", func(w http.ResponseWriter, _ *http.Request) {
		serveHTML(w, proxyPage)
	})
	mux.HandleFunc("GET /dev-environment", func(w http.ResponseWriter, _ *http.Request) {
		serveHTML(w, devEnvironmentPage)
	})
	mux.HandleFunc("GET /docs.md", func(w http.ResponseWriter, _ *http.Request) {
		serveMarkdown(w, docsMarkdownPage)
	})
	mux.HandleFunc("GET /proxy.md", func(w http.ResponseWriter, _ *http.Request) {
		serveMarkdown(w, proxyMarkdownPage)
	})
	mux.HandleFunc("GET /dev-environment.md", func(w http.ResponseWriter, _ *http.Request) {
		serveMarkdown(w, devEnvironmentMdPage)
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
