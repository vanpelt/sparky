package devpod

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/devpod/podspec"

	assets "github.com/vanpelt/sparky/tools/sparkbox/deploy/kubernetes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/deviceplugin"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// testOptions are the options every test renders with, chosen to exercise the
// arm64 / no-KVM / bind-mounted path this dev environment actually runs.
func testOptions() Options {
	return Options{
		Arch:        "arm64",
		Image:       "sparkbox-dev:latest",
		DataVolume:  "/srv/sparkbox/data/devpod",
		Driver:      "firecracker",
		ProxyDomain: "dev.catnip.sh",
		GatewayAddr: "192.168.64.1:2222",
		RealKVM:     true,
		TrustDir:    "/srv/sparkbox/devpod-trust",
		HostMemMB:   8192,
		NodeName:    "laptop-dev",
	}
}

func mustLoad(t *testing.T) *Source {
	t.Helper()
	src, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return src
}

func mustPlan(t *testing.T, o Options) *Plan {
	t.Helper()
	plan, err := BuildPlan(mustLoad(t), o)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return plan
}

func TestLoadDecodesShippedManifests(t *testing.T) {
	src := mustLoad(t)
	pod := src.Node.Spec.Template.Spec
	if got, want := len(pod.InitContainers), 2; got != want {
		t.Fatalf("init containers = %d, want %d", got, want)
	}
	if got, want := len(pod.Containers), 3; got != want {
		t.Fatalf("containers = %d, want %d", got, want)
	}
	names := append(containerNames(pod.InitContainers), containerNames(pod.Containers)...)
	want := "scrub-retired-gateway-state,prepare-vm-assets,vmm-helper,sluice,sparkbox-node"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("container order = %s, want %s", got, want)
	}
	if src.Gateway.Metadata.Name != "sparkbox-gateway" {
		t.Fatalf("gateway name = %q", src.Gateway.Metadata.Name)
	}
	if src.Service.Spec.Type != "LoadBalancer" {
		t.Fatalf("service type = %q", src.Service.Spec.Type)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if _, ok := src.Release[arch]; !ok {
			t.Fatalf("no embedded release manifest for %s", arch)
		}
	}
}

// mutate rewrites one of the shipped manifests for a drift fixture. The
// fixtures are derived from the real files rather than copied, because a
// checked-in copy of deployment.yaml is exactly the drift this package exists
// to prevent.
func mutate(t *testing.T, name string, edit func(string) string) fstest.MapFS {
	t.Helper()
	files := map[string][]byte{
		"deployment.yaml":         assets.NodeDeployment,
		"gateway-deployment.yaml": assets.GatewayDeployment,
		"service.yaml":            assets.LoadBalancer,
		"network-policy.yaml":     assets.NetworkPolicies,
	}
	original := string(files[name])
	edited := edit(original)
	if edited == original {
		t.Fatalf("fixture for %s changed nothing; the anchor it edits is gone", name)
	}
	files[name] = []byte(edited)
	out := fstest.MapFS{}
	for path, data := range files {
		out[path] = &fstest.MapFile{Data: data}
	}
	return out
}

func TestUnknownContainerFieldFailsLoad(t *testing.T) {
	fsys := mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"        - name: vmm-helper\n",
			"        - name: vmm-helper\n          terminationMessagePolicy: FallbackToLogsOnError\n",
			1)
	})
	_, err := LoadFS(fsys)
	if err == nil {
		t.Fatal("LoadFS accepted an unmodeled container field; strict decoding is not in effect")
	}
	if !strings.Contains(err.Error(), "terminationMessagePolicy") {
		t.Fatalf("error does not name the offending field: %v", err)
	}
}

func TestUnknownSecurityContextFieldFailsLoad(t *testing.T) {
	// A privilege field is the case that matters most: silently dropping one
	// would make the local pod claim a security posture it does not have.
	fsys := mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"            allowPrivilegeEscalation: false\n            appArmorProfile:\n              type: RuntimeDefault\n            capabilities:\n              drop: [\"ALL\"]\n              add:\n                - CHOWN\n",
			"            allowPrivilegeEscalation: false\n            procMount: Unmasked\n            appArmorProfile:\n              type: RuntimeDefault\n            capabilities:\n              drop: [\"ALL\"]\n              add:\n                - CHOWN\n",
			1)
	})
	_, err := LoadFS(fsys)
	if err == nil {
		t.Fatal("LoadFS accepted an unmodeled securityContext field")
	}
	if !strings.Contains(err.Error(), "procMount") {
		t.Fatalf("error does not name the offending field: %v", err)
	}
}

func TestUnknownPlaceholderFailsLoad(t *testing.T) {
	fsys := mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"            - name: SPARKBOX_NODE_NAME\n              value: cks-poc\n",
			"            - name: SPARKBOX_NODE_NAME\n              value: __SPARKBOX_NEW_KNOB__\n",
			1)
	})
	_, err := LoadFS(fsys)
	if err == nil {
		t.Fatal("LoadFS accepted a placeholder devpod cannot substitute")
	}
	if !strings.Contains(err.Error(), "__SPARKBOX_NEW_KNOB__") {
		t.Fatalf("error does not name the placeholder: %v", err)
	}
}

func TestNewContainerFailsBuildPlan(t *testing.T) {
	fsys := mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"        - name: sluice\n",
			"        - name: telemetry-shipper\n          image: __SPARKBOX_IMAGE__\n        - name: sluice\n",
			1)
	})
	src, err := LoadFS(fsys)
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	_, err = BuildPlan(src, testOptions())
	if err == nil {
		t.Fatal("BuildPlan rendered a container it has no overlay entry for")
	}
	if !strings.Contains(err.Error(), "telemetry-shipper") {
		t.Fatalf("error does not name the new container: %v", err)
	}
}

func TestNewHostPathVolumeFailsBuildPlan(t *testing.T) {
	fsys := mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"        - name: tmp\n          emptyDir:\n            sizeLimit: 1Gi\n",
			"        - name: extra-scratch\n          hostPath:\n            path: /mnt/local/extra\n            type: DirectoryOrCreate\n        - name: tmp\n          emptyDir:\n            sizeLimit: 1Gi\n",
			1)
	})
	src, err := LoadFS(fsys)
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	if _, err := BuildPlan(src, testOptions()); err == nil {
		t.Fatal("BuildPlan silently rendered a new host dependency")
	} else if !strings.Contains(err.Error(), "extra-scratch") {
		t.Fatalf("error does not name the volume: %v", err)
	}
}

func TestUnknownDeviceResourceFailsBuildPlan(t *testing.T) {
	fsys := mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"              sparkbox.dev/kvm: \"1\"\n              sparkbox.dev/tun: \"1\"\n            limits:",
			"              sparkbox.dev/kvm: \"1\"\n              sparkbox.dev/tun: \"1\"\n              sparkbox.dev/vfio: \"1\"\n            limits:",
			1)
	})
	src, err := LoadFS(fsys)
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	if _, err := BuildPlan(src, testOptions()); err == nil {
		t.Fatal("BuildPlan granted a device resource the plugin does not serve")
	} else if !strings.Contains(err.Error(), "sparkbox.dev/vfio") {
		t.Fatalf("error does not name the resource: %v", err)
	}
}

