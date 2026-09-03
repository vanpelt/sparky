package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/publicports"
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

func installGuestPayloadWithMOTD(t *testing.T, tree, motd string) {
	t.Helper()
	cmd := exec.Command("bash", "install-guest-identity.sh", tree)
	cmd.Env = append(os.Environ(), "GUEST_MOTD_FILE="+motd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install guest payload with motd: %v\n%s", err, out)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
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
		// Visibility is per port: the verb carries an optional PORT that
		// becomes ?port= on the same endpoint.
		"$META/self/visibility/$_vis?port=$2", "$META/self/visibility/$_vis",
		"$META/self/port/$2",
		// `repos` delegates instead of reimplementing the manifest read: the
		// rule that reports where a repo lives must be the same rule that put
		// it there, or the report names a directory nothing cloned into.
		"SPARKBOX_REPOS_BIN=${SPARKBOX_REPOS_BIN:-/usr/local/sbin/sparkbox-repos}",
		"SB=$SPARKBOX_REPOS_BIN",
		"exec $SB status",
		"exec $SB sync",
		"$META/github/authorization?slug=$_slug",
		"$META/github/authorization/$_id",
		// And it delegates as ROOT when it can. The boot unit runs as root and
		// writes the status file and the login banner; a user-run sync that
		// could not write them would clone successfully and leave the banner
		// reporting the old failure at every login, forever. -n so a template
		// without passwordless sudo degrades instead of hanging on a prompt.
		`SB="sudo -n $SPARKBOX_REPOS_BIN"`,
		"sudo -n true",
		// `update-tools` delegates the same way and for the same reason: the
		// installer writes /usr/local/bin, /usr/local/lib and /var/lib/sparkbox,
		// none of which the login user owns, so an unprivileged run would
		// download 150MB and then fail on its first install.
		"SB=/usr/local/sbin/sparkbox-update-tools",
		"sudo -n /usr/local/sbin/sparkbox-update-tools",
		"exec $SB --check",
		// The two verbs that can end the session go through _call, which reads
		// the status from -w '%{http_code}' instead of letting `curl -f` throw
		// the host's own sentence away. A `-f` here would be a regression that
		// no output test elsewhere could catch, because -f hides exactly the
		// text those tests read.
		"$META/self/pause",
		"$META/self/snapshot?tag=",
		// docs.<domain> can resolve to this fleet's own edge, which the guest's
		// tap firewall has no route to reach directly, so `docs` reads the same
		// content over the metadata port instead. Only an allowlisted page name
		// is interpolated — see TestGuestDocsRejectsAnUnrecognizedPageWithoutAsking.
		`$META/docs/${2:-docs}.md`,
		"-w '%{http_code}'",
		// The commit sends the PLAN's tag, name and token, never values the
		// shell re-derived: the derived name carries a minute in it.
		"plan=$SPARKBOX_PLAN",
		// The sync is not optional. A pause freezes dirty page cache into the
		// MEMORY snapshot while the capture reads the BLOCK DEVICE, so an
		// unflushed write is present on resume and absent from the template.
		"sync; printf 'ok\\n'",
	} {
		if !strings.Contains(cli, want) {
			t.Errorf("guest CLI missing %q", want)
		}
	}
	// The human-readable help is also the discovery surface for agents. Keep
	// commands one per line and document stable exit codes so it remains easy to
	// parse without resurrecting the old unreadable one-line usage blob.
	for _, want := range []string{
		"sparkbox — manage this sandbox from inside the VM",
		"status [--json]",
		"snapshot [OPTIONS] [TAG [NAME]]",
		"whoami [--json]",
		"update-tools [--check]",
		// The environment a box came out of is a question an agent working
		// inside one asks, so the verb has to be discoverable from the help
		// rather than only from the design doc.
		"env                            Show the environment",
		"Exit codes (stable for scripts and agents):",
	} {
		if !strings.Contains(cli, want) {
			t.Errorf("guest CLI help does not mention %q:\n%s", want, cli)
		}
	}
	if !strings.Contains(cli, "repo authorize OWNER/NAME") {
		t.Errorf("guest CLI usage line does not mention per-repository authorization:\n%s", cli)
	}
	if rev := guestFile(t, root, "etc/sparkbox/identity-rev"); rev != "IDENTITY_REV=27\n" {
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
// TestIdentityRevMovesWithThePayload is the guard that was missing, and the
// reason it is worth the small friction it adds.
//
// refresh-agent-tools.sh decides whether to re-patch a published rootfs
// template by comparing the template's stamped IDENTITY_REV against the one it
// reads out of the installer. Edit the payload without bumping the stamp and
// every host concludes "templates already current" and skips the work — so the
// change ships, deploys green, and simply is not there in any VM. There is no
// error anywhere; the only way to find out is for somebody to run the new
// command in a sandbox and get the old behaviour. That is exactly how a docs
// fix reached production and 404'd.
//
// The existing rev assertion below cannot catch it: it pins the VALUE, so it
// fires when you bump without telling it, which is the harmless direction.
// This pins the payload, so it fires when you edit without bumping.
//
// WHEN THIS FAILS: bump IDENTITY_REV in deploy/install-guest-identity.sh, then
// update wantPayloadSum here to the sum the failure prints. Both, in that order.
func TestIdentityRevMovesWithThePayload(t *testing.T) {
	const (
		wantRev        = 27
		wantPayloadSum = "6f807c2b594ad2fd5a87f3bbfb9dd6eb18b883f923ca6c88c078b16cd33f31d0"
	)
	src, err := os.ReadFile("install-guest-identity.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Everything but the stamp line, so bumping the stamp alone does not move
	// the sum and the two assertions stay independent.
	var payload strings.Builder
	rev := 0
	for _, line := range strings.Split(string(src), "\n") {
		if after, ok := strings.CutPrefix(line, "IDENTITY_REV="); ok && rev == 0 {
			n, convErr := strconv.Atoi(strings.TrimSpace(after))
			if convErr != nil {
				t.Fatalf("IDENTITY_REV is not a number: %q", line)
			}
			rev = n
			continue
		}
		payload.WriteString(line)
		payload.WriteByte('\n')
	}
	if rev != wantRev {
		t.Fatalf("IDENTITY_REV = %d, want %d — update wantRev here when you bump it", rev, wantRev)
	}
	sum := sha256.Sum256([]byte(payload.String()))
	if got := hex.EncodeToString(sum[:]); got != wantPayloadSum {
		t.Fatalf("the guest payload changed while IDENTITY_REV is %d.\n"+
			"If you have NOT already bumped the stamp for this change, bump it in\n"+
			"deploy/install-guest-identity.sh (and wantRev here) — otherwise hosts skip\n"+
			"re-patching their templates and the change reaches no VM at all.\n"+
			"Then record the new payload:\n"+
			"  wantPayloadSum = %q", rev, got)
	}
}

func TestGuestGitIdentityWritesAnAttributableAuthor(t *testing.T) {
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)
	writer := filepath.Join(root, "usr/local/sbin/sparkbox-git-identity")

	// Built from the real metadata.Doc rather than a hand-written sample, and
	// rendered in BOTH encodings.
	//
	// The first version of this test wrote compact JSON by hand while claiming
	// to be "the exact bytes metadata.Doc marshals to", and the guest's sed
	// patterns were written to match it. They were wrong: /identity is served
	// through an encoder with SetIndent("", "  "), so the file on disk reads
	// `"github": "vanpelt"` WITH a space, and every linked account fell
	// silently down the "no GitHub account linked" path.
	//
	// So the shape is no longer this test's to assume. The struct supplies the
	// field names, encoding/json supplies the punctuation, and both spacings
	// run — because which one a guest sees is the metadata service's choice and
	// can change again without anyone touching this file.
	docJSON := func(t *testing.T, indent bool, github string, githubID int64) string {
		t.Helper()
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		if indent {
			enc.SetIndent("", "  ")
		}
		if err := enc.Encode(metadata.Doc{
			Issuer: "https://catnip.sh/oidc", Subject: "user:vanpelt", Owner: "vanpelt",
			GitHub: github, GitHubID: githubID, KeyFP: "SHA256:x",
			Sandbox: "dazzling-canyon", SandboxID: "sb_01", Image: "ubuntu", Box: "sparky",
		}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
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

	for _, enc := range []struct {
		name   string
		indent bool
	}{
		// The one the service actually serves goes first: it is the one that
		// was broken, and the one a regression would break again.
		{name: "indented", indent: true},
		{name: "compact"},
	} {
		t.Run(enc.name, func(t *testing.T) {
			doc := func(github string, id int64) string {
				return docJSON(t, enc.indent, github, id)
			}

			t.Run("linked account gets the attributable address", func(t *testing.T) {
				got := run(t, doc("vanpelt", 271676), helper)
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
				got := run(t, doc("adrnswanberg", 0), "")
				if !strings.Contains(got, "\temail = adrnswanberg@users.noreply.github.com\n") {
					t.Errorf("expected the legacy noreply address:\n%s", got)
				}
				// "github" must not have matched inside "github_id", and an
				// absent number must not become a literal 0 in the address.
				if strings.Contains(got, "0+") {
					t.Errorf("a missing account number leaked into the address:\n%s", got)
				}
			})

			t.Run("unlinked account is told, not guessed at", func(t *testing.T) {
				got := run(t, doc("", 0), "")
				// A Sparkbox handle is not a GitHub login. Writing
				// <handle>@users.noreply.github.com would hand a stranger's
				// account the authorship of this person's commits, so the block
				// must carry no address at all — every line of it a comment.
				if strings.Contains(got, "[user]") || strings.Contains(got, "@users.noreply.github.com") {
					t.Errorf("guessed an address for an unlinked account:\n%s", got)
				}
				if !strings.Contains(got, "git config --global user.email") {
					t.Errorf("unlinked block does not say how to fix it:\n%s", got)
				}
			})

			t.Run("rewriting corrects rather than accumulates", func(t *testing.T) {
				// The identity can change under a running VM — a handle rename,
				// or a GitHub link made after boot — and this runs again on
				// every token refresh, so a second pass has to replace the
				// block it wrote.
				first := run(t, doc("vanpelt", 271676), helper)
				second := run(t, doc("vanpelt", 271676), first)
				if n := strings.Count(second, "sparkbox identity (managed)"); n != 2 {
					t.Errorf("expected exactly one managed block (2 markers), got %d:\n%s", n, second)
				}
				renamed := run(t, doc("newlogin", 42), first)
				if strings.Contains(renamed, "271676+vanpelt") {
					t.Errorf("stale identity survived the rewrite:\n%s", renamed)
				}
				if !strings.Contains(renamed, "42+newlogin@users.noreply.github.com") {
					t.Errorf("rename did not take:\n%s", renamed)
				}
			})
		})
	}

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

func TestGuestPayloadInstallsHostSuppliedMOTD(t *testing.T) {
	root := fakeGuestTree(t, true)
	motd := filepath.Join(t.TempDir(), "motd")
	want := "the CKS feature banner\nRun `sparkbox` for commands.\n"
	if err := os.WriteFile(motd, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	// First install replaces the released rootfs banner. A later install must
	// also replace a dynamic status line left in /etc/motd, while keeping the
	// canonical base clean for the repo worker's next rewrite.
	installGuestPayloadWithMOTD(t, root, motd)
	if err := os.WriteFile(filepath.Join(root, "etc/motd"), []byte(want+"repos: old status\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installGuestPayloadWithMOTD(t, root, motd)
	for _, name := range []string{"etc/motd", "etc/sparkbox/motd.base"} {
		if got := guestFile(t, root, name); got != want {
			t.Errorf("%s = %q, want canonical banner %q", name, got, want)
		}
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
	report := heredocBody(t, script, "sparkbox-repos-report.service\" <<'EOF'\n")
	timer := heredocBody(t, script, "sparkbox-repos-report.timer\" <<'EOF'\n")
	for _, want := range []string{"Type=oneshot", "ExecStart=/usr/local/sbin/sparkbox-repos report"} {
		if !strings.Contains(report, want) {
			t.Errorf("sparkbox-repos-report.service missing %q:\n%s", want, report)
		}
	}
	for _, want := range []string{"OnUnitInactiveSec=5min", "RandomizedDelaySec=30s"} {
		if !strings.Contains(timer, want) {
			t.Errorf("sparkbox-repos-report.timer missing %q:\n%s", want, timer)
		}
	}
	if !strings.Contains(script, "timers.target.wants/sparkbox-repos-report.timer") {
		t.Error("sparkbox-repos-report.timer is not enabled")
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
		// The drop is skipped only under a root override, where there is no
		// such account on this kernel to drop to. Pinned so the guard cannot
		// widen into "skip the drop when something else is unset" — a
		// root-owned checkout in a user's home is one every later git command
		// in that directory refuses for dubious ownership.
		`if [ -z "$R" ] && [ "$(id -u)" = 0 ] && [ "$SANDBOX_USER" != root ]; then`,
		// Whatever is already at EITHER default location keeps its place. Both
		// are probed because the default is a function of how many attachments
		// the tag carries right now, and that number moves under a checkout
		// that already exists: attaching a second repo would otherwise re-clone
		// the first one somewhere else and report the empty copy.
		//
		// What happens to the one that is found is refresh_checkout's, and it
		// is tested for real in repos_refresh_test.go rather than by string.
		`for candidate in "$dest" "$HOME_DIR/$name" "$HOME_DIR/src/$owner/$name"; do`,
		`if [ -e "$candidate" ]; then found=$candidate; break; fi`,
		`refresh_checkout "$found" "$slug" "$access" "$ref"`,
		// The three acts the refresh is allowed. Everything else is a way to
		// lose somebody's work.
		"--ff-only",
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
	// An allow-list rather than a ban-list, and the difference matters: a ban
	// list is a guess at the next dangerous verb somebody reaches for, and it
	// also trips over the prose and the report strings that NAME those verbs.
	//
	// Every git this worker runs against an existing checkout goes through
	// gitq. This worker runs as root, unattended, on a filesystem somebody is
	// working in, fired by events they did not cause, with no terminal to ask
	// in and no way to be undone — so what it may run there is a closed set:
	// read the state, fetch, switch a clean tree, fast-forward. Anything else
	// arriving in that position is a way to lose work, `git pull` included (it
	// merges or rebases depending on config the user set, so it is a
	// fast-forward right up until it silently is not).
	allowed := map[string]bool{
		"rev-parse": true, "status": true, "symbolic-ref": true,
		"fetch": true, "switch": true, "rev-list": true, "merge": true,
	}
	calls := regexp.MustCompile(`gitq "\$_dest" ([a-z-]+)`).FindAllStringSubmatch(worker, -1)
	if len(calls) == 0 {
		t.Error("no gitq calls found; the refresh either moved or stopped going through the one wrapper")
	}
	for _, call := range calls {
		if !allowed[call[1]] {
			t.Errorf("repo worker runs `git %s` against an existing checkout; it may only read, fetch, switch a clean tree, or fast-forward", call[1])
		}
	}
	// The one allowed verb that is only safe with its flag. A merge without
	// --ff-only writes a commit into somebody's branch.
	for _, merge := range regexp.MustCompile(`gitq "\$_dest" merge[^\n]*`).FindAllString(worker, -1) {
		if !strings.Contains(merge, "--ff-only") {
			t.Errorf("repo worker merges without --ff-only: %s", merge)
		}
	}
	if strings.Contains(worker, "@@") {
		t.Error("repo worker still carries an unsubstituted @@TOKEN@@")
	}
}

// toolsManifestFields is the wire contract between the two scripts in this
// directory: refresh-agent-tools.sh's write_tools_manifest emits these keys and
// install-guest-identity.sh's sparkbox-update-tools reads them back. Nothing
// else joins the two — the manifest travels through internal/metadata as opaque
// bytes — so a field renamed on one side and not the other produces an install
// that silently skips whatever that field decided.
var toolsManifestFields = []string{
	"name", "key", "version", "file", "sha256", "size",
	"kind", "bin", "dir", "exec", "link", "keep_only", "drop",
}

func TestToolsManifestFieldsAppearOnBothSidesOfTheWire(t *testing.T) {
	for _, field := range toolsManifestFields {
		quoted := `"` + field + `"`
		if !strings.Contains(string(RefreshToolsScript), quoted) {
			t.Errorf("refresh-agent-tools.sh never writes a %s field", quoted)
		}
		if !strings.Contains(string(GuestIdentityScript), quoted) {
			t.Errorf("the guest updater never reads a %s field", quoted)
		}
	}
	// The guest's stamp and the host's are different files with different
	// meanings, and the guest owns only the first. /etc/sparkbox/tools-rev is
	// what refresh-agent-tools.sh reads back with debugfs to decide which
	// templates to patch; a guest writing its own versions there would make the
	// next refresh believe a template was current that it never patched.
	if !strings.Contains(string(GuestIdentityScript), "/var/lib/sparkbox/tools-rev") {
		t.Error("the guest updater keeps no stamp of its own at /var/lib/sparkbox/tools-rev")
	}
}

// writeTarGz builds one of the bundle artifacts the refresher caches, so the
// guest's tar path (strip-components, the bin/ prune, the exec bit npm does not
// set) runs against a real archive rather than a directory copy.
func writeTarGz(t *testing.T, path string, files []struct {
	name string
	body string
	mode int64
}) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, file := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: file.name, Mode: file.mode, Size: int64(len(file.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

// hostToolsCache builds the $TOOLS_DIR a refresher run leaves behind, and
// writes the manifest with refresh-agent-tools.sh's OWN writer rather than a
// hand-rolled copy of it. That is the point of this helper: the guest parses
// JSON with tr+awk and no library, so the only assertion worth making is that
// the bytes the host actually emits are the bytes the guest can actually read.
//
// The versions are deliberately absurd (9.9.9) so nothing here can accidentally
// agree with a real release, and the arch is pinned so the fixture is the same
// on an x86 CI runner and an arm64 laptop.
func hostToolsCache(t *testing.T) string {
	t.Helper()
	for _, tool := range []string{"python3", "sha256sum", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on this machine; the guest updater needs it", tool)
		}
	}
	dir := t.TempDir()
	for name, body := range map[string]string{
		"claude-9.9.9-linux-arm64":   "#!/bin/sh\necho claude 9.9.9\n",
		"codex-rust-v9.9.9-aarch64":  "#!/bin/sh\necho codex\n",
		"hivemind-9.9.9-linux-arm64": "#!/bin/sh\necho hivemind\n",
	} {
		writeExecutable(t, filepath.Join(dir, name), body)
	}
	type entry = struct {
		name string
		body string
		mode int64
	}
	// pi's bundle: the executable is not alone in it, which is why the guest
	// installs the tree and links into it.
	writeTarGz(t, filepath.Join(dir, "pi-v9.9.9-linux-arm64.tar.gz"), []entry{
		{"pi-v9.9.9/pi", "#!/bin/sh\necho pi\n", 0o755},
		{"pi-v9.9.9/lib/data", "runtime asset\n", 0o644},
	})
	// agent-browser's: seven platform binaries in upstream, two here, plus the
	// scripts/ directory whose postinstall we never run and the SKILL.md the
	// harnesses link to. Mode 0644 on bin/* is what npm really publishes.
	writeTarGz(t, filepath.Join(dir, "agent-browser-9.9.9.tgz"), []entry{
		{"package/bin/agent-browser-linux-arm64", "#!/bin/sh\necho ab\n", 0o644},
		{"package/bin/agent-browser-linux-x64", strings.Repeat("x", 512), 0o644},
		{"package/bin/agent-browser.js", "require('./x')\n", 0o644},
		{"package/scripts/postinstall.js", "chmod()\n", 0o644},
		{"package/skills/agent-browser/SKILL.md", "# agent-browser 9.9.9\n", 0o644},
		{"package/skill-data/core.md", "the command reference\n", 0o644},
	})

	script := string(RefreshToolsScript)
	const marker = "\nwrite_tools_manifest() {\n"
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatal("refresh-agent-tools.sh no longer defines write_tools_manifest")
	}
	const end = "\nPY\n}\n"
	j := strings.Index(script[i:], end)
	if j < 0 {
		t.Fatal("write_tools_manifest is unterminated")
	}
	driver := `set -euo pipefail
TOOLS_DIR=$1
CLAUDE_BIN="$TOOLS_DIR/claude-9.9.9-linux-arm64"
CODEX_BIN="$TOOLS_DIR/codex-rust-v9.9.9-aarch64"
PI_BUNDLE="$TOOLS_DIR/pi-v9.9.9-linux-arm64.tar.gz"
HM_BIN="$TOOLS_DIR/hivemind-9.9.9-linux-arm64"
AB_TGZ="$TOOLS_DIR/agent-browser-9.9.9.tgz"
CLAUDE_VER=9.9.9; CODEX_TAG=rust-v9.9.9; PI_TAG=v9.9.9; HM_VER=9.9.9; AB_VER=9.9.9
AB_ARCH=arm64
TOOLS_REV="claude=$CLAUDE_VER codex=$CODEX_TAG pi=$PI_TAG hivemind=$HM_VER agentbrowser=$AB_VER"
` + script[i+1:i+j+len(end)] + `
write_tools_manifest
`
	cmd := exec.Command("bash", "-c", driver, "manifest", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write_tools_manifest: %v\n%s", err, out)
	}

	// The host serves each artifact under the tool's NAME and resolves the file
	// through the manifest; these links are that indirection, so the fixture
	// cannot accidentally pass by the guest guessing a filename.
	if err := os.Mkdir(filepath.Join(dir, "by-name"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{
		"claude":        "claude-9.9.9-linux-arm64",
		"codex":         "codex-rust-v9.9.9-aarch64",
		"pi":            "pi-v9.9.9-linux-arm64.tar.gz",
		"hivemind":      "hivemind-9.9.9-linux-arm64",
		"agent-browser": "agent-browser-9.9.9.tgz",
	} {
		if err := os.Symlink(filepath.Join("..", file), filepath.Join(dir, "by-name", name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// guestUpdater installs the payload into a fake rootfs, stubs the two host
// commands the updater shells out to, and returns a runner for the INSTALLED
// script — so the @@META_PORT@@ and @@SANDBOX_USER@@ substitutions are covered
// the way the repo worker's are.
func guestUpdater(t *testing.T, cache string) (root string, run func(args ...string) (string, error)) {
	t.Helper()
	root = fakeGuestTree(t, false)
	installGuestPayload(t, root)
	// What this VM booted with. The updater may read it and must never write it.
	if err := os.WriteFile(filepath.Join(root, "etc/sparkbox/tools-rev"),
		[]byte("claude=1.0.0 codex=rust-v1.0.0 pi=v1.0.0 hivemind=1.0.0 agentbrowser=1.0.0 identity=10 agentenv=11\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(root, "home/sparky/.agents/skills/agent-browser")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "SKILL.md"), []byte("# agent-browser 1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "ip"), `#!/bin/sh
[ "$1" = -4 ] && echo "default via 10.0.0.1 dev eth0"
`)
	// Serves $SPARKBOX_TEST_CACHE, honouring the two call shapes the updater
	// makes: the -w '%{http_code}' probe for the manifest and the -o fetch for
	// an artifact.
	writeExecutable(t, filepath.Join(bin, "curl"), `#!/bin/sh
out=; url=; code=0
while [ $# -gt 0 ]; do
	case "$1" in
		-o) out=$2; shift 2 ;;
		-w) code=1; shift 2 ;;
		http*) url=$1; shift ;;
		*) shift ;;
	esac
done
name=${url##*/}
src="$SPARKBOX_TEST_CACHE/by-name/$name"
[ "$name" = manifest ] && src="$SPARKBOX_TEST_CACHE/manifest.json"
if [ -f "$src" ]; then
	if [ -n "$out" ]; then cp "$src" "$out"; else cat "$src"; fi
	if [ "$code" = 1 ]; then printf '200'; fi
	exit 0
fi
if [ "$code" = 1 ]; then printf '404'; fi
exit 22
`)
	run = func(args ...string) (string, error) {
		cmd := exec.Command("sh", append([]string{filepath.Join(root, "usr/local/sbin/sparkbox-update-tools")}, args...)...)
		// Prepend, never replace: the script also runs awk, tar, install and
		// sha256sum, and a PATH holding only the stubs would fail every branch.
		cmd.Env = append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"),
			"SPARKBOX_TEST_CACHE="+cache,
			"SPARKBOX_TOOLS_ROOT="+root,
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	return root, run
}

// TestGuestToolUpdaterInstallsFromTheHostCache is the end-to-end half of this
// stage: the host's own manifest writer feeds the guest's own installer, and
// what lands on disk is what the patch loop would have baked into a template.
//
// The properties that are not obvious, and that this exists to hold:
//   - pi and agent-browser install as BUNDLES reached by a relative symlink.
//     Copying the executable out gives a CLI whose every `skills` subcommand
//     fails, because both resolve their assets against the real binary's path.
//   - agent-browser's bin/ is pruned to the one platform this box runs. Upstream
//     ships seven, ~92MB of ~13MB useful, and in a guest those bytes come out of
//     that VM's own 25 GiB ceiling and its owner's pool, once per sandbox.
//   - the exec bit is ours to set: npm publishes bin/* mode 0644 and chmods them
//     from a postinstall this never runs.
//   - the stamp the guest writes is its own, at /var/lib/sparkbox/tools-rev.
func TestGuestToolUpdaterInstallsFromTheHostCache(t *testing.T) {
	cache := hostToolsCache(t)
	root, run := guestUpdater(t, cache)
	template := guestFile(t, root, "etc/sparkbox/tools-rev")

	// --check first, on a VM that has never updated: the versions it reports as
	// installed can only have come from the template's stamp, and the identity=
	// and agentenv= words in it must not be along for the ride.
	out, err := run("--check")
	if err == nil {
		t.Errorf("--check exited 0 with five tools behind:\n%s", out)
	}
	for _, want := range []string{"claude", "1.0.0", "9.9.9", "behind", "agent-browser", "rust-v9.9.9"} {
		if !strings.Contains(out, want) {
			t.Errorf("--check output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "identity") || strings.Contains(out, "agentenv") {
		t.Errorf("--check reports host-only payload words it cannot install:\n%s", out)
	}

	if out, err := run(); err != nil {
		t.Fatalf("update-tools: %v\n%s", err, out)
	}

	// A plain binary, byte-identical and executable.
	claude := filepath.Join(root, "usr/local/bin/claude")
	if got := guestFile(t, root, "usr/local/bin/claude"); got != "#!/bin/sh\necho claude 9.9.9\n" {
		t.Errorf("installed claude = %q", got)
	}
	if info, serr := os.Stat(claude); serr != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("installed claude mode = %v (%v), want 0755", info.Mode().Perm(), serr)
	}

	// The two bundles, each reached by the manifest's own relative link.
	for bin, wantTarget := range map[string]string{
		"usr/local/bin/pi":            "../lib/pi/pi",
		"usr/local/bin/agent-browser": "../lib/agent-browser/bin/agent-browser-linux-arm64",
	} {
		target, lerr := os.Readlink(filepath.Join(root, bin))
		if lerr != nil {
			t.Errorf("%s is not a symlink into its bundle: %v", bin, lerr)
			continue
		}
		if target != wantTarget {
			t.Errorf("%s -> %q, want the manifest's own %q", bin, target, wantTarget)
		}
	}
	if got := guestFile(t, root, "usr/local/lib/pi/lib/data"); got != "runtime asset\n" {
		t.Errorf("pi's bundle lost its runtime assets: %q", got)
	}

	// The prune, and the exec bit npm leaves off.
	abBin := filepath.Join(root, "usr/local/lib/agent-browser/bin")
	names, err := os.ReadDir(abBin)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0].Name() != "agent-browser-linux-arm64" {
		var got []string
		for _, e := range names {
			got = append(got, e.Name())
		}
		t.Errorf("agent-browser bin/ = %v, want only this box's platform binary", got)
	}
	if info, serr := os.Stat(filepath.Join(abBin, "agent-browser-linux-arm64")); serr != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("agent-browser binary mode = %v (%v), want 0755", info.Mode().Perm(), serr)
	}
	if _, serr := os.Stat(filepath.Join(root, "usr/local/lib/agent-browser/scripts")); !os.IsNotExist(serr) {
		t.Errorf("agent-browser's scripts/ survived the install (%v); its postinstall never runs", serr)
	}
	// The skill stub has to track the CLI it describes, but only where the
	// template already wired one up.
	if got := guestFile(t, root, "home/sparky/.agents/skills/agent-browser/SKILL.md"); got != "# agent-browser 9.9.9\n" {
		t.Errorf("harness SKILL.md = %q, want the installed bundle's copy", got)
	}

	// The guest's own stamp, and the host's decision variable left alone.
	wantStamp := "claude=9.9.9 codex=rust-v9.9.9 pi=v9.9.9 hivemind=9.9.9 agentbrowser=9.9.9\n"
	if got := guestFile(t, root, "var/lib/sparkbox/tools-rev"); got != wantStamp {
		t.Errorf("guest stamp = %q, want %q", got, wantStamp)
	}
	if got := guestFile(t, root, "etc/sparkbox/tools-rev"); got != template {
		t.Errorf("the guest wrote the TEMPLATE's stamp (%q, was %q); refresh-agent-tools.sh reads that file "+
			"back to decide which templates to patch, so a guest writing it makes a template read as current "+
			"that was never patched", got, template)
	}

	out, err = run("--check")
	if err != nil {
		t.Errorf("--check after a successful update: %v\n%s", err, out)
	}
	if strings.Contains(out, "behind") {
		t.Errorf("--check still reports a tool behind after installing it:\n%s", out)
	}
}

// TestGuestToolUpdaterRefusesBytesTheManifestDoesNotDescribe: this is the only
// check on artifacts that are about to become every agent in the VM, and it has
// to happen BEFORE the install — a truncated `claude` that has already replaced
// the working one cannot be repaired from inside the sandbox. One bad artifact
// must also not take the other four down with it.
func TestGuestToolUpdaterRefusesBytesTheManifestDoesNotDescribe(t *testing.T) {
	for name, swap := range map[string]string{
		// Same length, different bytes: only the digest can tell.
		"a substituted artifact": "#!/bin/sh\necho claude 9.9.8\n",
		// Short: what a body cut off by the host's write deadline looks like.
		"a truncated download": "#!/bin/sh\n",
	} {
		t.Run(name, func(t *testing.T) {
			cache := hostToolsCache(t)
			if err := os.WriteFile(filepath.Join(cache, "claude-9.9.9-linux-arm64"), []byte(swap), 0o755); err != nil {
				t.Fatal(err)
			}
			root, run := guestUpdater(t, cache)

			out, err := run()
			if err == nil {
				t.Errorf("update-tools exited 0 after refusing an artifact:\n%s", out)
			}
			if _, serr := os.Stat(filepath.Join(root, "usr/local/bin/claude")); !os.IsNotExist(serr) {
				t.Errorf("claude was installed from bytes the manifest does not describe (%v)", serr)
			}
			// The other four are independent and must still land.
			if got := guestFile(t, root, "usr/local/bin/codex"); got != "#!/bin/sh\necho codex\n" {
				t.Errorf("one refused artifact took codex down with it: %q", got)
			}
			// And the stamp keeps claude at what is actually on this disk.
			if got := guestFile(t, root, "var/lib/sparkbox/tools-rev"); !strings.Contains(got, "claude=1.0.0") {
				t.Errorf("guest stamp = %q, want claude still recorded at the version it actually has", got)
			}
		})
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
		"sparkbox docs",
		"sparkbox pin",
		"sparkbox unpin",
		"sparkbox make-public",
		"sparkbox make-private",
		"sparkbox set-port PORT",
		"sparkbox repos",
		"sparkbox repos sync",
		"sparkbox repo authorize owner/name",
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
		// The VM's own name and domain, which the guidance can only express by
		// deriving them: this file is baked into a shared template and read by
		// every clone of it on every deployment, so no sandbox's name and no
		// deployment's domain can be literal here. The sparkbox-netcfg boot hook
		// makes the hostname the sandbox name; `sparkbox whoami`'s `domain:` line
		// is where the domain itself has to come from instead.
		"DOMAIN=$(sparkbox whoami | sed -n 's/^domain: //p')",
		`echo "https://$(hostname).$DOMAIN"`,
		// A dev service is only reachable if the default route points at the
		// port a person opens, and only findable later if the other ports are
		// recorded somewhere the session carries with it.
		"sparkbox set-port 5173",
		`hivemind tag api_url="https://$(hostname).$DOMAIN:8080"`,
		"hivemind tag --list",
		"hivemind tag --remove KEY",
		// The framework fixes live in the served doc, not retyped here — this is
		// the pointer an agent needs to go find them. `sparkbox docs`, not the
		// https:// URL: that hostname can resolve to this fleet's own edge, which
		// a guest has no network route to reach directly.
		"sparkbox docs dev-environment",
		"systemd --user",
		// The replay convention: what to write, where, and to check for one
		// before re-deriving the setup by hand.
		".sparkbox/setup.sh",
		"Check for one before redoing this",
		// An agent that hits "Please tell me who you are" and answers it
		// invents an author that cannot be corrected once pushed.
		"git's author is usually already set",
		// The unlinked case is real and supported, and it is the one case
		// where the agent must NOT leave the author alone — it must ask.
		"ask them to run `git config --global user.name`",
		// The tools in a VM are its template's, frozen on the day that template
		// was patched, and every agent's own updater is off. An agent that does
		// not know the pull exists has no way to move them.
		"sparkbox update-tools --check",
		"AGENT_ENV_REV=12",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("template guidance missing %q", want)
		}
	}
	flat := strings.Join(strings.Fields(got), " ")
	if !strings.Contains(flat, publicports.HumanList()) {
		t.Errorf("template guidance common HTTPS ports drifted from publicports: want %q", publicports.HumanList())
	}
}

// TestTemplateGuidanceNeverHardcodesADomain: this file is baked into every
// deployment's templates, not just the flagship catnip.sh one, and the guest
// CLI already has a way to ask its own host — `sparkbox whoami`'s `domain:`
// line — so a literal domain here would be silently wrong on every other
// deployment.
func TestTemplateGuidanceNeverHardcodesADomain(t *testing.T) {
	if got := string(RefreshToolsScript); strings.Contains(got, "catnip.sh") {
		t.Errorf("template guidance hardcodes a domain instead of deriving one from `sparkbox whoami`:\n%s", got)
	}
}

// TestGuestGetsAHyphenatedDockerCompose covers the command Ubuntu 24.04 does
// not ship.
//
// docker-compose-v2 installs only the CLI plugin, so `docker compose` works and
// `docker-compose` does not exist — while essentially every Makefile, README
// and CI script written before 2023 calls the hyphenated form. An agent that
// meets that writes the shim into its own ~/.local/bin, which fixes one VM and
// nothing else.
//
// The two things this must NOT be are asserted as well. Compose v1 would make
// the command exist and the builds subtly wrong; a symlink would take Compose's
// standalone path, which finds the daemon from the environment rather than
// through the docker CLI's contexts and config.
func TestGuestGetsAHyphenatedDockerCompose(t *testing.T) {
	got := string(RefreshToolsScript)
	for _, want := range []string{
		`"$mnt/usr/local/bin/docker-compose"`,
		`exec docker compose "$@"`,
		// Guarded on the plugin actually being there, so a template without it
		// gets no command rather than one that fails confusingly.
		`[ -x "$mnt/usr/libexec/docker/cli-plugins/docker-compose" ]`,
		"install_docker_compose_shim",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("guest conditioning missing %q", want)
		}
	}
	if strings.Contains(got, "apt-get install -y docker-compose\n") {
		t.Error("installs Compose v1, which shadows v2 with a tool that names containers differently")
	}
	if strings.Contains(got, "ln -s /usr/libexec/docker/cli-plugins/docker-compose") {
		t.Error("symlinks the plugin, taking the standalone path instead of the docker CLI's")
	}

	// And run it, against a fake docker, because "forwards to docker compose"
	// is a claim about argument handling: an unquoted "$@" would split
	// `-f my compose.yml` into two arguments and the failure would look like a
	// missing file rather than a broken shim.
	shim := heredocBody(t, got, `"$mnt/usr/local/bin/docker-compose" <<'EOF'`+"\n")
	dir := t.TempDir()
	shimPath := filepath.Join(dir, "docker-compose")
	if err := os.WriteFile(shimPath, []byte(shim+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "docker.log")
	writeExecutable(t, filepath.Join(dir, "docker"), `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done >> "$SPARKBOX_TEST_LOG"
printf -- '--\n' >> "$SPARKBOX_TEST_LOG"
`)
	cmd := exec.Command("sh", shimPath, "-f", "with space.yml", "up", "-d")
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"),
		"SPARKBOX_TEST_LOG="+logPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shim: %v\n%s", err, out)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "compose\n-f\nwith space.yml\nup\n-d\n--\n"
	if string(logged) != want {
		t.Errorf("docker received:\n%q\nwant:\n%q", logged, want)
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

func TestGuestSeedsAutomaticClaudeColorScheme(t *testing.T) {
	got := string(RefreshToolsScript)
	if !strings.Contains(got, `cfg.setdefault("theme", "auto")`) {
		t.Error("guest Claude seed does not follow the terminal's light/dark color scheme")
	}
	for _, overwrite := range []string{`cfg["theme"] =`, `cfg.setdefault("theme", "dark")`} {
		if strings.Contains(got, overwrite) {
			t.Errorf("guest Claude seed overrides or hardcodes the user's theme: %q", overwrite)
		}
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

// TestCKSManifestPlaceholdersAreAllSubstituted is the general form of a bug
// this repository has now had twice: a value that reaches the cluster only as a
// deploy.sh flag, and a manifest that names it.
//
// A `__PLACEHOLDER__` with no matching `-e "s|__PLACEHOLDER__|"` in deploy.sh
// does not fail the deploy. kubectl applies the literal string, and the symptom
// surfaces later and somewhere else — a Pod env var whose value is the word
// __HIVEMIND_MANIFEST__, an image reference that cannot be pulled. Checking the
// two files against each other costs nothing and catches the whole class.
func TestCKSManifestPlaceholdersAreAllSubstituted(t *testing.T) {
	deployScript, err := os.ReadFile("kubernetes/deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(deployScript)

	manifests, err := filepath.Glob("kubernetes/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) == 0 {
		t.Fatal("no CKS manifests found; this test is not checking anything")
	}
	placeholder := regexp.MustCompile(`__[A-Z0-9_]+__`)
	for _, path := range manifests {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range placeholder.FindAllString(string(body), -1) {
			if !strings.Contains(script, "s|"+name+"|") {
				t.Errorf("%s uses %s but deploy.sh never substitutes it, so the literal reaches the cluster",
					filepath.Base(path), name)
			}
		}
	}
}

// TestCKSGuestHivemindIsAFlagNotAHandEdit pins the shape of the override rather
// than any particular version.
//
// The pin it replaces lived only as a hand-edited live object that no file in
// this repository recorded, so every clean deploy.sh run — by anyone, for any
// reason — reverted it in silence, and the symptom was `hivemind: No such
// command` in a sandbox created days later. Three properties keep that from
// coming back: the flag exists, the manifest carries the value through, and the
// default URL is written down in exactly ONE place.
func TestCKSGuestHivemindIsAFlagNotAHandEdit(t *testing.T) {
	deployScript, err := os.ReadFile("kubernetes/deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(deployScript)
	for _, want := range []string{
		"--hivemind-manifest)",
		"requested_hivemind_manifest=",
		// Dropping the pin is allowed. Dropping it quietly is not.
		"NOTE: dropping the guest hivemind pin",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("deploy.sh missing %q", want)
		}
	}

	manifest, err := os.ReadFile("kubernetes/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "name: HIVEMIND_MANIFEST") {
		t.Error("the node manifest never passes HIVEMIND_MANIFEST to prepare-vm-assets")
	}

	// One source of truth for the default. deploy.sh substitutes an empty value
	// when the flag is absent, and refresh-agent-tools.sh's ${VAR:-default}
	// covers empty as well as unset — so the latest-release URL must appear
	// there and nowhere else. Two copies is how they drift.
	if strings.Contains(script, "hivemind-latest.json") {
		t.Error("deploy.sh hard-codes the default manifest URL; refresh-agent-tools.sh already owns it")
	}
	if !strings.Contains(string(RefreshToolsScript), "HIVEMIND_MANIFEST:-https://") {
		t.Error("refresh-agent-tools.sh no longer defaults HIVEMIND_MANIFEST, so an unpinned deploy resolves nothing")
	}
}

func TestCKSImageRefreshesTheCanonicalGuestMOTD(t *testing.T) {
	containerfile := string(readFile(t, "kubernetes/Containerfile"))
	entrypoint := string(readFile(t, "kubernetes/entrypoint.sh"))
	refresher := string(RefreshToolsScript)

	for name, pair := range map[string][2]string{
		"container image": {containerfile, "COPY sparkbox/images/motd /usr/local/share/sparkbox/motd"},
		"prepare step":    {entrypoint, "GUEST_MOTD_FILE=/usr/local/share/sparkbox/motd"},
		"template stamp":  {refresher, `WANT="$WANT motd=$MOTD_SHA"`},
		"guest installer": {refresher, `GUEST_MOTD_FILE="$GUEST_MOTD_FILE" "$GUEST_IDENTITY" "$MNT"`},
	} {
		if !strings.Contains(pair[0], pair[1]) {
			t.Errorf("%s does not carry the canonical MOTD through the CKS template refresh: missing %q", name, pair[1])
		}
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
	for _, n := range publicports.CommonHTTPS() {
		port := strconv.Itoa(n)
		for _, want := range []string{"name: https-" + port, "port: " + port} {
			if !strings.Contains(got, want) {
				t.Errorf("CKS Service missing common HTTPS mapping %q", want)
			}
		}
	}
	if !strings.Contains(got, "name: http\n") || !strings.Contains(got, "port: 80\n") {
		t.Fatal("CKS Service missing public HTTP redirect port 80")
	}
	wantMappings := len(publicports.CommonHTTPS()) + 2 // HTTP redirect and default HTTPS.
	if strings.Count(got, "targetPort: https") != wantMappings {
		t.Fatalf("HTTP(S) mappings = %d, want %d (ports 80/443 plus common HTTPS ports)",
			strings.Count(got, "targetPort: https"), wantMappings)
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

// ---------------------------------------------------------------------------
// The in-guest lifecycle verbs, driven for real
// ---------------------------------------------------------------------------

// guestSelfService installs the payload and returns a runner for the INSTALLED
// /usr/local/bin/sparkbox against a stubbed metadata endpoint.
//
// This is the half `go test ./...` cannot otherwise reach. The exit-code table
// and the "never print success when it was refused" rule live entirely in
// POSIX sh, and both are frozen into every template this feature captures — a
// box forked from one can never be fixed in place — so they are pinned here by
// running the script rather than by reading it.
//
// reply(method, code, headers, body) stages what the fake curl answers.
func guestSelfService(t *testing.T) (
	reply func(method, code, headers, body string),
	requests func() []string,
	run func(args ...string) (stdout, stderr string, code int),
) {
	t.Helper()
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)

	fixtures := t.TempDir()
	log := filepath.Join(fixtures, "requests.log")
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "ip"), `#!/bin/sh
[ "$1" = -4 ] && echo "default via 10.0.0.1 dev eth0"
`)
	// Honours the exact call shape _call makes: -D headers, -o body,
	// -w '%{http_code}', -X METHOD. A staged code of 000 stands for a
	// connection that died, which real curl reports by exiting non-zero.
	writeExecutable(t, filepath.Join(bin, "curl"), `#!/bin/sh
out=; hdr=; method=GET; url=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -D) hdr=$2; shift 2 ;;
    -X) method=$2; shift 2 ;;
    http*) url=$1; shift ;;
    *) shift ;;
  esac
done
echo "$method $url" >> "$SPARKBOX_TEST_LOG"
d=$SPARKBOX_TEST_DIR/$method
code=$(cat "$d.code" 2>/dev/null || echo 200)
[ -n "$hdr" ] && [ -f "$d.headers" ] && cp "$d.headers" "$hdr"
if [ -f "$d.body" ]; then
  if [ -n "$out" ]; then cp "$d.body" "$out"; else cat "$d.body"; fi
fi
if [ "$code" = 000 ]; then exit 7; fi
printf '%s' "$code"
`)

	reply = func(method, code, headers, body string) {
		t.Helper()
		for suffix, content := range map[string]string{".code": code, ".headers": headers, ".body": body} {
			if err := os.WriteFile(filepath.Join(fixtures, method+suffix), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	requests = func() []string {
		body, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		return strings.Fields(strings.TrimSpace(strings.ReplaceAll(string(body), "\n", " ")))
	}
	run = func(args ...string) (string, string, int) {
		t.Helper()
		cmd := exec.Command("sh", append([]string{filepath.Join(root, "usr/local/bin/sparkbox")}, args...)...)
		// Prepend, never replace: the script also runs sed, tr and sync.
		cmd.Env = append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"),
			"SPARKBOX_TEST_DIR="+fixtures,
			"SPARKBOX_TEST_LOG="+log)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		exit := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run sparkbox %v: %v", args, err)
		}
		return stdout.String(), stderr.String(), exit
	}
	return reply, requests, run
}

const guestPlanHeaders = "HTTP/1.1 200 OK\r\n" +
	"Sparkbox-Tag: web\r\n" +
	"Sparkbox-Snapshot: web-260829-1412\r\n" +
	"Sparkbox-Plan: tok-abc\r\n" +
	"Sparkbox-Ctl: ssh ctl@catnip.sh\r\n\r\n"

const guestPlanBody = "\n  this sandbox   quiet-lake   tags: default, web\n  capture as     web-260829-1412   (new)\n"

func TestGuestCanAuthorizeOneRepository(t *testing.T) {
	reply, requests, run := guestSelfService(t)
	reply("POST", "200", "HTTP/1.1 200 OK\r\n\r\n",
		`{"id":"abcdefghijklmnopqrstuvwxyz123456","user_code":"ABCD-EFGH","verification_uri":"https://github.com/login/device","interval_seconds":1}`)
	reply("GET", "200", "HTTP/1.1 200 OK\r\n\r\n",
		`{"state":"authorized","slug":"wandb/hivemind"}`)

	stdout, stderr, code := run("repo", "authorize", "wandb/hivemind")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"ABCD-EFGH", "https://github.com/login/device", "Authorized wandb/hivemind"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	got := requests()
	if len(got) != 4 || got[0] != "POST" || !strings.Contains(got[1], "/github/authorization?slug=wandb/hivemind") ||
		got[2] != "GET" || !strings.Contains(got[3], "/github/authorization/abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("requests = %v", got)
	}
}

// TestGuestCaptureSendsThePlansOwnValuesBack: the commit must carry the tag,
// the name and the token the PLAN reported, never anything re-derived in the
// shell. The derived name has a minute in it, so a slow prompt is all it would
// take to capture under a name nobody was shown.
func TestGuestCaptureSendsThePlansOwnValuesBack(t *testing.T) {
	reply, requests, run := guestSelfService(t)
	reply("GET", "200", guestPlanHeaders, guestPlanBody)
	reply("POST", "202", "HTTP/1.1 202 Accepted\r\n\r\n",
		"accepted — capturing quiet-lake as web-260829-1412, then binding `web` to it.\n")

	stdout, stderr, code := run("snapshot", "web", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The host's own body, both halves, printed verbatim.
	for _, want := range []string{"this sandbox   quiet-lake", "flushing writes… ok", "accepted — capturing"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	// The happy path must NOT print a transport error. Before the host learned
	// to answer before it pauses, this is exactly what it printed instead.
	if stderr != "" {
		t.Errorf("the happy path wrote to stderr:\n%s", stderr)
	}
	got := requests()
	if len(got) != 4 || got[0] != "GET" || got[2] != "POST" {
		t.Fatalf("requests = %v, want a GET plan then a POST commit", got)
	}
	if !strings.Contains(got[1], "/self/snapshot?tag=web&name=") {
		t.Errorf("the plan was not asked for: %s", got[1])
	}
	for _, want := range []string{"tag=web", "name=web-260829-1412", "plan=tok-abc"} {
		if !strings.Contains(got[3], want) {
			t.Errorf("the commit URL %s does not carry the plan's %s", got[3], want)
		}
	}
}

// TestGuestCaptureWantsATerminalToConfirmAt. The thing being warned about is the
// destruction of the terminal displaying the warning, so with nobody there to
// read it the answer is no. It costs a legitimate script one flag.
func TestGuestCaptureWantsATerminalToConfirmAt(t *testing.T) {
	reply, requests, run := guestSelfService(t)
	reply("GET", "200", guestPlanHeaders, guestPlanBody)

	stdout, stderr, code := run("snapshot", "web")
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The warnings still print, so the session log holds the record.
	if !strings.Contains(stdout, "this sandbox   quiet-lake") {
		t.Errorf("the plan was not printed:\n%s", stdout)
	}
	if !strings.Contains(stderr, "Re-run with --yes if you meant it") ||
		!strings.Contains(stderr, "sparkbox snapshot web --yes") {
		t.Errorf("stderr does not name the flag or the tag:\n%s", stderr)
	}
	if got := requests(); len(got) != 2 || got[0] != "GET" {
		t.Errorf("requests = %v — nothing may be committed without a confirmation", got)
	}
}

// TestGuestVerbsPrintTheHostsOwnRefusal is the reason these two verbs drop
// `curl -f`: -f throws the body away, and the body is the only place the host's
// explanation exists. Every other verb still prints curl's generic "returned
// error: 409" instead of the reason.
func TestGuestVerbsPrintTheHostsOwnRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, status, body string
		wantExit           int
	}{
		{"invalid", "400", "sparkbox: invalid tag \"Web_1\" (want [a-z0-9][a-z0-9-]*, max 40 chars)\n", 2},
		{"denied", "403", "sparkbox: quiet-lake does not carry the tag `cuda`.\n", 3},
		{"conflict", "409", "sparkbox: a snapshot named \"web-260829-1412\" already exists.\n", 4},
		{"rate limited", "429", "sparkbox: too many captures from this sandbox (3 per hour).\n", 4},
		{"unsupported", "501", "sparkbox: this host cannot capture templates.\n", 5},
		{"gateway down", "503", "sparkbox: the gateway that owns your tags is not reachable right now.\n", 75},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, requests, run := guestSelfService(t)
			reply("GET", tc.status, "HTTP/1.1 "+tc.status+"\r\n\r\n", tc.body)

			stdout, stderr, code := run("snapshot", "cuda", "--yes")
			if code != tc.wantExit {
				t.Errorf("exit = %d, want %d", code, tc.wantExit)
			}
			if stderr != tc.body {
				t.Errorf("stderr = %q, want the host's sentence %q", stderr, tc.body)
			}
			if stdout != "" {
				t.Errorf("a refusal printed to stdout: %q", stdout)
			}
			if got := requests(); len(got) != 2 {
				t.Errorf("requests = %v — a refused plan must not be committed", got)
			}
		})
	}
}

// TestGuestVerbsNeverReportSuccessAfterATransportFailure. This is the one case
// the guest genuinely cannot resolve — the reply was written and lost, or never
// written — and the whole requirement is that it claims nothing and exits
// non-zero.
func TestGuestVerbsNeverReportSuccessAfterATransportFailure(t *testing.T) {
	for _, verb := range [][]string{{"pause"}, {"snapshot", "web", "--yes"}} {
		t.Run(verb[0], func(t *testing.T) {
			reply, _, run := guestSelfService(t)
			reply("GET", "000", "", "")
			reply("POST", "000", "", "")

			stdout, stderr, code := run(verb...)
			if code != 75 {
				t.Errorf("exit = %d, want 75", code)
			}
			if !strings.Contains(stderr, "stopped answering before it confirmed") {
				t.Errorf("stderr does not say the outcome is unknown:\n%s", stderr)
			}
			// The guest does not know its own domain, so the fallback that gets
			// printed when no reply arrived has to be a placeholder rather than a
			// guess at a gateway name.
			if !strings.Contains(stderr, "ssh ctl@<gateway> snapshot ls") {
				t.Errorf("stderr does not point at where the truth is:\n%s", stderr)
			}
			if strings.Contains(stdout, "accepted") || strings.Contains(stdout, "pausing") {
				t.Errorf("it reported success after the connection died:\n%s", stdout)
			}
		})
	}
}

// TestGuestPauseIsAcknowledgedBeforeItStops: the host answers first and the
// guest prints that answer. The verb exists precisely so a person does not have
// to read a curl error and guess whether their box stopped.
func TestGuestPauseIsAcknowledgedBeforeItStops(t *testing.T) {
	reply, requests, run := guestSelfService(t)
	reply("POST", "202", "HTTP/1.1 202 Accepted\r\n\r\n",
		"pausing quiet-lake — memory and processes are snapshotted, so\n`ssh quiet-lake.catnip.sh` picks up exactly here.\n")

	stdout, stderr, code := run("pause")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "pausing quiet-lake") {
		t.Errorf("stdout = %q", stdout)
	}
	if got := requests(); len(got) != 2 || got[0] != "POST" || !strings.HasSuffix(got[1], "/self/pause") {
		t.Errorf("requests = %v", got)
	}
}

func TestGuestSnapshotUsageIsRefusedWithoutAsking(t *testing.T) {
	for _, args := range [][]string{{"snapshot", "--wat"}, {"snapshot", "web", "a", "b"}} {
		reply, requests, run := guestSelfService(t)
		reply("GET", "200", guestPlanHeaders, guestPlanBody)
		_, stderr, code := run(args...)
		if code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "usage: sparkbox snapshot [--yes] [--allow-busy] [TAG [NAME]]") {
			t.Errorf("%v: stderr = %q", args, stderr)
		}
		if got := requests(); len(got) != 0 {
			t.Errorf("%v: a malformed invocation reached the host: %v", args, got)
		}
	}
}

// ---------------------------------------------------------------------------
// `sparkbox docs`
// ---------------------------------------------------------------------------

// TestGuestDocsReadsFromMetadataNotTheEdge is the whole point of the verb:
// docs.<domain> is a public DNS name that can resolve to this fleet's own
// edge, which this VM's own tap firewall has no route to reach directly (only
// DNS and the metadata port are open guest-to-host). So `sparkbox docs` reads
// the same content over the metadata port instead of ever touching
// https://docs.<domain>.
func TestGuestDocsReadsFromMetadataNotTheEdge(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		wantPath string
	}{
		{[]string{"docs"}, "/docs/docs.md"},
		{[]string{"docs", "proxy"}, "/docs/proxy.md"},
		{[]string{"docs", "dev-environment"}, "/docs/dev-environment.md"},
	} {
		reply, requests, run := guestSelfService(t)
		reply("GET", "200", "HTTP/1.1 200 OK\r\n\r\n", "# doc body\n")

		stdout, stderr, code := run(tc.args...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: exit = %d, stderr = %q", tc.args, code, stderr)
		}
		if !strings.Contains(stdout, "# doc body") {
			t.Errorf("%v: stdout = %q", tc.args, stdout)
		}
		// The fake `ip` stub above reports the guest's gateway as 10.0.0.1, so a
		// request to the real edge hostname would show up as something other
		// than that address — this asserts the request went to $META, not
		// https://docs.<domain>.
		got := requests()
		if len(got) != 2 || got[0] != "GET" || got[1] != "http://10.0.0.1:8967"+tc.wantPath {
			t.Errorf("%v: requests = %v, want a GET of %q", tc.args, got, "http://10.0.0.1:8967"+tc.wantPath)
		}
	}
}

// TestGuestDocsRejectsAnUnrecognizedPageWithoutAsking mirrors the snapshot
// usage test: a malformed page name is refused locally, and never reaches the
// host as a path-traversal attempt or similar. What it now gets back is the
// table of contents rather than a one-line usage string — an unknown page is
// almost always somebody who has forgotten the page names, so the answer is
// the list.
func TestGuestDocsRejectsAnUnrecognizedPageWithoutAsking(t *testing.T) {
	for _, page := range []string{"../../etc/passwd", "docs.md", "a/b", "a b", "wat"} {
		reply, requests, run := guestSelfService(t)
		reply("GET", "200", "HTTP/1.1 200 OK\r\n\r\n", "# doc body\n")
		_, stderr, code := run("docs", page)
		if code != 2 {
			t.Errorf("docs %q: exit = %d, want 2", page, code)
		}
		if !strings.Contains(stderr, "no such doc page: "+page) {
			t.Errorf("docs %q: stderr does not name the page: %q", page, stderr)
		}
		for _, want := range []string{"sparkbox docs proxy", "sparkbox docs dev-environment"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("docs %q: stderr is missing %q: %q", page, want, stderr)
			}
		}
		if got := requests(); len(got) != 0 {
			t.Errorf("docs %q: a malformed page name reached the host: %v", page, got)
		}
	}
}

// TestGuestDocsHelpPrintsContentsWithoutAskingTheHost. `sparkbox docs help` and
// `sparkbox docs --help` are what somebody types when they have forgotten the
// page names. They used to be handed to curl as page names, which fetched
// /docs/help.md and /docs/--help.md and answered with curl's 404 — so the two
// most likely spellings of the question were the two that could not answer it.
//
// They are answered locally on purpose: the moment this is worth printing is
// the moment somebody is lost, and a contents page that needed the metadata
// service to explain the metadata service would be no use.
func TestGuestDocsHelpPrintsContentsWithoutAskingTheHost(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		reply, requests, run := guestSelfService(t)
		reply("GET", "200", "HTTP/1.1 200 OK\r\n\r\n", "# doc body\n")
		stdout, stderr, code := run("docs", arg)
		// Asked for on purpose, so it is success on stdout — the same contract
		// `sparkbox --help` has.
		if code != 0 || stderr != "" {
			t.Errorf("docs %q: exit = %d, stderr = %q", arg, code, stderr)
		}
		for _, want := range []string{
			"sparkbox docs", "sparkbox docs proxy", "sparkbox docs dev-environment",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("docs %q: stdout is missing %q: %q", arg, want, stdout)
			}
		}
		// A guest is never told its own domain, so the contents must not invent
		// one — it points at `sparkbox whoami` instead.
		if strings.Contains(stdout, "catnip.sh") || strings.Contains(stdout, "coreweave.app") {
			t.Errorf("docs %q: the contents hardcode a domain: %q", arg, stdout)
		}
		if got := requests(); len(got) != 0 {
			t.Errorf("docs %q: the contents asked the host: %v", arg, got)
		}
	}
}

// TestGuestDocsContentsListsEveryPageThatIsServed is the drift guard. The
// contents are spelled out in the shell — deliberately, because the moment they
// are worth printing is the moment somebody is lost and a contents page that
// needed the network would be no use — so the list has to be pinned against the
// pages internal/guestdocs actually serves. A fourth page cannot be added
// without this failing.
func TestGuestDocsContentsListsEveryPageThatIsServed(t *testing.T) {
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)
	cli := guestFile(t, root, "usr/local/bin/sparkbox")

	contents := cli[strings.Index(cli, "sparkbox_docs_toc() {"):]
	contents = contents[:strings.Index(contents, "\nTOC\n}")]
	if contents == "" {
		t.Fatal("the guest CLI has no sparkbox_docs_toc")
	}

	served, err := filepath.Glob(filepath.Join("..", "internal", "guestdocs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(served) == 0 {
		t.Fatal("no markdown pages found in internal/guestdocs")
	}
	for _, path := range served {
		page := strings.TrimSuffix(filepath.Base(path), ".md")
		if !strings.Contains(contents, "sparkbox docs "+page) && page != "docs" {
			t.Errorf("internal/guestdocs serves %q but `sparkbox docs help` does not list it", page)
		}
		// Every served page must also be reachable, not merely advertised.
		if !strings.Contains(cli, page+"|") && !strings.Contains(cli, "|"+page+")") &&
			!strings.Contains(cli, "|"+page+"|") {
			t.Errorf("internal/guestdocs serves %q but the guest CLI allowlist omits it", page)
		}
	}
}

// ---------------------------------------------------------------------------
// `sparkbox whoami`
// ---------------------------------------------------------------------------

// guestIdentityDoc is the host's answer, pretty-printed exactly as the metadata
// service serves it — SetIndent("", "  "), so every colon is followed by a
// space. That whitespace is the detail that has already broken one reader here
// (see sparkbox-git-identity), which is why the fixture carries it rather than
// the compact form a hand-written test would reach for.
const guestIdentityDoc = `{
  "iss": "https://oidc.catnip.sh",
  "sub": "sandbox:quiet-lake",
  "owner": "alice",
  "github": "alice-gh",
  "key_fp": "SHA256:x",
  "sandbox": "quiet-lake",
  "sandbox_id": "sbx_123",
  "image": "default",
  "box": "quiet-lake",
  "github_id": 271676
}`

// TestGuestWhoamiAnswersWhatGhCannot. `gh api user` inside a sandbox is a 403:
// the credential the box carries is a GitHub App INSTALLATION token, which has
// no authenticated user behind it, and no permission grant invents one. The
// fact is on the host, from the account link, so the verb reads it from there —
// and prints it in a shape a script can cut on, because the thing asking is
// usually an agent.
func TestGuestWhoamiAnswersWhatGhCannot(t *testing.T) {
	reply, requests, run := guestSelfService(t)
	reply("GET", "200", "HTTP/1.1 200 OK\r\n\r\n", guestIdentityDoc)

	stdout, stderr, code := run("whoami")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"github: alice-gh\n",
		"github_id: 271676\n",
		"owner: alice\n",
		"sandbox: quiet-lake\n",
		"domain: catnip.sh\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("whoami did not report %q:\n%s", want, stdout)
		}
	}
	// The account number is the half that makes an answer usable: it is what
	// attributes a commit, and it is what a relying party should match on
	// instead of a renameable login. A reader that matched "github" inside
	// "github_id" would report the number as the login.
	if strings.Contains(stdout, "github: 271676") {
		t.Errorf("the login reader matched the account number:\n%s", stdout)
	}
	if got := requests(); len(got) != 2 || got[0] != "GET" || !strings.HasSuffix(got[1], "/identity") {
		t.Errorf("requests = %v — whoami must read /identity, which mints nothing", got)
	}
}

// TestGuestWhoamiDerivesDomainFromIssuerNotAHardcodedSubdomain: main.go's
// IssuerURL is "https://" + oidc-subdomain + "." + proxy-domain, and the
// subdomain is a deployment flag (--oidc-subdomain, default "oidc"), not a
// constant. The domain line has to survive a deployment that renamed it,
// because nothing else in this script may hardcode a domain (see AGENTS.md).
func TestGuestWhoamiDerivesDomainFromIssuerNotAHardcodedSubdomain(t *testing.T) {
	doc := strings.Replace(guestIdentityDoc, `"iss": "https://oidc.catnip.sh"`,
		`"iss": "https://auth.example.internal"`, 1)
	reply, _, run := guestSelfService(t)
	reply("GET", "200", "HTTP/1.1 200 OK\r\n\r\n", doc)

	stdout, stderr, code := run("whoami")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "domain: example.internal\n") {
		t.Errorf("whoami did not derive the domain from a renamed oidc subdomain:\n%s", stdout)
	}
}

// TestGuestWhoamiJSONIsTheHostsOwnDocument: --json passes the host's answer
// through verbatim rather than reassembling it, so a field this shell reader
// does not know about still reaches whatever asked.
func TestGuestWhoamiJSONIsTheHostsOwnDocument(t *testing.T) {
	reply, _, run := guestSelfService(t)
	reply("GET", "200", "HTTP/1.1 200 OK\r\n\r\n", guestIdentityDoc)

	stdout, stderr, code := run("whoami", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if strings.TrimSpace(stdout) != guestIdentityDoc {
		t.Errorf("--json did not pass the host's document through:\n%s", stdout)
	}
	if strings.Contains(stdout, "github: ") {
		t.Errorf("--json also printed the human form, so nothing can parse it:\n%s", stdout)
	}
}

// TestGuestWhoamiFallsBackToTheIdentityOnDisk. The live read is preferred
// because a GitHub account linked after this box booted is only in the host's
// answer — but a host that cannot be reached must not turn "who am I" into an
// error, when sparkbox-token already left the answer on disk.
func TestGuestWhoamiFallsBackToTheIdentityOnDisk(t *testing.T) {
	onDisk := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(onDisk, []byte(guestIdentityDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPARKBOX_IDENTITY_FILE", onDisk)

	reply, _, run := guestSelfService(t)
	reply("GET", "000", "", "") // the gateway did not answer

	stdout, stderr, code := run("whoami")
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "github: alice-gh") || !strings.Contains(stdout, "github_id: 271676") {
		t.Errorf("the on-disk snapshot was not read:\n%s", stdout)
	}
}

// TestGuestWhoamiRefusesToInventALogin. An owner with no GitHub link has no
// GitHub login, and the sparkbox handle is not one: handles and GitHub logins
// are separate namespaces, so answering with the handle would hand a stranger's
// account whatever the caller does next. Exit non-zero for the same reason —
// a script reading an empty login as this person's login is the failure mode
// worth spending an exit code on.
func TestGuestWhoamiRefusesToInventALogin(t *testing.T) {
	t.Setenv("SPARKBOX_IDENTITY_FILE", filepath.Join(t.TempDir(), "absent.json"))
	reply, _, run := guestSelfService(t)
	reply("GET", "200", "HTTP/1.1 200 OK\r\n\r\n",
		"{\n  \"owner\": \"alice\",\n  \"sandbox\": \"quiet-lake\"\n}")

	stdout, stderr, code := run("whoami")
	if code != 1 {
		t.Errorf("exit = %d, want 1 — no login is not success", code)
	}
	if strings.Contains(stdout, "github:") || strings.Contains(stdout, "alice-gh") {
		t.Errorf("whoami named a GitHub account that is not linked:\n%s", stdout)
	}
	if !strings.Contains(stdout, "owner: alice") {
		t.Errorf("whoami dropped what it does know:\n%s", stdout)
	}
	if !strings.Contains(stderr, "no GitHub account is linked to alice") {
		t.Errorf("stderr does not say why there is no answer:\n%s", stderr)
	}
	// A guest is never told its own domain, so the pointer at the fix has to be
	// a placeholder rather than a guess at a gateway name.
	if !strings.Contains(stderr, "ssh ctl@<gateway> github link") {
		t.Errorf("stderr does not point at how to link one:\n%s", stderr)
	}
}

// TestGuestWhoamiSaysNothingWhenItKnowsNothing: no host, no file, no claim.
func TestGuestWhoamiSaysNothingWhenItKnowsNothing(t *testing.T) {
	t.Setenv("SPARKBOX_IDENTITY_FILE", filepath.Join(t.TempDir(), "absent.json"))
	reply, _, run := guestSelfService(t)
	reply("GET", "000", "", "")

	stdout, stderr, code := run("whoami")
	if code != 75 {
		t.Errorf("exit = %d, want 75 (temporary or ambiguous)", code)
	}
	if stdout != "" {
		t.Errorf("it answered anyway: %q", stdout)
	}
	if !strings.Contains(stderr, "could not read this sandbox's identity") {
		t.Errorf("stderr = %q", stderr)
	}
}

// TestGuestForkIdentityResetRunsAgainstATree exercises the installed script the
// way the guest does, because every property worth pinning is a property of
// what it does to a filesystem rather than of the text that produces it.
//
// This is the hook that replaced the host's loop mount. Capturing a template
// used to end with the host mounting the captured ext4 and deleting these same
// paths, and that mount is what CKS refuses outright — so the whole snapshot
// feature was refused with it. Nothing here needs host enforcement: a template
// is bound (owner, tag) and masked from every other owner, so the only person
// who boots it is the person who made it. See docs/cks-snapshot-design.md.
func TestGuestForkIdentityResetRunsAgainstATree(t *testing.T) {
	root := fakeGuestTree(t, true)
	installGuestPayload(t, root)
	script := filepath.Join(root, "usr/local/sbin/sparkbox-identity-reset")

	// guest builds a rootfs already carrying an identity, and cmdline is what
	// the HOST says this boot is — the two inputs whose disagreement is the
	// whole signal.
	type guest struct{ dir string }
	newGuest := func(t *testing.T, stamp string) guest {
		t.Helper()
		dir := t.TempDir()
		for _, sub := range []string{"etc/ssh", "var/lib/dbus", "var/lib/sparkbox"} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write := func(rel, body string) {
			if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("etc/ssh/ssh_host_ed25519_key", "PARENT PRIVATE KEY")
		write("etc/ssh/ssh_host_rsa_key", "PARENT PRIVATE KEY")
		write("etc/machine-id", "00000000000000000000000000000001\n")
		write("var/lib/dbus/machine-id", "00000000000000000000000000000001\n")
		if stamp != "" {
			write("var/lib/sparkbox/sandbox", stamp+"\n")
		}
		return guest{dir: dir}
	}
	run := func(t *testing.T, g guest, cmdline string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "cmdline")
		if err := os.WriteFile(path, []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", script)
		cmd.Env = append(os.Environ(), "SPARKBOX_ROOT="+g.dir, "SPARKBOX_CMDLINE="+path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("identity reset: %v\n%s", err, out)
		}
	}
	hostKeys := func(t *testing.T, g guest) []string {
		t.Helper()
		found, err := filepath.Glob(filepath.Join(g.dir, "etc/ssh/ssh_host_*"))
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	const forkCmdline = "console=ttyS0 quiet sparkbox_host=forked-box systemd.machine_id=aabbccddeeff00112233445566778899"

	t.Run("a fork sheds the identity it inherited", func(t *testing.T) {
		g := newGuest(t, "parent-box")
		run(t, g, forkCmdline)

		// Replaced, not merely deleted: the script generates the new pair
		// itself rather than trusting a later unit to notice they are missing.
		// A fork with NO host keys is an unreachable box — sshd exits on "no
		// hostkeys available" — which is a worse outcome than the one this
		// whole hook exists to prevent.
		keys := hostKeys(t, g)
		if len(keys) == 0 {
			t.Fatal("a fork was left with no SSH host keys at all; sshd exits on \"no hostkeys available\" and the sandbox is unreachable")
		}
		for _, k := range keys {
			if strings.Contains(string(readFile(t, k)), "PARENT PRIVATE KEY") {
				t.Errorf("%s still holds the parent's key material; anyone who can reach both boxes can impersonate either", k)
			}
		}
		if got := guestFile(t, g.dir, "etc/machine-id"); got != "aabbccddeeff00112233445566778899\n" {
			t.Errorf("machine-id = %q, want the one the host put on the cmdline", got)
		}
		if _, err := os.Stat(filepath.Join(g.dir, "var/lib/dbus/machine-id")); !os.IsNotExist(err) {
			t.Errorf("the dbus machine id survived the fork (%v); dbus regenerates it only when it is absent", err)
		}
		if got := guestFile(t, g.dir, "var/lib/sparkbox/sandbox"); got != "forked-box\n" {
			t.Errorf("stamp = %q, want the name this boot was given", got)
		}
	})

	t.Run("a resume changes nothing", func(t *testing.T) {
		// The overwhelmingly common boot. Regenerating here would hand somebody
		// a host-key warning every time their sandbox woke up.
		g := newGuest(t, "forked-box")
		run(t, g, forkCmdline)

		if keys := hostKeys(t, g); len(keys) != 2 {
			t.Errorf("a resume regenerated host keys (%v left); every wake would then look like a MITM to ssh", keys)
		}
		if got := guestFile(t, g.dir, "etc/machine-id"); got != "00000000000000000000000000000001\n" {
			t.Errorf("a resume rewrote machine-id to %q", got)
		}
	})

	t.Run("no marker leaves the guest alone", func(t *testing.T) {
		// An older host, or a boot this cannot reason about. Doing nothing costs
		// a fork a shared host key it would have had anyway before this existed;
		// resetting on a guess costs a running sandbox its identity.
		g := newGuest(t, "parent-box")
		run(t, g, "console=ttyS0 quiet")

		if keys := hostKeys(t, g); len(keys) != 2 {
			t.Errorf("host keys were reset with no sparkbox_host= to compare against (%v left)", keys)
		}
		if got := guestFile(t, g.dir, "var/lib/sparkbox/sandbox"); got != "parent-box\n" {
			t.Errorf("stamp = %q, want it untouched", got)
		}
	})

	t.Run("a first boot with no stamp at all is a fork", func(t *testing.T) {
		// Templates captured before this hook existed have no stamp, and they
		// are exactly the disks most likely to be carrying somebody else's keys.
		g := newGuest(t, "")
		run(t, g, forkCmdline)

		for _, k := range hostKeys(t, g) {
			if strings.Contains(string(readFile(t, k)), "PARENT PRIVATE KEY") {
				t.Errorf("%s still holds the key material the template shipped with", k)
			}
		}
	})
}

// The unit runs before sshd, and before the unit that would otherwise generate
// what it removes.
//
// The script no longer DEPENDS on that second ordering — it makes its own keys,
// precisely so the worst failure here cannot be a quiet one — but the ordering
// is still what keeps the two from doing the same work twice, and
// Before=ssh.service remains load-bearing on its own: a fork must never be
// reachable under the identity it inherited, not even for the seconds between
// sshd binding and this unit running.
func TestGuestForkIdentityResetIsOrderedBeforeKeyGeneration(t *testing.T) {
	root := fakeGuestTree(t, true)
	installGuestPayload(t, root)

	unit := guestFile(t, root, "etc/systemd/system/sparkbox-identity-reset.service")
	for _, want := range []string{
		"Before=sparkbox-net.service ssh.service sshd.service",
		"DefaultDependencies=no",
		"ExecStart=/usr/local/sbin/sparkbox-identity-reset",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the identity reset unit is missing %q:\n%s", want, unit)
		}
	}
	// A unit nothing enables is a unit that never runs, and the failure is
	// invisible: forks simply keep their parent's identity.
	link := filepath.Join(root, "etc/systemd/system/multi-user.target.wants/sparkbox-identity-reset.service")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the identity reset unit is not enabled: %v", err)
	}
}

// The environment build runner, driven for real.
//
// `ctl env build <name>` creates a builder sandbox, nudges the unit below, and
// then waits: the environment row sits in `building` until this worker posts a
// result. So the properties worth pinning are behavioural rather than textual —
// the script runs in the checkout, with the owner's environment, as the person
// and not as root; and a report comes back on every path a 200 can take.
//
// The stub curl serves the job and captures the report, exactly as the repo
// worker's stub serves a manifest: no network, no VM, no metadata service.
type envWorld struct {
	t     *testing.T
	root  string // the guest tree
	fix   string // job, job.code, requests and the captured report
	stub  string // the ip/curl stubs
	work  string // the checkout the setup is expected to run in
	extra []string
}

func newEnvWorld(t *testing.T) *envWorld {
	t.Helper()
	root := fakeGuestTree(t, true)
	w := &envWorld{
		t:    t,
		root: root,
		fix:  t.TempDir(),
		stub: t.TempDir(),
		work: filepath.Join(root, "home/sparky/proj"),
	}
	for _, dir := range []string{w.work, filepath.Join(root, "run/sparkbox")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// What sparkbox-repos publishes when the sandbox has one checkout, and the
	// owner's environment as internal/envsync's PushEnv leaves it: KEY="value"
	// lines, PATH included.
	w.write(filepath.Join(root, "run/sparkbox/repos.dir"), w.work+"\n")
	// The stub directory is on the PATH that /etc/environment sets, because in a
	// real guest the agent binary lives at /usr/local/bin/claude and that IS on
	// this PATH. The distinction matters: the worker sources /etc/environment
	// INSIDE the unprivileged child, so this PATH — not the one the test process
	// exports — is what a setup script or the gateway's agent runner resolves
	// against.
	w.write(filepath.Join(root, "etc/environment"),
		"PATH=\""+w.stub+":/usr/bin:/bin:/usr/local/bin\"\nSETUP_SECRET=\"s3cr3t\"\n")
	installGuestPayload(t, root)

	writeExecutable(t, filepath.Join(w.stub, "ip"), `#!/bin/sh
[ "$1" = -4 ] && echo "default via 10.0.0.1 dev eth0"
`)
	// A stub `claude`, ALWAYS installed, and its presence is not only about
	// convenience: the worker resolves the agent binary with `command -v
	// claude`, so without a stub on PATH an agent-mode test would find and run
	// the REAL claude on whatever machine is running `go test` — network, quota
	// and all — which is how this test first discovered agent mode worked.
	//
	// It records its argv so a test can assert the invocation, and it is driven
	// by files rather than by flags so a subtest can describe the agent's
	// behaviour without re-writing the stub: claude-exit is the status it exits
	// with, and claude-writes is the .sparkbox/setup.sh it leaves behind (none,
	// if the file is absent — which is the measured real-world case of an agent
	// that is denied every tool call and still exits 0).
	//
	// It also counts its calls, because the runner invokes an agent TWICE when
	// the script it gets does not run: claude-writes-1 and claude-writes-2
	// override claude-writes for one call each, which is how a test describes
	// an agent that writes something broken and then fixes it. The prompt of
	// each call is kept separately, so the repair round can be asserted to
	// carry the failure it is supposed to be repairing.
	writeExecutable(t, filepath.Join(w.stub, "claude"), `#!/bin/sh
n=$(cat "$SPARKBOX_TEST_DIR/claude-calls" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$SPARKBOX_TEST_DIR/claude-calls"
for a in "$@"; do printf '%s\n' "$a"; done >> "$SPARKBOX_TEST_DIR/claude-args"
printf '%s' "$2" > "$SPARKBOX_TEST_DIR/claude-prompt-$n"
printf 'agent ran in %s\n' "$(pwd)"
writes="$SPARKBOX_TEST_DIR/claude-writes-$n"
[ -f "$writes" ] || writes="$SPARKBOX_TEST_DIR/claude-writes"
if [ -f "$writes" ]; then
  mkdir -p .sparkbox
  cp "$writes" .sparkbox/setup.sh
fi
exit "$(cat "$SPARKBOX_TEST_DIR/claude-exit" 2>/dev/null || echo 0)"
`)
	// Honours both call shapes the worker makes: the -o/-w job fetch and the
	// --data-binary report. A staged job.code of 204 is the answer every VM in
	// the fleet that is not a builder gets.
	writeExecutable(t, filepath.Join(w.stub, "curl"), `#!/bin/sh
out=; code=; data=; url=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -w) code=1; shift 2 ;;
    --data-binary) data=${2#@}; shift 2 ;;
    http*) url=$1; shift ;;
    *) shift ;;
  esac
done
echo "$url" >> "$SPARKBOX_TEST_DIR/requests"
case "$url" in
  */self/setup/result)
    if [ -n "$data" ]; then cp "$data" "$SPARKBOX_TEST_DIR/result"; fi
    exit 0 ;;
  */self/setup)
    st=$(cat "$SPARKBOX_TEST_DIR/job.code" 2>/dev/null || echo 204)
    if [ "$st" = 200 ] && [ -n "$out" ]; then cp "$SPARKBOX_TEST_DIR/job" "$out"; fi
    if [ -n "$code" ]; then printf '%s' "$st"; fi
    exit 0 ;;
  */self*)
    printf 'sandbox: quiet-lake\nstate: running\n'
    exit 0 ;;
esac
exit 0
`)
	w.noJob()
	return w
}

func (w *envWorld) write(path, body string) {
	w.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// job stages the 200 the gateway serves a builder: the environment name, the
// mode, and base64 of the script — the only encoding a guest with no JSON
// encoder can read back with sed and base64.
func (w *envWorld) job(name, mode, script string) {
	w.t.Helper()
	w.write(filepath.Join(w.fix, "job"),
		name+"\n"+mode+"\n"+base64.StdEncoding.EncodeToString([]byte(script))+"\n")
	w.write(filepath.Join(w.fix, "job.code"), "200")
}

func (w *envWorld) noJob() {
	w.t.Helper()
	w.write(filepath.Join(w.fix, "job.code"), "204")
}

func (w *envWorld) env() []string {
	return append(append(os.Environ(),
		"PATH="+w.stub+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SPARKBOX_TEST_DIR="+w.fix,
		"SPARKBOX_ENV_ROOT="+w.root,
		"SPARKBOX_ENV_SETUP_TIMEOUT=60"), w.extra...)
}

func (w *envWorld) run() (stdout, stderr string, code int) {
	w.t.Helper()
	cmd := exec.Command("sh", filepath.Join(w.root, "usr/local/sbin/sparkbox-env-setup"))
	cmd.Env = w.env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		w.t.Fatalf("run sparkbox-env-setup: %v", err)
	}
	return out.String(), errb.String(), code
}

// report is the body the worker POSTed, split into its three header lines and
// the log tail beneath them.
func (w *envWorld) report() (state, exit, script, log string) {
	w.t.Helper()
	body, err := os.ReadFile(filepath.Join(w.fix, "result"))
	if err != nil {
		w.t.Fatalf("no report was sent: %v", err)
	}
	parts := strings.SplitN(string(body), "\n", 4)
	for len(parts) < 4 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2], parts[3]
}

// read returns one of the stub-driven fixture files, which is how a test asks
// what the agent was actually told and how many times it was asked.
func (w *envWorld) read(name string) string {
	w.t.Helper()
	body, err := os.ReadFile(filepath.Join(w.fix, name))
	if err != nil {
		w.t.Fatalf("the fixture file %s was never written: %v", name, err)
	}
	return string(body)
}

// decodeScript unwraps the third line of a report, which is the setup script as
// base64 — the only encoding a guest with no JSON encoder can produce.
func decodeScript(t *testing.T, script string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(script))
	if err != nil {
		t.Fatalf("reported script is not base64: %v (%q)", err, script)
	}
	return string(raw)
}

func (w *envWorld) requests() []string {
	body, err := os.ReadFile(filepath.Join(w.fix, "requests"))
	if err != nil {
		return nil
	}
	return strings.Fields(string(body))
}

func (w *envWorld) status() string {
	body, err := os.ReadFile(filepath.Join(w.root, "run/sparkbox/env-setup.status"))
	if err != nil {
		return ""
	}
	return string(body)
}

// TestEnvSetupSaysNothingWithoutAJob is the path every VM in the fleet takes.
// The unit is startable by hand in any sandbox, and in all but a builder the
// honest answer is 204 — one request, no files, no journal line. A worker that
// wrote a status file or complained here would put an environment nobody asked
// for into every box that ever ran it.
func TestEnvSetupSaysNothingWithoutAJob(t *testing.T) {
	w := newEnvWorld(t)
	stdout, stderr, code := w.run()
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := w.requests(); len(got) != 1 || !strings.HasSuffix(got[0], "/self/setup") {
		t.Fatalf("requests = %v; a no-job run must ask once and stop", got)
	}
	if s := w.status(); s != "" {
		t.Fatalf("a no-job run wrote a status file:\n%s", s)
	}
	for _, name := range []string{"env-setup.sh", "env-setup.log", "env-setup.lock"} {
		if _, err := os.Stat(filepath.Join(w.root, "run/sparkbox", name)); err == nil {
			t.Errorf("a no-job run left %s behind", name)
		}
	}
}

// TestEnvSetupRunsTheSetupWhereThePersonWould is the whole feature in one run:
// the script arrives base64 over the tap-authenticated metadata channel, runs in
// the primary checkout with the owner's pushed environment, and the file it
// leaves behind is what comes back for the environment row to keep.
func TestEnvSetupRunsTheSetupWhereThePersonWould(t *testing.T) {
	w := newEnvWorld(t)
	w.job("webapp", "script", `echo "cwd=$PWD"
echo "secret=$SETUP_SECRET"
echo "home=$HOME"
mkdir -p .sparkbox
printf '#!/bin/sh\necho rewritten\n' > .sparkbox/setup.sh
`)
	stdout, stderr, code := w.run()
	if code != 0 {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	state, exit, script, log := w.report()
	if state != "ok" || exit != "0" {
		t.Fatalf("report = %q/%q, log:\n%s", state, exit, log)
	}
	// The checkout, not the home directory: every relative path in a
	// `.sparkbox/setup.sh` is relative to the repository it lives in.
	if want := "cwd=" + w.work; !strings.Contains(log, want) {
		t.Errorf("the setup did not run in the checkout (%q):\n%s", want, log)
	}
	// A systemd unit gets no pam_env, so without the explicit source the run
	// would have none of the owner's secrets — which is most of the reason an
	// environment carries a tag at all.
	if !strings.Contains(log, "secret=s3cr3t") {
		t.Errorf("/etc/environment never reached the setup script:\n%s", log)
	}
	if want := "home=" + filepath.Join(w.root, "home/sparky"); !strings.Contains(log, want) {
		t.Errorf("HOME was not the login user's (%q):\n%s", want, log)
	}
	// The run's own output, not the file it was asked to run: a setup script
	// that discovers what a project actually needs may write itself down again,
	// and the environment must record what it would run NEXT time.
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(script))
	if err != nil {
		t.Fatalf("reported script is not base64: %v", err)
	}
	if string(decoded) != "#!/bin/sh\necho rewritten\n" {
		t.Errorf("reported script = %q", decoded)
	}
	if got := w.status(); !strings.Contains(got, "environment: webapp") ||
		!strings.Contains(got, "state: ok") || !strings.Contains(got, "exit: 0") {
		t.Errorf("status file does not describe the finished run:\n%s", got)
	}
	// The script is staged 0700, never world-readable: it is the owner's, it can
	// carry their setup decisions, and /run/sparkbox is traversable by anyone in
	// the box.
	info, err := os.Stat(filepath.Join(w.root, "run/sparkbox/env-setup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("staged setup script mode = %v, want 0700", perm)
	}
}

// TestEnvSetupAlwaysReportsSomething: every path past the 200 ends in a POST.
// The gateway holds the row in `building` until one arrives, so a run that gives
// up quietly is a build that never finishes and a person watching a spinner.
func TestEnvSetupAlwaysReportsSomething(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env, mode  string
		script     string
		wantState  string
		wantExit   string
		wantInLog  string
		wantNoRun  bool
		expectExit int
	}{
		{
			name: "a failing script is reported with its own status",
			env:  "webapp", mode: "script",
			script:    "echo trying\nexit 7\n",
			wantState: "failed", wantExit: "7", wantInLog: "trying",
			expectExit: 0,
		},
		{
			// `agent` used to be the unknown mode this case was written around.
			// It is a mode the payload now RUNS, so the property — an unknown
			// mode is refused by name rather than guessed at — needs a value
			// that is still unknown, or the assertion quietly stops testing
			// anything. It matters because a guest older than its gateway is
			// the normal state during a rollout.
			name: "a mode this payload does not know is refused, not guessed at",
			env:  "webapp", mode: "telepathy",
			script:    "echo should-not-run\n",
			wantState: "failed", wantInLog: "telepathy", wantNoRun: true,
			expectExit: 1,
		},
		{
			name: "a job with no usable environment name is refused",
			env:  "../etc/shadow", mode: "script",
			script:    "echo should-not-run\n",
			wantState: "failed", wantInLog: "environment name", wantNoRun: true,
			expectExit: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newEnvWorld(t)
			w.job(tc.env, tc.mode, tc.script)
			_, _, code := w.run()
			if code != tc.expectExit {
				t.Errorf("exit = %d, want %d", code, tc.expectExit)
			}
			state, exit, _, log := w.report()
			if state != tc.wantState {
				t.Errorf("state = %q, want %q (log %q)", state, tc.wantState, log)
			}
			if tc.wantExit != "" && exit != tc.wantExit {
				t.Errorf("exit line = %q, want %q", exit, tc.wantExit)
			}
			if !strings.Contains(log, tc.wantInLog) {
				t.Errorf("log does not mention %q:\n%s", tc.wantInLog, log)
			}
			if tc.wantNoRun && strings.Contains(log, "should-not-run") {
				t.Errorf("the script ran anyway:\n%s", log)
			}
		})
	}
}

// TestEnvSetupAgentModeJudgesTheArtifactNotTheExitStatus is the test that stops
// agent mode shipping broken and looking fine.
//
// `claude -p` exits 0 for a run in which every tool call was DENIED — measured,
// not supposed. So a worker that reports success on rc == 0 would report a
// successful build for an agent that touched nothing, and the gateway would go
// on to capture an untouched base image as the environment's disk. Nobody
// downstream can tell the difference: the row says ready, the snapshot exists,
// and the environment is empty.
//
// The deliverable of an agent build is the setup script, so the artifact is the
// only honest signal, and these cases pin it from both sides.
//
// The payload here is a stand-in rather than the real runner; the case below
// runs the real one.
func TestEnvSetupAgentModeJudgesTheArtifactNotTheExitStatus(t *testing.T) {
	const writesIt = "mkdir -p .sparkbox\nprintf '%s' \"$AGENT_WROTE\" > .sparkbox/setup.sh\n"
	for _, tc := range []struct {
		name       string
		payload    string
		wantState  string
		wantScript string
		wantInLog  string
	}{
		{
			name:      "an agent that writes the script succeeds",
			payload:   "echo working\n" + writesIt + "exit 0\n",
			wantState: "ok", wantScript: "#!/usr/bin/env bash\nnpm ci\n",
			wantInLog: "working",
		},
		{
			name:      "an agent that exits 0 having written nothing FAILS",
			payload:   "echo I am terribly sorry, I was not permitted to do that\nexit 0\n",
			wantState: "failed", wantInLog: "without writing",
		},
		{
			name:      "an agent that fails is reported with its own status",
			payload:   "echo could not authenticate\nexit 3\n",
			wantState: "failed", wantInLog: "could not authenticate",
		},
		{
			// The script an agent wrote is reported even when the run failed,
			// for the same reason script mode reports one: it is the record of
			// what happened, and a person finishing the build by hand starts
			// from what the agent got as far as.
			name:      "a failed agent's partial script is still reported",
			payload:   writesIt + "exit 5\n",
			wantState: "failed", wantScript: "#!/usr/bin/env bash\nnpm ci\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newEnvWorld(t)
			w.extra = append(w.extra, "AGENT_WROTE=#!/usr/bin/env bash\nnpm ci\n")
			w.job("webapp", "agent", tc.payload)
			if _, _, code := w.run(); code != 0 {
				t.Errorf("exit = %d, want 0 — the worker reports and exits 0 either way", code)
			}
			state, _, script, log := w.report()
			if state != tc.wantState {
				t.Errorf("state = %q, want %q (log %q)", state, tc.wantState, log)
			}
			var decoded string
			if script != "" {
				raw, err := base64.StdEncoding.DecodeString(script)
				if err != nil {
					t.Fatalf("reported script is not base64: %v (%q)", err, script)
				}
				decoded = string(raw)
			}
			if decoded != tc.wantScript {
				t.Errorf("reported script = %q, want %q", decoded, tc.wantScript)
			}
			if tc.wantInLog != "" && !strings.Contains(log, tc.wantInLog) {
				t.Errorf("log does not mention %q:\n%s", tc.wantInLog, log)
			}
		})
	}
}

// TestTheRealAgentRunnerWorksInTheRealGuestWorker runs the exact string the
// gateway ships — ctlops.AgentRunner — inside the exact worker a guest runs,
// against a stub `claude`.
//
// It crosses a package boundary on purpose. The gateway writes this script and
// a guest executes it, they live in different packages with no compiler
// relationship, and BOTH bugs review caught in the previous phase were at
// exactly this kind of seam. A test that asserted on a hand-written copy of the
// script would agree with itself forever.
func TestTheRealAgentRunnerWorksInTheRealGuestWorker(t *testing.T) {
	w := newEnvWorld(t)
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.write(filepath.Join(w.fix, "claude-writes"), goodSetupScript)
	w.job("webapp", "agent", ctlops.AgentRunner("webapp"))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	state, _, _, log := w.report()
	if state != "ok" {
		t.Fatalf("state = %q, want ok — the gateway's own runner did not complete: %s", state, log)
	}

	args, err := os.ReadFile(filepath.Join(w.fix, "claude-args"))
	if err != nil {
		t.Fatalf("the agent was never invoked: %v", err)
	}
	got := string(args)
	// One argv element carrying the whole prompt, un-expanded. The prompt rides
	// a QUOTED heredoc, so if that ever became unquoted the $(...) and backticks
	// a future prompt might contain would run instead of travelling as text.
	if !strings.Contains(got, "sparkbox docs dev-environment") {
		t.Errorf("the prompt did not reach the agent as one argument:\n%s", got)
	}
	if !strings.Contains(got, ".sparkbox/setup.sh") {
		t.Errorf("the prompt never names the deliverable:\n%s", got)
	}
	// bypassPermissions is REQUIRED, not preference. Under -p the `auto` mode
	// this platform seeds is downgraded to `default` and every Write and Bash is
	// denied, while the run still exits 0 — an agent build without this flag
	// does nothing and reports success.
	if !strings.Contains(got, "--permission-mode\nbypassPermissions\n") {
		t.Errorf("the agent was not run with --permission-mode bypassPermissions:\n%s", got)
	}
	// A print-mode bootstrap has no harness left to deliver a monitor event or
	// scheduled wakeup after the response ends. Letting the agent select either
	// tool recreates a live failure where it ended cleanly while `make dev` was
	// still running and never came back to write the deliverable.
	if !strings.Contains(got, "--disallowedTools\nMonitor\nScheduleWakeup\n") {
		t.Errorf("the agent could defer work past its one print-mode turn:\n%s", got)
	}
	// The transcript is the only window anybody has into an unattended agent:
	// the log tail is cut to a sentence by the time it reaches a row, and the
	// box is destroyed on success. Every template seeds a hivemind daemon that
	// syncs ~/.claude/projects as the run happens, so suppressing the session
	// would put the build back to being unwatchable.
	if strings.Contains(got, "--no-session-persistence") {
		t.Errorf("the agent ran with session persistence off, so nothing can watch the build:\n%s", got)
	}
}

// Darwin still ships Bash 3.2 at /bin/bash. The guest runner normally uses a
// newer Bash, but these cross-boundary tests execute on both Darwin and Linux,
// and the runner should not need a parser feature newer than its own syntax.
func TestTheRealAgentRunnerParsesWithDarwinSystemBash(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin system Bash is only present on Darwin runners")
	}
	cmd := exec.Command("/bin/bash", "-n")
	runner := ctlops.AgentRunner("webapp")
	cmd.Stdin = strings.NewReader(runner)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent runner does not parse with Darwin system Bash: %v\n%s", err, out)
	}
}

// TestTheRealAgentRunnerRecoversWhenTheFirstAgentWritesNothing is the failure
// seen on hardware: the first print-mode agent sent `make dev` to a Monitor,
// scheduled a later wakeup, and ended its response. The process therefore
// returned 0 before .sparkbox/setup.sh existed even though useful setup work
// was still running in the box. One fresh pass should inspect that state and
// finish the deliverable instead of failing immediately.
func TestTheRealAgentRunnerRecoversWhenTheFirstAgentWritesNothing(t *testing.T) {
	w := newEnvWorld(t)
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.write(filepath.Join(w.fix, "claude-writes-2"), goodSetupScript)
	w.job("webapp", "agent", ctlops.AgentRunner("webapp"))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	state, _, script, log := w.report()
	if state != "ok" {
		t.Fatalf("state = %q, want ok — the recovery agent wrote a valid script: %s", state, log)
	}
	if got := decodeScript(t, script); !strings.Contains(got, "deps installed") {
		t.Errorf("the environment did not record the recovery agent's script: %q", got)
	}
	if got := strings.TrimSpace(w.read("claude-calls")); got != "2" {
		t.Errorf("agent calls = %s, want 2 (initial run and one recovery)", got)
	}
	recovery := w.read("claude-prompt-2")
	for _, want := range []string{
		"previous unattended setup agent ended", ctlops.SetupScriptPath,
		"one and only recovery pass", "sparkbox docs dev-environment", "sparkbox set-port",
	} {
		if !strings.Contains(recovery, want) {
			t.Errorf("recovery prompt does not mention %q:\n%s", want, recovery)
		}
	}
}

// TestTheRealAgentRunnerStopsAfterTwoMissingArtifacts pins the retry bound. A
// missing deliverable gets one useful second chance, not an agent loop that
// consumes the whole environment-build budget while producing nothing.
func TestTheRealAgentRunnerStopsAfterTwoMissingArtifacts(t *testing.T) {
	w := newEnvWorld(t)
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.job("webapp", "agent", ctlops.AgentRunner("webapp"))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0 — the worker reports the runner's failure", code)
	}
	state, exit, script, log := w.report()
	if state != "failed" {
		t.Fatalf("state = %q, want failed\n%s", state, log)
	}
	if exit != "3" {
		t.Errorf("exit = %q, want 3 — the runner should report its bounded recovery failure", exit)
	}
	if script != "" {
		t.Errorf("reported script = %q, want none", script)
	}
	if !strings.Contains(log, "two agent passes finished without writing") {
		t.Errorf("log does not explain the exhausted missing-artifact recovery:\n%s", log)
	}
	if got := strings.TrimSpace(w.read("claude-calls")); got != "2" {
		t.Errorf("agent calls = %s, want exactly 2", got)
	}
}

// TestTheRealAgentRunnerDoesNotSpendAThirdPassRepairingTheRecoveryScript keeps
// the same two-agent ceiling when the missing-artifact recovery does produce a
// script but that script fails verification. The script is still reported so
// the paused builder remains useful to a person finishing it by hand.
func TestTheRealAgentRunnerDoesNotSpendAThirdPassRepairingTheRecoveryScript(t *testing.T) {
	w := newEnvWorld(t)
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.write(filepath.Join(w.fix, "claude-writes-2"), brokenSetupScript)
	w.job("webapp", "agent", ctlops.AgentRunner("webapp"))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0 — the worker reports the runner's failure", code)
	}
	state, exit, script, log := w.report()
	if state != "failed" || exit != "3" {
		t.Fatalf("state/exit = %q/%q, want failed/3\n%s", state, exit, log)
	}
	if got := decodeScript(t, script); !strings.Contains(got, "cd selfhost") {
		t.Errorf("the failed recovery script was not preserved: %q", got)
	}
	if !strings.Contains(log, "recovery agent wrote") || !strings.Contains(log, "does not run") {
		t.Errorf("log does not explain why the recovery script was refused:\n%s", log)
	}
	if got := strings.TrimSpace(w.read("claude-calls")); got != "2" {
		t.Errorf("agent calls = %s, want exactly 2", got)
	}
}

// TestTheRealAgentRunnerDegradesOnAnOldNodeInsteadOfRunningProse.
//
// nodelink.SelfSetupResp gained `mode` as an omitempty field, so a node running
// an older build DROPS it and then renders the hardcoded `script` its own
// metadata service shipped with. That skew is the normal state during a fleet
// rollout, and it is why the gateway ships a shell script rather than a bare
// prompt: run as `script`, the payload still invokes the agent correctly and
// only the artifact check is missed. A bare prompt would have been paragraphs
// of English executed by bash, backticks and all.
func TestTheRealAgentRunnerDegradesOnAnOldNodeInsteadOfRunningProse(t *testing.T) {
	w := newEnvWorld(t)
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.write(filepath.Join(w.fix, "claude-writes"), goodSetupScript)
	// mode `script`, which is what an old node's metadata service renders.
	w.job("webapp", "script", ctlops.AgentRunner("webapp"))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	state, _, _, log := w.report()
	if state != "ok" {
		t.Fatalf("state = %q, want ok — the agent did not run under the old node's mode: %s", state, log)
	}
	if _, err := os.ReadFile(filepath.Join(w.fix, "claude-args")); err != nil {
		t.Fatalf("the agent was never invoked under mode=script: %v", err)
	}
}

// The two setup scripts an agent-mode test describes its agent by. They are
// real scripts and not markers, because the runner RUNS what the agent leaves
// behind: goodSetupScript is written to be safe to run twice over its own work
// (which is what the prompt asks the agent for), and brokenSetupScript fails
// the way the first agent build on real hardware failed — a `cd` into a
// directory that only ever existed in the agent's memory of the session.
const (
	goodSetupScript = "#!/usr/bin/env bash\nset -e\nmkdir -p .sparkbox/state\necho \"deps installed\"\n"

	brokenSetupScript = "#!/usr/bin/env bash\nset -e\ncd selfhost\necho \"never reached\"\n"
)

// TestTheRealAgentRunnerRefusesAScriptThatDoesNotRun is the hardware bug, in a
// test.
//
// The first agent build on the live cluster wrote a good-looking script, the
// build reported `ready`, and the NEXT rebuild of that environment died on
// `cd: selfhost: No such file or directory`. Nothing had checked that the
// script ran, because the agent does the work interactively and writes the file
// at the end from memory — so the only failure available was the one that
// arrives months later, to whoever first depends on the environment
// reproducing itself.
//
// What it asserts is the whole point of the change: a build like that FAILS,
// and it fails with the script still reported, because the owner wants the
// eighty percent the agent got right on a box they can still ssh into.
func TestTheRealAgentRunnerRefusesAScriptThatDoesNotRun(t *testing.T) {
	w := newEnvWorld(t)
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.write(filepath.Join(w.fix, "claude-writes"), brokenSetupScript)
	w.job("webapp", "agent", ctlops.AgentRunner("webapp"))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0 — the worker reports, it does not fail", code)
	}
	state, exit, script, log := w.report()
	if state != "failed" {
		t.Fatalf("state = %q, want failed: a script that does not run is not a build\n%s", state, log)
	}
	if exit != "3" {
		t.Errorf("exit = %q, want 3 — the runner's own status for an unrunnable script", exit)
	}
	// The last non-empty line is what the gateway records as the build error, so
	// it has to say the thing that is true and surprising rather than repeating
	// the shell's error.
	if !strings.Contains(log, "does not run in it") {
		t.Errorf("the log never says the script is what failed:\n%s", log)
	}
	// KEPT. recordReportedScript stores a reported script whether the build
	// succeeded or not, and this is the case that makes that matter: the owner
	// finishes it by hand in the paused builder and runs `env capture`.
	if got := decodeScript(t, script); !strings.Contains(got, "cd selfhost") {
		t.Errorf("the failed build did not report the script the agent wrote: %q", got)
	}
	// Twice: written, run, handed back to be fixed, run again.
	if got := w.read("claude-calls"); strings.TrimSpace(got) != "2" {
		t.Errorf("the agent was invoked %s times, want 2 (the write and the one repair round)", got)
	}
}

// TestTheRealAgentRunnerHasTheScriptFixedAndFinishes: the repair round is not
// decoration. An agent writing from memory gets a path or an ordering wrong far
// more often than it gets the project wrong, and it is holding a box where the
// answer can be checked — so the cheap fix is to hand it the failure and let it
// try once, before failing a build whose work is otherwise done.
func TestTheRealAgentRunnerHasTheScriptFixedAndFinishes(t *testing.T) {
	w := newEnvWorld(t)
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.write(filepath.Join(w.fix, "claude-writes-1"), brokenSetupScript)
	w.write(filepath.Join(w.fix, "claude-writes-2"), goodSetupScript)
	w.job("webapp", "agent", ctlops.AgentRunner("webapp"))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	state, _, script, log := w.report()
	if state != "ok" {
		t.Fatalf("state = %q, want ok — the repaired script runs: %s", state, log)
	}
	if got := decodeScript(t, script); !strings.Contains(got, "deps installed") {
		t.Errorf("the environment recorded the broken script rather than the fixed one: %q", got)
	}
	// The repair round is a FRESH agent rather than a resume of the first, so
	// handing it the failure is the only way it can know what to fix.
	repair := w.read("claude-prompt-2")
	if !strings.Contains(repair, "cd: selfhost") {
		t.Errorf("the repair prompt does not carry the failure it is repairing:\n%s", repair)
	}
	if !strings.Contains(repair, "does not run") {
		t.Errorf("the repair prompt does not say what it is asking for:\n%s", repair)
	}
}

// TestTheRealAgentRunnerRefusesShellItCannotEvenParse. A syntax error is not a
// failed run and is worth its own sentence: `bash -n` costs nothing, and the
// alternative is a parse error buried in whatever the shell printed before it
// gave up.
func TestTheRealAgentRunnerRefusesShellItCannotEvenParse(t *testing.T) {
	w := newEnvWorld(t)
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.write(filepath.Join(w.fix, "claude-writes"), "#!/usr/bin/env bash\nif [ -f x ; then\n")
	w.job("webapp", "agent", ctlops.AgentRunner("webapp"))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	state, _, _, log := w.report()
	if state != "failed" {
		t.Fatalf("state = %q, want failed\n%s", state, log)
	}
	if !strings.Contains(log, "not valid shell") {
		t.Errorf("the log does not name the parse error as one:\n%s", log)
	}
}

// TestEnvSetupBoundsWhatItSendsBack. The host caps the body it will read, and a
// body it refuses is a report that never lands — so the guest has to be the one
// that truncates. The two halves truncate differently on purpose: a log is
// meaningful cut off at the tail, and a setup script is not meaningful cut off
// anywhere, so an oversized script is reported as absent (which the gateway
// reads as "unchanged") rather than as half of itself.
func TestEnvSetupBoundsWhatItSendsBack(t *testing.T) {
	w := newEnvWorld(t)
	if err := os.MkdirAll(filepath.Join(w.work, ".sparkbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.write(filepath.Join(w.work, ".sparkbox/setup.sh"),
		"#!/bin/sh\n"+strings.Repeat("# padding\n", 8000))
	w.job("webapp", "script", "i=0\nwhile [ $i -lt 4000 ]; do echo \"line $i of noise\"; i=$((i+1)); done\n")

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	state, _, script, log := w.report()
	if state != "ok" {
		t.Fatalf("state = %q", state)
	}
	if script != "" {
		t.Errorf("an oversized .sparkbox/setup.sh was reported anyway (%d bytes of base64)", len(script))
	}
	if len(log) > 8192 {
		t.Errorf("log tail = %d bytes, want at most 8192", len(log))
	}
	if !strings.Contains(log, "report cap") {
		t.Errorf("nothing in the log says the script was left out:\n%s", log)
	}
}

// TestEnvSetupStopsAScriptThatNeverStops. An unbounded run holds the
// environment in `building` for as long as the box lives, which is the failure
// mode with no floor: nobody is at a terminal, and the script is waiting on a
// prompt that will never be answered.
func TestEnvSetupStopsAScriptThatNeverStops(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("no timeout(1) in PATH; the guest image has coreutils")
	}
	w := newEnvWorld(t)
	w.extra = []string{"SPARKBOX_ENV_SETUP_TIMEOUT=1"}
	w.job("webapp", "script", "echo starting\nsleep 30\n")

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	state, exit, _, log := w.report()
	if state != "failed" {
		t.Fatalf("state = %q, want failed (log %q)", state, log)
	}
	if exit != "124" && exit != "137" {
		t.Errorf("exit line = %q, want timeout's 124 or 137", exit)
	}
	if !strings.Contains(log, "was stopped") {
		t.Errorf("the log does not say the script was stopped:\n%s", log)
	}
}

// TestEnvSetupRunsOnceAtATime: two nudges arriving together must not run
// somebody's setup script twice against the same tree. The second one is
// declined and says so, and it does not report — reporting would race a result
// the first run has not produced yet.
func TestEnvSetupRunsOnceAtATime(t *testing.T) {
	w := newEnvWorld(t)
	w.job("webapp", "script", "echo hello\n")
	lock := filepath.Join(w.root, "run/sparkbox/env-setup.lock")
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	// A live holder: this test's own pid is running by definition.
	w.write(filepath.Join(lock, "pid"), strconv.Itoa(os.Getpid())+"\n")

	stdout, stderr, code := w.run()
	if code != 0 || stdout != "" {
		t.Fatalf("exit = %d, stdout = %q", code, stdout)
	}
	if !strings.Contains(stderr, "already in progress") {
		t.Errorf("stderr = %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(w.fix, "result")); err == nil {
		t.Error("the declined run reported a result the other run had not finished")
	}
	// A lock left by a process that is gone must not wedge the environment
	// forever: the next nudge takes it over.
	w.write(filepath.Join(lock, "pid"), "2147483646\n")
	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d after a stale lock", code)
	}
	if state, _, _, _ := w.report(); state != "ok" {
		t.Errorf("state = %q after taking over a stale lock", state)
	}
}

// TestEnvSetupUnitIsNudgedNeverBooted. This unit has no [Install] section and
// no symlink, and that is the design: the gateway starts it after the secrets
// are pushed, which removes the race against that push entirely. A future
// `WantedBy=multi-user.target` would reintroduce it and make every VM in the
// fleet run a unit to be told 204.
func TestEnvSetupUnitIsNudgedNeverBooted(t *testing.T) {
	root := fakeGuestTree(t, true)
	installGuestPayload(t, root)
	unit := guestFile(t, root, "etc/systemd/system/sparkbox-env-setup.service")

	for _, want := range []string{
		"Type=oneshot",
		"RemainAfterExit=yes",
		"ExecStart=/usr/local/sbin/sparkbox-env-setup",
		"After=network-online.target sparkbox-net.service sparkbox-token.service sparkbox-repos.service",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("sparkbox-env-setup.service missing %q:\n%s", want, unit)
		}
	}
	for _, bad := range []string{"WantedBy=", "[Install]"} {
		if strings.Contains(unit, bad) {
			t.Errorf("sparkbox-env-setup.service carries %q; it is started by a gateway "+
				"nudge after the secret push, never by boot ordering:\n%s", bad, unit)
		}
	}
	// The same rule sparkbox-repos.service holds, for a job that takes longer
	// than a clone: nothing may put a setup script in front of a first attach.
	if strings.Contains("\n"+unit, "\nBefore=") {
		t.Errorf("sparkbox-env-setup.service orders itself before something:\n%s", unit)
	}
	if _, err := os.Lstat(filepath.Join(root,
		"etc/systemd/system/multi-user.target.wants/sparkbox-env-setup.service")); err == nil {
		t.Error("sparkbox-env-setup.service is enabled; it must only ever run when the gateway nudges it")
	}
	// systemd's kill must never be the thing that ends a build: the worker's own
	// timeout reports what happened, where a killed unit leaves the gateway
	// waiting on a POST that is never made.
	deadline := regexp.MustCompile(`TimeoutStartSec=(\d+)`).FindStringSubmatch(unit)
	if deadline == nil {
		t.Fatalf("sparkbox-env-setup.service has no TimeoutStartSec:\n%s", unit)
	}
	worker := guestFile(t, root, "usr/local/sbin/sparkbox-env-setup")
	own := regexp.MustCompile(`TIMEOUT=\$\{SPARKBOX_ENV_SETUP_TIMEOUT:-(\d+)\}`).FindStringSubmatch(worker)
	if own == nil {
		t.Fatalf("the worker has no default timeout")
	}
	unitSec, _ := strconv.Atoi(deadline[1])
	workerSec, _ := strconv.Atoi(own[1])
	if unitSec <= workerSec {
		t.Errorf("TimeoutStartSec=%d does not outlast the worker's own %ds bound", unitSec, workerSec)
	}
}

