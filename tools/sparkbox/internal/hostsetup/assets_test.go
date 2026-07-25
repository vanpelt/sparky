package hostsetup

import (
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/deploy"
)

func TestEmbeddedAssetsNonEmpty(t *testing.T) {
	if len(deploy.NetScript) == 0 || !strings.Contains(string(deploy.NetScript), "SPARKBOX_EDGE") {
		t.Error("NetScript embed missing or wrong")
	}
	if len(deploy.NetService) == 0 || !strings.Contains(string(deploy.NetService), "sparkbox-net.sh") {
		t.Error("NetService embed missing or wrong")
	}
	if len(deploy.SysctlConf) == 0 || !strings.Contains(string(deploy.SysctlConf), "ip_forward") {
		t.Error("SysctlConf embed missing or wrong")
	}
	if !strings.Contains(deploy.StandaloneServiceTemplate, "{{.StateDir}}") {
		t.Error("service template should be a text/template with placeholders")
	}
}

func TestRenderServiceStandalone(t *testing.T) {
	cfg := DefaultConfig()
	// Standalone must NOT reference fleet-only knobs.
	out, err := renderService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "--require-keys") || strings.Contains(out, "--key-dir") {
		t.Error("standalone unit must omit --require-keys/--key-dir (keys are local)")
	}
	if strings.Contains(out, "{{") {
		t.Errorf("unrendered template placeholder remains:\n%s", out)
	}
	if !strings.Contains(out, "--ssh-addr :2222") {
		t.Error("default (admin ssh not moved) should bind the gateway on :2222")
	}
	if !strings.Contains(out, "--state-dir "+cfg.StateDir) {
		t.Error("state-dir not templated")
	}
	// The unit must run the binary setup installs, not a hardcoded path that
	// nothing populates (F0). A misspelled placeholder renders as "<no value>"
	// rather than erroring, so assert the whole ExecStart prefix.
	if !strings.Contains(out, "ExecStart="+cfg.BinPath+" serve ") {
		t.Errorf("ExecStart should run %s\n%s", cfg.BinPath, out)
	}

	cfg.MoveAdminSSH = true
	out, _ = renderService(cfg)
	if !strings.Contains(out, "--ssh-addr :22 ") {
		t.Error("after taking port 22 the gateway should bind :22")
	}

	// A custom --bin-path is honoured…
	cfg.BinPath = "/opt/sparkbox/bin/sparkbox"
	out, _ = renderService(cfg)
	if !strings.Contains(out, "ExecStart=/opt/sparkbox/bin/sparkbox serve ") {
		t.Errorf("custom --bin-path not templated into ExecStart\n%s", out)
	}
	// …and a Config that never saw DefaultConfig still renders a runnable unit
	// rather than "ExecStart= serve …".
	out, err = renderService(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ExecStart="+defaultBinPath+" serve ") {
		t.Errorf("empty BinPath should fall back to %s\n%s", defaultBinPath, out)
	}
}
