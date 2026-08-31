package sshgw

// What is pinned here is the byte content of a line that gets pasted into a
// pull-request comment and then cannot be edited by anybody.
//
// That is why these assertions are equality against a whole string rather than
// a Contains: a stray space, a lost quote, a `&` that should have been `&amp;`
// or a second line where there was one are all invisible in a diff and fatal in
// a comment. The same reasoning covers the negative assertions — that a badge
// with no --ref carries no `?ref=` and no placeholder token — because a
// placeholder like BRANCH passes every validator in the tree and only fails at
// `git clone --branch BRANCH`, inside somebody else's VM, at a shell prompt.

import (
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
)

// newBadgeStack is the repo stack with the two labels `badge` needs to build a
// URL. They are set on the gateway directly because they are GatewayOptions
// fields rather than ctlops config, and the shared constructor's tweak hook
// only reaches the latter — reshaping that constructor for one command would
// touch every test in the package.
func newBadgeStack(t *testing.T) (*ctlStack, *fakeRepoStore) {
	t.Helper()
	st, store := newRepoStack(t)
	st.gw.launchSubdomain = "go"
	st.gw.xtermSubdomain = "xterm"
	return st, store
}

// checkBadgeSession holds every path to the two invariants this channel has:
// the client's terminal is in raw mode, so a bare \n leaves the cursor
// mid-line; and a handler that never calls Exit leaves the ssh client hanging
// with no output and no prompt.
func checkBadgeSession(t *testing.T, s *ctlSession) {
	t.Helper()
	if !s.exited {
		t.Error("command never called Exit; the ssh client would hang")
	}
	for stream, text := range map[string]string{"stdout": s.out.String(), "stderr": s.stderr.String()} {
		if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
			t.Errorf("%s contains a bare \\n: %q", stream, text)
		}
	}
}

// TestControlBadgeOnAHostWithoutALaunchDoor: the host fact comes before the
// argument check, so somebody who typed it wrong on a host that cannot do this
// learns what the host is instead of fixing the typo and being told the same
// thing on the second try. Every one of these argument shapes is wrong in a
// different way and all of them answer identically.
func TestControlBadgeOnAHostWithoutALaunchDoor(t *testing.T) {
	st, _ := newRepoStack(t) // no launch label, and no xterm label either
	const want = "sparkbox: launch links are not enabled on this host\r\n"
	for _, args := range [][]string{
		{"badge"},
		{"badge", "wandb/hivemind"},
		{"badge", "not a slug"},
		{"badge", "wandb/hivemind", "--ref"},
		{"badge", "--wat"},
	} {
		s := st.run(t, "alice", args...)
		if s.stderr.String() != want || s.code != 1 {
			t.Errorf("%v = exit %d, stderr %q; want exit 1 and %q", args, s.code, s.stderr.String(), want)
		}
		if s.out.Len() > 0 {
			t.Errorf("%v printed markdown on a host with no launch door: %q", args, s.out.String())
		}
		checkBadgeSession(t, s)
	}
}

// A launch door with no terminal behind it is still not a launch door: the
// button's whole payoff is a browser terminal to land in, and cmd/sparkbox
// declines to mount the door for exactly this reason, so `badge` must not print
// a link to a subdomain nothing answers on.
func TestControlBadgeNeedsABrowserTerminalToLandIn(t *testing.T) {
	st, _ := newRepoStack(t)
	st.gw.launchSubdomain = "go" // but xtermSubdomain stays empty
	s := st.run(t, "alice", "badge", "wandb/hivemind")
	if got, want := s.stderr.String(), "sparkbox: launch links are not enabled on this host\r\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if s.code != 1 {
		t.Errorf("exit = %d, want 1", s.code)
	}
	checkBadgeSession(t, s)
}

