// Package devpod translates the CKS manifests in deploy/kubernetes into a
// runnable local pod: one docker network namespace holding the same five
// containers, with the same uids, capability sets, devices, mounts and init
// ordering the cluster gets.
//
// It exists because the obvious alternative — a hand-written compose file or
// shell script that "looks like" the Pod — drifts from deployment.yaml without
// anyone noticing, and a dev environment that has drifted is worse than none:
// it reports fidelity it does not have. So everything the plan emits is
// derived from the shipped manifest, and anything the translator does not
// understand is an error rather than an omission:
//
//   - podspec decodes strictly, so an unknown field fails the load.
//   - A container name the overlay has no entry for fails BuildPlan.
//   - A sparkbox.dev/* resource with no device bundle fails BuildPlan.
//   - A __PLACEHOLDER__ with no substitution fails the load, and one that
//     survives into the emitted argv fails BuildPlan.
//
// What genuinely cannot be reproduced locally is not edited away — it is
// recorded, with a reason, by Plan.Divergences. A runtime with no scheduler is
// exactly why this design was picked over kind/k3s: editing cpu:"48" down to
// something a laptop can schedule would mean testing numbers we invented.
package devpod

import (
	"bufio"
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	assets "github.com/vanpelt/sparky/tools/sparkbox/deploy/kubernetes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/devpod/podspec"
)

//go:embed release/manifest-amd64.env release/manifest-arm64.env
var releaseManifests embed.FS

// Source is the decoded manifests plus the release metadata they pin.
type Source struct {
	// Node is the five-container VM-node Pod this package renders.
	Node *podspec.Deployment
	// Gateway is the public control plane. The dev pod does not run it —
	// Options.GatewayAddr points at whatever does — but it is loaded so a
	// change to it also has to pass the strict decode.
	Gateway *podspec.Deployment
	// Service is the public LoadBalancer, read for its port list.
	Service *podspec.Service
	// Policies is network-policy.yaml. Nothing local enforces any of it; it is
	// loaded so the plan can name what it is not enforcing rather than leave a
	// reader to assume the dev pod's egress is confined the way CKS confines
	// it.
	Policies []podspec.NetworkPolicy
	// Release is the release the node Pod's SPARKBOX_RELEASE names, per arch.
	Release map[string]ReleaseManifest
}

// ReleaseManifest is one published manifest-<arch>.env. The checksums here are
// the release's own; entrypoint.sh hardcodes the amd64 values as defaults and
// reads every one of them from the environment first, which is how an arch it
// was not written for can still be pinned.
type ReleaseManifest struct {
	Release            string
	Arch               string
	FirecrackerVersion string
	SHA256Vmlinux      string
	SHA256Firecracker  string
	SHA256Jailer       string
	SHA256Rootfs       string
	RootfsAsset        string
	RootfsLoginUser    string
	SparkboxAsset      string
	SHA256Sparkbox     string
	SluiceAsset        string
	SHA256Sluice       string
}

// placeholderPattern matches the __NAME__ tokens deploy.sh substitutes with
// sed. A token that is not in substitutions below is unknown, and an unknown
// token means the manifest grew a knob this translator does not set.
var placeholderPattern = regexp.MustCompile(`__[A-Z0-9_]+__`)

// substitutions lists every placeholder deploy.sh replaces (see the sed blocks
// in deploy/kubernetes/deploy.sh). Each becomes a distinctive token at load
// time rather than a real value, because Load has no deployment options: the
// token carries the placeholder's identity into BuildPlan, which resolves it
// or fails. A token that survives into the emitted argv is a bug, and
// resolvePlaceholders catches it.
var substitutions = []string{
	"__SPARKBOX_IMAGE__",
	"__SPARKBOX_PROXY_DOMAIN__",
	"__SPARKBOX_NODE_POOL__",
	"__SPARKBOX_NODE__",
	"__SPARKBOX_HIVEMIND_API__",
	"__SPARKBOX_HIVEMIND_SIGNIN_ORGS__",
	"__HIVEMIND_MANIFEST__",
	"__SPARKBOX_USERS_HASH__",
	"__SPARKBOX_GITHUB_APP_CLIENT_ID__",
}

// token is the load-time stand-in for a placeholder. It is a plain YAML scalar
// so the document still parses, and it is greppable so an unresolved one is
// obvious in a diff.
func token(placeholder string) string {
	return "unresolved-placeholder-" + strings.ToLower(strings.Trim(placeholder, "_"))
}

const tokenPrefix = "unresolved-placeholder-"