// TestGuestEnvVerbOrientsInsideTheBox. An agent that arrives in a builder — or
// in any box — should be able to ask what environment it is standing in without
// leaving the VM. It reads local state and the host's own /self; it cannot start
// a build, because composing and building an environment is a control-plane act
// taken from outside.
func TestGuestEnvVerbOrientsInsideTheBox(t *testing.T) {
	w := newEnvWorld(t)
	cli := filepath.Join(w.root, "usr/local/bin/sparkbox")
	status := filepath.Join(w.root, "run/sparkbox/env-setup.status")
	run := func() (string, int) {
		t.Helper()
		cmd := exec.Command("sh", cli, "env")
		cmd.Env = append(w.env(), "SPARKBOX_ENV_STATUS_FILE="+status)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		code := 0
		var ee *exec.ExitError
		if err := cmd.Run(); errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return out.String(), code
	}

	// A box that never built an environment says so, and points at the command
	// that does build one — which is not this one.
	out, code := run()
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out)
	}
	for _, want := range []string{"sandbox: quiet-lake", "no setup has run", "env build"} {
		if !strings.Contains(out, want) {
			t.Errorf("`sparkbox env` output missing %q:\n%s", want, out)
		}
	}

	w.job("webapp", "script", "echo hello\n")
	if _, _, rc := w.run(); rc != 0 {
		t.Fatalf("build run exit = %d", rc)
	}
	out, code = run()
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out)
	}
	for _, want := range []string{"environment: webapp", "mode: script", "state: ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("`sparkbox env` output missing %q:\n%s", want, out)
		}
	}
}
