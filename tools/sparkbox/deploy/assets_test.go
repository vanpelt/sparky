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
	if string(rev) != "IDENTITY_REV=3\n" {
		t.Fatalf("identity revision = %q", rev)
	}
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
		"AGENT_ENV_REV=3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("template guidance missing %q", want)
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
