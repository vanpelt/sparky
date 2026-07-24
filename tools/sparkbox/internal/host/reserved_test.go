package host

import "testing"

// A sandbox's name is its default subdomain, so a sandbox called "xterm" or
// "api" would sit on top of a platform surface — and the reserved-subdomain and
// reserved-suffix dispatch on the proxy edge would then make that sandbox
// permanently unreachable over HTTP. Reject the name at create time instead.
//
// The list itself lives in internal/reserved and is exercised there; what this
// pins is that sandbox naming actually consults it, in both directions.
func TestSandboxNamesConsultTheReservedList(t *testing.T) {
	for _, n := range []string{"xterm", "api", "my", "console", "login", "oidc", "new", "ctl", "signup", "node", "docs", "web-xterm"} {
		if !reservedName(n) {
			t.Errorf("reservedName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"crafty-axolotl", "xtermite", "admin-panel", "demo"} {
		if reservedName(n) {
			t.Errorf("reservedName(%q) = true, want false", n)
		}
	}
}
