package devpod

import (
	"fmt"
	"regexp"
	"strings"
)

// LabelPod is the docker label every object this plan creates carries, so
// `devpod down` and `devpod status` can find them without guessing at names.
const LabelPod = "sparkbox.devpod"

// LabelContainer names the manifest container an object came from.
const LabelContainer = "sparkbox.devpod.container"

// DockerArgv renders the plan as one argv per docker invocation, in the order
// they must run: the network, the volumes, the namespace holder, each init
// container to completion, then the long-lived containers.
//
// Argv rather than a shell string on purpose — nothing here needs a shell, and
// the two entrypoint scripts embedded as container commands contain newlines
// and backslashes that a generated shell line would mangle.
func (p *Plan) DockerArgv() [][]string {
	var out [][]string
	out = append(out, []string{
		"docker", "network", "create",
		"--label", LabelPod + "=" + p.Options.Prefix,
		// Explicit, never docker's allocator: see DefaultNetworkSubnet.
		"--subnet", p.Network.Subnet,
		p.Network.Name,
	})
	for _, v := range p.Volumes {
		if !v.Create {
			continue
		}
		out = append(out, []string{
			"docker", "volume", "create",
			"--label", LabelPod + "=" + p.Options.Prefix,
			v.Source,
		})
		if argv, ok := p.fsGroupArgv(v); ok {
			out = append(out, argv)
		}
	}
	out = append(out, p.holderArgv())
	for _, c := range p.Containers {
		out = append(out, p.containerArgv(c))
	}
	return out
}

// fsGroupArgv reproduces the kubelet's fsGroup pass over one freshly created
// volume: group-own it to the Pod's fsGroup and set the setgid bit so anything
// created inside inherits that group.
//
// Only volumes docker actually created, and only the ephemeral ones. That is
// not a shortcut, it is what Kubernetes does — fsGroup applies to volume types
// that support ownership management (emptyDir, Secret, projected) and NOT to
// hostPath, whose permissions belong to the host. Applying it to the data tier
// would also mean chowning a 25 GiB rootfs template on every `up`.
//
// The image is the pod's own, so this pulls nothing extra; it runs as root
// because chowning to another group requires it, and it exits immediately.
func (p *Plan) fsGroupArgv(v Volume) ([]string, bool) {
	if p.FSGroup == nil || !v.Create || v.Kind != "volume" || !v.Ephemeral {
		return nil, false
	}
	gid := fmt.Sprintf("%d", *p.FSGroup)
	return []string{
		"docker", "run", "--rm",
		"--label", LabelPod + "=" + p.Options.Prefix,
		"--platform", "linux/" + p.Options.Arch,
		"--user", "0:0",
		"--mount", "type=volume,src=" + v.Source + ",dst=/fsgroup",
		"--entrypoint", "/bin/sh",
		p.Options.Image,
		"-ec", "chown -R :" + gid + " /fsgroup && chmod g+rwXs /fsgroup",
	}, true
}

func (p *Plan) holderArgv() []string {
	argv := []string{
		"docker", "run", "--detach",
		"--name", p.Network.HolderName,
		"--label", LabelPod + "=" + p.Options.Prefix,
		"--label", LabelContainer + "=netns",
		"--platform", "linux/" + p.Options.Arch,
		"--network", p.Network.Name,
	}
	for _, s := range p.Network.Sysctls {
		argv = append(argv, "--sysctl", s.Name+"="+s.Value)
	}
	for _, port := range p.Network.Ports {
		spec := fmt.Sprintf("%d:%d", port.HostPort, port.Container)
		if port.HostIP != "" {
			spec = port.HostIP + ":" + spec
		}
		if port.Protocol != "" && port.Protocol != "tcp" {
			spec += "/" + port.Protocol
		}
		argv = append(argv, "--publish", spec)
	}
	// The holder only has to exist. It is the pod's network namespace and
	// nothing else, so it gets none of the pod's privileges, devices or data —
	// and it must not quietly become the most privileged thing in the pod
	// either, which docker's defaults (root, the full default capability set,
	// a writable rootfs, escalation allowed) would make it. `sleep infinity`
	// needs none of that.
	//
	// Dropping capabilities does not cost the sysctls above: docker writes them
	// into the OCI spec and runc applies them to the new namespaces during
	// container setup, before it drops privileges and execs this process. The
	// container process never touches /proc/sys.
	argv = append(argv,
		"--user", holderUser,
		"--cap-drop", "ALL",
		"--read-only",
		"--security-opt", "no-new-privileges",
	)
	argv = append(argv, "--entrypoint", "/bin/sleep", p.Network.HolderImage, "infinity")
	return argv
}