func TestControlBadgeGrammar(t *testing.T) {
	st, _ := newBadgeStack(t)
	for _, tc := range []struct {
		name     string
		args     []string
		wantErr  string
		wantExit int
	}{{
		name: "no arguments print the page that documents it", args: []string{"badge"},
		wantErr: badgeUsage, wantExit: 2,
	}, {
		name: "a bare word that is not a repository", args: []string{"badge", "hivemind"},
		wantErr: "sparkbox: \"hivemind\" is not an owner/name repository\r\n" + badgeUsage, wantExit: 2,
	}, {
		// The owner half is a GitHub login grammar, so this is refused for the
		// owner and not for the dot — repos.ValidSlug is what says so, and
		// node.js in the next case is what proves it is not a dot rule.
		name: "a slug with a hostname in it", args: []string{"badge", "github.com/wandb/hivemind"},
		wantErr:  "sparkbox: \"github.com/wandb/hivemind\" is not an owner/name repository\r\n" + badgeUsage,
		wantExit: 2,
	}, {
		name: "a ref that is an option in disguise",
		args: []string{"badge", "wandb/hivemind", "--ref=-upload-pack=evil"},
		wantErr: "sparkbox: \"-upload-pack=evil\" is not a branch or tag name " +
			"(it must start with a letter or digit, and cannot contain \"..\")\r\n",
		wantExit: 2,
	}, {
		name: "a ref that walks out of the checkout",
		args: []string{"badge", "wandb/hivemind", "--ref", "a/../../b"},
		wantErr: "sparkbox: \"a/../../b\" is not a branch or tag name " +
			"(it must start with a letter or digit, and cannot contain \"..\")\r\n",
		wantExit: 2,
	}, {
		name: "a dangling --ref", args: []string{"badge", "wandb/hivemind", "--ref"},
		wantErr: "sparkbox: --ref needs a value, e.g. --ref feat/x\r\n" + badgeUsage, wantExit: 2,
	}, {
		name: "an unknown flag", args: []string{"badge", "wandb/hivemind", "--tag", "hm"},
		wantErr: "sparkbox: unknown flag \"--tag\"\r\n" + badgeUsage, wantExit: 2,
	}, {
		name: "two repositories", args: []string{"badge", "wandb/hivemind", "wandb/sparky"},
		wantErr: "sparkbox: one repository at a time — \"wandb/hivemind\" and \"wandb/sparky\" " +
			"both look like repositories\r\n" + badgeUsage,
		wantExit: 2,
	}, {
		name: "a flag with nothing to make a button for", args: []string{"badge", "--ref", "feat/x"},
		wantErr:  "sparkbox: name the repository to make a button for, as <owner>/<name>\r\n" + badgeUsage,
		wantExit: 2,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s := st.run(t, "alice", tc.args...)
			if got := s.stderr.String(); got != tc.wantErr {
				t.Errorf("stderr = %q, want %q", got, tc.wantErr)
			}
			if got := s.out.String(); got != "" {
				t.Errorf("a refused badge printed markdown anyway: %q", got)
			}
			if s.code != tc.wantExit {
				t.Errorf("exit = %d, want %d", s.code, tc.wantExit)
			}
			checkBadgeSession(t, s)
		})
	}
}

