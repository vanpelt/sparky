package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestrictedNetScriptBuildsSluiceCompatibleCeiling(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "iptables.log")
	writeExecutable(t, filepath.Join(tmp, "ip"), `#!/bin/sh
if [ "$1" = route ]; then
	echo "default via 10.0.0.1 dev eth0"
fi
`)
	writeExecutable(t, filepath.Join(tmp, "iptables"), `#!/bin/sh
printf '%s\n' "$*" >> "$SPARKBOX_TEST_LOG"
case " $* " in
	*" -C "*|*" -D "*) exit 1 ;;
esac
exit 0
`)

	cmd := exec.Command("bash", "-c", string(NetScript))
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+":"+os.Getenv("PATH"),
		"SPARKBOX_TEST_LOG="+logPath,
		"SPARKBOX_GUEST_SUBNET=172.30.0.0/20",
		"SPARKBOX_RESTRICT_INTERNAL_EGRESS=1",
		"SPARKBOX_EDGE_REDIRECT=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restricted network script: %v\n%s", err, out)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	rules := string(logged)
	for _, want := range []string{
		"-A SPARKBOX_GUEST_OUT ! -s 172.30.0.0/20 -j DROP",
		"-A SPARKBOX_GUEST_OUT -d 10.0.0.0/8 -j DROP",
		"-A SPARKBOX_GUEST_OUT -d 169.254.0.0/16 -j DROP",
		"-A SPARKBOX_GUEST_HOST -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"-A SPARKBOX_GUEST_HOST -p udp --dport 53 -j ACCEPT",
		"-A SPARKBOX_GUEST_HOST -p tcp --dport 53 -j ACCEPT",
		"-A SPARKBOX_GUEST_HOST -p tcp --dport 8967 -j ACCEPT",
		"-I FORWARD 1 -i sbtap+ -j SPARKBOX_GUEST_OUT",
		"-I FORWARD 2 -o sbtap+ -j SPARKBOX_GUEST_IN",
	} {
		if !strings.Contains(rules, want+"\n") {
			t.Errorf("missing rule %q in:\n%s", want, rules)
		}
	}
	if strings.Contains(rules, "-I FORWARD 1 -i sbtap+ -j ACCEPT\n") {
		t.Fatalf("restricted mode installed the legacy blanket accept:\n%s", rules)
	}
}