// TestDeviceTableComesFromDevicePlugin asserts the granted devices are the
// device plugin's own table, not a copy of it: the loop bundle in particular
// is /dev/loop-control plus loop0..7, and a copy would rot.
func TestDeviceTableComesFromDevicePlugin(t *testing.T) {
	plan := mustPlan(t, testOptions())
	granted := map[string][]string{}
	for _, c := range plan.Containers {
		for _, d := range c.Devices {
			granted[d.Resource] = append(granted[d.Resource], d.HostPath+":"+d.ContainerPath+":"+d.Permissions)
		}
	}
	for _, resource := range deviceplugin.DefaultResources("") {
		want := []string{}
		for _, d := range resource.Devices {
			want = append(want, d.HostPath+":"+d.ContainerPath+":"+d.Permissions)
		}
		got, ok := granted[resource.Name]
		if !ok {
			t.Fatalf("no container was granted %s; the node Pod requests it", resource.Name)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s devices:\n got %v\nwant %v", resource.Name, got, want)
		}
	}
	if got, want := len(granted[deviceplugin.ResourceLoop]), 9; got != want {
		t.Fatalf("loop bundle has %d devices, want %d (loop-control + loop0..7)", got, want)
	}
}

func TestKVMOmittedWithoutRealKVM(t *testing.T) {
	o := testOptions()
	o.RealKVM = false
	plan := mustPlan(t, o)
	for _, argv := range plan.DockerArgv() {
		for _, arg := range argv {
			if strings.Contains(arg, "/dev/kvm") {
				t.Fatalf("plan grants /dev/kvm with RealKVM=false: %v", argv)
			}
		}
	}
	if !hasDivergence(plan, "devices", "/dev/kvm is not granted", true) {
		t.Fatal("dropping /dev/kvm was not recorded as a blocking divergence")
	}
	// /dev/net/tun is a separate resource and must survive.
	if !containsArg(plan.DockerArgv(), "/dev/net/tun:/dev/net/tun:rwm") {
		t.Fatal("dropping KVM also dropped TUN")
	}
}

