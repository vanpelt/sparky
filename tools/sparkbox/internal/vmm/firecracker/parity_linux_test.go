//go:build linux

package firecracker_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	fc "github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/firecracker"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/vmmtest"
)

// TestFirecrackerParity runs the driver-parity suite against real microVMs.
//
// This is the test the rest of the firecracker package does not have: its ~1,300
// lines of unit tests construct the Driver struct directly to avoid New()'s
// /dev/kvm requirement, and none of them ever boots a guest. Everything below
// boots one.
//
// It needs a Linux KVM host, a guest kernel, a rootfs template, and a
// reflink-capable filesystem to put VM state on. hack/parity/ arranges all four;
// see docs/vmm-parity-harness.md for where it runs.
//
// The gate is SPARKBOX_VMM_PARITY=1 and nothing else. Deliberately not a build
// tag: a tag keeps the file out of `go test ./...` by never compiling it, so it
// stops catching the signature drift that is half of what a parity suite is for.
// With an env gate, `go test ./...` on a LINUX checkout compiles every line here
// and skips at run time.
//
// It buys nothing on the arm64 Mac this project is developed on: this file, like
// the rest of the package, is //go:build linux, so `go test ./...` there omits
// the package entirely rather than compiling and skipping it. The Linux CI job
// (or `GOOS=linux go vet ./...`) is what actually catches signature drift.
// internal/vmm/qemu/parity_linux_test.go carries the same qualification.
func TestFirecrackerParity(t *testing.T) {
	vmmtest.RequireGate(t)
	cfg := loadParityConfig(t)

	vmmtest.Run(t, func(t *testing.T) *vmmtest.Fixture {
		// One scratch root per case, on the reflink-capable filesystem the
		// templates live on. Cross-filesystem is not a slow path here: the
		// driver refuses to fall back to a full 25 GiB copy, so getting this
		// wrong fails Create rather than quietly costing a minute a boot.
		root, err := os.MkdirTemp(cfg.scratch, "case-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(root) }) //nolint:errcheck

		templateDir := filepath.Join(root, "templates")
		if err := os.MkdirAll(templateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		clientKey := newParitySigner(t)
		authorized := string(xssh.MarshalAuthorizedKey(clientKey.PublicKey()))

		d, err := fc.New(fc.Options{
			KernelPath:     cfg.kernel,
			ImageDir:       cfg.imageDir,
			TemplateDir:    templateDir,
			VMStateDir:     root,
			FirecrackerBin: cfg.firecrackerBin,
			Subnet:         cfg.subnet,
			LoginUser:      cfg.loginUser,
		})
		if err != nil {
			t.Fatalf("firecracker.New: %v", err)
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
			Traits: vmmtest.Traits{
				RealGuest:           true,
				PreservesMemory:     true,
				SanitizesForks:      true,
				DistinctHostIPs:     true,
				BaseImageIsTemplate: true,
				// LiveDiskUsage is FALSE, and that is a measurement rather than
				// caution. DiskUsageMB reads s_free_blocks_count out of the
				// rootfs image's ext4 superblock, and Linux does not write that
				// field back for a mounted filesystem: a guest that wrote
				// 256 MiB and ran `sync` moved its own `df` by 273 MiB while the
				// driver's reading did not move at all, across four minutes of
				// repeated syncs and a pause. The number it reports is the
				// template's, from before the sandbox ever booted. See
				// docs/vmm-parity-harness.md.
				LiveDiskUsage: false,
			},
		}
	})
}

type parityConfig struct {
	kernel, imageDir, image, scratch string
	firecrackerBin, subnet           string
	loginUser                        string
	vcpus, memMB                     int64
	bootTimeout                      time.Duration
}

// loadParityConfig reads the fixtures from the environment. A missing one is
// fatal rather than a skip: the gate is already set, so the operator meant to
// run this, and a harness that silently declines to run is the thing we are
// replacing.
func loadParityConfig(t *testing.T) parityConfig {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Fatalf("%s=1 but /dev/kvm is not usable: %v", vmmtest.GateEnv, err)
	}
	cfg := parityConfig{
		kernel:         mustEnv(t, "SPARKBOX_PARITY_KERNEL"),
		imageDir:       mustEnv(t, "SPARKBOX_PARITY_IMAGE_DIR"),
		scratch:        mustEnv(t, "SPARKBOX_PARITY_STATE_DIR"),
		image:          envOr("SPARKBOX_PARITY_IMAGE", "universal"),
		firecrackerBin: envOr("SPARKBOX_PARITY_FIRECRACKER", "firecracker"),
		subnet:         envOr("SPARKBOX_PARITY_SUBNET", "172.31.0.0/24"),
		loginUser:      envOr("SPARKBOX_PARITY_LOGIN_USER", "root"),
		vcpus:          envInt(t, "SPARKBOX_PARITY_VCPUS", 2),
		memMB:          envInt(t, "SPARKBOX_PARITY_MEM_MB", 2048),
		bootTimeout:    time.Duration(envInt(t, "SPARKBOX_PARITY_BOOT_TIMEOUT_S", 180)) * time.Second,
	}
	for _, p := range []string{cfg.kernel, filepath.Join(cfg.imageDir, cfg.image+".ext4")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("parity fixture missing: %v", err)
		}
	}
	if err := os.MkdirAll(cfg.scratch, 0o755); err != nil {
		t.Fatalf("parity scratch dir: %v", err)
	}
	t.Logf("parity fixtures: kernel=%s image=%s/%s.ext4 scratch=%s subnet=%s user=%s %dvcpu %dMiB",
		cfg.kernel, cfg.imageDir, cfg.image, cfg.scratch, cfg.subnet, cfg.loginUser, cfg.vcpus, cfg.memMB)
	return cfg
}

func mustEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s=1 requires %s", vmmtest.GateEnv, key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(t *testing.T, key string, fallback int64) int64 {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("%s=%q: %v", key, v, err)
	}
	return n
}

func newParitySigner(t *testing.T) xssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
