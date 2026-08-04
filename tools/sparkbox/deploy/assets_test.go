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

func TestCKSManifestKeepsSluiceNarrowAndFailClosed(t *testing.T) {
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
	} {
		if !strings.Contains(controller, want) {
			t.Errorf("controller entrypoint missing fail-closed Sluice wiring %q", want)
		}
	}
	if strings.Contains(string(sluiceEntrypoint), "--open-untagged") {
		t.Error("CKS Sluice must enforce its base allow-list on every TAP")
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

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