// fakeGuestTree builds the smallest rootfs the installer will accept: one
// non-root account owning an authorized_keys (which is how the installer
// derives the login user), plus the two files the image ships that the payload
// has to ADD to rather than replace — /etc/gitconfig, which already carries
// init.defaultBranch, and the static /etc/motd.
//
// `systemd` chooses which of the two trees the installer supports it sees.
// build-rootfs.sh keeps a slim, systemd-less fallback template alive, so
// everything outside the unit block has to land in both.
func fakeGuestTree(t *testing.T, systemd bool) string {
	t.Helper()
	root := t.TempDir()
	dirs := []string{"etc", "home/sparky/.ssh"}
	if systemd {
		dirs = append(dirs, "lib/systemd")
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"etc/passwd":                       "sparky:x:1000:1000::/home/sparky:/bin/bash\n",
		"home/sparky/.ssh/authorized_keys": "ssh-ed25519 test\n",
		"etc/gitconfig":                    "[init]\n\tdefaultBranch = main\n",
		"etc/motd":                         "the baked banner\n",
	}
	if systemd {
		files["lib/systemd/systemd"] = "#!/bin/false\n"
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// installGuestPayload runs the real installer against tree, so the tests below
// assert on the bytes a guest actually receives rather than on the script that
// writes them — the @@TOKEN@@ substitutions in particular are only visible on
// the far side of it.
func installGuestPayload(t *testing.T, tree string) {
	t.Helper()
	cmd := exec.Command("bash", "install-guest-identity.sh", tree)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install guest payload: %v\n%s", err, out)
	}
}

func guestFile(t *testing.T, tree, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(tree, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestGuestPayloadInstallsSelfControlCLI(t *testing.T) {
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)

	cli := guestFile(t, root, "usr/local/bin/sparkbox")
	for _, want := range []string{
		"$META/self/pin", "$META/self/unpin", "$META/self",
		"$META/self/visibility/public", "$META/self/visibility/private",
		"$META/self/port/$2",
		// `repos` delegates instead of reimplementing the manifest read: the
		// rule that reports where a repo lives must be the same rule that put
		// it there, or the report names a directory nothing cloned into.
		"SB=/usr/local/sbin/sparkbox-repos",
		"exec $SB status",
		"exec $SB sync",
		// And it delegates as ROOT when it can. The boot unit runs as root and
		// writes the status file and the login banner; a user-run sync that
		// could not write them would clone successfully and leave the banner
		// reporting the old failure at every login, forever. -n so a template
		// without passwordless sudo degrades instead of hanging on a prompt.
		"sudo -n /usr/local/sbin/sparkbox-repos",
		"sudo -n true",
	} {
		if !strings.Contains(cli, want) {
			t.Errorf("guest CLI missing %q", want)
		}
	}
	if rev := guestFile(t, root, "etc/sparkbox/identity-rev"); rev != "IDENTITY_REV=8\n" {
		t.Fatalf("identity revision = %q — bump it whenever the payload changes, or refresh-agent-tools.sh will leave published templates stale", rev)
	}
}

// TestGuestGitIdentityWritesAnAttributableAuthor runs the installed writer
// against a real tree, because every property worth pinning here is a property
// of the file it produces rather than of the script that produces it.
//
// The address is the point. `<id>+<login>@users.noreply.github.com` is the only
// form github.com links back to an account created after 2017-07-18, so a
// sandbox that commits under anything else produces history authored by nobody
// — which is exactly what a user notices a week later, on a push they cannot
// re-author. The legacy fallback is deliberate and second: naming the right
// person unattributed still beats naming no one.
func TestGuestGitIdentityWritesAnAttributableAuthor(t *testing.T) {
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)
	writer := filepath.Join(root, "usr/local/sbin/sparkbox-git-identity")

	// The exact bytes metadata.Doc marshals to, so the extraction is tested
	// against the encoder's output shape and not against a hand-tidied sample.
	doc := func(extra string) string {
		return `{"iss":"https://catnip.sh/oidc","sub":"user:vanpelt","owner":"vanpelt",` +
			extra + `"key_fp":"SHA256:x","sandbox":"dazzling-canyon",` +
			`"sandbox_id":"sb_01","image":"ubuntu","box":"sparky"}`
	}
	run := func(t *testing.T, identity, gitconfig string) string {
		t.Helper()
		idPath := filepath.Join(t.TempDir(), "identity.json")
		if err := os.WriteFile(idPath, []byte(identity), 0o644); err != nil {
			t.Fatal(err)
		}
		cfgPath := filepath.Join(t.TempDir(), "gitconfig")
		if err := os.WriteFile(cfgPath, []byte(gitconfig), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", writer, idPath)
		cmd.Env = append(os.Environ(), "SPARKBOX_GITCONFIG="+cfgPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sparkbox-git-identity: %v\n%s", err, out)
		}
		body, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	const helper = "[credential \"https://github.com\"]\n\thelper = /usr/local/bin/sparkbox-git-credential\n"

	t.Run("linked account gets the attributable address", func(t *testing.T) {
		got := run(t, doc(`"github":"vanpelt","github_id":271676,`), helper)
		for _, want := range []string{
			"\tname = vanpelt\n",
			"\temail = 271676+vanpelt@users.noreply.github.com\n",
			// Rewriting the file must not cost the guest its credential
			// helper, which lives in the same file and is what makes a
			// private clone work at all.
			"helper = /usr/local/bin/sparkbox-git-credential",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("gitconfig missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("no account number falls back to the legacy form", func(t *testing.T) {
		got := run(t, doc(`"github":"adrnswanberg",`), "")
		if !strings.Contains(got, "\temail = adrnswanberg@users.noreply.github.com\n") {
			t.Errorf("expected the legacy noreply address:\n%s", got)
		}
		// "github" must not have matched inside "github_id", and an absent
		// number must not become a literal 0 in the address.
		if strings.Contains(got, "0+") {
			t.Errorf("a missing account number leaked into the address:\n%s", got)
		}
	})

	t.Run("unlinked account is told, not guessed at", func(t *testing.T) {
		got := run(t, doc(""), "")
		// A Sparkbox handle is not a GitHub login. Writing
		// <handle>@users.noreply.github.com would hand a stranger's account
		// the authorship of this person's commits, so the block must carry no
		// address at all — every line of it a comment.
		if strings.Contains(got, "[user]") || strings.Contains(got, "@users.noreply.github.com") {
			t.Errorf("guessed an address for an unlinked account:\n%s", got)
		}
		if !strings.Contains(got, "git config --global user.email") {
			t.Errorf("unlinked block does not say how to fix it:\n%s", got)
		}
	})

	t.Run("rewriting corrects rather than accumulates", func(t *testing.T) {
		// The identity can change under a running VM — a handle rename, or a
		// GitHub link made after boot — and this runs again on every token
		// refresh, so a second pass has to replace the block it wrote.
		first := run(t, doc(`"github":"vanpelt","github_id":271676,`), helper)
		second := run(t, doc(`"github":"vanpelt","github_id":271676,`), first)
		if n := strings.Count(second, "sparkbox identity (managed)"); n != 2 {
			t.Errorf("expected exactly one managed block (2 markers), got %d:\n%s", n, second)
		}
		renamed := run(t, doc(`"github":"newlogin","github_id":42,`), first)
		if strings.Contains(renamed, "271676+vanpelt") {
			t.Errorf("stale identity survived the rewrite:\n%s", renamed)
		}
		if !strings.Contains(renamed, "42+newlogin@users.noreply.github.com") {
			t.Errorf("rename did not take:\n%s", renamed)
		}
	})

	t.Run("no identity file is a no-op", func(t *testing.T) {
		// This runs on the boot path. A guest whose metadata fetch failed must
		// still boot, not die in a unit that exits non-zero.
		cfgPath := filepath.Join(t.TempDir(), "gitconfig")
		cmd := exec.Command("sh", writer, filepath.Join(t.TempDir(), "absent.json"))
		cmd.Env = append(os.Environ(), "SPARKBOX_GITCONFIG="+cfgPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("a missing identity must be a no-op, got %v\n%s", err, out)
		}
		if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
			t.Errorf("wrote a gitconfig with no identity to write from")
		}
	})
}

// TestGuestTokenScriptWritesTheGitAuthor pins the wiring: the writer above is
// only reached because the token script calls it, and it is called there rather
// than from its own unit because that script is the one thing already holding a
// fresh per-VM identity on every boot AND on the refresh timer.
func TestGuestTokenScriptWritesTheGitAuthor(t *testing.T) {
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)
	token := guestFile(t, root, "usr/local/sbin/sparkbox-token")
	// Absolute path: a bare name depends on the unit's PATH carrying
	// /usr/local/sbin, and `|| true` because a git author is a convenience and
	// the token this script exists to fetch is not.
	if !strings.Contains(token, `/usr/local/sbin/sparkbox-git-identity "$IDENTITY_FILE" || true`) {
		t.Error("sparkbox-token no longer writes the git author, so a fresh sandbox cannot commit")
	}
}

// TestGuestCredentialHelperIsSilentAndPathScoped pins the two properties that
// decide whether a headless clone works at all.
//
// Silence first: git falls through to prompting on the terminal when a helper
// fails, and the boot-time clone has no terminal, so a helper that reports its
// problems is a helper that hangs. Every failure path here has to be a bare
// `exit 0`.
//
// Then scope: `useHttpPath = true` is what makes git send the repository path,
// and it is the whole difference between a token scoped to this fetch and a
// token scoped to every repository the sandbox can reach.
func TestGuestCredentialHelperIsSilentAndPathScoped(t *testing.T) {
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)

	helper := guestFile(t, root, "usr/local/bin/sparkbox-git-credential")
	for _, want := range []string{
		`[ "${1:-}" = get ] || exit 0`,     // store and erase say nothing
		"/github/credential?slug=$path",    // the contract's query parameter
		"path=${path%.git}",                // a clone URL carries the suffix; the ledger does not
		`printf 'username=%s\npassword=%s`, // git's own reply format
	} {
		if !strings.Contains(helper, want) {
			t.Errorf("credential helper missing %q", want)
		}
	}
	for _, never := range []string{"echo ", ">&2"} {
		if strings.Contains(helper, never) {
			t.Errorf("credential helper says %q on a failure path; git responds to a "+
				"talkative helper by prompting for a username nobody is there to type", never)
		}
	}
	// A token that reaches the filesystem rides into every snapshot, fork and
	// archived rootfs made from this box. It is minted per request and printed
	// to git; there is nowhere for it to be written.
	for _, never := range []string{".git-credentials", "credential.helper store"} {
		if strings.Contains(helper, never) {
			t.Errorf("credential helper persists the token via %q", never)
		}
	}
	if fi, err := os.Stat(filepath.Join(root, "usr/local/bin/sparkbox-git-credential")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("credential helper is not executable (mode %v); git would silently ignore it", fi.Mode())
	}

	gitcfg := guestFile(t, root, "etc/gitconfig")
	for _, want := range []string{
		`[credential "https://github.com"]`,
		"helper = /usr/local/bin/sparkbox-git-credential", // absolute: a bare word makes git exec `git credential-<word>`
		"useHttpPath = true",
	} {
		if !strings.Contains(gitcfg, want) {
			t.Errorf("guest /etc/gitconfig missing %q", want)
		}
	}
	if !strings.Contains(gitcfg, "defaultBranch = main") {
		t.Error("the payload truncated /etc/gitconfig instead of appending to it; the image already writes init.defaultBranch there")
	}
}

// TestGuestPayloadRePatchesWithoutAccumulating pins idempotence, which is not a
// nicety here: refresh-agent-tools.sh re-runs this installer against every
// template whose IDENTITY_REV is behind, so a step that appends unconditionally
// appends once per release.
func TestGuestPayloadRePatchesWithoutAccumulating(t *testing.T) {
	root := fakeGuestTree(t, true)
	installGuestPayload(t, root)
	base := guestFile(t, root, "etc/sparkbox/motd.base")
	installGuestPayload(t, root)

	if n := strings.Count(guestFile(t, root, "etc/gitconfig"), "useHttpPath = true"); n != 1 {
		t.Errorf("credential config written %d times into /etc/gitconfig", n)
	}
	// The clone worker rewrites /etc/motd as (this file + status), so a re-patch
	// that re-captured an already-rewritten banner would compound the status
	// line into the banner on every refresh.
	if got := guestFile(t, root, "etc/sparkbox/motd.base"); got != base {
		t.Errorf("saved login banner changed on re-patch:\n%q\nwant:\n%q", got, base)
	}
	if base != "the baked banner\n" {
		t.Errorf("saved login banner = %q, want the image's own /etc/motd", base)
	}
}

// TestRepoCloneNeverBlocksTheFirstAttach is the one assertion in this file that
// exists because the mistake has already been made. sparkbox-net.service
// carries Before=ssh.service because it generates the host keys sshd needs, and
// copying that line into a unit that clones a repository would put a
// multi-minute network operation in front of the first attach — the same shape
// as the boot race main@e196d5f fixed, and slower.
func TestRepoCloneNeverBlocksTheFirstAttach(t *testing.T) {
	script := string(GuestIdentityScript)
	unit := heredocBody(t, script, "sparkbox-repos.service\" <<'EOF'\n")
	if strings.Contains("\n"+unit, "\nBefore=") {
		t.Errorf("sparkbox-repos.service orders itself before something; a clone must "+
			"make somebody wait for a directory, never for their shell:\n%s", unit)
	}
	for _, want := range []string{
		"Type=oneshot",
		// Correct here and wrong on sparkbox-token.service: nothing re-triggers
		// this unit, so staying active is what stops a stray `systemctl start`
		// from running the pass a second time.
		"RemainAfterExit=yes",
		"After=network-online.target sparkbox-net.service sparkbox-token.service",
		"ExecStart=/usr/local/sbin/sparkbox-repos sync",
		"WantedBy=multi-user.target",
		// A large monorepo outlives systemd's 90-second default and would be
		// killed mid-clone.
		"TimeoutStartSec=",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("sparkbox-repos.service missing %q:\n%s", want, unit)
		}
	}
	// [Install] alone does nothing in a tree nothing ever runs `systemctl
	// enable` against; the symlink IS the enablement.
	if !strings.Contains(script, "multi-user.target.wants/sparkbox-repos.service") {
		t.Error("sparkbox-repos.service is never symlinked into multi-user.target.wants, so it never runs at boot")
	}
}

// TestRepoWorkerClonesForTheUserAndLeavesCheckoutsAlone reads the installed
// worker rather than the installer, so the @@SANDBOX_USER@@ substitution is
// covered too — a hardcoded `sparky` would pass every string check against the
// script and then clone into the wrong home on a root-login template.
func TestRepoWorkerClonesForTheUserAndLeavesCheckoutsAlone(t *testing.T) {
	root := fakeGuestTree(t, true)
	installGuestPayload(t, root)
	worker := guestFile(t, root, "usr/local/sbin/sparkbox-repos")

	for _, want := range []string{
		`"$META/repos"`,
		// Full history with blobs on demand. Agents run git log, blame and
		// bisect, all of which a shallow clone breaks.
		"--filter=blob:none",
		// A repository the helper cannot get a token for must fail in seconds,
		// not block on a username prompt nobody is there to answer.
		"GIT_TERMINAL_PROMPT=0",
		// Derived from the tree, not assumed. git refuses to work in a tree
		// owned by somebody else, so a root-owned checkout in a user's home is
		// worse than no checkout at all — and a hardcoded `sparky` would clone
		// into the wrong home on a root-login template.
		"SANDBOX_USER=sparky",
		`RUNAS="runuser -u $SANDBOX_USER --"`,
		// Whatever is already at EITHER default location is left exactly as it
		// is. Both are probed because the default is a function of how many
		// attachments the tag carries right now, and that number moves under a
		// checkout that already exists: attaching a second repo would otherwise
		// re-clone the first one somewhere else and report the empty copy.
		`for candidate in "$dest" "$HOME_DIR/$name" "$HOME_DIR/src/$owner/$name"; do`,
		`if [ -e "$candidate" ]; then`,
		"continue 2",
		// The default layout, both halves.
		`dest="$HOME_DIR/src/$owner/$name"`,
		`dest="$HOME_DIR/$name"`,
		// A failed clone is a warning, never a failed boot.
		`if [ "$MODE" = sync ]; then exit 0; fi`,
	} {
		if !strings.Contains(worker, want) {
			t.Errorf("repo worker missing %q", want)
		}
	}
	// Redirections apply left to right, so `> file 2>/dev/null` still reports
	// "Permission denied" on the stderr it has not replaced yet. Every
	// privileged write in this script has to put 2>/dev/null FIRST, or an
	// unprivileged run sprays raw shell errors over its own output.
	for _, bad := range []string{
		`> /etc/.motd.sparkbox 2>/dev/null`,
		`> "$STATUS_FILE.new" 2>/dev/null`,
	} {
		if strings.Contains(worker, bad) {
			t.Errorf("repo worker has %q: stderr must be redirected before the redirection that can fail", bad)
		}
	}

	if strings.Contains(worker, "--depth") {
		t.Error("repo worker clones shallowly; --filter=blob:none is the default that keeps git log, blame and bisect working")
	}
	if strings.Contains(worker, "@@") {
		t.Error("repo worker still carries an unsubstituted @@TOKEN@@")
	}
}

// TestTokenLandsBeforeAnythingReadsIt pins WHO owns the boot fetch, which is
// not a tuning question but the difference between a guest having a token at
// startup and not.
//
// It used to be the timer's, via OnBootSec=10s plus a tight AccuracySec to stop
// systemd's default one-minute slack pushing it later still. That was always a
// guess against a deadline nobody measured, and the measurement lost: on a
// fresh CKS sandbox the hivemind daemon was up 1.7s after boot and gave up
// looking for a token at 2.4s, while the timer delivered one at 11.5s. hivemind
// resolves its credential chain once, at startup, so the box then ran its whole
// life telling the user to run `hivemind login` with a valid token on disk.
//
// So the fetch is ordered into boot as a service instead, and the readers wait
// for the FILE (see refresh-agent-tools.sh's drop-in) rather than for a unit
// they cannot be ordered against — a user unit and a system unit have different
// managers, and systemd drops an After= that crosses between them.
func TestTokenLandsBeforeAnythingReadsIt(t *testing.T) {
	script := string(GuestIdentityScript)
	timer := timerUnitFrom(t, script)
	// The timer is now only the refresh. A short OnBootSec here would mean the
	// boot fetch had quietly moved back onto it.
	for _, stale := range []string{"OnBootSec=10s", "OnBootSec=1s", "OnBootSec=0"} {
		if strings.Contains(timer, stale) {
			t.Errorf("token timer carries %q: the boot fetch belongs to the service, "+
				"which is ordered into boot, not to a guessed timer offset:\n%s", stale, timer)
		}
	}
	if !strings.Contains(timer, "OnUnitActiveSec=45min") {
		t.Errorf("token timer no longer refreshes 45 minutes after the last fetch:\n%s", timer)
	}
	// And the service has to actually be enabled at boot, or nothing fetches a
	// token until the timer's backstop 45 minutes later.
	if !strings.Contains(script, "WantedBy=multi-user.target") {
		t.Error("sparkbox-token.service is not wanted by multi-user.target, so nothing fetches a token at boot")
	}
	if !strings.Contains(script, "multi-user.target.wants/sparkbox-token.service") {
		t.Error("sparkbox-token.service is never symlinked into multi-user.target.wants, so [Install] does nothing in a tree that is never `systemctl enable`d")
	}
	// A oneshot that stays active cannot be re-triggered by its own timer: the
	// start job against an already-active unit is a no-op and the refresh would
	// silently stop after boot.
	// Matched as a directive line, not a substring: the unit talks about this
	// in a comment precisely so nobody adds it back.
	if strings.Contains("\n"+serviceUnitFrom(t, script), "\nRemainAfterExit=yes") {
		t.Error("sparkbox-token.service is RemainAfterExit=yes, which makes the 45-minute timer a no-op forever after the boot fetch")
	}
}

// TestTokenWaiterExistsForTheReaders pins the other half: the user units that
// read the token cannot be ordered against the system unit that writes it, so
// they wait on the file. A missing waiter is a drop-in that references nothing.
func TestTokenWaiterExistsForTheReaders(t *testing.T) {
	script := string(GuestIdentityScript)
	if !strings.Contains(script, "usr/local/bin/sparkbox-await-token") {
		t.Fatal("install-guest-identity.sh no longer installs sparkbox-await-token, " +
			"which refresh-agent-tools.sh's hivemind drop-in runs as ExecStartPre")
	}
	if !strings.Contains(script, "HIVEMIND_OIDC_TOKEN_FILE:-/var/run/secrets/hivemind/token") {
		t.Error("the waiter no longer watches the same path the fetcher writes and hivemind reads")
	}
}

// timerUnitFrom returns the sparkbox-token.timer heredoc body the installer
// writes, so the assertions above read the unit rather than the whole script
// and cannot be satisfied by the same string appearing in a comment elsewhere.
func timerUnitFrom(t *testing.T, script string) string {
	t.Helper()
	return heredocBody(t, script, "sparkbox-token.timer\" <<'EOF'\n")
}

// serviceUnitFrom is timerUnitFrom for sparkbox-token.service.
func serviceUnitFrom(t *testing.T, script string) string {
	t.Helper()
	return heredocBody(t, script, "sparkbox-token.service\" <<'EOF'\n")
}

// heredocBody returns the body of the heredoc that follows marker, so unit
// assertions read the unit rather than the whole script and cannot be satisfied
// by the same string appearing in a comment elsewhere.
func heredocBody(t *testing.T, script, marker string) string {
	t.Helper()
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatalf("install-guest-identity.sh no longer writes a heredoc for %q", marker)
	}
	rest := script[i+len(marker):]
	j := strings.Index(rest, "\nEOF\n")
	if j < 0 {
		t.Fatalf("unterminated heredoc for %q", marker)
	}
	return rest[:j]
}

