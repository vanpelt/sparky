package users

import "testing"

// Every platform subdomain must be unclaimable as a handle, because a handle is
// also a sandbox owner's name in URLs and a signup that took one would collide
// with a built-in door. This list is the deliberate duplicate of
// host.reservedNames; the two drift silently, so pin the newest entries here.
func TestPlatformSubdomainsAreNotClaimableHandles(t *testing.T) {
	for _, h := range []string{"xterm", "api", "my", "console", "login", "oidc", "new", "ctl", "signup"} {
		if ValidHandle(h) {
			t.Errorf("ValidHandle(%q) = true, want false", h)
		}
	}
	if !ValidHandle("alice") {
		t.Error("ValidHandle(\"alice\") = false, want true")
	}
}
