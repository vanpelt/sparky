package host

import "testing"

// A sandbox's name is its default subdomain, so a sandbox called "xterm" or
// "api" would sit on top of a platform surface — and the reserved-suffix and
// reserved-subdomain dispatch on the proxy edge would then make that sandbox
// permanently unreachable over HTTP. Reject the name at create time instead.
// This list is the deliberate duplicate of users.reserved.
func TestPlatformSubdomainsAreReservedNames(t *testing.T) {
	for _, n := range []string{"xterm", "api", "my", "console", "login", "oidc", "new", "ctl", "signup"} {
		if !reservedNames[n] {
			t.Errorf("reservedNames[%q] = false, want true", n)
		}
	}
}

// The browser terminal for "demo" is served at demo-xterm.<domain>, and the
// edge dispatches every name ending that way to the terminal handler before it
// ever looks at a route. So a sandbox actually NAMED "web-xterm" would have its
// own front door answered by the terminal handler — nothing about the request
// distinguishes the two — and would be reachable only over SSH. The name is the
// last point where that is still fixable.
func TestTerminalSuffixIsReserved(t *testing.T) {
	for _, n := range []string{"xterm", "web-xterm", "fuzzy-otter-2-xterm"} {
		if !reservedName(n) {
			t.Errorf("reservedName(%q) = false, want true", n)
		}
	}
	// The suffix is a whole segment, not a substring: these are ordinary names
	// and refusing them would be a silent, unexplained rejection.
	for _, n := range []string{"xtermite", "myxterm", "xterm-web", "fuzzy-otter-2"} {
		if reservedName(n) {
			t.Errorf("reservedName(%q) = true, want false", n)
		}
	}
}