// holderUser is the unprivileged uid:gid the namespace holder runs as. It is
// the controller's own uid, which needs no /etc/passwd entry: docker accepts a
// numeric user, and /bin/sleep in the image is mode 0755.
const holderUser = "65532:65532"

func (p *Plan) containerArgv(c Container) []string {
	argv := []string{"docker", "run"}
	if c.Init {
		// Init containers run to completion, in order, before anything else
		// starts. Blocking and self-removing is that contract in docker terms.
		argv = append(argv, "--rm")
	} else {
		argv = append(argv, "--detach")
	}
	argv = append(argv,
		"--name", p.Options.Prefix+"-"+c.Name,
		"--label", LabelPod+"="+p.Options.Prefix,
		"--label", LabelContainer+"="+c.Name,
		"--platform", "linux/"+p.Options.Arch,
		"--network", "container:"+p.Network.HolderName,
	)
	if c.User != "" {
		argv = append(argv, "--user", c.User)
	}
	if c.Privileged {
		argv = append(argv, "--privileged")
	}
	for _, capability := range c.CapDrop {
		argv = append(argv, "--cap-drop", capability)
	}
	for _, capability := range c.CapAdd {
		argv = append(argv, "--cap-add", capability)
	}
	if c.NoNewPrivileges {
		argv = append(argv, "--security-opt", "no-new-privileges")
	}
	for _, opt := range c.SecurityOpts {
		argv = append(argv, "--security-opt", opt)
	}
	if c.ReadOnlyRootfs {
		argv = append(argv, "--read-only")
	}
	for _, device := range c.Devices {
		argv = append(argv, "--device", device.HostPath+":"+device.ContainerPath+":"+device.Permissions)
	}
	for _, mount := range c.Mounts {
		argv = append(argv, "--mount", mountSpec(mount))
	}
	for _, env := range c.Env {
		argv = append(argv, "--env", env.Name+"="+env.Value)
	}
	if c.Health != nil {
		argv = append(argv, "--health-cmd", shellJoin(c.Health.Test[1:]))
		if c.Health.IntervalSec > 0 {
			argv = append(argv, "--health-interval", fmt.Sprintf("%ds", c.Health.IntervalSec))
		}
		if c.Health.Retries > 0 {
			argv = append(argv, "--health-retries", fmt.Sprintf("%d", c.Health.Retries))
		}
		if c.Health.StartPeriodSec > 0 {
			argv = append(argv, "--health-start-period", fmt.Sprintf("%ds", c.Health.StartPeriodSec))
		}
		if c.Health.TimeoutSec > 0 {
			// Always emitted: docker's default is 30s and Kubernetes' is 1s,
			// and a probe that waits thirty times longer to fail is a
			// different probe. healthFor resolves the manifest's value.
			argv = append(argv, "--health-timeout", fmt.Sprintf("%ds", c.Health.TimeoutSec))
		}
	}
	if len(c.Command) > 0 {
		argv = append(argv, "--entrypoint", c.Command[0])
	}
	argv = append(argv, c.Image)
	if len(c.Command) > 1 {
		argv = append(argv, c.Command[1:]...)
	}
	argv = append(argv, c.Args...)
	return argv
}

