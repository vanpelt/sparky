package hostsetup

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/vanpelt/sparky/tools/sparkbox/deploy"
)

// serviceParams fills the standalone sparkbox.service template.
type serviceParams struct {
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
