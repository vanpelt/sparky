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

func TestGuestPayloadInstallsSelfControlCLI(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"etc", "home/sparky/.ssh"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("sparky:x:1000:1000::/home/sparky:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "home/sparky/.ssh/authorized_keys"), []byte("ssh-ed25519 test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "install-guest-identity.sh", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install guest payload: %v\n%s", err, out)
	}
	cli, err := os.ReadFile(filepath.Join(root, "usr/local/bin/sparkbox"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"$META/self/pin", "$META/self/unpin", "$META/self",
		"$META/self/visibility/public", "$META/self/visibility/private",
		"$META/self/port/$2",
	} {
		if !strings.Contains(string(cli), want) {
			t.Errorf("guest CLI missing %q", want)
		}
	}
	rev, err := os.ReadFile(filepath.Join(root, "etc/sparkbox/identity-rev"))
	if err != nil {
		t.Fatal(err)
	}
	if string(rev) != "IDENTITY_REV=4\n" {
		t.Fatalf("identity revision = %q", rev)
	}
}

// TestTokenTimerFiresNearItsBootTarget pins the accuracy, which is not a tuning
// knob here but the difference between a guest having a token at startup and
// not.
//
// systemd may delay a timer by up to AccuracySec to coalesce wakeups, and the
// default is a full minute. On the 45-minute refresh that is free; on
// OnBootSec=10s it means the first fetch can land anywhere in the first 70
// seconds — and hivemind makes ONE authentication attempt at startup, so it
// gives up before the token file exists. The guest then runs unauthenticated
// until something restarts the daemon.
func TestTokenTimerFiresNearItsBootTarget(t *testing.T) {
	timer := timerUnitFrom(t, string(GuestIdentityScript))
	if strings.Contains(timer, "AccuracySec=1min") {
		t.Fatal("the boot fetch is back on systemd's default 1-minute slack, " +
			"which lets it land after hivemind's single startup auth attempt has given up")
	}
	for _, want := range []string{"OnBootSec=10s", "AccuracySec=1s"} {
		if !strings.Contains(timer, want) {
			t.Errorf("token timer missing %q:\n%s", want, timer)
		}
	}
}

// timerUnitFrom returns the sparkbox-token.timer heredoc body the installer
// writes, so the assertions above read the unit rather than the whole script
// and cannot be satisfied by the same string appearing in a comment elsewhere.
func timerUnitFrom(t *testing.T, script string) string {
	t.Helper()
	const marker = "sparkbox-token.timer\" <<'EOF'\n"
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatal("install-guest-identity.sh no longer writes sparkbox-token.timer from a heredoc")
	}
	rest := script[i+len(marker):]
	j := strings.Index(rest, "\nEOF\n")
	if j < 0 {
		t.Fatal("unterminated sparkbox-token.timer heredoc")
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
		"AGENT_ENV_REV=4",
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
