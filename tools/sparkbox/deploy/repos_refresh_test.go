package deploy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The refresh half of sparkbox-repos, driven for real.
//
// These tests run the installed worker against a tree holding actual git
// repositories, because the property under test is not "the script mentions
// --ff-only" — it is that a checkout somebody has work in comes out the other
// side with that work still in it. A string assertion cannot tell those apart,
// and this worker runs as root, unattended, on a filesystem somebody is using.
//
// The remote is a bare repository on local disk, so "fetch" is real git talking
// to a real remote with no network in the picture.

// repoWorld is a guest tree, a bare remote, a checkout of it, and the two stubs
// (ip, curl) the worker needs to find a gateway and a manifest outside a VM.
type repoWorld struct {
	t        *testing.T
	root     string
	remote   string
	checkout string
	env      []string
}

func newRepoWorld(t *testing.T) *repoWorld {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git in PATH")
	}
	root := fakeGuestTree(t, true)
	installGuestPayload(t, root)

	stub := filepath.Join(root, ".stub")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(stub, "ip"), `#!/bin/sh
[ "$1" = -4 ] && echo "default via 10.0.0.1 dev eth0"
`)
	// Serves $SPARKBOX_TEST_MANIFEST for the one GET the worker makes, in both
	// call shapes: the -w '%{http_code}' probe and the -o fetch.
	writeExecutable(t, filepath.Join(stub, "curl"), `#!/bin/sh
out=; code=0
while [ $# -gt 0 ]; do
	case "$1" in
		-o) out=$2; shift 2 ;;
		-w) code=1; shift 2 ;;
		*) shift ;;
	esac
done
if [ -f "$SPARKBOX_TEST_MANIFEST" ]; then
	if [ -n "$out" ]; then cp "$SPARKBOX_TEST_MANIFEST" "$out"; else cat "$SPARKBOX_TEST_MANIFEST"; fi
	[ "$code" = 1 ] && printf '200'
	exit 0
fi
[ "$code" = 1 ] && printf '404'
exit 22
`)

	w := &repoWorld{
		t:        t,
		root:     root,
		remote:   filepath.Join(root, "remote.git"),
		checkout: filepath.Join(root, "home/sparky/hivemind"),
		// Hermetic git: the developer's own config must not decide whether a
		// fast-forward happens (merge.ff=false and pull.rebase both would), and
		// an identity has to exist for the commits these tests make.
		env: append(os.Environ(),
			"PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"),
			"GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_CONFIG_SYSTEM="+os.DevNull,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"SPARKBOX_REPOS_ROOT="+root,
			"SPARKBOX_CMDLINE="+filepath.Join(root, "cmdline"),
			"SPARKBOX_TEST_MANIFEST="+filepath.Join(root, "manifest.json"),
		),
	}

	w.git(root, "init", "-q", "--bare", "-b", "main", w.remote)
	seed := t.TempDir()
	w.git(root, "clone", "-q", w.remote, seed)
	w.write(filepath.Join(seed, "a.txt"), "one\n")
	w.git(seed, "add", "-A")
	w.git(seed, "commit", "-qm", "one")
	w.git(seed, "push", "-q", "origin", "main")
	w.git(seed, "switch", "-qc", "feat/x")
	w.write(filepath.Join(seed, "b.txt"), "two\n")
	w.git(seed, "add", "-A")
	w.git(seed, "commit", "-qm", "two")
	w.git(seed, "push", "-q", "-u", "origin", "feat/x")

	w.git(root, "clone", "-q", w.remote, w.checkout)
	w.manifest("")
	w.fresh(false)
	return w
}