// TestVMMHelperCapabilitiesReachArgv is the check that matters: the entire
// long-lived Linux privilege boundary is that capability list, and a local pod
// that quietly ran with fewer (or more) would be testing a different program.
func TestVMMHelperCapabilitiesReachArgv(t *testing.T) {
	src := mustLoad(t)
	pod := src.Node.Spec.Template.Spec
	helper, ok := pod.FindContainer("vmm-helper")
	if !ok {
		t.Fatal("deployment.yaml has no vmm-helper container")
	}
	manifestCaps := append([]string(nil), helper.SecurityContext.Capabilities.Add...)
	sort.Strings(manifestCaps)
	want := []string{"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "KILL", "MKNOD", "NET_ADMIN", "SETGID", "SETUID", "SYS_CHROOT"}
	if strings.Join(manifestCaps, ",") != strings.Join(want, ",") {
		t.Fatalf("the manifest's vmm-helper capability set changed:\n got %v\nwant %v\nupdate this test deliberately, and say why in the commit", manifestCaps, want)
	}

	plan := mustPlan(t, testOptions())
	argv := argvFor(t, plan, "vmm-helper")
	for _, capability := range want {
		if !hasFlagValue(argv, "--cap-add", capability) {
			t.Fatalf("vmm-helper argv is missing --cap-add %s: %v", capability, argv)
		}
	}
	if !hasFlagValue(argv, "--cap-drop", "ALL") {
		t.Fatal("vmm-helper argv does not drop ALL first")
	}
	if !hasFlagValue(argv, "--user", "0:0") {
		t.Fatal("vmm-helper argv does not run as uid 0 gid 0")
	}
	if !hasFlagValue(argv, "--security-opt", "no-new-privileges") {
		t.Fatal("allowPrivilegeEscalation:false did not become no-new-privileges")
	}
	if !containsArg([][]string{argv}, "--read-only") {
		t.Fatal("readOnlyRootFilesystem:true did not become --read-only")
	}
	if containsArg([][]string{argv}, "--privileged") {
		t.Fatal("vmm-helper is not privileged in the manifest")
	}
}

func TestNodeRunsUnprivilegedAs65532(t *testing.T) {
	plan := mustPlan(t, testOptions())
	argv := argvFor(t, plan, "sparkbox-node")
	if !hasFlagValue(argv, "--user", "65532:65532") {
		t.Fatalf("sparkbox-node argv does not run as 65532:65532: %v", argv)
	}
	if !hasFlagValue(argv, "--cap-drop", "ALL") {
		t.Fatal("sparkbox-node argv does not drop ALL capabilities")
	}
	for i, arg := range argv {
		if arg == "--cap-add" {
			t.Fatalf("sparkbox-node was granted a capability: %s", argv[i+1])
		}
	}
	if containsArg([][]string{argv}, "--privileged") {
		t.Fatal("sparkbox-node must never be privileged")
	}
	// The read-only data mount and the writable subPaths that punch through it
	// are the asymmetry that stops a compromised controller from rewriting the
	// rootfs every future sandbox is cloned from.
	if !hasFlagValue(argv, "--mount", "type=bind,src=/srv/sparkbox/data/devpod,dst=/var/lib/sparkbox,readonly") {
		t.Fatalf("sparkbox-node does not mount the data volume read-only: %v", argv)
	}
	if !hasFlagValue(argv, "--mount", "type=bind,src=/srv/sparkbox/data/devpod/templates,dst=/var/lib/sparkbox/templates") {
		t.Fatalf("sparkbox-node does not get a writable templates subPath: %v", argv)
	}
}

// TestReadOnlyParentMountPrecedesSubPaths keeps the emitted order reviewable:
// /var/lib/sparkbox has to appear before the writable directories mounted over
// it, or a reader cannot tell what the container ends up with.
func TestReadOnlyParentMountPrecedesSubPaths(t *testing.T) {
	plan := mustPlan(t, testOptions())
	var node *Container
	for i := range plan.Containers {
		if plan.Containers[i].Name == "sparkbox-node" {
			node = &plan.Containers[i]
		}
	}
	if node == nil {
		t.Fatal("no sparkbox-node container in the plan")
	}
	parent := -1
	for i, m := range node.Mounts {
		if m.Target == "/var/lib/sparkbox" {
			parent = i
		}
		if strings.HasPrefix(m.Target, "/var/lib/sparkbox/") && parent == -1 {
			t.Fatalf("mount %s precedes its read-only parent", m.Target)
		}
	}
	if parent == -1 {
		t.Fatal("sparkbox-node never mounts /var/lib/sparkbox")
	}
}

// The Pod declares fsGroup 65532. Without emulating it, sluice's control
// socket — created by root at mode 0660 in a shared emptyDir — is unreachable
// by sparkbox-node at uid 65532, and the node hangs in entrypoint.sh's
// sluice-readiness poll forever, logging nothing while every container still
// reports Up and the two with healthchecks report healthy. That was observed on
// real hardware, which is the only reason this is emulated rather than merely
// recorded as a divergence.
func TestFSGroupIsAppliedToEphemeralVolumesOnly(t *testing.T) {
	plan := mustPlan(t, testOptions())
	if plan.FSGroup == nil || *plan.FSGroup != 65532 {
		t.Fatalf("fsGroup = %v, want the manifest's 65532", plan.FSGroup)
	}

	chowned := map[string]bool{}
	for _, a := range plan.DockerArgv() {
		if len(a) < 2 || a[1] != "run" {
			continue
		}
		script := a[len(a)-1]
		if !strings.Contains(script, "chown -R :65532 /fsgroup") {
			continue
		}
		if !strings.Contains(script, "chmod g+rwXs /fsgroup") {
			t.Errorf("fsGroup pass sets the group but not the setgid bit: %q", script)
		}
		src := ""
		for _, arg := range a {
			if strings.HasPrefix(arg, "type=volume,src=") {
				src = strings.TrimSuffix(strings.TrimPrefix(arg, "type=volume,src="), ",dst=/fsgroup")
			}
		}
		if src == "" {
			t.Fatalf("fsGroup pass names no volume: %v", a)
		}
		chowned[src] = true
	}

	for _, v := range plan.Volumes {
		if !v.Create || v.Kind != "volume" {
			continue
		}
		switch {
		case v.Ephemeral && !chowned[v.Source]:
			t.Errorf("ephemeral volume %s (%s) got no fsGroup pass; anything root creates in it "+
				"is unreachable by uid 65532", v.Name, v.Source)
		case !v.Ephemeral && chowned[v.Source]:
			// Kubernetes does not apply fsGroup to hostPath, and doing so here
			// would chown a 25 GiB rootfs template on every `up`.
			t.Errorf("durable volume %s (%s) got an fsGroup pass; Kubernetes applies fsGroup only to "+
				"volume types that support ownership management", v.Name, v.Source)
		}
	}
	if len(chowned) == 0 {
		t.Fatal("no volume got an fsGroup pass at all")
	}
}

func TestInitContainersRunFirstAndBlock(t *testing.T) {
	plan := mustPlan(t, testOptions())
	argv := plan.DockerArgv()
	order := []string{}
	for _, a := range argv {
		if len(a) > 1 && a[1] == "run" {
			// The fsGroup chown pass is also a `docker run`, but it is a
			// one-shot with no --name that runs before the holder exists. It
			// is not part of the Pod's container order.
			if n := nameFlag(a); n != "" {
				order = append(order, n)
			}
		}
	}
	want := []string{
		"sparkbox-dev-netns",
		"sparkbox-dev-scrub-retired-gateway-state",
		"sparkbox-dev-prepare-vm-assets",
		"sparkbox-dev-vmm-helper",
		"sparkbox-dev-sluice",
		"sparkbox-dev-sparkbox-node",
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("run order:\n got %v\nwant %v", order, want)
	}
	for _, name := range want[1:3] {
		a := argvNamed(argv, name)
		if !containsArg([][]string{a}, "--rm") || containsArg([][]string{a}, "--detach") {
			t.Fatalf("%s must run to completion, not detached: %v", name, a)
		}
	}
	for _, name := range want[3:] {
		a := argvNamed(argv, name)
		if !containsArg([][]string{a}, "--detach") {
			t.Fatalf("%s must be detached: %v", name, a)
		}
	}
}

func TestSharedNetworkNamespace(t *testing.T) {
	plan := mustPlan(t, testOptions())
	holder := argvNamed(plan.DockerArgv(), "sparkbox-dev-netns")
	if !hasFlagValue(holder, "--network", "sparkbox-dev-net") {
		t.Fatalf("holder does not create the network: %v", holder)
	}
	if !hasFlagValue(holder, "--publish", "127.0.0.1:9090:9090") {
		t.Fatalf("holder does not publish the node's declared metrics port: %v", holder)
	}
	for _, s := range podSysctls {
		if !hasFlagValue(holder, "--sysctl", s.Name+"="+s.Value) {
			t.Fatalf("holder does not set %s: %v", s.Name, holder)
		}
	}
	for _, c := range plan.Containers {
		a := argvNamed(plan.DockerArgv(), "sparkbox-dev-"+c.Name)
		if !hasFlagValue(a, "--network", "container:sparkbox-dev-netns") {
			t.Fatalf("%s does not join the pod namespace: %v", c.Name, a)
		}
		if containsArg([][]string{a}, "--publish") {
			t.Fatalf("%s publishes a port; only the namespace holder can", c.Name)
		}
	}
}

// TestStopArgvHonorsTheGracePeriod: sparkbox-node uses its termination grace
// period to quiesce running VMs, and the grace period is a `docker stop` flag,
// not a `docker rm` one.
func TestStopArgvHonorsTheGracePeriod(t *testing.T) {
	plan := mustPlan(t, testOptions())
	if plan.StopTimeout != 90 {
		t.Fatalf("StopTimeout = %d, want the manifest's terminationGracePeriodSeconds (90)", plan.StopTimeout)
	}
	argvs := plan.StopArgv(false)
	if argvs[0][1] != "stop" || !hasFlagValue(argvs[0], "--time", "90") {
		t.Fatalf("teardown does not stop with a grace period first: %v", argvs[0])
	}
	for _, argv := range argvs {
		if argv[1] == "rm" && containsArg([][]string{argv}, "--time") {
			t.Fatalf("docker rm has no --time flag: %v", argv)
		}
	}
	// The node stops before the namespace holder it depends on.
	var order []string
	for _, argv := range argvs {
		if argv[1] == "stop" {
			order = append(order, argv[len(argv)-1])
		}
	}
	want := []string{"sparkbox-dev-sparkbox-node", "sparkbox-dev-sluice", "sparkbox-dev-vmm-helper", "sparkbox-dev-netns"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("stop order:\n got %v\nwant %v", order, want)
	}
}

// TestLoadBalancerPortsAreNotPublished: service.yaml selects the gateway, so
// its ports belong to whatever runs the gateway locally, not to this pod.
func TestLoadBalancerPortsAreNotPublished(t *testing.T) {
	plan := mustPlan(t, testOptions())
	if len(plan.Network.ServicePorts) == 0 {
		t.Fatal("service.yaml's ports were not carried on the plan")
	}
	for _, port := range plan.Network.Ports {
		if port.HostPort == 443 || port.HostPort == 22 {
			t.Fatalf("the node pod published a gateway LoadBalancer port: %+v", port)
		}
	}
	if !hasDivergenceArea(plan, "service") {
		t.Fatal("not publishing the LoadBalancer was not recorded")
	}
}

func TestMockDriverOverridesTheEntrypointFlag(t *testing.T) {
	o := testOptions()
	o.Driver = "mock"
	plan := mustPlan(t, o)
	argv := argvFor(t, plan, "sparkbox-node")
	if got := strings.Join(argv[len(argv)-2:], " "); got != "--driver mock" {
		t.Fatalf("sparkbox-node argv does not end with the driver override: %v", argv)
	}
	// The other four containers must be untouched: the pod shape is the point.
	if len(plan.Containers) != 5 {
		t.Fatalf("mock driver changed the container set: %d", len(plan.Containers))
	}
}

// TestPlanEmitsNoChecksumOverrides guards the decision not to restate the
// asset checksums. entrypoint.sh resolves a pin per architecture from uname,
// and --platform makes uname agree with Options.Arch; a copy of the checksums
// in the emitted env would be a second place they have to be updated.
func TestPlanEmitsNoChecksumOverrides(t *testing.T) {
	plan := mustPlan(t, testOptions())
	for _, argv := range plan.DockerArgv() {
		for _, arg := range argv {
			if strings.Contains(arg, "_SHA256=") {
				t.Fatalf("the plan restates an asset checksum: %s", arg)
			}
		}
	}
	if !hasFlagValue(argvNamed(plan.DockerArgv(), "sparkbox-dev-prepare-vm-assets"), "--platform", "linux/arm64") {
		t.Fatal("prepare-vm-assets does not pin --platform, so uname inside it is not Options.Arch")
	}
}

// TestEntrypointPinsMatchReleaseManifests cross-checks the checksums pinned in
// entrypoint.sh against the published manifest-<arch>.env for BOTH arches.
// Those are the same eight values written down twice; this is the test that
// notices when one moves. hack/check-cks-pin.sh makes the same comparison at
// build time against the live release; this one runs offline.
func TestEntrypointPinsMatchReleaseManifests(t *testing.T) {
	src := mustLoad(t)
	pins, err := EntrypointSHA256Pins()
	if err != nil {
		t.Fatalf("EntrypointSHA256Pins: %v", err)
	}
	for arch, release := range src.Release {
		got, ok := pins[arch]
		if !ok {
			t.Fatalf("entrypoint.sh pins no %s checksums, but release %s publishes them", arch, release.Release)
		}
		for asset, want := range map[string]string{
			"firecracker": release.SHA256Firecracker,
			"jailer":      release.SHA256Jailer,
			"kernel":      release.SHA256Vmlinux,
			"rootfs":      release.SHA256Rootfs,
		} {
			if got[asset] != want {
				t.Fatalf("entrypoint.sh %s_sha256_%s is %s, but %s manifest-%s.env says %s",
					asset, arch, got[asset], release.Release, arch, want)
			}
		}
	}
}

// TestPodSysctlsMatchHelperEntrypoint keeps the namespace-holder sysctls equal
// to the ones vmm-helper-entrypoint.sh tries to set.
func TestPodSysctlsMatchHelperEntrypoint(t *testing.T) {
	pattern := regexp.MustCompile(`(?m)^sysctl -q -w ([a-z0-9_.]+)=([0-9]+)`)
	matches := pattern.FindAllStringSubmatch(string(assets.VMMHelperEntrypoint), -1)
	if len(matches) == 0 {
		t.Fatal("vmm-helper-entrypoint.sh sets no sysctls; this test's pattern is stale")
	}
	var fromScript []string
	for _, match := range matches {
		fromScript = append(fromScript, match[1]+"="+match[2])
	}
	var fromPlan []string
	for _, s := range podSysctls {
		fromPlan = append(fromPlan, s.Name+"="+s.Value)
	}
	sort.Strings(fromScript)
	sort.Strings(fromPlan)
	if strings.Join(fromScript, ",") != strings.Join(fromPlan, ",") {
		t.Fatalf("podSysctls drifted from vmm-helper-entrypoint.sh:\nscript %v\n plan  %v", fromScript, fromPlan)
	}
}

// TestOverlayCoversEveryContainer proves knownContainers is exhaustive, so the
// BuildPlan failure in TestNewContainerFailsBuildPlan is the only way a new
// container can arrive.
func TestOverlayCoversEveryContainer(t *testing.T) {
	src := mustLoad(t)
	pod := src.Node.Spec.Template.Spec
	seen := map[string]bool{}
	for _, c := range append(append([]string{}, containerNames(pod.InitContainers)...), containerNames(pod.Containers)...) {
		seen[c] = true
		if _, ok := knownContainers[c]; !ok {
			t.Fatalf("container %q has no knownContainers entry", c)
		}
	}
	for name := range knownContainers {
		if !seen[name] {
			t.Fatalf("knownContainers has a stale entry for %q", name)
		}
	}
}

func TestBinDirShadowsImageBinaries(t *testing.T) {
	o := testOptions()
	o.BinDir = "/work/bin"
	plan := mustPlan(t, o)
	argv := argvFor(t, plan, "sparkbox-node")
	for _, binary := range []string{"sparkbox", "sparkbox-vmm-helper"} {
		want := "type=bind,src=/work/bin/" + binary + ",dst=/usr/local/bin/" + binary + ",readonly"
		if !hasFlagValue(argv, "--mount", want) {
			t.Fatalf("sparkbox-node does not shadow %s: %v", binary, argv)
		}
	}
	if hasFlagValue(argvFor(t, plan, "sluice"), "--mount", "type=bind,src=/work/bin/sparkbox,dst=/usr/local/bin/sparkbox,readonly") {
		t.Fatal("sluice was given a binary it does not run")
	}
}

func TestOptionsValidation(t *testing.T) {
	src := mustLoad(t)
	for name, mutate := range map[string]func(*Options){
		"arch":   func(o *Options) { o.Arch = "riscv64" },
		"driver": func(o *Options) { o.Driver = "podman" },
		"image":  func(o *Options) { o.Image = "" },
		"data":   func(o *Options) { o.DataVolume = "" },
		"tilde":  func(o *Options) { o.DataVolume = "~/data" },
		"domain": func(o *Options) { o.ProxyDomain = "" },
		"bindir": func(o *Options) { o.BinDir = "relative/bin" },
	} {
		o := testOptions()
		mutate(&o)
		if _, err := BuildPlan(src, o); err == nil {
			t.Fatalf("BuildPlan accepted a bad %s option", name)
		}
	}
}

func TestDivergencesAllCarryAWhy(t *testing.T) {
	plan := mustPlan(t, testOptions())
	if len(plan.Divergences()) == 0 {
		t.Fatal("a pod rendered without a scheduler diverges; none were recorded")
	}
	for _, d := range plan.Divergences() {
		if d.Area == "" || d.What == "" || d.Why == "" {
			t.Fatalf("divergence is missing a field: %+v", d)
		}
	}
}

func TestGoldenDockerArgv(t *testing.T) {
	plan := mustPlan(t, testOptions())
	var buf bytes.Buffer
	// ShellLine, not strings.Join: a plain space renders `--foo bar`,
	// `--foo=bar` and a single argument containing a space identically, so a
	// golden file built that way cannot see an argument boundary move — which
	// is a bug this builder has already had once.
	for _, argv := range plan.DockerArgv() {
		fmt.Fprintln(&buf, ShellLine(argv))
	}
	buf.WriteString("\n# divergences\n")
	for _, d := range plan.Divergences() {
		blocking := ""
		if d.Blocking {
			blocking = " [BLOCKING]"
		}
		fmt.Fprintf(&buf, "%s: %s%s\n  why: %s\n", d.Area, d.What, blocking, d.Why)
	}

	golden := filepath.Join("testdata", "plan_arm64.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run: go test ./internal/devpod -update): %v", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Fatalf("docker argv changed. Review the diff, then re-record with:\n  go test ./internal/devpod -update\n\ngot:\n%s\nwant:\n%s", buf.String(), string(want))
	}
}

// TestDownKeepsTheDataVolume is the teardown safety property: the data volume
// is the local stand-in for the CKS hostPath, so it holds the VM inventory,
// the guest disks, the control SQLite, the node identity and the rootfs
// template. `devpod down` removing it would delete a dev node's entire state
// every time it was restarted.
func TestDownKeepsTheDataVolume(t *testing.T) {
	o := testOptions()
	// The CLI's own default: no -data, so the data tier is a docker volume
	// this plan creates. That is exactly the case that used to be deleted.
	o.DataVolume = "sparkbox-dev-data"
	o.TrustDir = ""
	plan := mustPlan(t, o)

	removed := func(purge bool) []string {
		var out []string
		for _, argv := range plan.StopArgv(purge) {
			if len(argv) > 2 && argv[1] == "volume" && argv[2] == "rm" {
				out = append(out, argv[len(argv)-1])
			}
		}
		return out
	}

	kept := removed(false)
	for _, name := range kept {
		if name == "sparkbox-dev-data" {
			t.Fatalf("devpod down deleted the data volume: %v", kept)
		}
	}
	// The per-run volumes still go: an emptyDir does not survive its Pod, and
	// neither does the docker volume standing in for the Secret.
	for _, want := range []string{
		"sparkbox-dev-tmp",
		"sparkbox-dev-checkpoint-scratch",
		"sparkbox-dev-vmm-socket",
		"sparkbox-dev-sluice-socket",
		"sparkbox-dev-gateway-trust",
	} {
		if !contains(kept, want) {
			t.Fatalf("devpod down left the ephemeral volume %s behind: %v", want, kept)
		}
	}

	purged := removed(true)
	if !contains(purged, "sparkbox-dev-data") {
		t.Fatalf("devpod down --purge-data did not remove the data volume: %v", purged)
	}

	// A host path is not docker's to delete, so nothing tries.
	bind := mustPlan(t, testOptions())
	for _, argv := range bind.StopArgv(true) {
		if len(argv) > 2 && argv[1] == "volume" && argv[2] == "rm" && strings.HasPrefix(argv[len(argv)-1], "/") {
			t.Fatalf("teardown ran `docker volume rm` on a host path: %v", argv)
		}
	}
}

// TestNamespaceHolderIsUnprivileged: the holder lives inside the namespace the
// manifest's containers were carefully de-privileged for, and it is not in the
// manifest at all. Docker's defaults would make it the most privileged process
// in the pod — root, full default capability set, writable rootfs, escalation
// allowed — to run `sleep infinity`.
func TestNamespaceHolderIsUnprivileged(t *testing.T) {
	plan := mustPlan(t, testOptions())
	holder := argvNamed(plan.DockerArgv(), "sparkbox-dev-netns")
	if !hasFlagValue(holder, "--user", "65532:65532") {
		t.Fatalf("holder does not run unprivileged: %v", holder)
	}
	if !hasFlagValue(holder, "--cap-drop", "ALL") {
		t.Fatalf("holder keeps docker's default capability set: %v", holder)
	}
	if !hasFlagValue(holder, "--security-opt", "no-new-privileges") {
		t.Fatalf("holder allows privilege escalation: %v", holder)
	}
	if !containsArg([][]string{holder}, "--read-only") {
		t.Fatalf("holder has a writable rootfs: %v", holder)
	}
	if containsArg([][]string{holder}, "--privileged") || containsArg([][]string{holder}, "--cap-add") {
		t.Fatalf("holder was granted privileges it has no use for: %v", holder)
	}
	// runc applies the sysctls during namespace setup, before the process
	// starts, so dropping capabilities must not have cost them.
	for _, s := range podSysctls {
		if !hasFlagValue(holder, "--sysctl", s.Name+"="+s.Value) {
			t.Fatalf("hardening the holder dropped sysctl %s: %v", s.Name, holder)
		}
	}
}

// TestGatewayAddrIsOnlyOverriddenWhenSet: SPARKBOX_GATEWAY_ADDR decides whether
// the node links to a control plane at all, so emptying it by default (which is
// what an unconditional override did) silently produced an unlinked node while
// the divergence list said nothing.
func TestGatewayAddrIsOnlyOverriddenWhenSet(t *testing.T) {
	set := mustPlan(t, testOptions())
	if got := envOf(t, set, "sparkbox-node", "SPARKBOX_GATEWAY_ADDR"); got != "192.168.64.1:2222" {
		t.Fatalf("Options.GatewayAddr did not reach the container: %q", got)
	}
	if hasDivergenceArea(set, "gateway link") {
		t.Fatal("a configured gateway is not a divergence")
	}

	o := testOptions()
	o.GatewayAddr = ""
	unset := mustPlan(t, o)
	want := "sparkbox-gateway.sparkbox-poc.svc.cluster.local:2222"
	if got := envOf(t, unset, "sparkbox-node", "SPARKBOX_GATEWAY_ADDR"); got != want {
		t.Fatalf("an unset Options.GatewayAddr rewrote the manifest value to %q", got)
	}
	if !hasDivergence(unset, "gateway link", "the manifest's "+want, true) {
		t.Fatalf("carrying the cluster's gateway address was not recorded as blocking: %+v", unset.Divergences())
	}

	// And when the manifest itself has no address, the node runs unlinked.
	fsys := mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"            - name: SPARKBOX_GATEWAY_ADDR\n              value: "+want+"\n",
			"            - name: SPARKBOX_GATEWAY_ADDR\n              value: \"\"\n",
			1)
	})
	src, err := LoadFS(fsys)
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	plan, err := BuildPlan(src, o)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if !hasDivergence(plan, "gateway link", "SPARKBOX_GATEWAY_ADDR is empty", true) {
		t.Fatalf("an empty gateway address was not recorded as blocking: %+v", plan.Divergences())
	}
}

