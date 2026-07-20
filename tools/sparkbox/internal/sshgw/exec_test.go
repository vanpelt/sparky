package sshgw

import "testing"

// The `new@` door consumes its arguments as tags, so they must not also reach
// the guest as a command — see execsCommand.
func TestExecsCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		tagsOnly bool
		want     bool
	}{
		{"existing sandbox with a command runs it", "uname -a", false, true},
		{"existing sandbox with no command gets a shell", "", false, false},
		{"new@ with tags gets a shell, not `claude`", "claude", true, false},
		{"new@ with a tag that isn't a binary still gets a shell", "ml prod", true, false},
		{"new@ with no tags gets a shell", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := execsCommand(tc.raw, tc.tagsOnly); got != tc.want {
				t.Errorf("execsCommand(%q, %v) = %v, want %v", tc.raw, tc.tagsOnly, got, tc.want)
			}
		})
	}
}

// `new+<name>` in the username is the only unambiguous channel for a requested
// sandbox name — see splitNewName.
func TestSplitNewName(t *testing.T) {
	for _, tc := range []struct {
		user      string
		routeTo   string
		requested string
	}{
		{"new+myvm", "new", "myvm"},
		{"new+gentle-ferret", "new", "gentle-ferret"},
		{"new", "new", ""},
		{"gentle-ferret", "gentle-ferret", ""},
		{"ctl", "ctl", ""},
		// A '+' that isn't the new@ door routes as-is; Create rejects it later.
		{"somebox+x", "somebox+x", ""},
		// Degenerate: `new+` asks for the empty name, which falls back to a
		// generated one rather than erroring.
		{"new+", "new", ""},
	} {
		t.Run(tc.user, func(t *testing.T) {
			routeTo, requested := splitNewName(tc.user)
			if routeTo != tc.routeTo || requested != tc.requested {
				t.Errorf("splitNewName(%q) = (%q, %q), want (%q, %q)",
					tc.user, routeTo, requested, tc.routeTo, tc.requested)
			}
		})
	}
}
