//go:build linux

package qemu_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/qemu"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/qemuargs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/vmmtest"
)

// TestQEMUParity runs the driver-parity suite against real QEMU/KVM guests.
//
// This is the whole reason the QEMU driver exists. internal/vmm's Driver and
// its ten capability interfaces were written against one engine, and until a
// second engine satisfies them unmodified the word "abstraction" is a claim.
// The same 19 cases run here and in internal/vmm/firecracker/parity_linux_test.go
// with no change to the harness; where the two drivers disagree, one of them is
// wrong about the contract.
//
// Nothing in this package has been run on hardware yet. Every measured fact it
// is built on came from the hand-driven spike in docs/qemu-spike.md, which
// booted the guest, snapshotted it, restored it and read its balloon — but by
// hand, not through vmmtest. This file is the first thing that will tell us
// which of the driver's inferences were wrong.
//
// It needs a Linux KVM host with QEMU 8.2 or newer (the `file:` migration URI
// Pause depends on landed in 8.2), the guest kernel, a rootfs template, and a
// reflink-capable filesystem for VM state. hack/parity/ arranges all of that
// for firecracker; see the note on parityConfig below for what a QEMU run needs
// on top.
//
// The gate is SPARKBOX_VMM_PARITY=1 and nothing else. Deliberately not a build
// tag: a tag keeps the file out of `go test ./...` by never compiling it, so it
// stops catching the signature drift that is half of what a parity suite is
// for. With an env gate, `go test ./...` on a LINUX checkout compiles every line
// here — including whether *qemu.Driver still satisfies vmm.Driver — and skips.
//
// On the arm64 Mac that is this project's dev machine it compiles nothing: every
// file in this package carries //go:build linux, so `go vet ./internal/vmm/qemu/`
// there reports "build constraints exclude all Go files" and `go test
// ./internal/vmm/...` lists vmm, mock and vmmtest with neither qemu nor
// firecracker in the output. A rename on *Driver or a changed capability
// signature is therefore caught by the Linux CI job or by a local
// `GOOS=linux go vet ./...`, and NOT by a green `go test ./...` on a laptop.
func TestQEMUParity(t *testing.T) {
	vmmtest.RequireGate(t)
	cfg := loadParityConfig(t)

	vmmtest.Run(t, func(t *testing.T) *vmmtest.Fixture {
		// One scratch root per case, on the reflink-capable filesystem the
		// templates live on. Cross-filesystem is not a slow path here: the
		// driver refuses to fall back to a full 25 GiB copy, so getting this
		// wrong fails Create rather than quietly costing a minute a boot.
		// A per-case VMStateDir is right for the direct launcher and IMPOSSIBLE
		// under the helper: --vm-state-dir is fixed at the helper's startup, and
		// it holds that directory open as an O_PATH fd for its whole life, so
		// every path it resolves goes through openat2 relative to that inode.
		// A MkdirTemp per case would leave the helper resolving against a
		// deleted directory the moment the first case cleaned up — every
		// subsequent openState failing ENOENT, which reads like a missing
		// rootfs rather than like this.
		//
		// So helper runs share one directory. That is safe because sandbox
		// names are time-derived (vmmtest.uniq) and every case destroys what it
		// created; and nothing is removed at the end, because the Pod is the
		// thing that gets thrown away.
		root := cfg.scratch
		if !cfg.helped() {
			var err error
			root, err = os.MkdirTemp(cfg.scratch, "case-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) }) //nolint:errcheck
		}

		templateDir := filepath.Join(root, "templates")
		if err := os.MkdirAll(templateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		clientKey := newParitySigner(t, cfg)
		authorized := string(xssh.MarshalAuthorizedKey(clientKey.PublicKey()))

		// New() is called once per subtest against the same subnet, so slot
		// allocation restarts at zero every case and the tap names repeat.
		// That is safe only because the subtests are sequential and each box's
		// teardown Destroy has removed its tap before the next factory call —
		// do not add t.Parallel() to the suite.
		d, err := qemu.New(qemu.Options{
			KernelPath:  cfg.kernel,
			ImageDir:    cfg.imageDir,
			TemplateDir: templateDir,
			VMStateDir:  root,
			QemuBin:     cfg.qemuBin,
			MachineType: cfg.machineType,
			Subnet:      cfg.subnet,
			LoginUser:   cfg.loginUser,
			// All empty on the direct path, and New treats them as the switch
			// between the two launchers rather than as tuning.
			PrivilegedHelperSocket: cfg.helperSocket,
			PrivilegedHelperBin:    cfg.helperBin,
			JailerChrootBase:       cfg.helperChroot,
			HelperControllerGID:    int(cfg.helperGID),
			// Set for helper runs and only for them, which makes those runs the
			// FIRST parity coverage of the mode CKS actually deploys. It is not
			// a concession to the harness: the controller there is unprivileged
			// and cannot loop-mount a guest's ext4 at all, which is the whole
			// reason the flag exists.
			DisableHostRootfsMounts: cfg.helped(),
		})
		if err != nil {
			t.Fatalf("qemu.New: %v", err)
		}
		t.Cleanup(func() { d.Close() }) //nolint:errcheck

		return &vmmtest.Fixture{
			Driver:         d,
			BaseImage:      cfg.image,
			TemplatePrefix: "parity",
			VCPUs:          cfg.vcpus,
			MemMB:          cfg.memMB,
			AuthorizedKey:  authorized,
			Signer:         clientKey,
			BootTimeout:    cfg.bootTimeout,

			// Every trait below is a claim about this driver, and a false claim
			// is worse than a missing capability: it converts an assertion the
			// suite would have made into a log line nobody reads. Each one says
			// what it rests on and whether that is measured or read out of the
			// code, because none of it has been through vmmtest yet.
			Traits: vmmtest.Traits{
				// TRUE, and this is the one trait it would be tempting to hedge
				// on before a first hardware run. It should not be hedged.
				// Create boots the vmlinux and the universal.ext4 template we
				// ship, over a tap, with the same sparkbox_host and ip= kernel
				// args firecracker passes, and installs this fixture's key into
				// the rootfs (Options.DisableHostRootfsMounts is not set here).
				// docs/qemu-spike.md booted exactly that guest to SSH in 2.3s.
				// If it is not a real guest the suite fails at BootAndSSH
				// regardless — but setting this false ALSO deletes the traffic
				// half of NetStats, which is what proves both counters are
				// readable, move under load, and are not swapped: the suite
				// sends 32 MiB out of the guest against 4 MiB in, so
				// caps_vmm.go's rx/tx inversion is checked rather than merely
				// read.
				RealGuest: true,

				// TRUE. Pause is stop -> migrate uri=file: -> query-migrate to
				// completed -> quit -> reap -> rename, and Resume is the same
				// machine and device argv plus -incoming plus cont. The spike
				// ran that round trip and found a balloon inflated BEFORE the
				// snapshot still inflated after it, which is guest RAM
				// surviving rather than a disk being reopened.
				//
				// "The same argv" covers -append too, but only because boot
				// records the cold boot's guest command line and bootCmdline
				// replays it; rebuilt, a sandbox that cold-booted fresh would
				// restore one token short (sparkbox_fresh=1 is cold-boot-only).
				// The one case that still rebuilds is a restore of a record
				// carrying no command line, which nothing in-process can
				// produce, and it is harmless anyway: -kernel/-append seed
				// guest RAM at reset and the incoming stream overwrites it.
				//
				// Claiming it is also what arms the assertion this driver most
				// needs on the snapshot-shape trap: with PreservesMemory the
				// Rename case demands RenameVM ERROR while a memory snapshot
				// exists. firecracker's predicate stats the literal pair
				// mem.snap/state.snap; against QEMU's single state.migrate that
				// stat matches nothing and the refusal silently stops refusing.
				// RenameVM here goes through Driver.hasSnapshot instead, and
				// this trait is what checks that it really does.
				PreservesMemory: true,

				// TRUE. Snapshot's sanitize pass is lifted from firecracker
				// unchanged — it deletes every etc/ssh/ssh_host_* entry from
				// the staged template through os.OpenRoot, so the fork's first
				// boot regenerates its own — and it is gated on
				// !DisableHostRootfsMounts, which this fixture does not set.
				// Read out of the code rather than measured here; the code path
				// is byte-identical to the one the firecracker run exercises,
				// and if it is wrong a fork shares its parent's host key, which
				// is a defect worth failing on rather than skipping past.
				// ...and FALSE under the helper, because that same gate is what
				// turns the pass off. Claiming it there would assert a
				// host-key scrub that provably does not run.
				SanitizesForks: !cfg.helped(),

				// FALSE, and unlike the others this is not a hedge either — it
				// is a defect this driver INHERITS, not one it might have.
				// DiskUsageMB is firecracker's ext4DiskMB copied verbatim: it
				// reads s_free_blocks_count out of the rootfs image's primary
				// superblock, and Linux does not write that field back while
				// the filesystem is mounted. From the moment a guest boots
				// until it is next stopped, the number reported is the
				// TEMPLATE's. firecracker measured the size of the gap on the
				// same helper — a guest wrote 256 MiB and synced, moving its
				// own df by 273 MiB, while the driver's reading did not move at
				// all across four minutes of syncs and a pause — and nothing
				// about that measurement is Firecracker-specific.
				//
				// Setting it false skips nothing: DiskReport still writes
				// 64 MiB, still pauses, still re-reads, and still logs how far
				// the reading moved. Read that line on the first hardware run;
				// if it moved, this driver differs from firecracker somewhere
				// and the trait, not the log, is what is wrong.
				LiveDiskUsage: false,

				// TRUE, and derivable without hardware. instance() sets
				// Instance.HostIP to guestIP(st.idx) — the GUEST's address, per
				// driver.go's "reachable from the host for the VM's own
				// services" — freeSlot hands out distinct indices to concurrent
				// sandboxes, and guestnet puts slot i's guest at base+4i+2. Two
				// live sandboxes therefore cannot share a HostIP unless
				// freeSlot is broken, which is what TwoAtOnce is checking.
				DistinctHostIPs: true,

				// TRUE. TemplateUsageMB resolves through templatePath, which
				// looks in ImageDir first, and loadParityConfig has already
				// stat'd <imageDir>/<image>.ext4 — so the reporter has a real
				// file to measure and must return a non-zero size for it.
				BaseImageIsTemplate: true,
			},
		}
	})
}

type parityConfig struct {
	kernel, imageDir, image, scratch string
	qemuBin, machineType, subnet     string
	loginUser                        string
	vcpus, memMB                     int64
	bootTimeout                      time.Duration

	// The privileged-helper fixtures. Empty helperSocket is the DIRECT
	// LAUNCHER, which is what every parity run before this one exercised and
	// what a dev box still uses. Set, and this suite drives the same nineteen
	// cases through internal/vmhelper instead: the driver builds a command line
	// for nothing, a root process on the far side of a Unix socket builds the
	// argv and execs a self-confining QEMU, and every capability the suite
	// checks has to survive that indirection.
	helperSocket, helperBin, helperChroot string
	helperGID                             int64
	// sshKeyFile is a private key the runner has already installed into the
	// template. It exists because helper mode cannot inject one: the driver
	// does that by loop-mounting the rootfs, mount(8) refuses to set up a loop
	// device for anyone but root (MEASURED — ambient CAP_SYS_ADMIN and
	// CAP_DAC_OVERRIDE are not enough, on arm64 natively), and the helper
	// REFUSES a controller uid of 0. So helper runs set
	// DisableHostRootfsMounts, exactly as the CKS deployment does, and take
	// their key the way a real sandbox takes one: baked into the template.
	sshKeyFile string
}

// helped reports whether this run drives the privileged helper. It gates the
// three things that are genuinely different about that path, all of them in
// loadParityConfig or the fixture factory rather than scattered.
func (c parityConfig) helped() bool { return c.helperSocket != "" }

// loadParityConfig reads the fixtures from the environment. A missing one is
// fatal rather than a skip: the gate is already set, so the operator meant to
// run this, and a harness that silently declines to run is the thing we are
// replacing.
//
// The SPARKBOX_PARITY_* names are shared with the firecracker wiring on
// purpose, because the fixtures they name are the same artifacts: both drivers
// boot the same vmlinux and the same universal.ext4, and a run that compares
// the two backends wants them pointed at one set of inputs rather than two that
// can drift. SPARKBOX_PARITY_FIRECRACKER is the only name that does not carry
// over; SPARKBOX_PARITY_QEMU and SPARKBOX_PARITY_MACHINE_TYPE replace it.
//
// THE SUBNET IS THE EXCEPTION, and it has its own name and its own default for
// the same reason tapPrefix differs. Every other shared fixture is read-only
// (the kernel, the image dir) or per-case (VMStateDir is a MkdirTemp, the
// templates live under it); the subnet is mutable HOST-GLOBAL state. Both
// drivers' freeSlot starts at 0, so on the shared 172.31.0.0/24 slot 0 is
// 172.31.0.1/172.31.0.2 in both, and the differing tap prefixes are precisely
// what stops either sweep from cleaning the collision up. `go test ./...` on a
// gated Linux host runs the two packages concurrently (go test's default -p is
// GOMAXPROCS), Linux accepts the same host address on sbtap0 and sbqtap0, the
// route to 172.31.0.0/30 becomes ambiguous, and a dial reaches whichever guest
// the kernel picked — failing the per-fixture ed25519 handshake, so it surfaces
// as a red run in one package with its cause in the other. A separate default
// (and no fallback to the shared name, which would reintroduce it the moment
// someone exported one value for both) makes that impossible rather than
// documented.
//
// Both of those default to empty and let qemu.New decide, which is not laziness
// about the binary name: New resolves qemu-system-<arch> to an absolute path
// once so a later PATH change cannot swap the binary under a running fleet, and
// it refuses to guess a machine type on amd64 at all, because the migration
// stream is bound to the machine model and no x86_64 run has ever happened.
// Setting SPARKBOX_PARITY_MACHINE_TYPE is how the first x86_64 operator states
// the model their snapshots will be tied to.
//
// hack/parity/run-on-mac.sh cannot drive this suite unchanged: it builds
// ./internal/vmm/firecracker, greps for TestFirecrackerParity, and layers the
// test binary onto sparkbox-cks:dev, which has no QEMU in it at all. See
// docs/qemu-spike.md and hack/qemu-spike/Dockerfile for the image that does.
func loadParityConfig(t *testing.T) parityConfig {
	t.Helper()
	cfg := parityConfig{
		kernel:      vmmtest.MustEnv(t, "SPARKBOX_PARITY_KERNEL"),
		imageDir:    vmmtest.MustEnv(t, "SPARKBOX_PARITY_IMAGE_DIR"),
		scratch:     vmmtest.MustEnv(t, "SPARKBOX_PARITY_STATE_DIR"),
		image:       vmmtest.EnvOr("SPARKBOX_PARITY_IMAGE", "universal"),
		qemuBin:     os.Getenv("SPARKBOX_PARITY_QEMU"),
		machineType: os.Getenv("SPARKBOX_PARITY_MACHINE_TYPE"),
		subnet:      vmmtest.EnvOr("SPARKBOX_PARITY_QEMU_SUBNET", "172.31.1.0/24"),
		loginUser:   vmmtest.EnvOr("SPARKBOX_PARITY_LOGIN_USER", "root"),
		vcpus:       vmmtest.EnvInt(t, "SPARKBOX_PARITY_VCPUS", 2),
		memMB:       vmmtest.EnvInt(t, "SPARKBOX_PARITY_MEM_MB", 2048),
		bootTimeout: time.Duration(vmmtest.EnvInt(t, "SPARKBOX_PARITY_BOOT_TIMEOUT_S", 180)) * time.Second,

		helperSocket: os.Getenv("SPARKBOX_PARITY_HELPER_SOCKET"),
		helperBin:    os.Getenv("SPARKBOX_PARITY_HELPER_BIN"),
		helperChroot: os.Getenv("SPARKBOX_PARITY_HELPER_CHROOT_BASE"),
		helperGID:    vmmtest.EnvInt(t, "SPARKBOX_PARITY_HELPER_GID", 0),
		sshKeyFile:   os.Getenv("SPARKBOX_PARITY_SSH_KEY_FILE"),
	}
	// The /dev/kvm probe belongs to whoever is going to OPEN it, which under the
	// helper is not this process. On a hardened node the controller has no
	// device node at all — the device-plugin allocation belongs to the helper —
	// and the driver's own New() gates its probe the same way. A check here
	// would make the suite refuse to run in exactly the configuration it exists
	// to test.
	if !cfg.helped() {
		if _, err := os.Stat("/dev/kvm"); err != nil {
			t.Fatalf("%s=1 but /dev/kvm is not usable: %v", vmmtest.GateEnv, err)
		}
	}
	if cfg.helped() {
		for what, v := range map[string]string{
			"SPARKBOX_PARITY_HELPER_BIN":         cfg.helperBin,
			"SPARKBOX_PARITY_HELPER_CHROOT_BASE": cfg.helperChroot,
		} {
			if v == "" {
				t.Fatalf("SPARKBOX_PARITY_HELPER_SOCKET is set, so %s is required too", what)
			}
		}
		if cfg.helperGID < 1 {
			t.Fatal("SPARKBOX_PARITY_HELPER_SOCKET is set, so SPARKBOX_PARITY_HELPER_GID must name the group the helper shares VM files with")
		}
		if cfg.sshKeyFile == "" {
			t.Fatal("SPARKBOX_PARITY_HELPER_SOCKET is set, so SPARKBOX_PARITY_SSH_KEY_FILE is required: " +
				"helper runs disable host rootfs mounts, so the runner must have baked this key into the template")
		}
	}
	for _, p := range []string{cfg.kernel, filepath.Join(cfg.imageDir, cfg.image+".ext4")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("parity fixture missing: %v", err)
		}
	}
	if err := os.MkdirAll(cfg.scratch, 0o755); err != nil {
		t.Fatalf("parity scratch dir: %v", err)
	}
	launcher := "direct"
	if cfg.helped() {
		launcher = "privileged helper at " + cfg.helperSocket
	}
	t.Logf("parity fixtures: kernel=%s image=%s/%s.ext4 scratch=%s subnet=%s user=%s qemu=%s machine=%s %dvcpu %dMiB launcher=%s",
		cfg.kernel, cfg.imageDir, cfg.image, cfg.scratch, cfg.subnet, cfg.loginUser,
		vmmtest.EnvOr("SPARKBOX_PARITY_QEMU", "(New's default for this arch)"),
		effectiveMachineType(t, cfg),
		cfg.vcpus, cfg.memMB, launcher)
	return cfg
}

// effectiveMachineType is the machine model this run will actually boot.
//
// It exists because the log line used to print the ENV VALUE, with
// "(New's default for this arch)" standing in for the empty case — so the one
// field that decides whether a snapshot can ever be restored was the one field
// the log did not report. That is not a cosmetic gap: run-on-cks.sh compares
// this against the machine type the privileged helper logged, and a placeholder
// makes that comparison unable to pass.
//
// Resolved through qemuargs.DefaultMachineType, the same function New calls, so
// this reports New's answer rather than a second guess at it. It is deliberately
// NOT passed to New: letting New default is what makes the helper-mode
// comparison a test of two processes independently reaching the same value.
func effectiveMachineType(t *testing.T, cfg parityConfig) string {
	t.Helper()
	if cfg.machineType != "" {
		return cfg.machineType
	}
	machineType, err := qemuargs.DefaultMachineType()
	if err != nil {
		t.Fatalf("no default machine type: %v", err)
	}
	return machineType
}

// newParitySigner returns the key this run's guests will accept.
//
// The direct launcher generates a fresh one per case and the driver writes it
// into each rootfs. Helper mode cannot: see parityConfig.sshKeyFile. There the
// runner generated one keypair, installed the public half into the template
// before the suite started, and named the private half here — so every case
// shares a key, which is fine for what these cases assert (they check that the
// guest is reachable, not that keys are per-sandbox).
func newParitySigner(t *testing.T, cfg parityConfig) xssh.Signer {
	t.Helper()
	if cfg.sshKeyFile != "" {
		pem, err := os.ReadFile(cfg.sshKeyFile)
		if err != nil {
			t.Fatalf("parity ssh key: %v", err)
		}
		s, err := xssh.ParsePrivateKey(pem)
		if err != nil {
			t.Fatalf("parity ssh key %s: %v", cfg.sshKeyFile, err)
		}
		return s
	}
	return vmmtest.NewSigner(t)
}
