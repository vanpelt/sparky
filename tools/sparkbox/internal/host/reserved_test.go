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
