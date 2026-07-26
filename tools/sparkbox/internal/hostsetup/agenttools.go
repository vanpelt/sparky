package hostsetup

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/vanpelt/sparky/tools/sparkbox/deploy"
)

// Installing the agent tooling — the thing that makes a sandbox worth creating.
//
// # Why this step exists
//
// A rootfs template out of a release carries a shell, a toolchain and nothing
// that can write code. claude, codex and hivemind are baked into it afterwards
// by deploy/refresh-agent-tools.sh, which patches the template in place (~a
// minute) instead of rebuilding the image (~65 minutes), and re-runs daily so a
// box picks up new agent releases on its own.
//
// Until now the only things that installed it were deploy/cloud-init.yaml and
// deploy/install-host-tooling.sh — a cloud user-data blob and a script you run
// by hand from a repo checkout. Neither is reachable from a released binary, so
// every host `sparkbox setup` provisioned by itself — which is every macOS host
// and every fleet node — produced sandboxes with no agent in them. There was no
// flag to miss; there was nothing to set. That is why this is on by default and
// takes an explicit --agent-tools=false to turn off, rather than being opt-in
// like --sluice: sluice CHANGES what a sandbox can do, this only adds what
// everyone provisioning a sandbox host was going to install anyway.
//
// # What "already satisfied" means here
//
// Not "the script is installed" — the DGX had the script, its timer, and a
// host-side stamp saying everything was current, while its actual template held
// no claude at all (a v0.4.0 upgrade had replaced the template underneath the
// stamp). So the question this step asks is the one that matters: does THIS
// host's rootfs template carry the agent tools, read out of the template. The
// script now writes that stamp inside each template it patches, and both layers
// read the same file.
//
// Version currency is deliberately NOT part of it. Asking "are these the newest
// agent releases" would put a network round-trip and a possible multi-hundred-MB
// download in every `setup` run, including an upgrade run that changed one flag.
// Staying current is the daily timer's job; setup's job is that the box has them
// at all.
const (
	refreshToolsUnit   = "sparkbox-refresh-tools.service"
	refreshToolsTimer  = "sparkbox-refresh-tools.timer"
	refreshToolsScript = "sparkbox-refresh-tools.sh"
	guestIdentityName  = "sparkbox-install-guest-identity.sh"

	// templateToolsStamp is the file refresh-agent-tools.sh writes INSIDE every
	// template it patches. Its content is that run's version tuple; this side
	// only asks whether it is there, because deciding it is out of date is the
	// script's job and it is the half that knows what "current" resolves to
	// today.
	templateToolsStamp = "/etc/sparkbox/tools-rev"
)

// refreshToolsParams fills the sparkbox-refresh-tools.service template.
type refreshToolsParams struct {
	ScriptPath string
	ImageDir   string
	ToolsDir   string
}

// toolsDir is where the refresher caches downloaded agent binaries and records
// the versions it last resolved. It hangs off Root rather than being a flag:
// it is a cache, and an operator who wants it elsewhere is better served by
// moving Root.
func (c Config) toolsDir() string { return filepath.Join(c.Root, "tools") }

func renderRefreshToolsService(cfg Config, sbinDir string) (string, error) {
	t, err := template.New("refresh-tools").Parse(deploy.RefreshToolsServiceTemplate)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	err = t.Execute(&b, refreshToolsParams{
		ScriptPath: filepath.Join(sbinDir, refreshToolsScript),
		ImageDir:   cfg.ImageDir,
		ToolsDir:   cfg.toolsDir(),
	})
	return b.String(), err
}

// agentToolsAssets is every file this step lays down, so the Satisfied
// comparison and Apply cannot disagree about what "installed" means. Byte
// comparison, not existence: these ship with the release, and an existence
// check is how a fix to the refresher would stop dead at the first host that
// had ever been provisioned.
func agentToolsAssets(e *Env) ([]fileAsset, error) {
	svc, err := renderRefreshToolsService(e.Cfg, e.SbinDir)
	if err != nil {
		return nil, err
	}
	return []fileAsset{
		{filepath.Join(e.SbinDir, refreshToolsScript), deploy.RefreshToolsScript, 0o755},
		{filepath.Join(e.SbinDir, guestIdentityName), deploy.GuestIdentityScript, 0o755},
		{filepath.Join(e.SystemdDir, refreshToolsUnit), []byte(svc), 0o644},
		{filepath.Join(e.SystemdDir, refreshToolsTimer), deploy.RefreshToolsTimer, 0o644},
	}, nil
}