// The line itself, byte for byte, in the two forms that will ever be pasted.
func TestControlBadgeMarkdown(t *testing.T) {
	st, store := newBadgeStack(t)
	if err := store.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"hm"}); err != nil {
		t.Fatal(err)
	}

	s := st.run(t, "alice", "badge", "wandb/hivemind", "--ref", "feat/x")
	want := `<a href="https://go.hivemind.tools/wandb/hivemind?ref=feat/x">` +
		`<img align="right" src="https://go.hivemind.tools/badge.svg" alt="Open in Sparkbox" height="28"></a>` + "\r\n"
	if got := s.out.String(); got != want {
		t.Errorf("stdout =\n%q\nwant\n%q", got, want)
	}
	if s.code != 0 {
		t.Errorf("exit = %d, stderr %q", s.code, s.stderr.String())
	}
	checkBadgeSession(t, s)

	// The float is the layout decision, pinned by name: wrapped in a
	// `<div align="right">` the button takes a line of its own above the
	// heading, floated it sits in the comment's top-right with the heading
	// beside it. And no `width`, ever — GitHub computes an injected
	// aspect-ratio from the attributes it finds, which would letterbox every
	// comment already posted the day the badge is redrawn.
	if strings.Contains(s.out.String(), "<div") {
		t.Error("the snippet wraps the button in a block; it must float so the heading flows beside it")
	}
	if strings.Contains(s.out.String(), "width=") {
		t.Error("the snippet declares a width, which pins GitHub's injected aspect-ratio in every posted comment")
	}

	// No --ref means no ref at all, and in particular never a placeholder: a
	// literal like BRANCH satisfies every validator between here and the guest
	// and only fails at the clone, in a VM belonging to whoever clicked.
	s = st.run(t, "alice", "badge", "wandb/hivemind")
	want = `<a href="https://go.hivemind.tools/wandb/hivemind">` +
		`<img align="right" src="https://go.hivemind.tools/badge.svg" alt="Open in Sparkbox" height="28"></a>` + "\r\n"
	if got := s.out.String(); got != want {
		t.Errorf("stdout =\n%q\nwant\n%q", got, want)
	}
	for _, never := range []string{"?ref=", "BRANCH", "<ref>", "{ref}"} {
		if strings.Contains(s.out.String(), never) {
			t.Errorf("a badge with no --ref carries %q anyway: %q", never, s.out.String())
		}
	}
	checkBadgeSession(t, s)
}

// A pipe has to get the snippet and nothing else, which is what makes
// `badge … | pbcopy` produce exactly the right bytes.
func TestControlBadgeExplanationStaysOffStdout(t *testing.T) {
	st, store := newBadgeStack(t)
	if err := store.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"hm"}); err != nil {
		t.Fatal(err)
	}
	s := st.run(t, "alice", "badge", "wandb/hivemind", "--ref", "feat/x")
	if n := strings.Count(s.out.String(), "\r\n"); n != 1 {
		t.Errorf("stdout is %d lines, want exactly 1: %q", n, s.out.String())
	}
	if strings.HasPrefix(s.out.String(), " ") {
		t.Errorf("stdout is indented, which makes a markdown code block: %q", s.out.String())
	}
	for _, want := range []string{
		"paste that into a pull request comment on wandb/hivemind.",
		"checked out on feat/x",
		"note: whoever clicks runs that branch's code in their own sandbox",
	} {
		if !strings.Contains(s.stderr.String(), want) {
			t.Errorf("stderr does not say %q: %q", want, s.stderr.String())
		}
	}
	checkBadgeSession(t, s)
}

// An unattached repository still gets a button. Refusing would leave the user
// with nothing, and the markdown is correct for whoever will click it — who may
// well have the attachment even when the author does not. What they get instead
// is a note naming the one command that changes it.
func TestControlBadgeForAnUnattachedRepoStillPrintsIt(t *testing.T) {
	st, _ := newBadgeStack(t)
	s := st.run(t, "alice", "badge", "wandb/hivemind")
	if !strings.Contains(s.out.String(), "https://go.hivemind.tools/wandb/hivemind") {
		t.Fatalf("no button was printed: %q", s.out.String())
	}
	if s.code != 0 {
		t.Fatalf("exit = %d, stderr %q", s.code, s.stderr.String())
	}
	// The note must describe what the door ACTUALLY does with an unattached
	// repository, which is refuse and teach — not build an empty sandbox. This
	// is the one command whose output people paste into a comment and keep, so
	// a sentence that misdescribes the feature outlives the misunderstanding.
	want := "note: nothing of yours is attached to wandb/hivemind, so clicking the button shows you\r\n" +
		"      the \"attach it first\" screen rather than building anything. attach it:\r\n" +
		"      ssh ctl@hivemind.tools repo add wandb/hivemind --tag hm\r\n" +
		"      attachments are per person — whoever clicks needs their own either way.\r\n"
	if !strings.Contains(s.stderr.String(), want) {
		t.Errorf("stderr does not carry the attach note:\n%q", s.stderr.String())
	}
	if strings.Contains(s.stderr.String(), "sandbox with no checkout") {
		t.Error("the note still claims the button builds an empty sandbox; the launch door refuses instead")
	}
	checkBadgeSession(t, s)
}

