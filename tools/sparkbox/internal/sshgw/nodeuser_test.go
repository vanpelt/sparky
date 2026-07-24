package sshgw

import (
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/reserved"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

// The fleet door is a username, not an address. cmd/sparkbox mints a front-door
// IPv6 and publishes a public DNS record for every entry in ReservedUsers, and
// resolveDoor maps a connection back to a door by destination IP — so a NodeUser
// that joined the slice would publish node.<domain> and let anyone reach the
// fleet control door by dialing it, bypassing the username check entirely.
func TestNodeUserGetsNoFrontDoor(t *testing.T) {
	for _, u := range ReservedUsers {
		if u == NodeUser {
			t.Fatalf("NodeUser %q is in ReservedUsers, so a front door is minted for it", u)
		}
	}
}

// The name has to be claimed even though it mints no door: a sandbox, route or
// handle called "node" would sit on top of the door the fleet dials.
func TestNodeNameIsClaimed(t *testing.T) {
	if !reserved.Name(NodeUser) {
		t.Errorf("reserved.Name(%q) = false — the fleet door answers there", NodeUser)
	}
	if routes.ValidSubdomain(NodeUser) {
		t.Errorf("routes.ValidSubdomain(%q) = true, want false", NodeUser)
	}
}