func TestTemplateGuidanceTargetsHarnessGlobalFiles(t *testing.T) {
	got := string(RefreshToolsScript)
	for _, want := range []string{
		".agents/AGENTS.md",
		".codex/AGENTS.md",
		".claude/CLAUDE.md",
		"https://docs.catnip.sh/docs.md",
		"sparkbox pin",
		"sparkbox unpin",
		"sparkbox make-public",
		"sparkbox make-private",
		"sparkbox set-port PORT",
		"sparkbox repos",
		"sparkbox repos sync",
		// The sentence that stops an agent solving a private clone the way the
		// old documentation told it to: by asking for a personal access token.
		"Do not create, paste, or store a GitHub token",
		// The browser is only "out of the box" if the CLI, the Chrome it drives,
		// and the skill that tells an agent the CLI exists all land together.
		// Each of these is one of those three, and the env var is the one that
		// stops the agent trying to download a second Chrome it cannot get.
		"agent-browser skills get core",
		".agents/skills/agent-browser",
		"../../.agents/skills/agent-browser",
		"../../../.agents/skills/agent-browser",
		"AGENT_BROWSER_EXECUTABLE_PATH /headless-shell/headless-shell",
		"AGENT_BROWSER_SOCKET_DIR",
		// The VM's own name and public URL, which the guidance can only express
		// as $(hostname): this file is baked into a shared template and read by
		// every clone of it, so no sandbox's name can be literal here. The
		// sparkbox-netcfg boot hook makes the hostname the sandbox name.
		"https://$(hostname).catnip.sh",
		// A dev service is only reachable if the default route points at the
		// port a person opens, and only findable later if the other ports are
		// recorded somewhere the session carries with it.
		"sparkbox set-port 5173",
		"hivemind tag api_url=https://$(hostname).catnip.sh:8080",
		"hivemind tag --list",
		"hivemind tag --remove KEY",
		// An agent that hits "Please tell me who you are" and answers it
		// invents an author that cannot be corrected once pushed.
		"git's author is already set",
		"AGENT_ENV_REV=8",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("template guidance missing %q", want)
		}
	}
}

