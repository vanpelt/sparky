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

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