func (w *repoWorld) git(dir string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = w.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (w *repoWorld) write(path, body string) {
	w.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// manifest points the sandbox's one attachment at ref ("" = the attachment
// pins nothing).
func (w *repoWorld) manifest(ref string) {
	w.t.Helper()
	w.write(filepath.Join(w.root, "manifest.json"),
		`{"repos":[{"host":"github.com","slug":"wandb/hivemind","ref":"`+ref+`","path":"","access":"read"}]}`)
}

// fresh writes the kernel command line the worker reads, with or without the
// host's first-boot marker.
func (w *repoWorld) fresh(yes bool) {
	w.t.Helper()
	line := "console=ttyS0 reboot=k sparkbox_host=brave-otter"
	if yes {
		line += " sparkbox_fresh=1"
	}
	w.write(filepath.Join(w.root, "cmdline"), line+"\n")
}

// pushToRemote adds one commit to the remote's main from a scratch clone, so
// the sandbox's checkout falls behind.
func (w *repoWorld) pushToRemote(name string) {
	w.t.Helper()
	scratch := w.t.TempDir()
	w.git(w.root, "clone", "-q", w.remote, scratch)
	w.write(filepath.Join(scratch, name), name+"\n")
	w.git(scratch, "add", "-A")
	w.git(scratch, "commit", "-qm", name)
	w.git(scratch, "push", "-q", "origin", "main")
}

// run executes the worker, requires it to succeed, and returns the report line
// for the one attachment.
func (w *repoWorld) run(mode string) string {
	w.t.Helper()
	line, code := w.runCode(mode)
	if code != 0 {
		w.t.Fatalf("sparkbox-repos %s exited %d: %s", mode, code, line)
	}
	return line
}

func (w *repoWorld) runCode(mode string) (string, int) {
	w.t.Helper()
	cmd := exec.Command("sh", filepath.Join(w.root, "usr/local/sbin/sparkbox-repos"), mode)
	cmd.Env = w.env
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		w.t.Fatalf("sparkbox-repos %s: %v\n%s", mode, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "wandb/hivemind") {
			return line, code
		}
	}
	w.t.Fatalf("sparkbox-repos %s reported nothing about the attachment:\n%s", mode, out)
	return "", code
}

func (w *repoWorld) branch() string { return w.git(w.checkout, "rev-parse", "--abbrev-ref", "HEAD") }
func (w *repoWorld) head() string   { return w.git(w.checkout, "rev-parse", "HEAD") }

func (w *repoWorld) want(t *testing.T, got string, substrings ...string) {
	t.Helper()
	for _, s := range substrings {
		if !strings.Contains(got, s) {
			t.Errorf("report line %q does not mention %q", got, s)
		}
	}
}

// A clean checkout that has fallen behind is the case the whole refresh exists
// for: a fork of a template made a week ago, or a long-lived box nobody has
// synced. It fast-forwards, and re-running is a no-op rather than a second
// merge.
func TestRepoRefreshFastForwardsACleanCheckout(t *testing.T) {
	w := newRepoWorld(t)
	w.pushToRemote("c.txt")

	w.want(t, w.run("sync"), "ready", "fast-forwarded")
	if got := w.git(w.checkout, "rev-parse", "HEAD"); got != w.git(w.checkout, "rev-parse", "origin/main") {
		t.Error("the fast-forward did not land on the upstream commit")
	}
	w.want(t, w.run("sync"), "ready", "up to date")
}

// The invariant, tested as a property rather than asserted in a comment: a tree
// with uncommitted work in it comes out with that work still in it and HEAD
// where the person left it, however far behind the remote has moved.
func TestRepoRefreshNeverMovesADirtyTree(t *testing.T) {
	w := newRepoWorld(t)
	before := w.head()
	w.pushToRemote("c.txt")

	tracked := filepath.Join(w.checkout, "a.txt")
	w.write(tracked, "one, edited\n")
	scratch := filepath.Join(w.checkout, "scratch.txt")
	w.write(scratch, "notes\n")

	w.want(t, w.run("sync"), "stale", "uncommitted changes")

	if got := w.head(); got != before {
		t.Errorf("HEAD moved under a dirty tree: %s -> %s", before, got)
	}
	if body, err := os.ReadFile(tracked); err != nil || string(body) != "one, edited\n" {
		t.Errorf("an uncommitted edit was lost: %q, %v", body, err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("an untracked file was removed: %v", err)
	}
}

// Local commits are work too, and one that is only in this box is the easiest
// thing in the world to destroy with a reset. Ahead and diverged are both
// reported and neither is acted on.
func TestRepoRefreshLeavesUnpushedCommitsAlone(t *testing.T) {
	w := newRepoWorld(t)
	w.write(filepath.Join(w.checkout, "local.txt"), "mine\n")
	w.git(w.checkout, "add", "-A")
	w.git(w.checkout, "commit", "-qm", "mine")
	mine := w.head()

	w.want(t, w.run("sync"), "stale", "not pushed")
	if w.head() != mine {
		t.Error("an unpushed commit was discarded")
	}

	w.pushToRemote("c.txt")
	w.want(t, w.run("sync"), "stale", "diverged")
	if w.head() != mine {
		t.Error("a diverged branch was rewritten")
	}
}

// The two modes. A named ref may move HEAD only on the first boot of a disk
// that came from a template; every other run reports the difference and leaves
// the branch alone, because somebody may be standing on it.
func TestRepoRefreshSwitchesBranchesOnlyOnAFreshDisk(t *testing.T) {
	w := newRepoWorld(t)
	w.manifest("feat/x")

	w.fresh(false)
	w.want(t, w.run("sync"), "stale", "on main, not feat/x")
	if got := w.branch(); got != "main" {
		t.Errorf("a refresh switched the branch to %q under a live box", got)
	}

	w.fresh(true)
	w.want(t, w.run("sync"), "ready", "switched to feat/x")
	if got := w.branch(); got != "feat/x" {
		t.Errorf("adoption left the checkout on %q, not the branch the manifest names", got)
	}
	// Idempotent: the marker is on the command line for the whole first boot,
	// so a hand-run sync during it must not be a second switch.
	w.want(t, w.run("sync"), "ready", "up to date")
}

// Adoption is still gated on a clean tree. The dirt in an inherited checkout is
// somebody's captured work, and a fork is not the place to discover it is gone.
func TestRepoAdoptionStillRefusesADirtyTree(t *testing.T) {
	w := newRepoWorld(t)
	w.manifest("feat/x")
	w.fresh(true)
	w.write(filepath.Join(w.checkout, "scratch.txt"), "captured\n")

	w.want(t, w.run("sync"), "stale", "left as captured")
	if got := w.branch(); got != "main" {
		t.Errorf("adoption switched a dirty inherited tree to %q", got)
	}
}

// An operation frozen into a template by a capture that paused mid-rebase. The
// fork inherits a git that refuses to run; the worker must name it and the way
// out, and must not decide the rebase's outcome on somebody's behalf.
func TestRepoRefreshReportsAnOperationInFlight(t *testing.T) {
	w := newRepoWorld(t)
	for _, tc := range []struct{ marker, dir, says string }{
		{"rebase-merge", "dir", "rebase --abort"},
		{"MERGE_HEAD", "file", "merge --abort"},
		{"CHERRY_PICK_HEAD", "file", "cherry-pick --abort"},
		{"BISECT_LOG", "file", "bisect reset"},
	} {
		path := filepath.Join(w.checkout, ".git", tc.marker)
		if tc.dir == "dir" {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
		} else {
			w.write(path, w.head()+"\n")
		}
		w.want(t, w.run("sync"), "failed", tc.says)
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
}

// Not every occupied path is a checkout. A directory somebody made, or a
// tarball they unpacked, is reported where it sits and never touched — the
// promise this worker has always made about a path that is already in use.
func TestRepoRefreshLeavesANonRepositoryAlone(t *testing.T) {
	w := newRepoWorld(t)
	if err := os.RemoveAll(w.checkout); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(w.checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	w.write(filepath.Join(w.checkout, "notes.txt"), "mine\n")

	w.want(t, w.run("sync"), "ready", "present")
	if _, err := os.Stat(filepath.Join(w.checkout, "notes.txt")); err != nil {
		t.Errorf("a directory that was not a checkout was disturbed: %v", err)
	}
}

// `status` is a question somebody asked out loud and expects back at once, so
// it stays off the network: no fetch, and therefore no ahead/behind. Proven by
// taking the remote away — a status that reached for it would say so.
func TestRepoStatusAnswersWithoutTheNetwork(t *testing.T) {
	w := newRepoWorld(t)
	if err := os.Rename(w.remote, w.remote+".gone"); err != nil {
		t.Fatal(err)
	}
	line := w.run("status")
	w.want(t, line, "ready", "on main")
	if strings.Contains(line, "could not reach") {
		t.Errorf("status went to the network: %q", line)
	}
}

// A remote that cannot be reached is a state, not a failure: the checkout is
// fine, it is the answer about it that is missing, and the next sync repairs it.
func TestRepoRefreshSurvivesAnUnreachableRemote(t *testing.T) {
	w := newRepoWorld(t)
	before := w.head()
	if err := os.Rename(w.remote, w.remote+".gone"); err != nil {
		t.Fatal(err)
	}
	w.want(t, w.run("sync"), "stale", "could not reach the remote")
	if w.head() != before {
		t.Error("an unreachable remote moved HEAD")
	}
}

// The capture survey.
//
// A template is a byte-for-byte copy of a disk somebody is working on, and the
// gateway that answers the capture plan cannot see inside it. So the one place
// that can tell the person what they are about to freeze into every future fork
// is the guest, in the session, before the prompt — which is what these cover.
//
// The stub stands in for sparkbox-repos; what the real one reports is proven
// against real git repositories above. What is under test here is the wiring
// and the single refusal.
func guestCaptureWithSurvey(t *testing.T, surveyOut string, surveyExit int) (
	requests func() []string,
	run func(args ...string) (string, string, int),
) {
	t.Helper()
	reply, requests, baseRun := guestSelfService(t)
	reply("GET", "200", guestPlanHeaders, guestPlanBody)
	reply("POST", "202", "HTTP/1.1 202 Accepted\r\n\r\n", "accepted — capturing.\n")

	stub := filepath.Join(t.TempDir(), "sparkbox-repos")
	writeExecutable(t, stub, "#!/bin/sh\n"+
		"[ \"$1\" = survey ] || { echo 'wrong mode' >&2; exit 9; }\n"+
		"cat <<'REPORT'\n"+surveyOut+"\nREPORT\n"+
		"exit "+strconv.Itoa(surveyExit)+"\n")

	run = func(args ...string) (string, string, int) {
		t.Helper()
		t.Setenv("SPARKBOX_REPOS_BIN", stub)
		return baseRun(args...)
	}
	return requests, run
}

// A checkout with uncommitted work in it is a SURPRISE, not a defect: the
// person may well mean to capture it. It is printed and the capture proceeds.
func TestGuestCaptureShowsWhatItWouldFreeze(t *testing.T) {
	requests, run := guestCaptureWithSurvey(t,
		"wandb/hivemind    ready  read  ~/hivemind  on feat/x, 2 not pushed, uncommitted changes", 0)

	stdout, stderr, code := run("snapshot", "web", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{"repos, as they would be captured", "2 not pushed", "uncommitted changes"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the capture never showed %q:\n%s", want, stdout)
		}
	}
	if got := requests(); len(got) != 4 {
		t.Errorf("the capture did not proceed past the survey: %v", got)
	}
}

// The one refusal. A git operation in flight is copied into the template
// verbatim, so every fork inherits a git that refuses to run — in a box whose
// owner never saw the rebase. Refused before the commit, and nothing reaches
// the host but the plan that was already read.
func TestGuestCaptureRefusesAnOperationInFlight(t *testing.T) {
	requests, run := guestCaptureWithSurvey(t,
		"wandb/hivemind    failed  read  ~/hivemind  a rebase is in progress — git rebase --abort", 3)

	stdout, stderr, code := run("snapshot", "web", "--yes")
	if code != 3 {
		t.Fatalf("exit = %d, want 3\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "--allow-busy") {
		t.Errorf("the refusal does not say how to override it:\n%s", stderr)
	}
	if !strings.Contains(stdout, "rebase --abort") {
		t.Errorf("the refusal does not show which checkout, or the way out:\n%s", stdout)
	}
	for _, got := range requests() {
		if got == "POST" {
			t.Errorf("a refused capture still committed: %v", requests())
		}
	}
}

// …and the override means it. Somebody who says they meant to capture a box
// mid-rebase gets to.
func TestGuestCaptureAllowsAnOperationInFlightWhenTold(t *testing.T) {
	requests, run := guestCaptureWithSurvey(t,
		"wandb/hivemind    failed  read  ~/hivemind  a rebase is in progress — git rebase --abort", 3)

	stdout, stderr, code := run("snapshot", "web", "--yes", "--allow-busy")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if got := requests(); len(got) != 4 {
		t.Errorf("--allow-busy did not let the capture through: %v", got)
	}
}

// `survey` is what `sparkbox snapshot` asks before it freezes this disk into a
// template every future fork copies. It is `status` with an opinion: no
// network, the counts taken against the remote-tracking refs this checkout
// already has, and every fact that would surprise somebody named in one line.
func TestRepoSurveyNamesWhatACaptureWouldFreeze(t *testing.T) {
	w := newRepoWorld(t)
	w.write(filepath.Join(w.checkout, "local.txt"), "mine\n")
	w.git(w.checkout, "add", "-A")
	w.git(w.checkout, "commit", "-qm", "mine")
	w.write(filepath.Join(w.checkout, "scratch.txt"), "notes\n")

	line, code := w.runCode("survey")
	if code != 0 {
		t.Errorf("a surprising checkout is not a defect; survey exited %d", code)
	}
	w.want(t, line, "on main", "1 not pushed", "uncommitted changes")
}

// The one thing a survey refuses. Exit 3 is the signal `sparkbox snapshot`
// turns into a refusal, and it has to be distinguishable from "the survey could
// not run" (exit 1) and from an ordinary dirty tree (exit 0).
func TestRepoSurveyExitsThreeOnAnOperationInFlight(t *testing.T) {
	w := newRepoWorld(t)
	if err := os.MkdirAll(filepath.Join(w.checkout, ".git/rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	line, code := w.runCode("survey")
	if code != 3 {
		t.Errorf("survey exited %d over a rebase in flight, want 3: %s", code, line)
	}
	w.want(t, line, "rebase", "--abort")
}
