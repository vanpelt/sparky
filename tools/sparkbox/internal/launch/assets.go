package launch

// The static bytes this door serves, embedded and prepared once at package
// init.
//
// Everything that has to happen at build time happens here, because this
// project has no generate step in its release pipeline: anything not done at
// package init will not be done at all. go:embed cannot reach outside the
// directory of the package that declares it, which is why badge.svg lives
// beside the Go file that serves it rather than in a shared assets tree.

import (
	"crypto/sha256"
	"encoding/hex"
	"html/template"

	_ "embed"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/webui"
)

//go:embed badge.svg
var badgeSVG []byte

//go:embed page.html
var pageTemplate []byte

// pageTpl is every screen this door renders — the confirm page, the two
// refusals and the explainer — parsed once at package init.
//
// Package level, and template.Must, for the reason every webui consumer is a
// package-level var (internal/xterm/assets.go:19-25): a malformed template must
// panic at STARTUP, on the machine that built it, rather than on the first
// visit — which for this page means the first time a stranger clicks a badge in
// a pull request, which is the worst possible moment to discover a typo.
//
// webui.Compose and not webui.Build. Build minifies, and tdewolff/minify
// tokenises HTML with no knowledge whatsoever of {{...}} actions: it is free to
// requote an attribute an action sits in or move the bytes around one, and what
// comes back out is either markup template.Parse refuses — a panic here, on
// every host, for a page nobody has requested yet — or, worse because it is
// silent, a template that parses and renders the wrong thing.
// internal/edgeauth/login.html is this tree's standing counter-example: it is a
// template, so it could not go through Build, and it ended up pasting its own
// copy of the design tokens instead. Compose is the way out of that trade — the
// whole design system, none of the minifier risk, at the cost of shipping a few
// unminified kilobytes on a page nobody loads in a loop.
//
// Compose returns []byte because Build's input is []byte; template.Parse wants
// a string, hence the conversion, which happens exactly once.
var pageTpl = template.Must(template.New("launch").Parse(string(webui.Compose(pageTemplate))))

// badgeETag is the validator for a body that never changes at runtime, computed
// once here rather than per request.
//
// It is a strong ETag on purpose: the bytes are byte-identical for every
// viewer, so two responses with this tag really are interchangeable, and a
// conditional request can be answered with a 304 that carries no body at all.
// Truncating the digest to 16 hex characters is the usual bargain — a 64-bit
// tag over a body that changes only when somebody edits badge.svg and redeploys
// leaves no realistic chance of two different badges colliding, and it keeps the
// header short enough to read in a curl -I while debugging a camo cache.
//
// The quotes are part of the header value's grammar (RFC 9110 §8.8.3), not
// decoration: an unquoted ETag is malformed and an intermediary is entitled to
// drop it, which would silently turn every camo refetch into a full download.
var badgeETag = func() string {
	sum := sha256.Sum256(badgeSVG)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}()
