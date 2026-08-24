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
	if rev := guestFile(t, root, "etc/sparkbox/identity-rev"); rev != "IDENTITY_REV=6\n" {
		t.Fatalf("identity revision = %q", rev)
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
		"AGENT_ENV_REV=6",
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