// TestServiceNamedTargetPortsResolve: every port in service.yaml but ssh names
// `https`, which is a port name in the GATEWAY Pod — the one the Service's
// selector matches. Leaving it unresolved made every carried port claim the
// Service's own number as its container port.
func TestServiceNamedTargetPortsResolve(t *testing.T) {
	plan := mustPlan(t, testOptions())
	byName := map[string]Port{}
	for _, port := range plan.Network.ServicePorts {
		byName[port.Name] = port
	}
	if got := byName["ssh"]; got.HostPort != 22 || got.Container != 2222 {
		t.Fatalf("ssh port did not resolve to the gateway's containerPort: %+v", got)
	}
	for _, name := range []string{"http", "https", "https-3000", "https-8080"} {
		got, ok := byName[name]
		if !ok {
			t.Fatalf("service port %s was not carried", name)
		}
		if got.Container != 8081 {
			t.Fatalf("service port %s targets containerPort %d, want the gateway's 8081", name, got.Container)
		}
	}

	// A name no Pod declares is an error, not a guessed number.
	fsys := mutate(t, "service.yaml", func(in string) string {
		return strings.Replace(in,
			"    - name: ssh\n      protocol: TCP\n      port: 22\n      targetPort: ssh\n",
			"    - name: ssh\n      protocol: TCP\n      port: 22\n      targetPort: sshd\n",
			1)
	})
	src, err := LoadFS(fsys)
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	if _, err := BuildPlan(src, testOptions()); err == nil {
		t.Fatal("BuildPlan emitted a port number for a targetPort name nothing declares")
	} else if !strings.Contains(err.Error(), "sshd") {
		t.Fatalf("error does not name the unresolved port: %v", err)
	}
}

