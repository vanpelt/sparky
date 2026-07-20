// Package deploy embeds the host-side provisioning assets (the packet-filter
// script, systemd units, and sysctl config) so `sparkbox setup` can lay them
// down on a bare host with no repo checkout — the binary is self-contained.
//
// The assets live here as ordinary files (not vendored copies) so they stay the
// single source of truth: deploy/cloud-init.yaml b64-embeds sparkbox-net.sh and
// hack/install-host-tooling.sh installs it, and a `.go` file inside deploy/ is
// the only place go:embed can reach them from (embed cannot cross into a parent
// or sibling directory).
package deploy

import _ "embed"

// NetScript is the idempotent host packet-filter script (sandbox NAT, metadata
// port lockdown, any-port REDIRECT). Installed at /usr/local/sbin/sparkbox-net.sh
// and run on every boot by sparkbox-net.service.
//
//go:embed sparkbox-net.sh
var NetScript []byte

// StandaloneServiceTemplate is the text/template for the sparkbox.service unit
// on a standalone (non-fleet) host: keys are generated locally in --state-dir,
// so it omits the Secret Manager fetch, --key-dir, and --require-keys that the
// cloud-init unit uses.
//
//go:embed units/sparkbox-standalone.service.tmpl
var StandaloneServiceTemplate string

// NetService is the sparkbox-net.service unit that applies NetScript at boot.
//
//go:embed units/sparkbox-net.service
var NetService []byte

// SysctlConf is the /etc/sysctl.d/99-sparkbox.conf networking knobs.
//
//go:embed sysctl/99-sparkbox.conf
var SysctlConf []byte
