package devpod

import (
	"fmt"
	"net/netip"
	"path"
	"sort"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/deviceplugin"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/devpod/podspec"
)

// Options are the local facts the manifest cannot know.
type Options struct {
	Arch        string // "arm64" or "amd64"
	Image       string // the local image ref
	DataVolume  string // host dir (absolute path) or docker volume name for /var/lib/sparkbox
	Driver      string // "mock" or "firecracker"
	ProxyDomain string
	GatewayAddr string // the node's outbound nodelink target
	RealKVM     bool   // true when /dev/kvm genuinely exists on the docker host
	BinDir      string // optional: host dir of working-tree binaries to bind-mount

	// Prefix names the docker network, volumes and containers. Empty means
	// DefaultPrefix.
	Prefix string
	// TrustDir replaces the sparkbox-node-trust Secret: a host directory
	// holding gateway_host_key.pub and gateway_upstream_key.pub. Empty gets a
	// docker volume nothing populates, which the node cannot start without.
	TrustDir string
	// HostMemMB overrides SPARKBOX_HOST_MEM_MB. The manifest says 480000
	// because that is what a CKS node has; leaving it means admission control
	// admits far more than a laptop can hold. 0 keeps the manifest value and
	// records the divergence.
	HostMemMB int64
	// NodeName overrides SPARKBOX_NODE_NAME so a dev node does not enroll
	// under the live node's identity. Empty keeps the manifest value.
	NodeName string
	// HivemindAPI fills __SPARKBOX_HIVEMIND_API__. Empty disables the presence
	// lease, which is also the flag's own default.
	HivemindAPI string
	// NetworkSubnet is the docker bridge subnet. Empty means
	// DefaultNetworkSubnet; see the constant for why this is not left to
	// docker's allocator.
	NetworkSubnet string
}

// DefaultPrefix names everything this package creates in docker.
const DefaultPrefix = "sparkbox-dev"

// DefaultNetworkSubnet is the bridge subnet `devpod up` asks for explicitly.
//
// Without --subnet the daemon picks from its default address pool, and a stock
// daemon's is 172.17.0.0/12 handed out in /16 steps — a range that contains
// 172.30.0.0/16, and so the Pod's SPARKBOX_GUEST_SUBNET (172.30.0.0/20). It
// takes thirteen existing bridge networks to walk from 172.17 to 172.30, so
// the collision is unlikely rather than impossible; when it does happen the
// symptom is guest traffic routed to the bridge, which looks like anything but
// an address clash.
//
// The pool is daemon configuration, not a constant: measured on this Mac,
// OrbStack ships a pool of 192.168.x.0/24 bases instead, where the collision
// cannot occur at all. The dev pod is meant to run against the Apple container
// machine's stock daemon, which sets no override. Asking for a subnet outside
// the guest range costs nothing and removes the question; BuildPlan fails if
// the two ever do overlap.
const DefaultNetworkSubnet = "172.29.128.0/24"

// Plan is the rendered pod: what to create, in the order to create it.
type Plan struct {
	Options    Options
	Network    Network
	Volumes    []Volume
	Containers []Container
	// StopTimeout comes from the Pod's terminationGracePeriodSeconds.
	StopTimeout int64
	// FSGroup is the Pod's securityContext.fsGroup, emulated in DockerArgv.
	//
	// It is not cosmetic and skipping it does not merely lose a permission
	// nicety: sluice creates its control socket as root with mode 0660, and
	// sparkbox-node runs as uid 65532. On CKS the shared emptyDir is
	// group-owned by fsGroup, so the node's gid matches and it connects. Get
	// this wrong locally and the node hangs forever in the sluice-readiness
	// poll at the top of entrypoint.sh, logging nothing at all — which is a
	// remarkably hard failure to read, because every container reports Up and
	// the two with healthchecks report healthy. Measured, on the first run
	// that got this far.
	FSGroup *int64

	divergences []Divergence
}

// Network is the shared namespace the containers live in. Kubernetes gives a
// Pod one netns; docker's equivalent is a holder container the rest join, and
// the node Pod genuinely needs that: vmm-helper creates the TAPs and the
// sluice resolver address that sluice binds and sparkbox-node routes to.
type Network struct {
	Name        string
	HolderName  string
	HolderImage string
	// Subnet is the bridge's address range, always set: see
	// DefaultNetworkSubnet for why it is never left to docker's allocator.
	Subnet string
	// Sysctls are applied to the namespace at creation because
	// vmm-helper-entrypoint.sh writes them best-effort ("|| true"): /proc/sys
	// is read-only inside a container that is not privileged.
	Sysctls []KeyValue
	// Ports are published on the holder. A container that joins another
	// container's namespace cannot publish anything itself.
	Ports []Port
	// ServicePorts is service.yaml's port list, kept for whoever runs the
	// gateway locally. It is not published here; see Divergences.
	ServicePorts []Port
}

// KeyValue is an ordered name/value pair.
type KeyValue struct{ Name, Value string }

// Port is one published port.
type Port struct {
	Name      string
	HostIP    string
	HostPort  int32
	Container int32
	Protocol  string
}

// Container is one rendered container.
type Container struct {
	Name  string
	Init  bool
	Image string

	Command []string // container command; entrypoint override when set
	Args    []string
	Env     []KeyValue

	User            string // "uid:gid"
	CapDrop         []string
	CapAdd          []string
	Privileged      bool
	ReadOnlyRootfs  bool
	NoNewPrivileges bool
	SecurityOpts    []string

	Devices []Device
	Mounts  []Mount
	Health  *Health
}

// Device is one granted host device. In CKS these arrive through
// internal/deviceplugin; locally they are --device flags built from the same
// table, imported rather than copied.
type Device struct {
	Resource      string
	HostPath      string
	ContainerPath string
	Permissions   string
}

// Mount is one rendered volume mount.
type Mount struct {
	Volume   string
	Kind     string // "bind" or "volume"
	Source   string
	Target   string
	SubPath  string
	ReadOnly bool
}

// Volume is one rendered pod volume.
type Volume struct {
	Name   string
	Kind   string // "bind" or "volume"
	Source string
	Create bool // docker volume create is needed
	// Ephemeral says the volume's contents belong to one run, so `devpod down`
	// may delete it. It comes from the manifest volume's KIND, not from
	// whether this plan happens to create it: an emptyDir, a Secret and a
	// projected volume are per-Pod and are gone when the Pod is, while the
	// hostPath data tier holds the VM inventory, the guest disks, the control
	// SQLite, the node identity and the rootfs template — deleting that on
	// teardown would destroy exactly the state a dev node is supposed to keep
	// between runs.
	Ephemeral bool
}