func mountSpec(m Mount) string {
	parts := []string{"type=" + m.Kind, "src=" + m.Source, "dst=" + m.Target}
	if m.Kind == "volume" && m.SubPath != "" {
		parts = append(parts, "volume-subpath="+m.SubPath)
	}
	if m.ReadOnly {
		parts = append(parts, "readonly")
	}
	return strings.Join(parts, ",")
}

// shellSafe matches an argument that needs no quoting.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// ShellLine renders an argv for a human to read, paste, or diff, quoting every
// argument that is not plainly safe. It is the only rendering of an argv this
// package offers, because the alternative — joining with spaces — cannot tell
// `--foo bar` from `--foo=bar` or from a single argument containing a space,
// which is precisely the class of bug the golden file exists to catch. The
// scrub container also carries a multi-line shell script as one argument, so
// the quoting is not cosmetic.
func ShellLine(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		if shellSafe.MatchString(arg) {
			quoted = append(quoted, arg)
			continue
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(arg, "'", `'\''`)+"'")
	}
	return strings.Join(quoted, " ")
}

// shellJoin renders an exec probe's argv as the single string docker's
// --health-cmd wants (it runs the value through a shell).
func shellJoin(args []string) string { return ShellLine(args) }

// StopArgv renders the teardown, newest container first. The stop is a
// separate invocation from the remove because the grace period lives on
// `docker stop`: sparkbox-node needs its terminationGracePeriodSeconds to
// checkpoint running VMs, and `docker rm --force` would not give it any.
//
// purgeData additionally removes the durable data volume — the local stand-in
// for the CKS hostPath, holding the VM inventory, the guest disks, the control
// SQLite, the node identity and the rootfs template. That is a `rm -rf` of the
// node's entire state, so it happens only when the caller asks for it by name;
// the ephemeral volumes (the emptyDirs, and the docker volume that stands in
// for the Secret) go every time, because those are per-Pod in CKS too.
func (p *Plan) StopArgv(purgeData bool) [][]string {
	var out [][]string
	timeout := fmt.Sprintf("%d", p.StopTimeout)
	names := []string{}
	for i := len(p.Containers) - 1; i >= 0; i-- {
		if p.Containers[i].Init {
			continue // ran with --rm; already gone
		}
		names = append(names, p.Options.Prefix+"-"+p.Containers[i].Name)
	}
	names = append(names, p.Network.HolderName)
	for _, name := range names {
		out = append(out, []string{"docker", "stop", "--time", timeout, name})
		out = append(out, []string{"docker", "rm", "--force", name})
	}
	for _, v := range p.Volumes {
		if !v.Create || v.Kind != "volume" {
			continue
		}
		if !v.Ephemeral && !purgeData {
			continue
		}
		out = append(out, []string{"docker", "volume", "rm", "--force", v.Source})
	}
	out = append(out, []string{"docker", "network", "rm", "--force", p.Network.Name})
	return out
}

// StatusArgv lists everything this plan created, running or not.
func (p *Plan) StatusArgv() []string {
	return []string{
		"docker", "ps", "--all",
		"--filter", "label=" + LabelPod + "=" + p.Options.Prefix,
		"--format", "table {{.Names}}\t{{.Status}}\t{{.Image}}",
	}
}

// DataVolume returns the rendered data tier, the durable one. Callers use it
// to say what `devpod down --purge-data` would (or, for a host path, would
// not) delete.
func (p *Plan) DataVolume() (Volume, bool) { return p.findVolume("data") }

// BindDirs lists the host directories `devpod up` has to create before docker
// does. Docker creates a missing bind source itself, but as a root-owned
// directory — which for a subPath mount silently produces an empty directory
// where the controller expected its own state.
func (p *Plan) BindDirs() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range p.Containers {
		for _, m := range c.Mounts {
			if m.Kind != "bind" || m.Volume == "working-tree-bin" || seen[m.Source] {
				continue
			}
			seen[m.Source] = true
			out = append(out, m.Source)
		}
	}
	return out
}
