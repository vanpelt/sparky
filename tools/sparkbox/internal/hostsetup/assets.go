package hostsetup

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/vanpelt/sparky/tools/sparkbox/deploy"
)

// defaultBinPath is the ExecStart fallback for a Config that never went through
// DefaultConfig (a hand-built literal in a test, say). A unit rendered with an
// empty path would be `ExecStart= serve …`, which systemd rejects at load time
// in a way that reads as a systemd problem rather than a config one.
const defaultBinPath = "/usr/local/bin/sparkbox"

// binPath is the path the unit's ExecStart names — Cfg.BinPath, or the default
// when the config never went through DefaultConfig. Everything that renders or
// describes the unit reads it here so the plan text and the rendered ExecStart
// cannot disagree.
func (c Config) binPath() string {
	if c.BinPath == "" {
		return defaultBinPath
	}
	return c.BinPath
}

// serviceParams fills the standalone sparkbox.service template.
type serviceParams struct {
	// BinPath is the sparkbox binary ExecStart runs. It used to be hardcoded in
	// the template while nothing installed a binary there (F0); it is now the
	// same value stepInstallBinary copies this executable to, so the unit can
	// never name a path setup did not populate.
	BinPath    string
	EnvPath    string
	StateDir   string
	KernelPath string
	ImageDir   string
	// ToolsDir is the agent-CLI cache the gateway serves to its own guests at
	// /tools. It is rendered ALWAYS, not through OptFlags: like --state-dir it
	// is filesystem layout rather than an optional subsystem, and it is the
	// SAME value the refresher unit receives as TOOLS_DIR (both come from
	// Config.toolsDir), so the process that fills the cache and the process
	// that serves it cannot end up naming different directories.
	ToolsDir     string
	DefaultImage string
	UsersPath    string
	// The four listen addresses. They used to be a mix of a MoveAdminSSH branch
	// (SSHAddr), a package constant (ProxyAddr) and two literals baked into the
	// template (--api-addr 127.0.0.1:8080), which is why the only way to move
	// them was to re-set them from a flag bundle appended later in the same
	// ExecStart. Now they are ordinary config — see Config.sshAddr and friends.
	SSHAddr   string
	ProxyAddr string
	APIAddr   string
	// OptFlags is every optional subsystem flag this config turns on, already
	// rendered as "--flag value" lines and ordered (see optionalFlags). The
	// template emits one continuation line per entry and NOTHING when the slice
	// is empty — an unset subsystem must leave no trace in ExecStart, because a
	// flag with an empty value ends Go's flag parsing and silently drops
	// everything after it.
	//
	// It carries --dns-addr too: A2 templated that one inline, and keeping a
	// second mechanism for one flag would mean two places to get the
	// omit-when-unset rule wrong.
	OptFlags []string
}

// renderService renders the standalone sparkbox.service unit for cfg.
func renderService(cfg Config) (string, error) {
	tmpl, err := template.New("svc").Parse(deploy.StandaloneServiceTemplate)
	if err != nil {
		return "", fmt.Errorf("parse service template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, serviceParams{
		BinPath:      cfg.binPath(),
		EnvPath:      cfg.envPath(),
		StateDir:     cfg.StateDir,
		KernelPath:   cfg.KernelPath,
		ImageDir:     cfg.ImageDir,
		ToolsDir:     cfg.toolsDir(),
		DefaultImage: cfg.DefaultImage,
		UsersPath:    cfg.UsersPath,
		SSHAddr:      cfg.sshAddr(),
		ProxyAddr:    cfg.proxyAddr(),
		APIAddr:      cfg.apiAddr(),
		OptFlags:     optionalFlags(cfg),
	}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