// templateHasTools reports whether the default rootfs template carries the
// stamp refresh-agent-tools.sh writes into everything it patches.
//
// Read with debugfs, which opens the image read-only and needs no loop device —
// so this is safe to run against a template that sandboxes are being reflinked
// from right now. debugfs exits 0 even when the path does not exist, so the
// OUTPUT is the answer; an unreadable template (no debugfs, no image yet, a
// filesystem it cannot parse) reports false and costs a re-patch, which is the
// right direction to be wrong in.
func templateHasTools(e *Env) bool {
	img := e.Cfg.rootfsPath()
	if fi, err := os.Stat(img); err != nil || fi.Size() == 0 {
		return false
	}
	out, _ := e.run("debugfs", "-R", "cat "+templateToolsStamp, img)
	return strings.Contains(string(out), "claude=")
}

func stepAgentTools() Step {
	return Step{
		Name: "agent-tools",
		Satisfied: func(e *Env) (bool, string, error) {
			if !e.Cfg.AgentTools {
				// Named rather than silent, exactly like --sluice: nothing else
				// about a green run would hint that the sandboxes this host
				// creates have no agent in them.
				return true, "skipped (--agent-tools=false; sandboxes ship no claude/codex/hivemind)", nil
			}
			assets, err := agentToolsAssets(e)
			if err != nil {
				return false, "", err
			}
			for _, a := range assets {
				got, rerr := os.ReadFile(a.path)
				if rerr != nil || !bytes.Equal(got, a.body) {
					return false, "", nil
				}
			}
			// The installed-and-current script is worth nothing if it has never
			// reached the template — which is the failure this whole step was
			// written for. Ask the image.
			if !templateHasTools(e) {
				return false, "", nil
			}
			return true, "refresher + timer current, template carries the agent CLIs", nil
		},
		Plan: func(e *Env) string {
			return fmt.Sprintf("install %s + %s, enable %s (daily), bake claude/codex/hivemind into %s",
				refreshToolsScript, guestIdentityName, refreshToolsTimer, e.Cfg.rootfsPath())
		},
		Apply: func(e *Env) error {
			assets, err := agentToolsAssets(e)
			if err != nil {
				return err
			}
			for _, a := range assets {
				if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(a.path, a.body, a.mode); err != nil {
					return err
				}
			}
			if err := os.MkdirAll(e.Cfg.toolsDir(), 0o755); err != nil {
				return err
			}
			// daemon-reload here rather than leaving it to enable-services: the
			// timer is enabled in this step, and systemd would enable the text
			// it was already holding.
			if _, err := e.run("systemctl", "daemon-reload"); err != nil {
				return err
			}
			if _, err := e.run("systemctl", "enable", "--now", refreshToolsTimer); err != nil {
				return err
			}

			// The first bake, now, rather than waiting up to a day for the timer:
			// a host that finishes `setup` and reports itself provisioned should
			// be able to create a sandbox somebody can work in.
			//
			// BEST-EFFORT, and this is the one place in the pipeline where a
			// non-zero exit is not fatal. It downloads several hundred MB from
			// three third-party release channels; a transient failure there must
			// not undo a provisioning run whose gateway, network and units are
			// all correct. The timer retries within the day, `doctor` reports
			// the gap in the meantime, and the next `setup` will try again
			// because Satisfied asks the template rather than trusting a stamp.
			e.logf("   baking claude + codex + hivemind into %s (a few minutes on a first run)\n", e.Cfg.rootfsPath())
			out, rerr := e.run(filepath.Join(e.SbinDir, refreshToolsScript))
			for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
				if line != "" {
					e.logf("   tools| %s\n", line)
				}
			}
			if rerr != nil {
				e.logf("   WARNING: the agent-CLI bake failed (%v). The gateway is fine and %s will retry\n",
					rerr, refreshToolsTimer)
				e.logf("            within the day; until it succeeds, sandboxes have no claude/codex/hivemind.\n")
				e.logf("            Retry now with: %s\n", filepath.Join(e.SbinDir, refreshToolsScript))
			}
			return nil
		},
	}
}