// The emitting half of the launch door's normalize rule. Without it a button
// carrying ?ref=main against an attachment whose default is already main fails
// to match the sandbox already sitting on main, and every click makes another
// one.
func TestControlBadgeDropsARefThatIsAlreadyTheAttachmentDefault(t *testing.T) {
	st, store := newBadgeStack(t)
	if err := store.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind", Ref: "main"}, []string{"hm"}); err != nil {
		t.Fatal(err)
	}
	s := st.run(t, "alice", "badge", "wandb/hivemind", "--ref", "main")
	if strings.Contains(s.out.String(), "?ref=") {
		t.Errorf("the attachment's own branch survived into the link: %q", s.out.String())
	}
	want := "note: main is already the attachment's own branch, so the link leaves it out —\r\n" +
		"      a sandbox on it matches instead of a second one being created.\r\n"
	if !strings.Contains(s.stderr.String(), want) {
		t.Errorf("nothing said the ref was dropped:\n%q", s.stderr.String())
	}
	checkBadgeSession(t, s)

	// A different ref on the same attachment is not the default, so it stays —
	// and the comparison is byte for byte, because feat/X and feat/x are two
	// branches and nothing in this tree folds a ref.
	s = st.run(t, "alice", "badge", "wandb/hivemind", "--ref", "Main")
	if !strings.Contains(s.out.String(), "?ref=Main") {
		t.Errorf("a ref that only case-matches the default was dropped: %q", s.out.String())
	}
	checkBadgeSession(t, s)
}

// The `default` tag is stamped on every sandbox this host creates, so a button
// for a repo carried on it also clones everything else the author has there —
// into a VM a stranger asked for. `repo add` prints this at attach time; this
// is the moment it stops being only the author's problem.
// Every badge warns about `default`, including one for a narrowly tagged
// attachment — and that is the correction, not an over-warning.
//
// The note used to fire only when the ATTACHMENT carried `default`, which read
// as careful and was wrong: ctlops.Ops.Create stamps `default` on every sandbox
// it builds, and repos.ReposForSandbox joins sandbox_tags to repo_tags, so the
// box a button makes always also clones whatever the CLICKER keeps on
// `default`. An author whose attachment was tagged `hm` therefore got silence
// and shipped a button they believed produced a hivemind-only sandbox.
func TestControlBadgeWarnsAboutTheDefaultTag(t *testing.T) {
	st, store := newBadgeStack(t)
	if err := store.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"default"}); err != nil {
		t.Fatal(err)
	}
	s := st.run(t, "alice", "badge", "wandb/hivemind")
	want := "note: every sandbox carries the `default` tag, so the button's sandbox clones\r\n" +
		"      this repo AND everything the person clicking has on `default`.\r\n" +
		"      that is their attachment list, not yours, and nothing narrows it away.\r\n"
	if !strings.Contains(s.stderr.String(), want) {
		t.Errorf("nothing warned about the default tag:\n%q", s.stderr.String())
	}
	checkBadgeSession(t, s)

	// The case the old guard got wrong: tagged `hm`, and the consequence still
	// holds, so the note must still print.
	if err := store.PutRepo("alice", repos.Repo{Slug: "wandb/sparky"}, []string{"hm"}); err != nil {
		t.Fatal(err)
	}
	s = st.run(t, "alice", "badge", "wandb/sparky")
	if !strings.Contains(s.stderr.String(), want) {
		t.Errorf("a narrowly tagged attachment was not warned about `default`:\n%q", s.stderr.String())
	}
	// And it must not offer a remedy that cannot work: no command can strip
	// `default` from a sandbox, so suggesting `--tag <t>` here would be advice
	// that fails silently.
	if strings.Contains(s.stderr.String(), "narrow it with") {
		t.Errorf("the note still offers a narrowing remedy that does not remove `default`:\n%q", s.stderr.String())
	}
	checkBadgeSession(t, s)
}