// humanize turns load-time tokens back into the __PLACEHOLDER__ they stand
// for, so a divergence about a dropped nodeSelector reads the way the manifest
// does instead of leaking this package's internal spelling.
func humanize(s string) string {
	for _, placeholder := range substitutions {
		s = strings.ReplaceAll(s, token(placeholder), placeholder)
	}
	return s
}

// Load reads the manifests embedded from deploy/kubernetes.
func Load() (*Source, error) {
	return load(
		assets.NodeDeployment,
		assets.GatewayDeployment,
		assets.LoadBalancer,
		assets.NetworkPolicies,
	)
}

// LoadFS reads deployment.yaml, gateway-deployment.yaml, service.yaml and
// network-policy.yaml from fsys instead of the embedded copies. Tests use it
// to prove that a manifest the translator does not understand fails the load.
func LoadFS(fsys fs.FS) (*Source, error) {
	read := func(name string) ([]byte, error) {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	node, err := read("deployment.yaml")
	if err != nil {
		return nil, err
	}
	gateway, err := read("gateway-deployment.yaml")
	if err != nil {
		return nil, err
	}
	service, err := read("service.yaml")
	if err != nil {
		return nil, err
	}
	policies, err := read("network-policy.yaml")
	if err != nil {
		return nil, err
	}
	return load(node, gateway, service, policies)
}

func load(node, gateway, service, policies []byte) (*Source, error) {
	nodeDoc, err := podspec.DecodeDeployment("deployment.yaml", substitute(node))
	if err != nil {
		return nil, err
	}
	if err := checkPlaceholders("deployment.yaml", node); err != nil {
		return nil, err
	}
	gatewayDoc, err := podspec.DecodeDeployment("gateway-deployment.yaml", substitute(gateway))
	if err != nil {
		return nil, err
	}
	if err := checkPlaceholders("gateway-deployment.yaml", gateway); err != nil {
		return nil, err
	}
	serviceDoc, err := podspec.DecodeService("service.yaml", substitute(service))
	if err != nil {
		return nil, err
	}
	if err := checkPlaceholders("service.yaml", service); err != nil {
		return nil, err
	}

	policyDocs, err := podspec.DecodeNetworkPolicies("network-policy.yaml", substitute(policies))
	if err != nil {
		return nil, err
	}
	if err := checkPlaceholders("network-policy.yaml", policies); err != nil {
		return nil, err
	}

	releases, err := loadReleaseManifests()
	if err != nil {
		return nil, err
	}
	src := &Source{Node: nodeDoc, Gateway: gatewayDoc, Service: serviceDoc, Policies: policyDocs, Release: releases}
	if err := src.checkRelease(); err != nil {
		return nil, err
	}
	return src, nil
}

func substitute(data []byte) []byte {
	out := data
	for _, placeholder := range substitutions {
		out = bytes.ReplaceAll(out, []byte(placeholder), []byte(token(placeholder)))
	}
	return out
}

// checkPlaceholders fails on a placeholder deploy.sh substitutes that this
// package has never heard of. Silently leaving it in place would put a literal
// __SPARKBOX_SOMETHING__ into a container's environment.
func checkPlaceholders(name string, data []byte) error {
	known := map[string]bool{}
	for _, placeholder := range substitutions {
		known[placeholder] = true
	}
	var unknown []string
	seen := map[string]bool{}
	for _, match := range placeholderPattern.FindAll(data, -1) {
		got := string(match)
		if known[got] || seen[got] {
			continue
		}
		seen[got] = true
		unknown = append(unknown, got)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("%s: unknown deploy.sh placeholder %s; add it to devpod.substitutions and give it a value in BuildPlan",
		name, strings.Join(unknown, ", "))
}

// checkRelease makes SPARKBOX_RELEASE in the node Pod and the embedded release
// manifests one fact rather than two. Bumping the manifest without refetching
// internal/devpod/release would otherwise pin the previous release's checksums
// onto the new assets, which fails as a mid-boot checksum mismatch inside a
// container instead of here.
func (s *Source) checkRelease() error {
	want := ""
	for _, container := range append(append([]podspec.Container{}, s.Node.Spec.Template.Spec.InitContainers...), s.Node.Spec.Template.Spec.Containers...) {
		for _, env := range container.Env {
			if env.Name != "SPARKBOX_RELEASE" {
				continue
			}
			if want != "" && want != env.Value {
				return fmt.Errorf("deployment.yaml pins two SPARKBOX_RELEASE values, %q and %q", want, env.Value)
			}
			want = env.Value
		}
	}
	if want == "" {
		return fmt.Errorf("deployment.yaml sets no SPARKBOX_RELEASE; devpod cannot pin guest asset checksums")
	}
	for arch, manifest := range s.Release {
		if manifest.Release != want {
			return fmt.Errorf("release/manifest-%s.env is %s but deployment.yaml pins SPARKBOX_RELEASE=%s; refetch the manifests from the release",
				arch, manifest.Release, want)
		}
	}
	return nil
}

func loadReleaseManifests() (map[string]ReleaseManifest, error) {
	out := map[string]ReleaseManifest{}
	entries, err := fs.ReadDir(releaseManifests, "release")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		data, err := releaseManifests.ReadFile("release/" + entry.Name())
		if err != nil {
			return nil, err
		}
		manifest, err := parseReleaseManifest(entry.Name(), data)
		if err != nil {
			return nil, err
		}
		out[manifest.Arch] = manifest
	}
	return out, nil
}

// parseReleaseManifest reads the KEY=VALUE form published beside the release
// artifacts. Unknown keys are ignored on purpose: the manifest is the release
// pipeline's contract, not this package's, and it grows keys (GATEWAY_PUBKEY
// arrived that way) that a node plan has no opinion about.
func parseReleaseManifest(name string, data []byte) (ReleaseManifest, error) {
	fields := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return ReleaseManifest{}, fmt.Errorf("%s: %q is not KEY=VALUE", name, line)
		}
		fields[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	if err := scanner.Err(); err != nil {
		return ReleaseManifest{}, fmt.Errorf("%s: %w", name, err)
	}
	manifest := ReleaseManifest{
		Release:            fields["RELEASE"],
		Arch:               fields["ARCH"],
		FirecrackerVersion: fields["FIRECRACKER_VERSION"],
		SHA256Vmlinux:      fields["SHA256_VMLINUX"],
		SHA256Firecracker:  fields["SHA256_FIRECRACKER"],
		SHA256Jailer:       fields["SHA256_JAILER"],
		SHA256Rootfs:       fields["SHA256_ROOTFS"],
		RootfsAsset:        fields["ROOTFS_ASSET"],
		RootfsLoginUser:    fields["ROOTFS_LOGIN_USER"],
		SparkboxAsset:      fields["SPARKBOX_ASSET"],
		SHA256Sparkbox:     fields["SHA256_SPARKBOX"],
		SluiceAsset:        fields["SLUICE_ASSET"],
		SHA256Sluice:       fields["SHA256_SLUICE"],
	}
	for field, value := range map[string]string{
		"RELEASE":            manifest.Release,
		"ARCH":               manifest.Arch,
		"SHA256_VMLINUX":     manifest.SHA256Vmlinux,
		"SHA256_FIRECRACKER": manifest.SHA256Firecracker,
		"SHA256_JAILER":      manifest.SHA256Jailer,
		"SHA256_ROOTFS":      manifest.SHA256Rootfs,
	} {
		if value == "" {
			return ReleaseManifest{}, fmt.Errorf("%s: missing %s", name, field)
		}
	}
	return manifest, nil
}

// EntrypointSHA256Pins returns the per-architecture asset checksums pinned in
// deploy/kubernetes/entrypoint.sh, as pins[arch][asset]. Assets are named the
// way the script names them: firecracker, jailer, kernel, rootfs.
//
// The script keeps each pin on its own
// `readonly <asset>_sha256_<arch>="${SPARKBOX_<ASSET>_SHA256_<ARCH>:-<sha>}"`
// line and says so in a comment, because hack/check-cks-pin.sh reads the same
// shape at build time. This reads it too, so a Go test can compare both arches
// against the release's own manifests with no network.
func EntrypointSHA256Pins() (map[string]map[string]string, error) {
	pattern := regexp.MustCompile(`(?m)^readonly ([a-z]+)_sha256_([a-z0-9]+)="\$\{SPARKBOX_[A-Z0-9_]+:-([0-9a-f]{64})\}"`)
	matches := pattern.FindAllStringSubmatch(string(assets.NodeEntrypoint), -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("entrypoint.sh declares no `readonly <asset>_sha256_<arch>=\"${SPARKBOX_..._<ARCH>:-<sha>}\"` pins; its checksum layout changed")
	}
	out := map[string]map[string]string{}
	for _, match := range matches {
		asset, arch, sum := match[1], match[2], match[3]
		if out[arch] == nil {
			out[arch] = map[string]string{}
		}
		out[arch][asset] = sum
	}
	return out, nil
}
