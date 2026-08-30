package launch

import (
	"bytes"
	"net/http"
	"time"
)

// badge serves the button image: one embedded SVG, identical for every
// repository, every branch and every viewer, with no session required.
//
// # Why it takes no parameters
//
// GitHub rewrites every image src in a comment to its own camo proxy, whose URL
// is a pure function of the origin URL. So a fixed URL is one heavily-cached
// object shared by every reader of every comment, and a per-repo or per-branch
// badge would be one camo object per comment — for information the reader is
// already looking at, since they are reading that repository's pull request.
// Worse, a badge that rendered caller-supplied text would be rendering an
// attacker-chosen string into markup on the ONE route in this package that has
// no session behind it, served back from a githubusercontent.com hostname. That
// is an injection and a spoofing primitive bought for a decoration. Static bytes
// also make the ETag a compile-time constant and a HEAD free.
//
// # Why it cannot be gated
//
// edgeauth.Require sets Cache-Control: no-store on EVERY response it produces,
// including the ones it allows (internal/edgeauth/middleware.go:50), which by
// itself defeats camo's cache and turns every reader of every comment into a
// live request. And camo fetches with no cookies at all, so it would never be
// allowed in the first place: challenge answers an uncredentialed request with
// either a 303 to an HTML login page or a 401 in text/plain, branching on
// strings.Contains(Accept, "text/html") — a substring match, so a client whose
// Accept merely mentions text/html takes the redirect branch. Camo's exact
// Accept header is not knowable from inside this repository, and it does not
// matter: both branches render as a broken image in a comment that will never
// be edited again. Hence the route is mounted outside the wrapper, the way
// internal/xterm mounts its asset subtree and internal/restapi marks its docs
// rows authPublic.
//
// This handler therefore must never learn anything about the request beyond its
// conditional headers. It reads no query, no cookie and no session, and it
// writes no Set-Cookie — which is what makes "the badge is safe to cache
// publicly for an hour" a true statement rather than a hopeful one.
func (h *Handler) badge(w http.ResponseWriter, r *http.Request) {
	head := w.Header()
	// Set before ServeContent so it is not sniffed from the extension. An SVG
	// served as anything else does not render, and an SVG served as text/html
	// would be a document rather than an image.
	head.Set("Content-Type", "image/svg+xml")
	// An hour is the deliberate middle: long enough that a busy repository's
	// readers hit camo rather than this process, short enough that a redrawn
	// badge reaches everybody the same day. The validator below makes the
	// revalidation after that hour a 304 with no body.
	head.Set("Cache-Control", "public, max-age=3600")
	head.Set("ETag", badgeETag)
	// The bytes are not compressed here, but an intermediary may compress them,
	// and a cache that stored one encoding under a key that ignored
	// Accept-Encoding would hand gzip to a client that cannot read it.
	head.Set("Vary", "Accept-Encoding")
	// The body is developer-controlled, so a sniffed content type can only ever
	// be a way to be wrong about it — and the one thing a sniffer might decide
	// an SVG is, is a document to execute script in.
	head.Set("X-Content-Type-Options", "nosniff")
	// ServeContent rather than a bare Write: it answers If-None-Match against
	// the ETag we just set, handles HEAD without a body, and honours Range —
	// three behaviours a hand-written image handler gets wrong one at a time.
	// The zero modtime deliberately suppresses Last-Modified, because there is
	// no honest value for it: the bytes are compiled into the binary, so a
	// file's mtime on the build machine says nothing a client should act on.
	http.ServeContent(w, r, "badge.svg", time.Time{}, bytes.NewReader(badgeSVG))
}
