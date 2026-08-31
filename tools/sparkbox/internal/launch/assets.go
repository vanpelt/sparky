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
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html/template"

	_ "embed"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/webui"
)

//go:embed badge.svg
var badgeSVG []byte

//go:embed page.html
var pageTemplate []byte

//go:embed progress.js
var progressJS []byte

// pageJSMarker is where progress.js goes: a comment inside the page's one
// <script> element, replaced with the file's exact bytes before the template is
// parsed. Same shape as webui's /*SHARED_CSS*/, and for the same reason — the
// script has to be IN the document (there is no img-src-style allowance for a
// second request, and default-src 'none' would block one anyway), while still
// being a real .js file a developer can read, lint and diff.
const pageJSMarker = "/*PAGE_JS*/"

// withProgressJS substitutes the script and refuses to produce a page without
// it.
//
// The panic is the point. The Content-Security-Policy hands out a hash for
// these exact bytes, so a template that quietly lost its marker would ship a
// policy granting a script that is not there — harmless today, and exactly the
// state in which somebody later concludes the hash is vestigial and replaces it
// with something looser. Failing at startup, on the machine that built it,
// keeps the two halves one fact.
func withProgressJS(tmpl []byte) []byte {
	if !bytes.Contains(tmpl, []byte(pageJSMarker)) {
		panic("launch: page.html has no " + pageJSMarker + " marker for progress.js")
	}
	return bytes.Replace(tmpl, []byte(pageJSMarker), progressJS, 1)
}

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
var pageTpl = template.Must(template.New("launch").Parse(string(webui.Compose(withProgressJS(pageTemplate)))))

// progressJSHash is the CSP source expression that admits the page's script:
// base64 of its sha256, quoted, exactly as
// https://www.w3.org/TR/CSP3/#grammardef-hash-source spells it.
//
// It is computed from the RENDERED page and not from progress.js, and that is
// not belt-and-braces — it is the only correct source. html/template lexes a
// <script> element as JavaScript and strips its comments on the way out,
// replacing each with a newline, so the bytes the browser hashes are never the
// bytes on disk for any script that carries a comment. Hashing the file would
// produce a digest that is right about our intent and wrong about the document,
// and the failure mode is the whole script silently not running in production
// while every unit test passes.
//
// A hash and not a nonce, because a nonce has to be minted per response and
// therefore composed at render time rather than once in New, and a page whose
// CSP varies per request cannot be pinned by a test. The script block is static
// — no template action is inside it — so one execution at init settles it for
// the life of the process.
//
// The full 32 bytes, not the truncated form badgeETag uses: an ETag is a cache
// validator, and this is the only thing standing between this page and an
// attacker-authored script.
var progressJSHash = func() string {
	var buf bytes.Buffer
	if err := pageTpl.Execute(&buf, pageData{}); err != nil {
		panic("launch: the page template does not render: " + err.Error())
	}
	script, err := inlineScript(buf.Bytes())
	if err != nil {
		panic("launch: " + err.Error())
	}
	sum := sha256.Sum256(script)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}()

// inlineScript returns the bytes between the page's one <script> and its
// </script> — what a browser hashes when it checks the policy.
//
// Shared with the tests, which do the same arithmetic against a real rendered
// screen. A second copy of this parsing in the test would be free to agree with
// itself while disagreeing with the browser.
func inlineScript(page []byte) ([]byte, error) {
	const open, closing = "<script>", "</script>"
	i := bytes.Index(page, []byte(open))
	j := bytes.Index(page, []byte(closing))
	if i < 0 || j < i {
		return nil, errors.New("the rendered page has no inline <script> block")
	}
	if k := bytes.Index(page[i+len(open):], []byte("<script")); k >= 0 {
		return nil, errors.New("the rendered page has more than one <script> block")
	}
	script := page[i+len(open) : j]
	// A marker substituted with nothing, or a template that lost the file,
	// would otherwise produce a perfectly valid hash for an empty script and a
	// page with no progress on it.
	if !bytes.Contains(script, []byte("addEventListener")) {
		return nil, errors.New("the page's script block is empty")
	}
	return script, nil
}

var badgeETag = func() string {
	sum := sha256.Sum256(badgeSVG)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}()