// Health is a probe rendered as a docker HEALTHCHECK.
type Health struct {
	Test           []string
	IntervalSec    int32
	Retries        int32
	StartPeriodSec int32
	// TimeoutSec is the probe's timeoutSeconds, defaulted the way Kubernetes
	// defaults it (1s) rather than the way docker does (30s). A curl against a
	// wedged unix socket is exactly the case the two disagree about, so the
	// value is always emitted.
	TimeoutSec int32
}

// Divergence is one deliberate deviation from the manifest. Every one of them
// is printed by `sparkbox devpod diff`; the point is that the list is short,
// enumerated, and explained rather than discovered later.
type Divergence struct {
	Area     string
	What     string
	Why      string
	Blocking bool // the pod will not run correctly until this is resolved
}

// knownContainers is the overlay's per-container knowledge. A container in
// deployment.yaml with no entry here fails BuildPlan: a new container in the
// node Pod is a decision someone has to make locally, not something to render
// with defaults and hope.
var knownContainers = map[string]struct {
	// Binaries are the image paths under /usr/local/bin that Options.BinDir
	// can shadow with a working-tree build.
	Binaries []string
}{
	"scrub-retired-gateway-state": {},
	"prepare-vm-assets":           {},
	"vmm-helper":                  {Binaries: []string{"sparkbox-vmm-helper"}},
	"sluice":                      {Binaries: []string{"sluice"}},
	// entrypoint.sh pings the helper with /usr/local/bin/sparkbox-vmm-helper
	// before it execs sparkbox, so this container needs both.
	"sparkbox-node": {Binaries: []string{"sparkbox", "sparkbox-vmm-helper"}},
}

// podSysctls mirrors the sysctls vmm-helper-entrypoint.sh sets. They are
// applied to the namespace holder instead, because the script itself can only
// try: CKS exposes /proc/sys read-only on some nodes and docker always does
// for an unprivileged container. TestPodSysctlsMatchHelperEntrypoint keeps
// this list equal to the script's.
var podSysctls = []KeyValue{
	{"net.ipv4.ip_forward", "1"},
	{"net.ipv6.conf.all.forwarding", "1"},
	{"net.ipv4.conf.all.rp_filter", "1"},
	{"net.ipv4.conf.default.rp_filter", "1"},
}

