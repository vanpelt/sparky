package sshgw

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseSecretSetRefusesAValueOnTheCommandLine is the security property of
// this grammar. `secret set NAME VALUE` is what somebody will try first, and
// accepting it would put a long-lived credential in their shell history, in
// their local ssh process's argv, and in anything between that logs commands.
// The refusal has to name the pipe, or the user just tries again with quotes.
func TestParseSecretSetRefusesAValueOnTheCommandLine(t *testing.T) {
	_, _, err := parseSecretSet([]string{"CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-secret"})
	if err == nil {
		t.Fatal("a value on the command line was accepted")
	}
	if !strings.Contains(err.Error(), "stdin") {
		t.Errorf("refusal %q does not tell the user to pipe it in", err)
	}
	if strings.Contains(err.Error(), "sk-ant-oat-secret") {
		t.Errorf("the refusal echoes the credential back: %q", err)
	}
}

func TestParseSecretSet(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantName string
		wantTags []string
		wantErr  bool
	}{
		{"bare name", []string{"TOKEN"}, "TOKEN", nil, false},
		{"one tag", []string{"TOKEN", "--tag", "ci"}, "TOKEN", []string{"ci"}, false},
		{"repeated", []string{"TOKEN", "--tag", "ci", "--tag", "web"}, "TOKEN", []string{"ci", "web"}, false},
		{"equals form", []string{"TOKEN", "--tag=ci"}, "TOKEN", []string{"ci"}, false},
		{"comma list stays whole for NormalizeTags to split",
			[]string{"TOKEN", "--tags", "ci,web"}, "TOKEN", []string{"ci,web"}, false},
		{"dangling flag", []string{"TOKEN", "--tag"}, "", nil, true},
		{"no name", nil, "", nil, true},
	} {
		gotName, gotTags, err := parseSecretSet(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if gotName != tc.wantName || !reflect.DeepEqual(gotTags, tc.wantTags) {
			t.Errorf("%s: got %q %v, want %q %v", tc.name, gotName, gotTags, tc.wantName, tc.wantTags)
		}
	}
}

// Every CLI that prints a token ends with a newline, so a pipe always delivers
// one. Storing it would trip the store's own newline check, which the user
// would read as "my token is invalid" rather than "your pipe added a byte".
func TestCleanSecretValueTrimsWhatAPipeAdds(t *testing.T) {
	got, err := cleanSecretValue("sk-ant-oat-abc123\n", "TOKEN")
	if err != nil {
		t.Fatalf("cleanSecretValue: %v", err)
	}
	if got != "sk-ant-oat-abc123" {
		t.Errorf("got %q, want the value with no trailing newline", got)
	}
}

func TestCleanSecretValueRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty", "", "stdin"},
		{"whitespace only", "  \n\t ", "stdin"},
		{"multi-line", "line one\nline two", "multiple lines"},
	} {
		_, err := cleanSecretValue(tc.in, "TOKEN")
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %q, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestResyncNote(t *testing.T) {
	if got := resyncNote(nil); got != "" {
		t.Errorf("empty fan-out rendered %q, want nothing", got)
	}
	if got := resyncNote([]string{"one"}); !strings.Contains(got, "one") {
		t.Errorf("single = %q", got)
	}
	got := resyncNote([]string{"one", "two"})
	if !strings.Contains(got, "2 sandboxes") || !strings.Contains(got, "one, two") {
		t.Errorf("plural = %q", got)
	}
}

func TestSplitDryRun(t *testing.T) {
	for _, tc := range []struct {
		in       []string
		wantRest []string
		wantDry  bool
	}{
		{[]string{"wandb"}, []string{"wandb"}, false},
		{[]string{"wandb", "--dry-run"}, []string{"wandb"}, true},
		{[]string{"--dry-run", "wandb"}, []string{"wandb"}, true},
		{[]string{"-n", "a", "b"}, []string{"a", "b"}, true},
	} {
		rest, dry := splitDryRun(tc.in)
		if !reflect.DeepEqual(rest, tc.wantRest) || dry != tc.wantDry {
			t.Errorf("splitDryRun(%v) = %v/%v, want %v/%v", tc.in, rest, dry, tc.wantRest, tc.wantDry)
		}
	}
}

// The token for an org sync must arrive on stdin for the same reason a secret
// value does, and with more force: it is a read:org credential for a whole
// organization. The usage text is the only place that can teach the pipe.
func TestUserUsageTeachesThePipe(t *testing.T) {
	if !strings.Contains(userUsage, "gh auth token |") {
		t.Error("the user usage does not show how to supply the token")
	}
	if !strings.Contains(userUsage, "never operators") {
		t.Error("the user usage does not say provisioned accounts are ordinary users")
	}
}
