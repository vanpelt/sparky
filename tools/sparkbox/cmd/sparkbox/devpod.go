package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/devpod"
)

// devpodCommand runs the local pod-shape development environment: the same
// five containers deploy/kubernetes/deployment.yaml describes, rendered into
// docker argv by internal/devpod.
//
// Everything interesting lives in internal/devpod. This file only parses
// flags, prints, and shells out to docker.
func devpodCommand(args []string) error {
	const usage = "usage: sparkbox devpod <plan|diff|up|down|status> [flags]"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	sub := args[0]
	fs := flag.NewFlagSet("devpod "+sub, flag.ExitOnError)
	var (
		arch = fs.String("arch", runtime.GOARCH, "architecture to render: amd64 | arm64")
		// The dev image is built from deploy/kubernetes/Containerfile; there
		// is no registry involved, so nothing here ever pulls.
		image       = fs.String("image", "sparkbox-dev:latest", "local image ref for all five containers")
		data        = fs.String("data", "", "host directory or docker volume for /var/lib/sparkbox; empty uses the docker volume <prefix>-data. A host path must support reflinks: a sandbox is `cp --reflink=always` of the template")
		driver      = fs.String("driver", "firecracker", "vm driver the node runs: mock | firecracker")
		proxyDomain = fs.String("proxy-domain", "devpod.localhost", "value for SPARKBOX_PROXY_DOMAIN")
		gatewayAddr = fs.String("gateway", "", "host:port of the gateway this node links out to")
		trustDir    = fs.String("trust-dir", "", "host directory replacing the sparkbox-node-trust Secret; must hold gateway_host_key.pub")
		binDir      = fs.String("bin-dir", "", "host directory of linux binaries to bind-mount over the image's (sparkbox, sparkbox-vmm-helper, sluice)")
		hostMemMB   = fs.Int64("host-mem-mb", 0, "SPARKBOX_HOST_MEM_MB for admission control; 0 keeps the manifest's CKS value, which is far larger than a laptop")
		defVCPUs    = fs.Int64("default-vcpus", 0, "SPARKBOX_DEFAULT_VCPUS: vCPUs for a sandbox nobody sized, which is every `new@` sandbox; 0 keeps the binary's CKS-sized built-in (4)")
		defMemMB    = fs.Int64("default-mem-mb", 0, "SPARKBOX_DEFAULT_MEM_MB: RAM for a sandbox nobody sized; 0 keeps the built-in 12288, which on a laptop's container machine is a guest larger than the machine")
		blockIO     = fs.String("block-io-engine", "", "SPARKBOX_BLOCK_IO_ENGINE: Sync | Async; empty CLEARS the manifest's CKS Sync pin so the binary's Async default applies, which boots 2.4x faster here")
		nodeName    = fs.String("node-name", "", "SPARKBOX_NODE_NAME; empty keeps the manifest's cks-poc")
		hivemindAPI = fs.String("hivemind-api", "", "SPARKBOX_HIVEMIND_API; empty leaves the presence lease off")
		prefix      = fs.String("prefix", devpod.DefaultPrefix, "name prefix for the docker network, volumes and containers")
		realKVM     = fs.Bool("kvm", kvmPresent(), "grant /dev/kvm; defaults to whether /dev/kvm exists on this machine")
		subnet      = fs.String("network-subnet", devpod.DefaultNetworkSubnet, "docker bridge subnet; must not overlap the Pod's SPARKBOX_GUEST_SUBNET")
		dryRun      = fs.Bool("dry-run", false, "up/down: print the docker commands instead of running them")
		purgeData   = fs.Bool("purge-data", false, "down: ALSO delete the data volume — the VM inventory, guest disks, control database, node identity and rootfs template. Off by default; teardown otherwise keeps the data tier the way removing a Pod keeps its hostPath")
	)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if *data == "" {
		*data = *prefix + "-data"
	}

	src, err := devpod.Load()
	if err != nil {
		return err
	}
	plan, err := devpod.BuildPlan(src, devpod.Options{
		Arch:          *arch,
		Image:         *image,
		DataVolume:    *data,
		Driver:        *driver,
		ProxyDomain:   *proxyDomain,
		GatewayAddr:   *gatewayAddr,
		RealKVM:       *realKVM,
		BinDir:        *binDir,
		Prefix:        *prefix,
		TrustDir:      *trustDir,
		HostMemMB:     *hostMemMB,
		DefaultVCPUs:  *defVCPUs,
		DefaultMemMB:  *defMemMB,
		BlockIOEngine: *blockIO,
		NodeName:      *nodeName,
		HivemindAPI:   *hivemindAPI,
		NetworkSubnet: *subnet,
	})
	if err != nil {
		return err
	}

	switch sub {
	case "plan":
		printArgv(plan)
		fmt.Println()
		printDivergences(plan)
		return nil
	case "diff":
		printDivergences(plan)
		return nil
	case "up":
		return devpodUp(plan, *dryRun)
	case "down":
		return devpodDown(plan, *dryRun, *purgeData)
	case "status":
		return runDocker(plan.StatusArgv(), false)
	default:
		return fmt.Errorf("%s", usage)
	}
}

