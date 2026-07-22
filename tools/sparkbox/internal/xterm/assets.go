package xterm

// The embedded page and the vendored xterm.js bundle it loads.
//
// Everything is embedded and composed at package init, matching the consoles:
// there is no generate step in this project's release pipeline, so anything
// that has to happen at build time has to happen here or it will not happen at
// all. go:embed cannot reach outside this directory, which is why assets/ is a
// subdirectory of the package that serves it.

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/webui"
)

//go:embed index.html
var indexTemplate []byte

// indexPage is composed against the shared console design system, minified and
// pre-gzipped once. A malformed template panics here, at startup, rather than
// surfacing as a broken page on the first visit.
var indexPage = webui.Build(indexTemplate)

//go:embed assets
var assetsFS embed.FS

// assetServer serves the vendored xterm.js bundle. The files are immutable —
// upgrading xterm.js changes the binary, not the contents of a URL — so they
// are cached hard. That matters more here than on the consoles: xterm.js is
// 290 KB, and a terminal that re-downloads it on every reconnect would feel
// slow for a reason the user cannot see.
func assetServer() http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic("xterm: embedded assets missing: " + err.Error())
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		// The bundle is developer-controlled and served from the same origin as
		// the terminal, so a sniffed content type is only ever a way to be
		// wrong about it.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
}
