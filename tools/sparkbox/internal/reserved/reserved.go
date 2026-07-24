// Package reserved is the single list of names the platform has claimed, and
// the only place that decides whether a name is claimed.
//
// Three different things end up as a hostname under the zone — a sandbox name
// (its default subdomain), a route subdomain, and a user handle — and each used
// to carry its own hand-copied list. They drifted, which is the predictable
// outcome and not an interesting one; worse, routes.ValidSubdomain never had a
// list at all, so a route could be created at `console` or `api`, stored,
// listed, and then never served, because reserved dispatch runs before the
// route lookup and wins silently.
//
// This package imports nothing but the standard library, which is what lets
// host, routes, users and cmd/sparkbox all depend on it without a cycle —
// internal/xterm already imports host, so the dependency could never have run
// the other way.
//
// Adding a name here is cheap and safe: validation runs at create and rename
// time only, so nothing already on disk is retroactively broken, and a name
// with no handler mounted at it is refused rather than shadowed. Removing one
// is the direction that needs thought.
package reserved

import "strings"

// names are claimed outright. A name here may not be a sandbox, a route
// subdomain, or a user handle.
//
// The first two groups are load-bearing — something already answers there, so a
// collision is a name that silently goes dark. The rest are held in advance:
// cheaper to reserve now than to discover that the obvious hostname for a new
// surface was taken by a sandbox, and several would be actively dangerous in a
// stranger's hands on a domain that also serves a sign-in page.
var names = map[string]bool{
	// Doors that exist today. Each is a --*-subdomain flag or an SSH gateway
	// username; cmd/sparkbox warns at startup if one is relabelled onto a name
	// something already answers for.
	"console": true, // operator console      (--console-subdomain)
	"my":      true, // user console          (--user-console-subdomain)
	"api":     true, // REST API + /docs      (--api-subdomain)
	"login":   true, // browser sign-in       (--login-subdomain)
	"oidc":    true, // OIDC issuer + JWKS    (--oidc-subdomain)
	"xterm":   true, // browser terminals     (--xterm-subdomain; see suffixes)
	"new":     true, // ssh new@<domain>      — create a sandbox
	"ctl":     true, // ssh ctl@<domain>      — control plane
	"signup":  true, // ssh signup@<domain>   — register
	"node":    true, // ssh node@<domain>     — a node joins the fleet

	// Identity of the platform itself.
	"sparkbox":      true,
	"root":          true,
	"admin":         true,
	"administrator": true,

	// Surfaces we are plausibly one decision away from wanting.
	"docs": true, "portal": true, "dashboard": true, "status": true,
	"health": true, "metrics": true, "logs": true, "billing": true,
	"account": true, "accounts": true, "settings": true, "profile": true,
	"auth": true, "sso": true, "signin": true, "signout": true,
	"logout": true, "register": true, "verify": true, "invite": true,
	"invites": true, "help": true, "support": true, "blog": true,

	// Names whose familiarity is the problem: on a zone that also serves a
	// sign-in page, a stranger holding `www` or `mail` is a phishing primitive,
	// and the mail and nameserver labels are the ones a mail client or resolver
	// will try on its own.
	"www": true, "mail": true, "email": true, "webmail": true,
	"smtp": true, "imap": true, "pop": true, "mx": true,
	"ns": true, "ns1": true, "ns2": true, "dns": true, "ftp": true,
	"autoconfig": true, "autodiscover": true,

	// Conventional operational and abuse-reporting names (RFC 2142's spirit).
	"security": true, "abuse": true, "postmaster": true,
	"webmaster": true, "hostmaster": true, "noc": true,

	// Infrastructure words that would read as the platform's own.
	"cdn": true, "static": true, "assets": true, "media": true,
	"registry": true, "storage": true, "backup": true,
	"gateway": true, "edge": true, "proxy": true, "vpn": true,
}

// suffixes are claimed as trailing segments: any name ENDING in one belongs to
// a built-in handler, however it begins. Today that is the browser terminal,
// which serves sandbox "demo" at "demo-xterm.<domain>" — so a sandbox or route
// actually named "web-xterm" would be answered by the terminal handler and be
// unreachable as itself.
//
// The literal is the default label rather than whatever --xterm-subdomain was
// set to, because nothing here can see a flag. A relabelled deployment is
// covered by cmd/sparkbox's startup warning instead, which scans what is
// already on disk against the configured label.
var suffixes = []string{"-xterm"}

// Name reports whether s is claimed by the platform, either outright or by
// suffix. It is the whole public surface of this package: sandbox names, route
// subdomains and user handles all ask exactly this question.
//
// A dotted route subdomain is checked as a whole, which matches how the edge
// dispatches: `s.reserved` is an exact-match lookup on the entire subdomain, so
// "api.myvm" is an ordinary route and only a bare "api" collides.
func Name(s string) bool {
	s = strings.ToLower(s)
	if names[s] {
		return true
	}
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// All returns every outright-reserved name, for tests and for anything that
// wants to show an operator the list. The order is unspecified.
func All() []string {
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	return out
}

// Suffixes returns the claimed trailing segments, "-xterm" among them.
func Suffixes() []string { return append([]string(nil), suffixes...) }