// kvmPresent reports whether this machine has a KVM device. On the Mac the
// answer is no, and the plan says so rather than pretending; the pod is meant
// to run inside a Linux VM where the answer is yes.
func kvmPresent() bool {
	info, err := os.Stat("/dev/kvm")
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func printArgv(plan *devpod.Plan) {
	for _, argv := range plan.DockerArgv() {
		fmt.Println(shellLine(argv))
	}
}

func printDivergences(plan *devpod.Plan) {
	divergences := plan.Divergences()
	blocking := 0
	for _, d := range divergences {
		if d.Blocking {
			blocking++
		}
	}
	fmt.Printf("# %d deliberate divergences from deploy/kubernetes/deployment.yaml (%d blocking)\n", len(divergences), blocking)
	for _, d := range divergences {
		marker := "  "
		if d.Blocking {
			marker = "! "
		}
		fmt.Printf("%s%s: %s\n    why: %s\n", marker, d.Area, d.What, d.Why)
	}
}

func devpodUp(plan *devpod.Plan, dryRun bool) error {
	// Docker creates a missing bind source as a root-owned directory. For the
	// node's subPath mounts that would silently produce an empty directory
	// where the controller expected its state, so make them first.
	for _, dir := range plan.BindDirs() {
		if dryRun {
			fmt.Println("mkdir -p", dir)
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create bind source %s: %w", dir, err)
		}
	}
	for _, argv := range plan.DockerArgv() {
		if dryRun {
			fmt.Println(shellLine(argv))
			continue
		}
		if err := runDocker(argv, false); err != nil {
			return fmt.Errorf("%s: %w", describe(argv), err)
		}
	}
	return nil
}

func devpodDown(plan *devpod.Plan, dryRun, purgeData bool) error {
	if purgeData {
		// A host path is the user's own directory and docker cannot remove it;
		// say so rather than silently doing nothing with a flag that promised
		// deletion.
		if data, ok := plan.DataVolume(); ok && data.Kind == "bind" {
			fmt.Fprintf(os.Stderr, "sparkbox: -purge-data cannot remove the host directory %s; delete it yourself if that is what you meant\n", data.Source)
		}
	}
	// Teardown is best-effort by design: a partial `up` leaves some of these
	// missing, and refusing to remove the rest would strand them.
	for _, argv := range plan.StopArgv(purgeData) {
		if dryRun {
			fmt.Println(shellLine(argv))
			continue
		}
		if err := runDocker(argv, true); err != nil {
			fmt.Fprintf(os.Stderr, "sparkbox: %s: %v\n", describe(argv), err)
		}
	}
	return nil
}

func runDocker(argv []string, quiet bool) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if quiet {
		cmd.Stderr = nil
	}
	return cmd.Run()
}

// describe names an invocation for an error message: the --name it creates, or
// the subcommand.
func describe(argv []string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--name" {
			return "docker run " + argv[i+1]
		}
	}
	if len(argv) >= 3 {
		return strings.Join(argv[:3], " ")
	}
	return strings.Join(argv, " ")
}

// shellLine renders an argv for a human to read or paste. The quoting lives in
// internal/devpod so that what is printed here and what the golden test
// compares are the same rendering.
func shellLine(argv []string) string { return devpod.ShellLine(argv) }