// BuildPlan renders the node Pod for the local runtime.
func BuildPlan(src *Source, o Options) (*Plan, error) {
	if src == nil {
		return nil, fmt.Errorf("devpod: nil source")
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	if o.Prefix == "" {
		o.Prefix = DefaultPrefix
	}

	pod := src.Node.Spec.Template.Spec
	p := &Plan{Options: o}
	if pod.TerminationGracePeriodSeconds != nil {
		p.StopTimeout = *pod.TerminationGracePeriodSeconds
	}
	if pod.SecurityContext != nil {
		p.FSGroup = pod.SecurityContext.FSGroup
	}

	if err := p.buildVolumes(pod); err != nil {
		return nil, err
	}
	if err := p.buildNetwork(src, pod); err != nil {
		return nil, err
	}
	if err := p.buildContainers(src, pod); err != nil {
		return nil, err
	}
	p.recordDivergences(src, pod)

	if err := p.checkResolved(); err != nil {
		return nil, err
	}
	return p, nil
}

func (o Options) validate() error {
	switch o.Arch {
	case "amd64", "arm64":
	default:
		return fmt.Errorf("devpod: unsupported arch %q (want amd64 or arm64)", o.Arch)
	}
	switch o.Driver {
	case "mock", "firecracker":
	default:
		return fmt.Errorf("devpod: unsupported driver %q (want mock or firecracker)", o.Driver)
	}
	if o.Image == "" {
		return fmt.Errorf("devpod: Options.Image is required")
	}
	if o.DataVolume == "" {
		return fmt.Errorf("devpod: Options.DataVolume is required")
	}
	if strings.HasPrefix(o.DataVolume, "~") {
		return fmt.Errorf("devpod: Options.DataVolume %q must be an absolute path or a docker volume name", o.DataVolume)
	}
	if o.ProxyDomain == "" {
		return fmt.Errorf("devpod: Options.ProxyDomain is required")
	}
	if o.BinDir != "" && !path.IsAbs(o.BinDir) {
		return fmt.Errorf("devpod: Options.BinDir %q must be an absolute path", o.BinDir)
	}
	if o.TrustDir != "" && !path.IsAbs(o.TrustDir) {
		return fmt.Errorf("devpod: Options.TrustDir %q must be an absolute path", o.TrustDir)
	}
	return nil
}

// isBind reports whether a data location names a host path rather than a
// docker volume.
func isBind(source string) bool { return strings.HasPrefix(source, "/") }

func (p *Plan) buildVolumes(pod podspec.PodSpec) error {
	o := p.Options
	for _, v := range pod.Volumes {
		switch {
		case v.HostPath != nil:
			// The one hostPath in this Pod is the data tier, and its path is
			// chosen for reflink support rather than for its name. Any other
			// hostPath is a new host dependency that needs a local decision.
			if v.Name != "data" {
				return fmt.Errorf("devpod: unmapped hostPath volume %q (%s); decide where it lives locally", v.Name, v.HostPath.Path)
			}
			kind := "volume"
			if isBind(o.DataVolume) {
				kind = "bind"
			}
			// Durable: this is the data tier. `devpod down` must not delete it.
			p.Volumes = append(p.Volumes, Volume{Name: v.Name, Kind: kind, Source: o.DataVolume, Create: kind == "volume"})
		case v.Secret != nil:
			if v.Name != "gateway-trust" {
				return fmt.Errorf("devpod: unmapped Secret volume %q (secret %s); a local pod has no cluster Secrets", v.Name, v.Secret.SecretName)
			}
			if o.TrustDir != "" {
				// A host directory the user curated: theirs to delete, not ours.
				p.Volumes = append(p.Volumes, Volume{Name: v.Name, Kind: "bind", Source: o.TrustDir})
				continue
			}
			p.Volumes = append(p.Volumes, Volume{Name: v.Name, Kind: "volume", Source: p.volumeName(v.Name), Create: true, Ephemeral: true})
		case v.EmptyDir != nil:
			p.Volumes = append(p.Volumes, Volume{Name: v.Name, Kind: "volume", Source: p.volumeName(v.Name), Create: true, Ephemeral: true})
		case v.PersistentVolumeClaim != nil:
			return fmt.Errorf("devpod: volume %q is a PersistentVolumeClaim (%s); the node Pod has none and there is no local storageClass", v.Name, v.PersistentVolumeClaim.ClaimName)
		case v.Projected != nil:
			// Modeled by podspec so that introducing one is a decision. It is
			// one here too: a projected serviceAccountToken has no local
			// meaning, and a projected Secret's item paths are a layout this
			// package does not reproduce.
			return fmt.Errorf("devpod: volume %q is a projected volume; there is no API server to mint a serviceAccountToken and no cluster Secret to project, so decide what it means locally", v.Name)
		default:
			return fmt.Errorf("devpod: volume %q has no source this package understands", v.Name)
		}
	}
	return nil
}

func (p *Plan) volumeName(name string) string { return p.Options.Prefix + "-" + name }

func (p *Plan) findVolume(name string) (Volume, bool) {
	for _, v := range p.Volumes {
		if v.Name == name {
			return v, true
		}
	}
	return Volume{}, false
}

func (p *Plan) buildNetwork(src *Source, pod podspec.PodSpec) error {
	o := p.Options
	subnet := o.NetworkSubnet
	if subnet == "" {
		subnet = DefaultNetworkSubnet
	}
	bridge, err := netip.ParsePrefix(subnet)
	if err != nil {
		return fmt.Errorf("devpod: Options.NetworkSubnet %q is not a CIDR: %w", subnet, err)
	}
	// The guest subnet is read from the Pod rather than restated, so this check
	// keeps working if the manifest moves the guests somewhere else.
	if guest := guestSubnet(pod); guest != "" {
		guestPrefix, err := netip.ParsePrefix(guest)
		if err != nil {
			return fmt.Errorf("devpod: the Pod's SPARKBOX_GUEST_SUBNET %q is not a CIDR: %w", guest, err)
		}
		if bridge.Overlaps(guestPrefix) {
			return fmt.Errorf("devpod: the docker bridge subnet %s overlaps the Pod's guest subnet %s; guest traffic would route to the bridge instead of a TAP. Pick a different Options.NetworkSubnet", bridge, guestPrefix)
		}
	}
	p.Network = Network{
		Name:        o.Prefix + "-net",
		HolderName:  o.Prefix + "-netns",
		HolderImage: o.Image,
		Subnet:      bridge.String(),
		Sysctls:     append([]KeyValue(nil), podSysctls...),
	}

	// Publish what the Pod itself declares. Bound to loopback: this is a
	// development namespace, not a service.
	for _, c := range pod.Containers {
		for _, port := range c.Ports {
			protocol := strings.ToLower(port.Protocol)
			if protocol == "" {
				protocol = "tcp"
			}
			p.Network.Ports = append(p.Network.Ports, Port{
				Name:      port.Name,
				HostIP:    "127.0.0.1",
				HostPort:  port.ContainerPort,
				Container: port.ContainerPort,
				Protocol:  protocol,
			})
		}
	}

	// service.yaml's LoadBalancer selects the gateway, not this Pod. Read it
	// anyway and carry the ports, so that if the Service is ever repointed the
	// dev pod publishes them without anyone editing this file.
	matches := selects(src.Service.Spec.Selector, src.Node.Spec.Template.Metadata.Labels)
	// A named targetPort ("targetPort: https") is a name in the Pod the
	// selector actually matches, so resolve it there. Today that is the
	// gateway, which is why gateway-deployment.yaml is loaded: without this the
	// carried ports would all claim the Service's own port number as the
	// container port, which is wrong for every one of them (80 and 443 and the
	// twenty web-dev ports all target the gateway's 8081).
	target, targetName := pod, "the node Pod"
	if !matches {
		if selects(src.Service.Spec.Selector, src.Gateway.Spec.Template.Metadata.Labels) {
			target, targetName = src.Gateway.Spec.Template.Spec, "the gateway Pod"
		} else {
			target, targetName = podspec.PodSpec{}, "no Pod in deploy/kubernetes"
		}
	}
	for _, sp := range src.Service.Spec.Ports {
		protocol := strings.ToLower(sp.Protocol)
		if protocol == "" {
			protocol = "tcp"
		}
		port := Port{Name: sp.Name, HostIP: "127.0.0.1", HostPort: sp.Port, Container: sp.Port, Protocol: protocol}
		switch {
		case sp.TargetPort.IsInt:
			port.Container = sp.TargetPort.IntVal
		case sp.TargetPort.StrVal == "":
			// Kubernetes defaults an unset targetPort to the Service port.
		default:
			named, ok := namedPort(target, sp.TargetPort.StrVal)
			if !ok {
				return fmt.Errorf("devpod: service port %s targets port %q, which %s declares no containerPort for; devpod will not guess a port number",
					sp.Name, sp.TargetPort.StrVal, targetName)
			}
			port.Container = named
		}
		if matches {
			p.Network.Ports = append(p.Network.Ports, port)
		} else {
			p.Network.ServicePorts = append(p.Network.ServicePorts, port)
		}
	}
	return nil
}

// selects reports whether an equality selector matches a label set. An empty
// selector matches nothing here on purpose: a Service with no selector picks
// its endpoints from a hand-managed Endpoints object, which is not this.
func selects(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for key, want := range selector {
		if labels[key] != want {
			return false
		}
	}
	return true
}

func namedPort(pod podspec.PodSpec, name string) (int32, bool) {
	for _, c := range pod.Containers {
		for _, port := range c.Ports {
			if port.Name == name {
				return port.ContainerPort, true
			}
		}
	}
	return 0, false
}

// guestSubnet reads SPARKBOX_GUEST_SUBNET out of the Pod. Like sluiceDNSIP,
// reading beats restating: the guest range is a manifest fact.
func guestSubnet(pod podspec.PodSpec) string {
	for _, c := range append(append([]podspec.Container{}, pod.InitContainers...), pod.Containers...) {
		for _, env := range c.Env {
			if env.Name == "SPARKBOX_GUEST_SUBNET" {
				return env.Value
			}
		}
	}
	return ""
}

func (p *Plan) buildContainers(src *Source, pod podspec.PodSpec) error {
	for _, c := range pod.InitContainers {
		built, err := p.buildContainer(src, c, true)
		if err != nil {
			return err
		}
		p.Containers = append(p.Containers, built)
	}
	for _, c := range pod.Containers {
		built, err := p.buildContainer(src, c, false)
		if err != nil {
			return err
		}
		p.Containers = append(p.Containers, built)
	}
	return nil
}

func (p *Plan) buildContainer(src *Source, c podspec.Container, init bool) (Container, error) {
	role, known := knownContainers[c.Name]
	if !known {
		return Container{}, fmt.Errorf("devpod: deployment.yaml container %q has no devpod overlay entry; add one to knownContainers and say what it needs locally", c.Name)
	}

	out := Container{
		Name:    c.Name,
		Init:    init,
		Image:   p.Options.Image, // the manifest's image is __SPARKBOX_IMAGE__
		Command: append([]string(nil), c.Command...),
		Args:    append([]string(nil), c.Args...),
	}

	env, err := p.buildEnv(src, c)
	if err != nil {
		return Container{}, err
	}
	out.Env = env

	if sc := c.SecurityContext; sc != nil {
		// runAsNonRoot is an assertion the kubelet enforces before start: it
		// refuses to run the container if the resolved UID is 0. docker has no
		// equivalent, so the only honest local reproduction is to check what
		// this plan is about to emit and fail here instead.
		if sc.RunAsNonRoot != nil && *sc.RunAsNonRoot {
			if sc.RunAsUser == nil {
				return Container{}, fmt.Errorf("devpod: container %q sets runAsNonRoot:true with no runAsUser; the kubelet would resolve the image's USER and refuse root, and docker would silently run as the image's user", c.Name)
			}
			if *sc.RunAsUser == 0 {
				return Container{}, fmt.Errorf("devpod: container %q sets runAsNonRoot:true and runAsUser:0; the kubelet would refuse to start it", c.Name)
			}
		}
		if sc.RunAsUser != nil {
			user := fmt.Sprintf("%d", *sc.RunAsUser)
			if sc.RunAsGroup != nil {
				user = fmt.Sprintf("%d:%d", *sc.RunAsUser, *sc.RunAsGroup)
			}
			out.User = user
		}
		if sc.Capabilities != nil {
			out.CapDrop = append([]string(nil), sc.Capabilities.Drop...)
			out.CapAdd = append([]string(nil), sc.Capabilities.Add...)
		}
		out.Privileged = sc.Privileged != nil && *sc.Privileged
		out.ReadOnlyRootfs = sc.ReadOnlyRootFilesystem != nil && *sc.ReadOnlyRootFilesystem
		out.NoNewPrivileges = sc.AllowPrivilegeEscalation != nil && !*sc.AllowPrivilegeEscalation
		if sc.SeccompProfile != nil {
			switch sc.SeccompProfile.Type {
			case "RuntimeDefault":
			case "Unconfined":
				out.SecurityOpts = append(out.SecurityOpts, "seccomp=unconfined")
			default:
				return Container{}, fmt.Errorf("devpod: container %q uses seccompProfile type %q, which devpod does not translate", c.Name, sc.SeccompProfile.Type)
			}
		}
		if sc.AppArmorProfile != nil {
			switch sc.AppArmorProfile.Type {
			case "RuntimeDefault":
			case "Unconfined":
				out.SecurityOpts = append(out.SecurityOpts, "apparmor=unconfined")
			default:
				return Container{}, fmt.Errorf("devpod: container %q uses appArmorProfile type %q, which devpod does not translate", c.Name, sc.AppArmorProfile.Type)
			}
		}
	}

	devices, err := p.devicesFor(c)
	if err != nil {
		return Container{}, err
	}
	out.Devices = devices

	mounts, err := p.mountsFor(c)
	if err != nil {
		return Container{}, err
	}
	for _, binary := range role.Binaries {
		if p.Options.BinDir == "" {
			continue
		}
		mounts = append(mounts, Mount{
			Volume:   "working-tree-bin",
			Kind:     "bind",
			Source:   path.Join(p.Options.BinDir, binary),
			Target:   "/usr/local/bin/" + binary,
			ReadOnly: true,
		})
	}
	sortMounts(mounts)
	out.Mounts = mounts

	out.Health = healthFor(c)

	// The image entrypoint ends with "$@", so an appended flag reaches
	// `sparkbox serve` and wins over the entrypoint's own: Go's flag package
	// keeps the last occurrence. That is why the mock driver needs no second
	// copy of the entrypoint's forty-flag command line.
	if c.Name == "sparkbox-node" && p.Options.Driver == "mock" {
		out.Args = append(out.Args, "--driver", "mock")
	}
	return out, nil
}

func (p *Plan) buildEnv(src *Source, c podspec.Container) ([]KeyValue, error) {
	o := p.Options
	// Values the local pod has to supply for a placeholder or a
	// cluster-specific fact. Each is applied in place so env order still
	// matches the manifest.
	overrides := map[string]string{
		token("__SPARKBOX_PROXY_DOMAIN__"): o.ProxyDomain,
		token("__SPARKBOX_HIVEMIND_API__"): o.HivemindAPI,
		token("__HIVEMIND_MANIFEST__"):     "", // empty means "the latest release"
	}

	var out []KeyValue
	for _, env := range c.Env {
		if env.ValueFrom != nil {
			return nil, fmt.Errorf("devpod: container %q reads env %s from a cluster Secret; a local pod has none", c.Name, env.Name)
		}
		value := env.Value
		if replacement, ok := overrides[value]; ok {
			value = replacement
		}
		switch env.Name {
		case "SPARKBOX_GATEWAY_ADDR":
			// Only override when set, like the two below it. Overriding
			// unconditionally emptied the variable whenever -gateway was
			// omitted, which reads as "no gateway configured" rather than as
			// the manifest's cluster address the plan claims to carry.
			if o.GatewayAddr != "" {
				value = o.GatewayAddr
			}
		case "SPARKBOX_NODE_NAME":
			if o.NodeName != "" {
				value = o.NodeName
			}
		case "SPARKBOX_HOST_MEM_MB":
			if o.HostMemMB > 0 {
				value = fmt.Sprintf("%d", o.HostMemMB)
			}
		}
		out = append(out, KeyValue{env.Name, value})
	}

	// No SPARKBOX_*_SHA256 overrides are emitted, deliberately. entrypoint.sh
	// carries a pin per architecture and resolves it from uname, and --platform
	// makes uname agree with Options.Arch. Injecting our own copy of the
	// checksums here would make the plan a second place they are written down,
	// which is the failure this package exists to prevent.
	if c.Name == "prepare-vm-assets" {
		if _, ok := src.Release[o.Arch]; !ok {
			return nil, fmt.Errorf("devpod: no release manifest embedded for arch %q; devpod cannot say what this pod will download", o.Arch)
		}
	}
	return out, nil
}

// devicesFor resolves the sparkbox.dev/* extended resources a container asks
// for into host devices, using internal/deviceplugin's own table. Importing it
// rather than restating it is the point: the device plugin and the dev pod
// cannot disagree about what /dev/loop bundle means.
func (p *Plan) devicesFor(c podspec.Container) ([]Device, error) {
	if c.Resources == nil {
		return nil, nil
	}
	wanted := map[string]bool{}
	for _, list := range []podspec.ResourceList{c.Resources.Requests, c.Resources.Limits} {
		for name := range list {
			if strings.HasPrefix(name, "sparkbox.dev/") {
				wanted[name] = true
			}
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)

	byName := map[string]deviceplugin.Resource{}
	for _, resource := range deviceplugin.DefaultResources("") {
		byName[resource.Name] = resource
	}

	var out []Device
	for _, name := range names {
		resource, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("devpod: container %q requests %s, which internal/deviceplugin does not serve", c.Name, name)
		}
		for _, device := range resource.Devices {
			if device.HostPath == "/dev/kvm" && !p.Options.RealKVM {
				continue
			}
			out = append(out, Device{
				Resource:      name,
				HostPath:      device.HostPath,
				ContainerPath: device.ContainerPath,
				Permissions:   device.Permissions,
			})
		}
	}
	return out, nil
}

func (p *Plan) mountsFor(c podspec.Container) ([]Mount, error) {
	var out []Mount
	for _, m := range c.VolumeMounts {
		volume, ok := p.findVolume(m.Name)
		if !ok {
			return nil, fmt.Errorf("devpod: container %q mounts volume %q, which the Pod does not declare", c.Name, m.Name)
		}
		mount := Mount{
			Volume:   m.Name,
			Kind:     volume.Kind,
			Source:   volume.Source,
			Target:   m.MountPath,
			SubPath:  m.SubPath,
			ReadOnly: m.ReadOnly,
		}
		if m.SubPath != "" && volume.Kind == "bind" {
			// A bind has no subpath option worth using: the subdirectory of
			// the host path IS the subpath, and `devpod up` creates it.
			mount.Source = path.Join(volume.Source, m.SubPath)
		}
		out = append(out, mount)
	}
	return out, nil
}

// sortMounts orders by target depth so a parent mount lands before the mounts
// laid over it. Docker sorts internally too; emitting them in order keeps the
// argv reviewable, because the node's read-only /var/lib/sparkbox has to come
// before the writable subPaths that punch through it.
func sortMounts(mounts []Mount) {
	sort.SliceStable(mounts, func(i, j int) bool {
		di, dj := strings.Count(mounts[i].Target, "/"), strings.Count(mounts[j].Target, "/")
		if di != dj {
			return di < dj
		}
		return mounts[i].Target < mounts[j].Target
	})
}

// healthFor renders an exec probe as a docker HEALTHCHECK. Only exec probes
// translate; a tcpSocket probe has no docker equivalent and is recorded as a
// divergence instead. Readiness is the one modeled: it is the probe that says
// "this container is serving", which is what `devpod status` wants to show.
func healthFor(c podspec.Container) *Health {
	probe := c.ReadinessProbe
	if probe == nil || probe.Exec == nil || len(probe.Exec.Command) == 0 {
		return nil
	}
	health := &Health{
		Test:        append([]string{"CMD"}, probe.Exec.Command...),
		IntervalSec: probe.PeriodSeconds,
		Retries:     probe.FailureThreshold,
		TimeoutSec:  probe.TimeoutSeconds,
	}
	if health.TimeoutSec == 0 {
		// Kubernetes' default, which the manifest relies on by not setting it.
		// docker's default is 30s, so leaving this unset would give the local
		// pod a probe thirty times more patient than the deployed one.
		health.TimeoutSec = 1
	}
	if c.StartupProbe != nil && c.StartupProbe.Exec != nil {
		// A startup probe buys a slower container more time before failures
		// count. docker spells the same idea --start-period.
		health.StartPeriodSec = c.StartupProbe.PeriodSeconds * c.StartupProbe.FailureThreshold
	}
	return health
}

// checkResolved fails on any placeholder token that reached the rendered plan.
// It is the backstop for substitutions gaining an entry that BuildPlan never
// gives a value.
func (p *Plan) checkResolved() error {
	report := func(where, value string) error {
		if !strings.Contains(value, tokenPrefix) {
			return nil
		}
		return fmt.Errorf("devpod: %s still holds %q; BuildPlan gives that deploy.sh placeholder no value", where, value)
	}
	for _, c := range p.Containers {
		for _, env := range c.Env {
			if err := report(fmt.Sprintf("container %s env %s", c.Name, env.Name), env.Value); err != nil {
				return err
			}
		}
		for _, arg := range append(append([]string{c.Image}, c.Command...), c.Args...) {
			if err := report("container "+c.Name, arg); err != nil {
				return err
			}
		}
		for _, m := range c.Mounts {
			if err := report(fmt.Sprintf("container %s mount %s", c.Name, m.Target), m.Source); err != nil {
				return err
			}
		}
	}
	return nil
}

// Divergences returns every deliberate deviation from the manifest.
func (p *Plan) Divergences() []Divergence {
	return append([]Divergence(nil), p.divergences...)
}

func (p *Plan) add(d Divergence) { p.divergences = append(p.divergences, d) }

func (p *Plan) recordDivergences(src *Source, pod podspec.PodSpec) {
	o := p.Options

	if len(pod.NodeSelector) > 0 {
		keys := make([]string, 0, len(pod.NodeSelector))
		for key := range pod.NodeSelector {
			keys = append(keys, key+"="+pod.NodeSelector[key])
		}
		sort.Strings(keys)
		p.add(Divergence{
			Area: "nodeSelector",
			What: "dropped: " + humanize(strings.Join(keys, ", ")),
			Why:  "there is no scheduler. The CoreWeave node-pool and hostname labels name one physical machine, and kubernetes.io/arch is pinned to the cluster's amd64 while this plan renders " + o.Arch + ".",
		})
	}

	// Resource requests are recorded, never edited. Editing cpu:"48" and
	// memory:400Gi down to fit a laptop is what makes a local environment lie:
	// you would then be testing numbers nobody deployed.
	var asks []string
	for _, c := range append(append([]podspec.Container{}, pod.InitContainers...), pod.Containers...) {
		if c.Resources == nil || len(c.Resources.Requests) == 0 {
			continue
		}
		cpu, memory := c.Resources.Requests["cpu"], c.Resources.Requests["memory"]
		if cpu == "" && memory == "" {
			continue
		}
		asks = append(asks, fmt.Sprintf("%s cpu=%s memory=%s", c.Name, cpu, memory))
	}
	p.add(Divergence{
		Area: "resources",
		What: "requests and limits are not enforced (" + strings.Join(asks, "; ") + ")",
		Why:  "docker has no scheduler to admit against and no node with 400Gi. The manifest values are left exactly as deployed so the plan keeps reporting what CKS reserves.",
	})

	if o.HostMemMB == 0 {
		p.add(Divergence{
			Area:     "admission",
			What:     "SPARKBOX_HOST_MEM_MB is the manifest's CKS value",
			Why:      "admission control will size itself for the cluster node rather than this machine. Set Options.HostMemMB.",
			Blocking: true,
		})
	}

	// SPARKBOX_GATEWAY_ADDR decides whether this node is a node at all: with
	// no gateway it boots, serves metrics, and links to nothing. Report the
	// value the plan actually emits, not the option that may not have set it.
	switch addr := p.envValue("sparkbox-node", "SPARKBOX_GATEWAY_ADDR"); {
	case addr == "":
		p.add(Divergence{
			Area:     "gateway link",
			What:     "SPARKBOX_GATEWAY_ADDR is empty",
			Why:      "neither Options.GatewayAddr nor the manifest supplies one, so the node runs unlinked: it never dials a gateway, no sandbox is reachable through SSH or the public edge, and nothing on the control plane knows this node exists. Set Options.GatewayAddr.",
			Blocking: true,
		})
	case o.GatewayAddr == "":
		p.add(Divergence{
			Area:     "gateway link",
			What:     "SPARKBOX_GATEWAY_ADDR is the manifest's " + addr,
			Why:      "Options.GatewayAddr is unset, so the plan carries the cluster's in-namespace Service DNS name. It does not resolve outside CKS, so the node retries a link it can never make. Set Options.GatewayAddr to whatever runs the gateway locally.",
			Blocking: true,
		})
	}

	p.add(Divergence{
		Area: "pod network",
		What: "the docker bridge is pinned to " + p.Network.Subnet,
		Why:  "a Pod does not own a bridge subnet, but a stock daemon's allocator takes one from 172.17.0.0/12 in /16 steps — a pool that contains the guest range " + guestSubnet(pod) + ", and a bridge that lands on top of the guest network breaks routing in a way that looks like anything but an address collision.",
	})

	p.add(Divergence{
		Area: "pod network",
		What: "the Pod netns is a holder container (" + p.Network.HolderName + ") the others join with --network container:",
		Why:  "docker has no Pod. The shared namespace is load-bearing here: vmm-helper creates the TAPs and the " + sluiceDNSIP(pod) + " resolver address that sluice binds and sparkbox-node routes guests to.",
	})

	p.add(Divergence{
		Area: "pod network",
		What: fmt.Sprintf("sysctls %s are set on the namespace holder", sysctlNames()),
		Why:  "vmm-helper-entrypoint.sh sets them best-effort with '|| true' because /proc/sys is read-only for an unprivileged container. Setting them at namespace creation is the only way they take effect locally.",
	})

	if len(p.Network.ServicePorts) > 0 {
		p.add(Divergence{
			Area: "service",
			What: fmt.Sprintf("service.yaml's LoadBalancer (%d ports) is not published", len(p.Network.ServicePorts)),
			Why:  "its selector points at the gateway Pod, not this one. The ports are carried on Plan.Network.ServicePorts for whatever runs the gateway locally; if the Service is ever repointed at the node they publish automatically.",
		})
	}

	for _, v := range pod.Volumes {
		switch {
		case v.HostPath != nil:
			p.add(Divergence{
				Area: "volume " + v.Name,
				What: fmt.Sprintf("hostPath %s (type %s) -> %s, which `devpod down` keeps", v.HostPath.Path, v.HostPath.Type, o.DataVolume),
				Why:  "the CKS path is /mnt/local because that is the only storage there that supports reflinks. The local target must support them too — a VM clone is cp --reflink=always. It holds the VM inventory, the guest disks, the control SQLite, the node identity and the rootfs template, so teardown leaves it alone; `devpod down --purge-data` is the way to say otherwise. DirectoryOrCreate is reproduced by `devpod up` creating the bind sources itself.",
			})
		case v.Secret != nil:
			d := Divergence{
				Area: "volume " + v.Name,
				What: fmt.Sprintf("Secret %s (defaultMode %s) -> %s", v.Secret.SecretName, modeText(v.Secret.DefaultMode), mustSource(p, v.Name)),
				Why:  "there is no cluster Secret. defaultMode is not reproduced either; a bind keeps the host's modes.",
			}
			if len(v.Secret.Items) > 0 {
				paths := make([]string, 0, len(v.Secret.Items))
				for _, item := range v.Secret.Items {
					paths = append(paths, item.Key+"->"+item.Path)
				}
				d.What += fmt.Sprintf(", items %s not projected", strings.Join(paths, ", "))
				d.Why += " The item list renames keys into paths, which a plain bind of a host directory does not do: name the files that way on the host."
			}
			if v.Secret.Optional != nil && *v.Secret.Optional {
				d.What += ", optional:true not honored"
			}
			if o.TrustDir == "" {
				d.What += " (empty)"
				d.Why = "there is no cluster Secret and Options.TrustDir is unset, so nothing populates gateway_host_key.pub. sparkbox-node runs with --gateway-host-key and will not start without it."
				d.Blocking = true
			}
			p.add(d)
		case v.EmptyDir != nil:
			d := Divergence{
				Area: "volume " + v.Name,
				What: fmt.Sprintf("emptyDir sizeLimit %s -> unbounded docker volume %s", v.EmptyDir.SizeLimit, mustSource(p, v.Name)),
				Why:  "docker volumes have no size limit. `devpod down` removes this one, which is what keeps it per-run the way an emptyDir is.",
			}
			if v.EmptyDir.Medium != "" {
				// Medium: Memory is a tmpfs, and the difference is observable:
				// it is RAM-backed, it is charged to the Pod's memory limit,
				// and it does not survive a container restart.
				d.What += fmt.Sprintf(", medium %s -> disk", v.EmptyDir.Medium)
				d.Why += " A docker volume is disk-backed, so a Memory medium becomes an ordinary directory here."
			}
			p.add(d)
		}
	}

	if sc := pod.SecurityContext; sc != nil {
		if sc.FSGroup != nil {
			p.add(Divergence{
				Area: "securityContext",
				What: fmt.Sprintf("pod fsGroup %d is emulated with a chown pass, not applied by the runtime (fsGroupChangePolicy %s is ignored)",
					*sc.FSGroup, sc.FSGroupChangePolicy),
				Why: "docker has no fsGroup, so DockerArgv chowns each created volume to that group and sets the setgid bit — " +
					"what the kubelet does, and only for the volume types Kubernetes applies it to. The hostPath data tier is " +
					"deliberately excluded, exactly as in Kubernetes, because prepare-vm-assets owns those paths. " +
					"fsGroupChangePolicy has no analogue: the pass is unconditional, where OnRootMismatch would skip it when the " +
					"root already matches, so a large data tier would be slower here than on CKS.",
			})
		}
		// Everything else the Pod-level context can say is a privilege
		// decision that containerArgv does not emit, because docker applies
		// security options per container. Kubernetes would push these down as
		// defaults into every container that does not set its own, so a Pod
		// that grew one and said nothing here would run locally with a weaker
		// posture than CKS gives it.
		var podLevel []string
		if sc.RunAsNonRoot != nil {
			podLevel = append(podLevel, fmt.Sprintf("runAsNonRoot %t", *sc.RunAsNonRoot))
		}
		if sc.RunAsUser != nil {
			podLevel = append(podLevel, fmt.Sprintf("runAsUser %d", *sc.RunAsUser))
		}
		if sc.RunAsGroup != nil {
			podLevel = append(podLevel, fmt.Sprintf("runAsGroup %d", *sc.RunAsGroup))
		}
		if sc.SeccompProfile != nil {
			podLevel = append(podLevel, "seccompProfile "+sc.SeccompProfile.Type)
		}
		if len(podLevel) > 0 {
			p.add(Divergence{
				Area:     "securityContext",
				What:     "pod-level " + strings.Join(podLevel, ", ") + " is not pushed into the containers",
				Why:      "docker has no Pod-level security context: every container in this plan carries only its OWN securityContext. Kubernetes would default the containers that set none from these, so the local pod runs with a different posture than CKS. Set them per container in deployment.yaml, or teach buildContainer to inherit them.",
				Blocking: true,
			})
		}
	}

	if pod.ServiceAccountName != "" {
		mounted := "automountServiceAccountToken is unset, so Kubernetes would mount one"
		if pod.AutomountServiceAccountToken != nil && !*pod.AutomountServiceAccountToken {
			mounted = "automountServiceAccountToken is false in the manifest, so nothing in the Pod reads one"
		}
		p.add(Divergence{
			Area: "serviceAccount",
			What: "serviceAccountName " + pod.ServiceAccountName + " is ignored",
			Why:  "there is no API server. " + mounted + ".",
		})
	}

	p.add(Divergence{
		Area: "devices",
		What: "sparkbox.dev/kvm, /tun and /loop are granted with --device from internal/deviceplugin.DefaultResources",
		Why:  "the device plugin needs a kubelet. The device table is imported rather than copied, so the plugin and this plan cannot disagree about what the loop bundle contains.",
	})

	if !o.RealKVM {
		p.add(Divergence{
			Area:     "devices",
			What:     "/dev/kvm is not granted",
			Why:      "Options.RealKVM is false. vmm-helper-entrypoint.sh checks for a /dev/kvm character device and exits 1, so the helper container will fail its readiness probe until the pod runs somewhere KVM exists.",
			Blocking: true,
		})
	}

	// The Deployment wrapper itself: replicas, strategy and selector are
	// decoded and deliberately not acted on, because there is no controller to
	// act on them. Saying so keeps "modeled but unread" from meaning "silently
	// ignored".
	if spec := src.Node.Spec; spec.Replicas != nil || spec.Strategy != nil {
		what := []string{}
		if spec.Replicas != nil {
			what = append(what, fmt.Sprintf("replicas %d", *spec.Replicas))
		}
		if spec.Strategy != nil {
			what = append(what, "strategy "+spec.Strategy.Type)
		}
		p.add(Divergence{
			Area: "deployment",
			What: strings.Join(what, ", ") + " is not enforced; devpod runs exactly one pod",
			Why:  "there is no Deployment controller. Recreate matters in CKS because the node owns exclusive devices and a SQLite file and must never overlap itself; locally the same guarantee comes from the fixed container names, which docker refuses to duplicate.",
		})
	}

	p.add(Divergence{
		Area: "restartPolicy",
		What: "crashed containers are not restarted",
		Why:  "the PodSpec omits restartPolicy, so Kubernetes restarts Always. Locally a crash should stay visible instead of looping, so no --restart is emitted.",
	})

	p.add(Divergence{
		Area: "imagePullPolicy",
		What: "imagePullPolicy Always is ignored; the local image ref " + o.Image + " is used as-is",
		Why:  "the image being tested is a local build, and pulling would replace it.",
	})

	var tcpProbes, execLiveness, unreadProbeFields []string
	for _, c := range pod.Containers {
		for name, probe := range map[string]*podspec.Probe{"startup": c.StartupProbe, "readiness": c.ReadinessProbe, "liveness": c.LivenessProbe} {
			if probe == nil {
				continue
			}
			if probe.TCPSocket != nil {
				tcpProbes = append(tcpProbes, c.Name+" "+name)
			}
			if name == "liveness" && probe.Exec != nil {
				execLiveness = append(execLiveness, c.Name+": "+shellJoin(probe.Exec.Command))
			}
			// initialDelaySeconds and successThreshold are decoded but have no
			// docker equivalent this plan uses: --health-start-period is spent
			// on the startup probe, and a HEALTHCHECK is healthy after one
			// success. Both are unset today; say so if one appears.
			if probe.InitialDelaySeconds > 0 {
				unreadProbeFields = append(unreadProbeFields, fmt.Sprintf("%s %s initialDelaySeconds=%d", c.Name, name, probe.InitialDelaySeconds))
			}
			if probe.SuccessThreshold > 1 {
				unreadProbeFields = append(unreadProbeFields, fmt.Sprintf("%s %s successThreshold=%d", c.Name, name, probe.SuccessThreshold))
			}
		}
	}
	if len(tcpProbes) > 0 {
		sort.Strings(tcpProbes)
		p.add(Divergence{
			Area: "probes",
			What: "tcpSocket probes are not reproduced (" + strings.Join(tcpProbes, ", ") + ")",
			Why:  "docker's HEALTHCHECK runs a command in the container, and the image has no TCP prober. Exec readiness probes are translated; these are not.",
		})
	}
	if len(execLiveness) > 0 {
		sort.Strings(execLiveness)
		p.add(Divergence{
			Area: "probes",
			What: "exec livenessProbes are not reproduced (" + strings.Join(execLiveness, "; ") + ")",
			Why:  "a container has exactly one docker HEALTHCHECK and this plan spends it on the readiness probe, which is the one `devpod status` reads. Nothing would act on a liveness failure anyway: no --restart is emitted (see restartPolicy), so a wedged helper stays visible instead of being killed and restarted the way CKS kills it.",
		})
	}
	if len(unreadProbeFields) > 0 {
		sort.Strings(unreadProbeFields)
		p.add(Divergence{
			Area: "probes",
			What: "probe fields with no docker equivalent are dropped (" + strings.Join(unreadProbeFields, ", ") + ")",
			Why:  "docker's HEALTHCHECK has no initial delay (--health-start-period is already spent on the startup probe) and becomes healthy on the first success.",
		})
	}

	if release, ok := src.Release[o.Arch]; ok && o.Arch != nodeSelectorArch(pod) {
		p.add(Divergence{
			Area: "arch",
			What: fmt.Sprintf("rendered linux/%s; the cluster runs %s", o.Arch, nodeSelectorArch(pod)),
			Why:  "the guest assets are built per architecture and are not bit-reproducible, so this pod fetches " + release.RootfsAsset + " and a different Firecracker binary than CKS does. entrypoint.sh resolves its own pin from uname, and --platform makes uname agree with this.",
		})
	}

	if o.Driver == "mock" {
		p.add(Divergence{
			Area: "driver",
			What: "sparkbox-node is passed '--driver mock' as trailing args",
			Why:  "entrypoint.sh ends with \"$@\" and Go's flag package keeps the last occurrence, so this overrides the entrypoint's --driver firecracker without restating its command line. vmm-helper and sluice still run, so the pod shape is unchanged; the node simply never asks the helper for a VM.",
		})
	}

	if o.BinDir != "" {
		p.add(Divergence{
			Area: "image",
			What: "working-tree binaries from " + o.BinDir + " are bind-mounted over the image's",
			Why:  "iterating without a rebuild. They must be linux/" + o.Arch + " static builds; docker creates a directory where a bind source is missing, which surfaces as an exec format or 'is a directory' failure.",
		})
	}

	p.recordPolicyDivergences(src, pod)

	p.add(Divergence{
		Area: "init containers",
		What: "init containers run as separate `docker run --rm` invocations, in order",
		Why:  "docker has no init-container concept. Each must exit 0 before the next command runs, which is the ordering guarantee prepare-vm-assets depends on.",
	})
}

// recordPolicyDivergences names the network policy that selects this Pod and
// is not enforced locally. Nothing in docker reads a NetworkPolicy or a
// CiliumNetworkPolicy, so the honest thing is to say which rules the deployed
// node runs under and this one does not — the egress confinement in particular
// is a security property, not a convenience, and a dev pod that silently
// dropped it would make "the guests cannot reach RFC1918" untestable here.
func (p *Plan) recordPolicyDivergences(src *Source, pod podspec.PodSpec) {
	labels := src.Node.Spec.Template.Metadata.Labels
	for _, policy := range src.Policies {
		selector := policy.Spec.EndpointSelector
		if selector == nil {
			selector = policy.Spec.PodSelector
		}
		// An EMPTY selector on a policy selects every Pod in the namespace —
		// the opposite of what an empty Service selector means, and the reason
		// the default-deny document is spelled `podSelector: {}`.
		if selector == nil {
			continue
		}
		if len(selector.MatchLabels) > 0 && !selects(selector.MatchLabels, labels) {
			continue
		}
		var rules []string
		for _, rule := range append(append([]podspec.NetworkRule{}, policy.Spec.Egress...), policy.Spec.Ingress...) {
			for _, svc := range rule.ToServices {
				if svc.K8sService == nil {
					continue
				}
				rules = append(rules, fmt.Sprintf("%s.%s:%s", svc.K8sService.ServiceName, svc.K8sService.Namespace, portList(rule)))
			}
			for _, set := range rule.ToCIDRSet {
				rules = append(rules, fmt.Sprintf("%s except %s", set.CIDR, strings.Join(set.Except, " ")))
			}
		}
		if len(rules) == 0 {
			rules = append(rules, "deny all")
		}
		why := "docker has no NetworkPolicy and no Cilium. This one confines the node's egress — and, after the helper's IPv4 masquerade, its guests' egress — to the public internet; locally nothing stops either from reaching the whole host LAN. sluice still enforces its DNS allow-list inside the pod, so what is missing is the IP-level backstop underneath it."
		if len(policy.Spec.Egress) == 0 {
			why = "docker has no NetworkPolicy. In CKS nothing may open a connection to this Pod except what a later policy allows; locally the ports the Pod declares are published on the namespace holder, bound to 127.0.0.1 so at least they do not leave the machine."
		}
		p.add(Divergence{
			Area: "network policy",
			What: fmt.Sprintf("%s %s selects this Pod and is not enforced: %s", policy.Kind, policy.Metadata.Name, strings.Join(rules, "; ")),
			Why:  why,
		})
	}
}

// portList renders a Cilium toPorts list for a divergence line.
func portList(rule podspec.NetworkRule) string {
	var out []string
	for _, peer := range rule.ToPorts {
		for _, port := range peer.Ports {
			out = append(out, port.Port.String()+"/"+strings.ToLower(port.Protocol))
		}
	}
	for _, port := range rule.Ports {
		out = append(out, port.Port.String()+"/"+strings.ToLower(port.Protocol))
	}
	if len(out) == 0 {
		return "any port"
	}
	return strings.Join(out, ",")
}

// modeText renders a Secret defaultMode the way the manifest writes it.
func modeText(mode *int32) string {
	if mode == nil {
		return "unset"
	}
	return fmt.Sprintf("%#o", *mode)
}

// envValue reads a rendered container's environment, so a divergence reports
// what the plan emits rather than what an Option was set to.
func (p *Plan) envValue(container, name string) string {
	for _, c := range p.Containers {
		if c.Name != container {
			continue
		}
		for _, env := range c.Env {
			if env.Name == name {
				return env.Value
			}
		}
	}
	return ""
}

// nodeSelectorArch is the architecture the cluster pins the Pod to.
func nodeSelectorArch(pod podspec.PodSpec) string {
	return pod.NodeSelector["kubernetes.io/arch"]
}

func mustSource(p *Plan, name string) string {
	if v, ok := p.findVolume(name); ok {
		return v.Source
	}
	return "?"
}

func sysctlNames() string {
	names := make([]string, 0, len(podSysctls))
	for _, s := range podSysctls {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// sluiceDNSIP reads the resolver address out of the Pod rather than repeating
// it, so the divergence text cannot go stale.
func sluiceDNSIP(pod podspec.PodSpec) string {
	for _, c := range append(append([]podspec.Container{}, pod.InitContainers...), pod.Containers...) {
		for _, env := range c.Env {
			if env.Name == "SPARKBOX_SLUICE_DNS_IP" {
				return env.Value
			}
		}
	}
	return "sluice"
}
