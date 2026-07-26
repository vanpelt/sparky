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

// SluiceServiceTemplate is the text/template for the sluice.service unit that
// `sparkbox setup --sluice` installs.
//
// It is a NEAR-COPY of tools/sluice/deploy/sluice.service, and that duplication
// is forced rather than chosen: tools/sluice is a separate Go module, and
// go:embed cannot reach outside its own package directory, let alone across a
// module boundary. The two differ on purpose as well — this one templates the
// paths and the DNS listen address (a gateway that also runs sparkbox's own
// wildcard responder cannot leave sluice on the wildcard :53), where the
// standalone file hardcodes the /srv/sparkbox layout for a hand install.
//
// If you change one, change the other. The sluice-side file says the same.
//
//go:embed units/sluice.service.tmpl
var SluiceServiceTemplate string

// SluiceAllowlistSeed is the initial /srv/sparkbox/allowlist.txt.
//
// Seeded only when the file is absent — see the long note inside it. sluice
// exits 1 when the path its --allowlist names does not exist, so shipping the
// unit without shipping this would install a permanent crash loop.
//
//go:embed sluice-allowlist.txt
var SluiceAllowlistSeed []byte
