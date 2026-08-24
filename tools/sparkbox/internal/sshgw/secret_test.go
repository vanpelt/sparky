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

// Blank lines around the value are the same mistake as a trailing newline, and
// several tools pad their output that way.
func TestCleanSecretValueDropsBlankLines(t *testing.T) {
	got, err := cleanSecretValue("\n\n  sk-ant-oat-abc123  \n\n", "TOKEN")
	if err != nil {
		t.Fatalf("cleanSecretValue: %v", err)
	}
	if got != "sk-ant-oat-abc123" {
		t.Errorf("got %q, want the one line of content", got)
	}
}

// A banner around the token is `claude setup-token`'s actual output, and this
// is the command the whole ctl secret channel exists to be the far end of.
//
// The credential is found by its own shape, so nothing here depends on where in
// the banner it sits, what the surrounding sentences say, or whether the tool
// paints them. Note the token is also mentioned INSIDE a sentence: the same
// value twice is one credential, not an ambiguity.
func TestCleanSecretValuePicksTheCredentialOutOfABanner(t *testing.T) {
	const token = "sk-ant-oat01-Fk3nQ7pR2sVt8wXy1zA4bC6dE9gH0jK5mN7pQ2sT4vW6xY8zB1cD3fG5hJ7kL9nP1rS3tV5wX7yZ9aC2eF4gH6jM8"
	out := "\x1b[1mWelcome to Claude Code\x1b[0m v2.1.241\n" +
		"   ___ __    ___ __ __ ___  ___\n\n" +
		"Your OAuth token (valid for 1 year):\n\n" +
		"  \x1b[32m" + token + "\x1b[0m  \n\n" +
		"Store " + token + " securely. It will not be shown again.\n"

	got, err := cleanSecretValue(out, "CLAUDE_CODE_OAUTH_TOKEN")
	if err != nil {
		t.Fatalf("setup-token's own output was refused: %v", err)
	}
	if got != token {
		t.Errorf("got %q, want the token with the banner and the styling gone", got)
	}
}

// The shapes are matched, not the tools. A GitHub PAT announced by some other
// program has to come out of that program's chatter too, or this would be the
// one-off for one CLI that the doc comment says it is not.
func TestCleanSecretValueFindsEachCredentialShape(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"anthropic oauth", "sk-ant-oat01-" + strings.Repeat("aB3", 20)},
		{"anthropic api key", "sk-ant-api03-" + strings.Repeat("cD4", 20)},
		{"github fine-grained pat", "github_pat_" + strings.Repeat("eF5", 12)},
		{"github classic", "ghp_" + strings.Repeat("gH6", 14)},
		{"openai project key", "sk-proj-" + strings.Repeat("iJ7", 20)},
	} {
		out := "some tool being helpful\n\nhere it is: " + tc.value + "\n\nkeep it safe\n"
		got, err := cleanSecretValue(out, "TOKEN")
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.value {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.value)
		}
	}
}

// The other side of the wrap check: a closing sentence, a rule, or a line of
// ASCII art under the token is not a wrapped tail, and treating one as though
// it were would refuse the ordinary banner this feature exists to accept.
func TestCleanSecretValueDoesNotMistakeDecorationForAWrap(t *testing.T) {
	const token = "sk-ant-oat01-" + "aB3aB3aB3aB3aB3aB3aB3aB3aB3aB3aB3aB3"
	for _, under := range []string{
		"Storeitsecurely",             // one long word, no digits
		"________________________",    // a rule
		"____----____----____----",    // the bottom of a logo
		"It will not be shown again.", // an ordinary sentence
	} {
		got, err := cleanSecretValue("Your token:\n"+token+"\n"+under+"\n", "TOKEN")
		if err != nil {
			t.Errorf("%q under the token was read as a wrap: %v", under, err)
			continue
		}
		if got != token {
			t.Errorf("%q under the token: got %q", under, got)
		}
	}
}

// A value nobody stamps a recognisable prefix on stays acceptable on its own
// line. Most secrets are like this — a connection string, a webhook URL, a
// password — and requiring a known shape would break every one of them.
func TestCleanSecretValueStillTakesAnUnrecognisableValue(t *testing.T) {
	got, err := cleanSecretValue("postgres://u:p@host:5432/db\n", "DATABASE_URL")
	if err != nil {
		t.Fatalf("cleanSecretValue: %v", err)
	}
	if got != "postgres://u:p@host:5432/db" {
		t.Errorf("got %q, want the line verbatim", got)
	}
}

// The refusals. Each is a case where storing something would mean storing a
// credential the user did not pipe in, which is worse than the pipe not
// working: they all have to fail closed and point at the prompt.
func TestCleanSecretValueRefusesRatherThanGuess(t *testing.T) {
	const tokenA = "sk-ant-oat01-" + "aB3aB3aB3aB3aB3aB3aB3aB3aB3aB3aB3aB3"
	const tokenB = "sk-ant-oat01-" + "zY9zY9zY9zY9zY9zY9zY9zY9zY9zY9zY9zY9"
	for _, tc := range []struct{ name, in, want string }{
		{
			// Two genuinely different credentials: an example beside the real
			// one, or two tools' output concatenated.
			"two different credentials", "here is one: " + tokenA + "\nand another: " + tokenB,
			"refuses to guess",
		},
		{
			// Prose only. Nothing here is a credential, so there is nothing to
			// pick and the banner must not be stored as if it were.
			"a banner with no credential in it", "Signed in as alice\nNothing to see here",
			"none of them holds anything shaped like a credential",
		},
		{
			// A terminal broke the token in two, so the second line is the
			// rest of it. Storing the first half would be storing a
			// plausible-looking wrong value — the single outcome this whole
			// design is arranged to avoid.
			"a wrapped credential", "Your token:\n" + tokenA + "\nqR7sT2vW9xY4zB6cD1fG3hJ5",
			"broken across two lines",
		},
	} {
		got, err := cleanSecretValue(tc.in, "CLAUDE_CODE_OAUTH_TOKEN")
		if err == nil {
			t.Errorf("%s: accepted, storing %q", tc.name, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %q, want it to mention %q", tc.name, err, tc.want)
		}
		if !strings.Contains(err.Error(), "ssh -t") {
			t.Errorf("%s: refusal %q does not point at the prompt", tc.name, err)
		}
		if strings.Contains(err.Error(), tokenA) || strings.Contains(err.Error(), tokenB) {
			t.Errorf("%s: the refusal echoes a credential back: %q", tc.name, err)
		}
	}
}

func TestCleanSecretValueRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty", "", "stdin"},
		{"whitespace only", "  \n\t ", "stdin"},
		{"two lines of prose", "line one\nline two", "shaped like a credential"},
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