// TestProbeTimeoutIsTheManifestTimeout: docker's --health-timeout defaults to
// 30s and Kubernetes' timeoutSeconds defaults to 1s. Emitting nothing gave
// every translated probe thirty times the patience the deployed one has.
func TestProbeTimeoutIsTheManifestTimeout(t *testing.T) {
	plan := mustPlan(t, testOptions())
	if !hasFlagValue(argvFor(t, plan, "vmm-helper"), "--health-timeout", "1s") {
		t.Fatalf("vmm-helper's probe did not get Kubernetes' 1s default: %v", argvFor(t, plan, "vmm-helper"))
	}

	fsys := mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"                - test -S /run/sparkbox-vmm/helper.sock\n            periodSeconds: 2\n",
			"                - test -S /run/sparkbox-vmm/helper.sock\n            timeoutSeconds: 5\n            periodSeconds: 2\n",
			1)
	})
	src, err := LoadFS(fsys)
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	plan, err = BuildPlan(src, testOptions())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if !hasFlagValue(argvFor(t, plan, "vmm-helper"), "--health-timeout", "5s") {
		t.Fatal("a manifest timeoutSeconds did not reach --health-timeout")
	}
}

// TestExecLivenessProbesAreNamed: healthFor spends the single docker
// HEALTHCHECK on the readiness probe, so vmm-helper's exec livenessProbe is not
// reproduced. It has to be said out loud, because the container it watches is
// the one whose wedged socket the cluster restarts it for.
func TestExecLivenessProbesAreNamed(t *testing.T) {
	plan := mustPlan(t, testOptions())
	if !hasDivergence(plan, "probes", "exec livenessProbes are not reproduced", false) {
		t.Fatalf("exec liveness probes are dropped silently: %+v", plan.Divergences())
	}
	if !hasDivergence(plan, "probes", "/run/sparkbox-vmm/helper.sock", false) {
		t.Fatal("the divergence does not say which probe is missing")
	}
}

