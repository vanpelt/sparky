package sshgw

import (
	"strings"
	"testing"
)

// The `new@` door's worst failure mode is not an error — it is a silence that
// looks like one. Passing tags makes the client's ssh see a command word and
// allocate no PTY, so the shell that opens is live but prints no prompt and
// echoes nothing. The last line on screen is whatever the banner said, so the
// user concludes that thing is hung.
func TestNoTerminalNoticeExplainsTheMuteShell(t *testing.T) {
	got := noTerminalNotice("--tag hm", false, "ssh nifty-heron@ssh.catnip.sh")
	for _, want := range []string{
		`"--tag hm"`,                           // what their client read as a command
		"no PTY",                               // why
		"ssh -t ssh nifty-heron@ssh.catnip.sh", // and the fix, spelled out
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notice %q is missing %q", got, want)
		}
	}
	// Line endings must be CRLF: this goes to a client that asked for no
	// terminal, so nothing downstream translates a bare \n for it.
	for _, line := range strings.Split(strings.TrimSuffix(got, "\r\n"), "\r\n") {
		if strings.Contains(line, "\n") {
			t.Errorf("notice has a bare newline: %q", got)
		}
	}
}

// Silence in both of the ordinary cases. A user who got a terminal has nothing
// to be told, and `ssh new@host` with no words at all gets one.
func TestNoTerminalNoticeIsSilentWhenThereIsNothingWrong(t *testing.T) {
	if got := noTerminalNotice("--tag hm", true, "hint"); got != "" {
		t.Errorf("notice printed for a session that HAS a terminal: %q", got)
	}
	if got := noTerminalNotice("", false, "hint"); got != "" {
		t.Errorf("notice printed for a session that asked for nothing: %q", got)
	}
}
