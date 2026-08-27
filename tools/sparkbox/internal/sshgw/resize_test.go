package sshgw

// Tests for `ctl resize` argument handling and the help listing.

import (
	"strings"
	"testing"
)

func TestParseSizeMB(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"25G", 25600},
		{"25GB", 25600},
		{"25g", 25600},
		{"25gb", 25600},
		{"512M", 512},
		{"512MB", 512},
		{" 25G ", 25600},
		{"25", 25600}, // bare numbers are GB, the unit a disk size is named in
		{"1", 1024},
	} {
		got, err := parseSizeMB(tc.in)
		if err != nil {
			t.Errorf("parseSizeMB(%q) errored: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSizeMB(%q) = %d MB, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseSizeMBRejects(t *testing.T) {
	for _, tc := range []struct{ in, why string }{
		{"", "empty"},
		{"big", "not a number"},
		{"25T", "unsupported unit parses as garbage rather than silently meaning GB"},
		{"0", "zero"},
		{"-5G", "negative"},
		{"99999G", "past the fat-finger ceiling"},
	} {
		if got, err := parseSizeMB(tc.in); err == nil {
			t.Errorf("parseSizeMB(%q) = %d, want an error (%s)", tc.in, got, tc.why)
		}
	}
}

// TestControlUsageListsEveryCommand keeps `ctl help` honest: the help is the
// only discovery surface for this channel, so a command that dispatches but is
// undocumented is invisible. It reads the whole surface — the index plus every
// page — because that is where the detail lives now.
func TestControlUsageListsEveryCommand(t *testing.T) {
	surface := helpSurface()
	for _, cmd := range []string{
		"ls", "pause", "archive", "restore", "resize", "rm", "rename", "tags",
		"pin", "unpin", "snapshot", "fork", "schedule", "whoami", "keys",
		"passkey", "email", "share", "session-token", "invite", "help",
		"checkpoint", "github", "node", "secret", "user", "sessions",
	} {
		if !strings.Contains(surface, "  "+cmd) {
			t.Errorf("ctl help does not document %q", cmd)
		}
	}
}

// TestHelpReachesEveryTopicByCommandName: `help <thing you typed>` has to work
// for the command names, not only for the topic headings — that is the whole
// point of the aliases.
func TestHelpReachesEveryTopicByCommandName(t *testing.T) {
	for _, word := range []string{
		"ls", "rename", "rm", "tags", "pin", "sessions", "resize", "checkpoint",
		"secret", "repo", "snapshot", "fork", "schedule", "share",
		"session-token", "whoami", "keys", "github", "passkey", "email",
		"invite", "user", "node",
	} {
		if _, ok := helpPage(word, true); !ok {
			t.Errorf("help %q reaches no topic", word)
		}
	}
}