// TestCiliumEgressPolicyIsNamed: network-policy.yaml's vm-node-public-egress
// selects this exact Pod and nothing local enforces it, which means the dev
// pod can reach every private network CKS excepts. That is a security property
// the plan must not leave unstated.
func TestCiliumEgressPolicyIsNamed(t *testing.T) {
	plan := mustPlan(t, testOptions())
	if !hasDivergence(plan, "network policy", "vm-node-public-egress", false) {
		t.Fatalf("the CiliumNetworkPolicy that selects this Pod is not mentioned: %+v", plan.Divergences())
	}
	for _, cidr := range []string{"0.0.0.0/0", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"} {
		if !hasDivergence(plan, "network policy", cidr, false) {
			t.Fatalf("the divergence does not name the unenforced CIDR %s", cidr)
		}
	}
	// The namespace's default-deny ingress selects every Pod, so it counts too.
	if !hasDivergence(plan, "network policy", "default-deny-ingress", false) {
		t.Fatal("the default-deny ingress policy is not mentioned")
	}
}

// TestBridgeSubnetCannotOverlapTheGuestSubnet: docker's default address pool is
// 172.17.0.0/12, which contains the Pod's 172.30.0.0/20 guest range, so the
// bridge is pinned explicitly.
func TestBridgeSubnetCannotOverlapTheGuestSubnet(t *testing.T) {
	plan := mustPlan(t, testOptions())
	create := plan.DockerArgv()[0]
	if create[1] != "network" || !hasFlagValue(create, "--subnet", DefaultNetworkSubnet) {
		t.Fatalf("the network is created without an explicit subnet: %v", create)
	}
	o := testOptions()
	o.NetworkSubnet = "172.30.0.0/16" // contains the guest /20
	if _, err := BuildPlan(mustLoad(t), o); err == nil {
		t.Fatal("BuildPlan accepted a bridge subnet that swallows the guest network")
	} else if !strings.Contains(err.Error(), "172.30.0.0/20") {
		t.Fatalf("error does not name the guest subnet: %v", err)
	}
}

// TestModeledButUnreadFieldsAreNotSilent: strict decoding only catches fields
// the model does not have. A field podspec DOES decode and plan.go never reads
// would change the deployed Pod while the local one rendered identically, which
// is the same drift with a longer fuse.
func TestModeledButUnreadFieldsAreNotSilent(t *testing.T) {
	// emptyDir medium: a tmpfs is RAM-backed and charged to the Pod's memory.
	fsys := mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"        - name: tmp\n          emptyDir:\n            sizeLimit: 1Gi\n",
			"        - name: tmp\n          emptyDir:\n            medium: Memory\n            sizeLimit: 1Gi\n",
			1)
	})
	src, err := LoadFS(fsys)
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	plan, err := BuildPlan(src, testOptions())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if !hasDivergence(plan, "volume tmp", "medium Memory", false) {
		t.Fatalf("emptyDir medium was dropped silently: %+v", plan.Divergences())
	}

	// A Pod-level privilege field: Kubernetes would push it down into every
	// container that sets none, and docker has no Pod-level context at all.
	fsys = mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"      securityContext:\n        fsGroup: 65532\n",
			"      securityContext:\n        runAsUser: 65532\n        fsGroup: 65532\n",
			1)
	})
	if src, err = LoadFS(fsys); err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	if plan, err = BuildPlan(src, testOptions()); err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if !hasDivergence(plan, "securityContext", "runAsUser 65532", true) {
		t.Fatalf("a pod-level securityContext field was dropped silently: %+v", plan.Divergences())
	}

	// A Secret item list renames keys into paths; a bind of a host directory
	// does not.
	fsys = mutate(t, "deployment.yaml", func(in string) string {
		return strings.Replace(in,
			"          secret:\n            secretName: sparkbox-node-trust\n            defaultMode: 0444\n",
			"          secret:\n            secretName: sparkbox-node-trust\n            defaultMode: 0444\n            items:\n              - key: gateway_host_key.pub\n                path: host.pub\n",
			1)
	})
	if src, err = LoadFS(fsys); err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	if plan, err = BuildPlan(src, testOptions()); err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if !hasDivergence(plan, "volume gateway-trust", "gateway_host_key.pub->host.pub", false) {
		t.Fatalf("a Secret item list was dropped silently: %+v", plan.Divergences())
	}
}

