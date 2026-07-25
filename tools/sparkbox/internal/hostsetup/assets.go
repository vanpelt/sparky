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
	BinPath      string
	EnvPath      string
	StateDir     string
	KernelPath   string
	ImageDir     string
	DefaultImage string
	UsersPath    string
	SSHAddr      string
	ProxyAddr    string
}

// renderService renders the standalone sparkbox.service unit for cfg. The SSH
// gateway binds :22 when setup took over the admin port, else :2222.
func renderService(cfg Config) (string, error) {
	sshAddr := ":2222"
	if cfg.MoveAdminSSH {
		sshAddr = ":22"
	}
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
		DefaultImage: cfg.DefaultImage,
		UsersPath:    cfg.UsersPath,
		SSHAddr:      sshAddr,
		ProxyAddr:    fmt.Sprintf(":%d", proxyPort),
	}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