// TestGuestSeedsAPermissionDefault pins the setting that lets an agent in a
// sandbox get on with it.
//
// A sandbox is provisioned precisely so work can run while nobody is watching,
// and a per-turn approval prompt is answerable only by a human at a terminal —
// so a guest that starts in the default mode is a guest that stops on its first
// tool call and waits forever. The user scope is load-bearing too: the same file
// under a project directory would cover that one directory and nothing the owner
// later clones.
func TestGuestSeedsAPermissionDefault(t *testing.T) {
	got := string(RefreshToolsScript)
	for _, want := range []string{
		`"$mnt$home/.claude/settings.json"`, // user scope, not per-project
		`perms.setdefault("defaultMode", "auto")`,
		`cfg.setdefault("skipAutoPermissionPrompt", True)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("guest permission seed missing %q", want)
		}
	}
	// setdefault throughout, never assignment: the same loop walks fork
	// templates holding a real user's settings, and someone who deliberately
	// moved off auto must not have it restored under them by a refresh.
	for _, never := range []string{
		`perms["defaultMode"] =`,
		`cfg["skipAutoPermissionPrompt"] =`,
	} {
		if strings.Contains(got, never) {
			t.Errorf("guest permission seed overwrites the user's own choice: %q", never)
		}
	}
	// Seeding a blanket bypass would make that decision once, here, for every
	// user of every sandbox. It stays something a person types for themselves.
	// Matched on the seeded value rather than the bare word, which the comment
	// above it in the script legitimately uses to explain the choice.
	if strings.Contains(got, `"defaultMode", "bypassPermissions"`) {
		t.Error("the template seeds a full permission bypass; `auto` is the line this is allowed to draw")
	}
}

func TestCKSManifestKeepsSluiceNarrowAndOpenUntilTagged(t *testing.T) {
	manifest, err := os.ReadFile("kubernetes/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	entrypoint, err := os.ReadFile("kubernetes/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	sluiceEntrypoint, err := os.ReadFile("kubernetes/sluice-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	helperEntrypoint, err := os.ReadFile("kubernetes/vmm-helper-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	got := string(manifest)
	for _, want := range []string{
		"- name: sluice",
		"- BPF",
		"- NET_ADMIN",
		"- NET_BIND_SERVICE",
		"value: 172.30.0.53",
		"mountPath: /run/sluice",
		`value: "480000"`,
		`value: "90"`,
		`value: "50"`,
		"memory: 448Gi",
		`cpu: "56"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CKS manifest missing Sluice invariant %q", want)
		}
	}
	if strings.Contains(got, "name: sluice\n          securityContext:\n            privileged: true") {
		t.Error("Sluice must not be privileged")
	}
	controller := string(entrypoint)
	for _, want := range []string{
		"curl --fail --silent --show-error --unix-socket \"$sluice_socket\"",
		"sluice_args+=(--sluice-socket \"$sluice_socket\")",
		"sluice_args+=(--guest-dns \"$guest_dns\")",
		`--mem-admission-pct "$mem_admission_pct"`,
		`--max-running-per-owner "$max_running_per_owner"`,
	} {
		if !strings.Contains(controller, want) {
			t.Errorf("controller entrypoint missing fail-closed Sluice wiring %q", want)
		}
	}
	if !strings.Contains(string(sluiceEntrypoint), "--open-untagged") {
		t.Error("CKS Sluice must leave an ungoverned sandbox open until a network-rule tag applies")
	}
	baseAllowlist, err := os.ReadFile("sluice-allowlist.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(baseAllowlist), "hivemind.wandb.tools") {
		t.Error("CKS base allow-list must permit the platform workload-identity exchange")
	}
	if !strings.Contains(string(helperEntrypoint), `helper_args+=(--sluice-socket "$sluice_socket")`) {
		t.Error("VMM helper does not require Sluice readiness before launch")
	}
}