// TestEveryModeledFieldHasADecision walks the podspec model and requires each
// field to appear in fieldDecisions. That table is the anti-drift claim written
// down: adding a field to podspec is only half a change, and the other half —
// consuming it, failing on it, or recording a divergence — is what this test
// refuses to let anyone skip.
func TestEveryModeledFieldHasADecision(t *testing.T) {
	seen := map[string]bool{}
	var walk func(reflect.Type, map[reflect.Type]bool)
	walk = func(typ reflect.Type, visiting map[reflect.Type]bool) {
		for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || visiting[typ] {
			return
		}
		visiting[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			seen[typ.Name()+"."+field.Name] = true
			walk(field.Type, visiting)
		}
	}
	for _, root := range []interface{}{podspec.Deployment{}, podspec.Service{}, podspec.NetworkPolicy{}} {
		walk(reflect.TypeOf(root), map[reflect.Type]bool{})
	}

	var missing, stale []string
	for field := range seen {
		if _, ok := fieldDecisions[field]; !ok {
			missing = append(missing, field)
		}
	}
	for field := range fieldDecisions {
		if !seen[field] {
			stale = append(stale, field)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Fatalf("podspec models fields nothing has decided about: %v\nconsume them in plan.go, fail on them, or record a Divergence — then add them to fieldDecisions", missing)
	}
	if len(stale) > 0 {
		t.Fatalf("fieldDecisions describes fields podspec no longer has: %v", stale)
	}
}

// fieldDecisions records what BuildPlan does with every field the podspec model
// decodes. "read" means the plan uses the value; "divergence" means it renders
// into Plan.Divergences; "error" means BuildPlan refuses; "structural" means
// the field only carries other fields.
var fieldDecisions = map[string]string{
	"Deployment.APIVersion":     "unused: DecodeDeployment checks kind, and there is one apps/v1",
	"Deployment.Kind":           "read: DecodeDeployment rejects anything but Deployment",
	"Deployment.Metadata":       "structural",
	"Deployment.Spec":           "structural",
	"DeploymentSpec.Replicas":   "divergence: deployment",
	"DeploymentSpec.Strategy":   "divergence: deployment",
	"DeploymentSpec.Selector":   "unused: no controller selects pods locally; the container names are unique instead",
	"DeploymentSpec.Template":   "structural",
	"Strategy.Type":             "divergence: deployment",
	"LabelSelector.MatchLabels": "read: recordPolicyDivergences matches a policy against the Pod's labels",
	"Meta.Name":                 "read: policy and gateway names appear in divergences",
	"Meta.Namespace":            "unused: docker has no namespaces; Options.Prefix separates instances",
	"Meta.Labels":               "read: the Service and policy selectors are matched against them",
	"Meta.Annotations":          "unused: every annotation here is a CoreWeave LB or users-hash hint",
	"PodTemplateSpec.Metadata":  "structural",
	"PodTemplateSpec.Spec":      "structural",

	"PodSpec.ServiceAccountName":            "divergence: serviceAccount",
	"PodSpec.AutomountServiceAccountToken":  "read: the serviceAccount divergence says whether a token would be mounted",
	"PodSpec.TerminationGracePeriodSeconds": "read: Plan.StopTimeout, the `docker stop --time`",
	"PodSpec.SecurityContext":               "structural",
	"PodSpec.NodeSelector":                  "divergence: nodeSelector, and read for the cluster's arch",
	"PodSpec.InitContainers":                "read: rendered, in order, as `docker run --rm`",
	"PodSpec.Containers":                    "read: rendered as detached containers",
	"PodSpec.Volumes":                       "read: buildVolumes",

	"PodSecurityContext.FSGroup":             "divergence: securityContext",
	"PodSecurityContext.FSGroupChangePolicy": "divergence: securityContext",
	"PodSecurityContext.RunAsNonRoot":        "divergence: securityContext (blocking; docker has no pod-level context)",
	"PodSecurityContext.RunAsUser":           "divergence: securityContext (blocking)",
	"PodSecurityContext.RunAsGroup":          "divergence: securityContext (blocking)",
	"PodSecurityContext.SeccompProfile":      "divergence: securityContext (blocking)",

	"Container.Name":            "read: overlay lookup, container name, labels",
	"Container.Image":           "unused: always __SPARKBOX_IMAGE__; Options.Image replaces it",
	"Container.ImagePullPolicy": "divergence: imagePullPolicy",
	"Container.Command":         "read: --entrypoint plus trailing argv",
	"Container.Args":            "read: trailing argv",
	"Container.Env":             "read: --env, with the local overrides",
	"Container.SecurityContext": "structural",
	"Container.Resources":       "divergence: resources, and read for sparkbox.dev/* devices",
	"Container.Ports":           "read: published on the namespace holder",
	"Container.VolumeMounts":    "read: --mount",
	"Container.StartupProbe":    "read: its budget becomes --health-start-period",
	"Container.ReadinessProbe":  "read: becomes the docker HEALTHCHECK",
	"Container.LivenessProbe":   "divergence: probes (exec) / probes (tcpSocket)",

	"EnvVar.Name":                "read",
	"EnvVar.Value":               "read",
	"EnvVar.ValueFrom":           "error: a local pod has no cluster Secret",
	"EnvVarSource.SecretKeyRef":  "error: see EnvVar.ValueFrom",
	"SecretKeySelector.Name":     "error: see EnvVar.ValueFrom",
	"SecretKeySelector.Key":      "error: see EnvVar.ValueFrom",
	"SecretKeySelector.Optional": "error: see EnvVar.ValueFrom",

	"SecurityContext.AllowPrivilegeEscalation": "read: --security-opt no-new-privileges",
	"SecurityContext.AppArmorProfile":          "read: --security-opt apparmor=...",
	"SecurityContext.Capabilities":             "read: --cap-drop / --cap-add",
	"SecurityContext.Privileged":               "read: --privileged",
	"SecurityContext.ReadOnlyRootFilesystem":   "read: --read-only",
	"SecurityContext.RunAsNonRoot":             "read: BuildPlan fails if the resolved uid would be root",
	"SecurityContext.RunAsUser":                "read: --user",
	"SecurityContext.RunAsGroup":               "read: --user",
	"SecurityContext.SeccompProfile":           "read: --security-opt seccomp=...",
	"Capabilities.Add":                         "read",
	"Capabilities.Drop":                        "read",
	"SeccompProfile.Type":                      "read: RuntimeDefault and Unconfined translate, anything else errors",
	"SeccompProfile.LocalhostProfile":          "error: a Localhost type fails the switch on Type before this is read",
	"AppArmorProfile.Type":                     "read: RuntimeDefault and Unconfined translate, anything else errors",
	"AppArmorProfile.LocalhostProfile":         "error: a Localhost type fails the switch on Type before this is read",

	"Resources.Requests":          "divergence: resources; read for sparkbox.dev/* resources",
	"Resources.Limits":            "divergence: resources; read for sparkbox.dev/* resources",
	"ContainerPort.Name":          "read: Port.Name, and resolves a named Service targetPort",
	"ContainerPort.ContainerPort": "read: --publish",
	"ContainerPort.Protocol":      "read: --publish suffix",
	"VolumeMount.Name":            "read",
	"VolumeMount.MountPath":       "read: mount target",
	"VolumeMount.SubPath":         "read: volume-subpath, or a subdirectory of a bind",
	"VolumeMount.ReadOnly":        "read: mount readonly",

	"Probe.Exec":                "read: --health-cmd",
	"Probe.TCPSocket":           "divergence: probes",
	"Probe.InitialDelaySeconds": "divergence: probes (no docker equivalent)",
	"Probe.PeriodSeconds":       "read: --health-interval",
	"Probe.TimeoutSeconds":      "read: --health-timeout",
	"Probe.SuccessThreshold":    "divergence: probes (a HEALTHCHECK is healthy after one success)",
	"Probe.FailureThreshold":    "read: --health-retries",
	"ExecAction.Command":        "read",
	"TCPSocketAction.Port":      "divergence: probes",
	"IntOrString.IsInt":         "read: targetPort resolution",
	"IntOrString.IntVal":        "read: targetPort resolution",
	"IntOrString.StrVal":        "read: targetPort resolution",

	"Volume.Name":                           "read",
	"Volume.HostPath":                       "read: the data tier",
	"Volume.EmptyDir":                       "read: a per-run docker volume",
	"Volume.Secret":                         "read: Options.TrustDir or an empty docker volume",
	"Volume.PersistentVolumeClaim":          "error: there is no local storageClass",
	"Volume.Projected":                      "error: no API server, no cluster Secret",
	"HostPathVolume.Path":                   "divergence: volume data",
	"HostPathVolume.Type":                   "divergence: volume data (`devpod up` creates the bind sources itself)",
	"EmptyDirVolume.Medium":                 "divergence: volume <name> (a docker volume is disk-backed)",
	"EmptyDirVolume.SizeLimit":              "divergence: volume <name> (docker volumes are unbounded)",
	"SecretVolume.SecretName":               "divergence: volume gateway-trust",
	"SecretVolume.DefaultMode":              "divergence: volume gateway-trust (a bind keeps the host's modes)",
	"SecretVolume.Optional":                 "divergence: volume gateway-trust",
	"SecretVolume.Items":                    "divergence: volume gateway-trust (a bind does not rename keys)",
	"KeyToPath.Key":                         "divergence: volume gateway-trust",
	"KeyToPath.Path":                        "divergence: volume gateway-trust",
	"KeyToPath.Mode":                        "divergence: volume gateway-trust (defaultMode already is not reproduced)",
	"PVCVolume.ClaimName":                   "error: see Volume.PersistentVolumeClaim",
	"PVCVolume.ReadOnly":                    "error: see Volume.PersistentVolumeClaim",
	"ProjectedVolume.DefaultMode":           "error: see Volume.Projected",
	"ProjectedVolume.Sources":               "error: see Volume.Projected",
	"ProjectedSource.Secret":                "error: see Volume.Projected",
	"ProjectedSource.ServiceAccountToken":   "error: see Volume.Projected",
	"ServiceAccountToken.Audience":          "error: see Volume.Projected",
	"ServiceAccountToken.ExpirationSeconds": "error: see Volume.Projected",
	"ServiceAccountToken.Path":              "error: see Volume.Projected",

	"Service.APIVersion":                "unused: DecodeService checks kind",
	"Service.Kind":                      "read: DecodeService rejects anything but Service",
	"Service.Metadata":                  "structural",
	"Service.Spec":                      "structural",
	"ServiceSpec.Type":                  "divergence: service (a LoadBalancer has no local equivalent)",
	"ServiceSpec.ExternalTrafficPolicy": "unused: there is no load balancer to preserve a client IP through",
	"ServiceSpec.Selector":              "read: decides whether these ports belong to this Pod",
	"ServiceSpec.Ports":                 "read: Plan.Network.Ports or ServicePorts",
	"ServicePort.Name":                  "read",
	"ServicePort.Protocol":              "read",
	"ServicePort.Port":                  "read: the host port",
	"ServicePort.TargetPort":            "read: resolved against the Pod the selector matches",
	"ServicePort.NodePort":              "unused: a nodePort needs a kube-proxy; unset in service.yaml",

	"NetworkPolicy.APIVersion":           "read: distinguishes the Cilium policy in the divergence text",
	"NetworkPolicy.Kind":                 "read: DecodeNetworkPolicies rejects other kinds; named in the divergence",
	"NetworkPolicy.Metadata":             "structural",
	"NetworkPolicy.Spec":                 "structural",
	"NetworkPolicySpec.PodSelector":      "read: which policies select this Pod",
	"NetworkPolicySpec.EndpointSelector": "read: Cilium's spelling of the same",
	"NetworkPolicySpec.PolicyTypes":      "unused: the rule lists already say ingress or egress",
	"NetworkPolicySpec.Ingress":          "divergence: network policy",
	"NetworkPolicySpec.Egress":           "divergence: network policy",
	"NetworkRule.Ports":                  "divergence: network policy",
	"NetworkRule.ToPorts":                "divergence: network policy",
	"NetworkRule.ToServices":             "divergence: network policy",
	"NetworkRule.ToCIDRSet":              "divergence: network policy",
	"NetworkPolicyPeer.Ports":            "divergence: network policy",
	"NetworkPolicyPort.Port":             "divergence: network policy",
	"NetworkPolicyPort.Protocol":         "divergence: network policy",
	"ServiceRef.K8sService":              "divergence: network policy",
	"K8sServiceRef.ServiceName":          "divergence: network policy",
	"K8sServiceRef.Namespace":            "divergence: network policy",
	"CIDRRule.CIDR":                      "divergence: network policy",
	"CIDRRule.Except":                    "divergence: network policy",
}

// helpers

func contains(haystack []string, want string) bool {
	for _, got := range haystack {
		if got == want {
			return true
		}
	}
	return false
}

func envOf(t *testing.T, p *Plan, container, name string) string {
	t.Helper()
	for _, c := range p.Containers {
		if c.Name != container {
			continue
		}
		for _, env := range c.Env {
			if env.Name == name {
				return env.Value
			}
		}
		t.Fatalf("container %s has no %s", container, name)
	}
	t.Fatalf("no container %s", container)
	return ""
}

func containerNames(in []podspec.Container) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, c.Name)
	}
	return out
}

func hasDivergence(p *Plan, area, what string, blocking bool) bool {
	for _, d := range p.Divergences() {
		if d.Area == area && strings.Contains(d.What, what) && d.Blocking == blocking {
			return true
		}
	}
	return false
}

func hasDivergenceArea(p *Plan, area string) bool {
	for _, d := range p.Divergences() {
		if d.Area == area {
			return true
		}
	}
	return false
}

func hasFlagValue(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

func containsArg(argvs [][]string, want string) bool {
	for _, argv := range argvs {
		for _, arg := range argv {
			if arg == want {
				return true
			}
		}
	}
	return false
}

func nameFlag(argv []string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--name" {
			return argv[i+1]
		}
	}
	return ""
}

func argvNamed(argvs [][]string, name string) []string {
	for _, argv := range argvs {
		if nameFlag(argv) == name {
			return argv
		}
	}
	return nil
}

func argvFor(t *testing.T, p *Plan, container string) []string {
	t.Helper()
	argv := argvNamed(p.DockerArgv(), p.Options.Prefix+"-"+container)
	if argv == nil {
		t.Fatalf("no docker argv for container %q", container)
	}
	return argv
}
