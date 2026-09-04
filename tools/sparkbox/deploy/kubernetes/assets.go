// Package kubernetes embeds the CKS manifests and container entrypoints that
// deploy.sh applies, so Go code can read the objects we actually ship instead
// of a hand-transcribed copy of them.
//
// The transcribed copy is the thing being avoided: internal/devpod renders the
// node Pod into docker argv for local development, and a second description of
// that Pod would drift from this one silently. A `.go` file inside this
// directory is the only place go:embed can reach these files from (embed cannot
// cross into a parent or sibling directory) — deploy/assets.go says the same
// about the host-side assets.
//
// These are the raw files, placeholders and all. Substituting
// __SPARKBOX_IMAGE__ and friends is the reader's job, the way deploy.sh does it
// with sed.
package kubernetes

import _ "embed"

// NodeDeployment is the five-container VM-node Pod: the
// scrub-retired-gateway-state and prepare-vm-assets init containers followed by
// vmm-helper, sluice, and sparkbox-node.
//
//go:embed deployment.yaml
var NodeDeployment []byte

// GatewayDeployment is the public control-plane Pod.
//
//go:embed gateway-deployment.yaml
var GatewayDeployment []byte

// NetworkPolicies is the multi-document policy file: the namespace's
// default-deny ingress, the gateway's two allowed ingress ports, and the
// CiliumNetworkPolicy that confines the VM node's egress to the public
// internet. Only the last one selects the node Pod, and nothing off-cluster
// enforces it — a reader that reproduces the Pod has to say so.
//
//go:embed network-policy.yaml
var NetworkPolicies []byte

// LoadBalancer is the public Service. Its port list is the source of truth for
// which sandbox ports reach the outside world, and internal/publicports must
// agree with it.
//
//go:embed service.yaml
var LoadBalancer []byte

// NodeEntrypoint is the image ENTRYPOINT shared by the node containers. It
// pins SPARKBOX_RELEASE's asset checksums, so anything that reproduces the Pod
// has to agree with the checksums written here.
//
//go:embed entrypoint.sh
var NodeEntrypoint []byte

// VMMHelperEntrypoint owns the Pod-network setup (TAPs, the sluice resolver
// address, sysctls) that every other container in the namespace depends on.
//
//go:embed vmm-helper-entrypoint.sh
var VMMHelperEntrypoint []byte

// SluiceEntrypoint runs the DNS allow-list and eBPF enforcer.
//
//go:embed sluice-entrypoint.sh
var SluiceEntrypoint []byte

// GatewayEntrypoint runs the public gateway.
//
//go:embed gateway-entrypoint.sh
var GatewayEntrypoint []byte