func TestCKSSplitMigrationCommitsOnlyAfterOwnership(t *testing.T) {
	manifest, err := os.ReadFile("kubernetes/migration-job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got := string(manifest)
	marker := strings.LastIndex(got, `touch "$marker"`)
	ownership := strings.LastIndex(got, "chown -R 65532:65532 /durable/gateway")
	if marker < 0 || ownership < 0 || marker < ownership {
		t.Fatalf("migration marker must be written after recursive ownership is fixed")
	}
	if strings.Count(got, "chown -R 65532:65532 /durable/gateway") < 2 {
		t.Fatal("completed migration fast path does not repair ownership")
	}
	if strings.Count(got, `test -s "$dst/`) < 2 {
		t.Fatal("completed migration does not validate both gateway databases")
	}
}

func TestCKSCleanInstallSkipsLegacyMigration(t *testing.T) {
	deployScript, err := os.ReadFile("kubernetes/deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	got := string(deployScript)
	for _, want := range []string{
		"legacy_deployment=0",
		`if [ "$legacy_deployment" = 1 ]; then`,
		"No legacy combined Deployment found",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("deploy script missing clean-install guard %q", want)
		}
	}

	nodeManifest, err := os.ReadFile("kubernetes/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nodeManifest), "type: DirectoryOrCreate") {
		t.Fatal("clean install cannot create the node-local hot tier")
	}
}

func TestCKSPublicPortPrototypeTargetsOneHTTPSListener(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "kubectl.log")
	writeExecutable(t, filepath.Join(tmp, "kubectl"), `#!/bin/sh
printf '%s\n' "$*" >> "$SPARKBOX_TEST_LOG"
case " $* " in
  *" get service "*)
    printf '22\tssh\tssh\n443\thttps\thttps\n'
    ;;
esac
`)

	cmd := exec.Command("bash", "kubernetes/public-port.sh",
		"--context", "test", "--namespace", "sandbox-test", "add", "6454")
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+":"+os.Getenv("PATH"),
		"SPARKBOX_TEST_LOG="+logPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add public port: %v\n%s", err, out)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(logged)
	for _, want := range []string{
		"--context test -n sandbox-test patch service sparkbox",
		`"name":"https-6454"`,
		`"port":6454`,
		`"targetPort":"https"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("public-port patch missing %q in:\n%s", want, got)
		}
	}
}

func TestCKSServicePreloadsCommonHTTPSPorts(t *testing.T) {
	manifest, err := os.ReadFile("kubernetes/service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got := string(manifest)
	for _, port := range []string{
		"3000", "3001", "4000", "4200", "5000", "5173", "6006",
		"7860", "8000", "8080", "8443", "8501", "8888", "9000",
	} {
		for _, want := range []string{"name: https-" + port, "port: " + port} {
			if !strings.Contains(got, want) {
				t.Errorf("CKS Service missing common HTTPS mapping %q", want)
			}
		}
	}
	if !strings.Contains(got, "name: http\n") || !strings.Contains(got, "port: 80\n") {
		t.Fatal("CKS Service missing public HTTP redirect port 80")
	}
	if strings.Contains(got, "name: https-8081") {
		t.Fatal("CKS Service must not advertise internal gateway listener port 8081")
	}
	if strings.Count(got, "targetPort: https") != 16 {
		t.Fatalf("HTTP(S) mappings = %d, want ports 80/443 plus fourteen common HTTPS ports",
			strings.Count(got, "targetPort: https"))
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runGHWrapper executes the installed `gh` wrapper against stub tools and
// returns what the stub `gh` recorded.
//
// The wrapper's path to the real binary is absolute on purpose — a relative
// `gh` would find the wrapper itself — so the test rewrites it rather than
// asking the shipped script for a test seam. A seam here would be a variable
// that decides which binary `gh` means, which is not a knob worth shipping to
// make an assertion easier.
func runGHWrapper(t *testing.T, remote string, env []string) (stdout string, sawToken string, ranReal bool) {
	t.Helper()
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)

	bin := t.TempDir()
	real := filepath.Join(bin, "real-gh")
	tokenLog := filepath.Join(bin, "token")
	writeExecutable(t, real, `#!/bin/sh
printf '%s' "${GH_TOKEN-}" > "`+tokenLog+`"
echo "real gh ran: $*"
`)
	writeExecutable(t, filepath.Join(bin, "git"), `#!/bin/sh
# Only `+"`git config --get remote.origin.url`"+` is consulted.
[ "$1" = config ] || exit 1
[ -n "`+remote+`" ] || exit 1
echo "`+remote+`"
`)
	writeExecutable(t, filepath.Join(bin, "ip"), `#!/bin/sh
[ "$1" = -4 ] && echo "default via 10.0.0.1 dev eth0"
`)
	writeExecutable(t, filepath.Join(bin, "curl"), `#!/bin/sh
# Echo the URL back inside the credential document so the test can assert on
# exactly what slug the wrapper asked for.
for a in "$@"; do case "$a" in http*) url=$a ;; esac; done
printf '{"username":"x-access-token","password":"tok-for %s"}' "$url"
`)

	script := strings.ReplaceAll(guestFile(t, root, "usr/local/bin/gh"), "/usr/bin/gh", real)
	wrapper := filepath.Join(bin, "wrapper.sh")
	writeExecutable(t, wrapper, script)

	cmd := exec.Command("sh", wrapper, "pr", "list")
	// Prepend, never replace: the wrapper shells out to awk and sed, and a PATH
	// holding only the stubs would make every branch fail its way to the
	// unauthenticated fallback — which is what these tests are trying to tell
	// apart from a real one.
	cmd.Env = append(append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH")), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh wrapper: %v\n%s", err, out)
	}
	if body, rerr := os.ReadFile(tokenLog); rerr == nil {
		ranReal, sawToken = true, string(body)
	}
	return string(out), sawToken, ranReal
}

// TestGuestGHWrapperScopesTheTokenToTheCheckout is the property that makes `gh`
// usable in a sandbox at all: it has no credential helper, so without this it
// reports "not logged into any GitHub hosts" beside a warm private checkout.
//
// The token must be scoped to the repository the caller is standing in, which
// is both the right answer for a repository-scoped tool and the narrowest one.
func TestGuestGHWrapperScopesTheTokenToTheCheckout(t *testing.T) {
	for _, remote := range []string{
		"https://github.com/wandb/hivemind.git",
		"https://github.com/wandb/hivemind",
		"git@github.com:wandb/hivemind.git",
		"ssh://git@github.com/wandb/hivemind.git",
	} {
		_, token, ranReal := runGHWrapper(t, remote, nil)
		if !ranReal {
			t.Fatalf("%s: the real gh never ran", remote)
		}
		if !strings.Contains(token, "slug=wandb/hivemind") {
			t.Errorf("%s: minted for %q, want the checkout's own slug", remote, token)
		}
	}
}

// TestGuestGHWrapperNeverBlocksAGHCommand pins the fallback. This wrapper sits
// in front of every `gh` invocation in the sandbox, so any failure of its own —
// no git, no remote, a remote pointing somewhere else, no metadata service —
// has to fall through to an unauthenticated gh rather than become the reason a
// command did not run. `gh --version` and `gh auth login` must work in an empty
// directory.
func TestGuestGHWrapperNeverBlocksAGHCommand(t *testing.T) {
	for name, remote := range map[string]string{
		"no remote at all":     "",
		"not github":           "https://gitlab.com/wandb/hivemind.git",
		"three path segments":  "https://github.com/wandb/hivemind/extra",
		"shell metacharacters": "https://github.com/wandb/hive;mind",
		"query injection":      "https://github.com/wandb/hivemind&x=1",
	} {
		out, token, ranReal := runGHWrapper(t, remote, nil)
		if !ranReal || !strings.Contains(out, "real gh ran: pr list") {
			t.Errorf("%s: the real gh did not run (%q)", name, out)
		}
		if token != "" {
			t.Errorf("%s: minted a token for a slug it should have refused: %q", name, token)
		}
	}
}

// TestGuestGHWrapperYieldsToAnExplicitToken: somebody who exported GH_TOKEN
// meant it. Overriding it with ours would be the same class of surprise the
// wrapper exists to remove, and would make `gh auth login --with-token`
// impossible to use in a sandbox that has any repo attached.
func TestGuestGHWrapperYieldsToAnExplicitToken(t *testing.T) {
	for _, kv := range []string{"GH_TOKEN=mine", "GITHUB_TOKEN=mine"} {
		_, token, ranReal := runGHWrapper(t, "https://github.com/wandb/hivemind.git", []string{kv})
		if !ranReal {
			t.Fatalf("%s: the real gh never ran", kv)
		}
		if strings.Contains(token, "slug=") {
			t.Errorf("%s: the wrapper overrode an explicit token with %q", kv, token)
		}
	}
}