// The casing that goes into the comment is the one the user wrote down when
// they attached the repository, not whatever they typed just now — the same
// rule `repo add` follows when it echoes the stored slug back.
func TestControlBadgeUsesTheStoredCasing(t *testing.T) {
	st, store := newBadgeStack(t)
	if err := store.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"hm"}); err != nil {
		t.Fatal(err)
	}
	s := st.run(t, "alice", "badge", "WandB/HiveMind")
	if !strings.Contains(s.out.String(), "/wandb/hivemind\"") {
		t.Errorf("the URL did not use the stored casing: %q", s.out.String())
	}
	if strings.Contains(s.stderr.String(), "nothing of yours is attached") {
		t.Errorf("a case-different slug was treated as unattached:\n%q", s.stderr.String())
	}
	checkBadgeSession(t, s)
}

// Somebody else's attachment of the same repository is not the caller's. It
// must read exactly like never having attached it at all, or the note becomes a
// way to ask this host who has what attached.
func TestControlBadgeIgnoresAnotherOwnersAttachment(t *testing.T) {
	st, store := newBadgeStack(t)
	if err := store.PutRepo("mallory", repos.Repo{Slug: "wandb/hivemind"}, []string{"hm"}); err != nil {
		t.Fatal(err)
	}
	s := st.run(t, "alice", "badge", "wandb/hivemind")
	if !strings.Contains(s.stderr.String(), "nothing of yours is attached to wandb/hivemind") {
		t.Errorf("stderr did not read as unattached:\n%q", s.stderr.String())
	}
	checkBadgeSession(t, s)
}

// The flag dialect is parseRepoAdd's, unchanged, so `--ref x`, `--ref=x` and
// `--branch` mean here exactly what they mean one command over.
func TestParseBadge(t *testing.T) {
	for _, tc := range []struct {
		name           string
		args           []string
		wantSlug, want string
	}{
		{"just a slug", []string{"wandb/hivemind"}, "wandb/hivemind", ""},
		{"a detached ref", []string{"wandb/hivemind", "--ref", "feat/x"}, "wandb/hivemind", "feat/x"},
		{"an attached ref", []string{"--ref=feat/x", "wandb/hivemind"}, "wandb/hivemind", "feat/x"},
		{"the --branch spelling", []string{"wandb/hivemind", "--branch", "v1"}, "wandb/hivemind", "v1"},
		{"the last one wins", []string{"wandb/hivemind", "--ref", "a", "--ref=b"}, "wandb/hivemind", "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slug, ref, err := parseBadge(tc.args)
			if err != nil {
				t.Fatalf("parseBadge(%v) errored: %v", tc.args, err)
			}
			if slug != tc.wantSlug || ref != tc.want {
				t.Errorf("parseBadge(%v) = %q, %q; want %q, %q", tc.args, slug, ref, tc.wantSlug, tc.want)
			}
		})
	}
}

// `badge` is dispatched, so it has to be findable: the help is this channel's
// only discovery surface, and TestControlUsageListsEveryCommand covers the
// index while this covers the page a person is sent to.
//
// That page is `badge`'s own now. It used to be the repos page, and the cost
// was that a forgotten slug answered with forks, ref overrides and sync rules —
// so the page a refusal prints is pinned here to the one about the button.
func TestBadgeHasItsOwnHelpPage(t *testing.T) {
	page, ok := helpPage("badge", false)
	if !ok {
		t.Fatal("help badge reaches no topic")
	}
	if page != badgeUsage {
		t.Errorf("help badge does not print the badge page:\n%s", page)
	}
	if !strings.Contains(page, "badge <owner>/<name> [--ref <r>]") {
		t.Errorf("the badge page has no synopsis:\n%s", page)
	}
	// The two paragraphs that belong to `repo`, checked by the words that are
	// unique to them: a refusal on this verb must not print either.
	for _, stray := range []string{"fork cuda-12", "repos sync", "--write"} {
		if strings.Contains(page, stray) {
			t.Errorf("the badge page still carries the repos page's %q", stray)
		}
	}
	// And the pointer the other way stays, because `badge` is a thing you do
	// with an attachment and `help repos` is where somebody looking for it will
	// be standing.
	if repos, _ := helpPage("repos", false); !strings.Contains(repos, "help badge") {
		t.Error("the repos page no longer points at the badge page")
	}
}
