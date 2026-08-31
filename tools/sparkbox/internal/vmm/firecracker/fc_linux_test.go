//go:build linux

package firecracker

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	sdk "github.com/firecracker-microvm/firecracker-go-sdk"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Compile-time capability checks, mirroring the mock's: the manager
// type-asserts for these, so losing one silently degrades the fleet.
var (
	_ vmm.Renamer          = (*Driver)(nil)
	_ vmm.Rebooter         = (*Driver)(nil)
	_ vmm.CPUStatser       = (*Driver)(nil)
	_ vmm.TemplateReporter = (*Driver)(nil)
)

// TestV6Addressing checks the per-slot /127 carving from the delegated /64.
// Constructs the Driver directly to avoid New()'s /dev/kvm requirement.
func TestV6Addressing(t *testing.T) {
	_, ipNet, err := net.ParseCIDR("2001:bc8:702:1c7::/64")
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{prefix6: ipNet.IP.To16()}

	cases := []struct {
		idx         int
		host, guest string
	}{
		{0, "2001:bc8:702:1c7::2", "2001:bc8:702:1c7::3"},
		{1, "2001:bc8:702:1c7::4", "2001:bc8:702:1c7::5"},
		{255, "2001:bc8:702:1c7::200", "2001:bc8:702:1c7::201"},
	}
	for _, c := range cases {
		if got := d.hostIP6(c.idx); got != c.host {
			t.Errorf("idx %d hostIP6 = %s, want %s", c.idx, got, c.host)
		}
		if got := d.guestIP6(c.idx); got != c.guest {
			t.Errorf("idx %d guestIP6 = %s, want %s", c.idx, got, c.guest)
		}
		// host must be the even (network) address of the /127, guest the odd one.
		if hi := net.ParseIP(c.host).To16()[15]; hi%2 != 0 {
			t.Errorf("idx %d host address not /127-aligned: %s", c.idx, c.host)
		}
	}
}

// newTestDriver constructs the Driver directly (avoiding New()'s /dev/kvm
// requirement) over a temp state dir, for the capabilities that are pure
// filesystem work.
func newTestDriver(t *testing.T) *Driver {
	t.Helper()
	return &Driver{
		opts: Options{VMStateDir: t.TempDir()}, vms: map[string]*vmState{},
		guestNet: guestnet.MustParse(""),
	}
}

func TestVMDirUsesVMStateDir(t *testing.T) {
	hot := t.TempDir()
	d := &Driver{opts: Options{VMStateDir: hot}}
	if got, want := d.vmDir("box"), filepath.Join(hot, "fc-vms", "box"); got != want {
		t.Errorf("vmDir = %q, want %q", got, want)
	}
}

func TestJailerUsesSlotScopedIdentityAndWorkspace(t *testing.T) {
	base := t.TempDir()
	d := &Driver{opts: Options{
		FirecrackerBin:   "/opt/sparkbox/firecracker",
		JailerBin:        "/opt/sparkbox/jailer",
		JailerChrootBase: base,
		JailerUIDBase:    100000,
	}}
	if got, want := d.jailUID(7), 100007; got != want {
		t.Fatalf("jail uid = %d, want %d", got, want)
	}
	if got, want := d.jailRoot(7),
		filepath.Join(base, "firecracker", "sparkbox-7", "root"); got != want {
		t.Fatalf("jail root = %q, want %q", got, want)
	}
}

func TestChrootJailerUsesCurrentMountNamespaceAndClearsIdentity(t *testing.T) {
	cmd := chrootProcess(context.Background(), "/jails/firecracker/box/root", "firecracker", 100007)
	if got, want := cmd.Path, "/firecracker"; got != want {
		t.Fatalf("command path = %q, want %q", got, want)
	}
	if got, want := cmd.SysProcAttr.Chroot, "/jails/firecracker/box/root"; got != want {
		t.Fatalf("chroot = %q, want %q", got, want)
	}
	cred := cmd.SysProcAttr.Credential
	if cred == nil || cred.Uid != 100007 || cred.Gid != 100007 {
		t.Fatalf("credential = %+v", cred)
	}
	if len(cred.Groups) != 1 || cred.Groups[0] != 100007 {
		t.Fatalf("supplementary groups = %v, want only slot uid", cred.Groups)
	}
	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("environment = %v, want explicitly empty", cmd.Env)
	}
	if cmd.SysProcAttr.Cloneflags != 0 || cmd.SysProcAttr.Unshareflags != 0 {
		t.Fatalf("chroot launcher requested a namespace: clone=%#x unshare=%#x",
			cmd.SysProcAttr.Cloneflags, cmd.SysProcAttr.Unshareflags)
	}
}

func TestCopyJailExecutableRejectsNonRegularSource(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "firecracker")
	if err := copyJailExecutable(t.TempDir(), destination); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory executable error = %v", err)
	}
}

func TestCopyJailExecutablePinsReadOnlyMode(t *testing.T) {
	source := filepath.Join(t.TempDir(), "firecracker")
	if err := os.WriteFile(source, []byte("static-vmm"), 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "firecracker")
	if err := copyJailExecutable(source, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o555); got != want {
		t.Fatalf("jailed executable mode = %o, want %o", got, want)
	}
}

func TestChrootAndExternalJailersAreMutuallyExclusive(t *testing.T) {
	_, err := New(Options{JailerBin: "/jailer", ChrootJailer: true})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("mixed jailers error = %v", err)
	}
}

// Mount-free mode no longer refuses a capture outright.
//
// It used to, and that refusal was the whole reason tag templates were inert on
// CKS. The mount was only ever needed by sanitizeTemplate — reflink, e2fsck and
// zerofree all work on the image file — and the guest-side pre-pack hook now
// clears the machine identity and journal before the pause, while the fork's
// first-boot reset remains a backstop for SSH identity. So the capture proceeds
// and the one step that would mount is skipped.
//
// The assertion is about which error comes back, because this driver has no
// stopped VM named "box" and must fail for THAT reason. The refusal sentence
// coming back instead would mean the guard is still in the path. The rest —
// that a real capture on a mountless host produces a bootable template — is a
// cluster proof, not a unit one. See docs/cks-snapshot-design.md.
func TestHostMountFreeModeNoLongerRefusesTemplateSnapshot(t *testing.T) {
	d := newTestDriver(t)
	d.opts.DisableHostRootfsMounts = true
	err := d.Snapshot(context.Background(), "box", "fork")
	if err == nil {
		t.Fatal("snapshot of a sandbox this driver does not have succeeded")
	}
	if strings.Contains(err.Error(), "host rootfs mounts are disabled") {
		t.Fatalf("snapshot still refused for want of a mount: %v", err)
	}
}

func TestJailerPairMustMatch(t *testing.T) {
	dir := t.TempDir()
	fake := func(name, version string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+version+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	firecracker := fake("firecracker", "Firecracker v1.16.1")
	jailer := fake("jailer", "Jailer v1.16.1")
	if err := validateJailerPair(firecracker, jailer); err != nil {
		t.Fatal(err)
	}
	oldJailer := fake("old-jailer", "Jailer v1.15.0")
	if err := validateJailerPair(firecracker, oldJailer); err == nil ||
		!strings.Contains(err.Error(), "do not match") {
		t.Fatalf("mismatched pair error = %v", err)
	}
}

func TestLinkJailedResourceRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "kernel")
	if err := os.WriteFile(source, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkJailedResource(root, os.Getuid(), source, "vmlinux", false); err != nil {
		t.Fatal(err)
	}
	linked, err := os.ReadFile(filepath.Join(root, "vmlinux"))
	if err != nil || string(linked) != "kernel" {
		t.Fatalf("linked kernel = %q, %v", linked, err)
	}
	for _, path := range []string{source, filepath.Join(root, "vmlinux")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := info.Mode().Perm(), os.FileMode(0o444); got != want {
			t.Errorf("mode of %s = %o, want %o", path, got, want)
		}
	}

	symlink := filepath.Join(dir, "symlink")
	if err := os.Symlink(source, symlink); err != nil {
		t.Fatal(err)
	}
	if err := linkJailedResource(root, os.Getuid(), symlink, "bad", false); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink jail resource error = %v", err)
	}
}

func TestUnpackRootfsFailurePreservesExistingDisk(t *testing.T) {
	d := newTestDriver(t)
	dir := d.vmDir("box")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("original disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(t.TempDir(), "checkpoint.ext4.zst")
	if err := os.WriteFile(in, []byte("not important to fake zstd"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fakeZstd := filepath.Join(bin, "zstd")
	script := `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then out=$2; shift 2; continue; fi
  shift
done
printf 'partial restored disk' > "$out"
exit 1
`
	if err := os.WriteFile(fakeZstd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	if err := d.UnpackRootfs(context.Background(), "box", in); err == nil {
		t.Fatal("UnpackRootfs with failing zstd should fail")
	}
	got, err := os.ReadFile(rootfs)
	if err != nil || string(got) != "original disk" {
		t.Fatalf("failed restore changed rootfs to %q, %v", got, err)
	}
}

func TestReflinkCloneRequiresAlways(t *testing.T) {
	binDir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "args")
	fakeCP := filepath.Join(binDir, "cp")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SPARKBOX_CP_ARGS\"\n: > \"$3\"\n"
	if err := os.WriteFile(fakeCP, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("SPARKBOX_CP_ARGS", argsPath)

	source := filepath.Join(t.TempDir(), "source.ext4")
	destination := filepath.Join(t.TempDir(), "destination.ext4")
	if err := reflinkClone(context.Background(), source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "--reflink=always\n" + source + "\n" + destination + "\n"
	if string(got) != want {
		t.Fatalf("cp args = %q, want %q", got, want)
	}

	// cp may create its destination before discovering that FICLONE cannot work.
	// A retry must not mistake that torn file for a complete pre-existing disk.
	failureScript := "#!/bin/sh\nprintf partial > \"$3\"\necho no-reflink >&2\nexit 1\n"
	if err := os.WriteFile(fakeCP, []byte(failureScript), 0o755); err != nil {
		t.Fatal(err)
	}
	err = reflinkClone(context.Background(), source, destination)
	if err == nil || !strings.Contains(err.Error(), "no-reflink") {
		t.Fatalf("failed clone error = %v, want cp diagnostic", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed clone left destination behind: %v", err)
	}
}

func TestGuestSubnetAddressing(t *testing.T) {
	d := newTestDriver(t)
	d.guestNet = guestnet.MustParse("10.44.16.9/20")

	tests := []struct {
		idx         int
		host, guest string
	}{
		{0, "10.44.16.1", "10.44.16.2"},
		{1, "10.44.16.5", "10.44.16.6"},
		{1023, "10.44.31.253", "10.44.31.254"},
	}
	for _, test := range tests {
		if got := d.hostIP(test.idx); got != test.host {
			t.Errorf("hostIP(%d) = %s, want %s", test.idx, got, test.host)
		}
		if got := d.guestIP(test.idx); got != test.guest {
			t.Errorf("guestIP(%d) = %s, want %s", test.idx, got, test.guest)
		}
	}
}

func TestNewRejectsGuestSubnetLargerThanMACSpace(t *testing.T) {
	if _, err := New(Options{Subnet: "10.0.0.0/13"}); err == nil ||
		!strings.Contains(err.Error(), "at most 65536") {
		t.Fatalf("New broad guest subnet error = %v", err)
	}
}

func TestNewRejectsIPv6PrefixTooSmallForGuestSlots(t *testing.T) {
	if _, err := New(Options{
		Subnet: "10.0.0.0/14", Subnet6: "2001:db8::/112",
	}); err == nil || !strings.Contains(err.Error(), "needs at least 131074") {
		t.Fatalf("New mismatched IPv4/IPv6 capacity error = %v", err)
	}
}

// touch creates name's VM dir containing the given files.
func touch(t *testing.T, d *Driver, name string, files ...string) {
	t.Helper()
	dir := d.vmDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRootfsLoginIdentity(t *testing.T) {
	passwd := []byte("root:x:0:0:root:/root:/bin/bash\nsparky:x:1000:1001::/home/sparky:/bin/bash\n")

	got, err := rootfsLoginIdentity(passwd, "sparky")
	if err != nil {
		t.Fatal(err)
	}
	if got.home != "/home/sparky" || got.uid != 1000 || got.gid != 1001 {
		t.Fatalf("identity = %+v", got)
	}

	got, err = rootfsLoginIdentity(passwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.home != "/root" || got.uid != 0 || got.gid != 0 {
		t.Fatalf("default identity = %+v", got)
	}

	for _, tc := range []struct {
		name   string
		passwd string
		user   string
	}{
		{"missing user", string(passwd), "nobody"},
		{"bad uid", "sparky:x:nope:1000::/home/sparky:/bin/sh\n", "sparky"},
		{"unsafe home", "sparky:x:1000:1000::/:/bin/sh\n", "sparky"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := rootfsLoginIdentity([]byte(tc.passwd), tc.user); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestInstallAuthorizedKeyReportsParseFailure(t *testing.T) {
	err := installAuthorizedKey(context.Background(), "/not-mounted", "sparky", "not-an-ssh-key")
	if err == nil {
		t.Fatal("expected invalid gateway key to fail before mounting")
	}
	if !strings.Contains(err.Error(), "ssh:") {
		t.Fatalf("error %q does not preserve the SSH parser reason", err)
	}
}

// fakeRootfs builds an unmounted stand-in for a guest rootfs: just the
// /etc/passwd that writeAuthorizedKey reads, with the login user owned by
// whoever runs the test so the chowns succeed unprivileged.
func fakeRootfs(t *testing.T) string {
	t.Helper()
	mnt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mnt, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("sparky:x:%d:%d::/home/sparky:/bin/bash\n", os.Getuid(), os.Getgid())
	if err := os.WriteFile(filepath.Join(mnt, "etc", "passwd"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return mnt
}

const testGuestKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHqQ0kZ3vJZmZ1TzjJ4mUq8kMuQm/kZPq6ZQ0mVUOwvE gateway"

// A guest owns its own rootfs, so it can replace ~/.ssh with a symlink and try
// to steer the root-owned gateway's next cold-boot write onto the host. It must
// not land outside the image.
func TestWriteAuthorizedKeyRefusesSymlinkEscape(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"absolute", ""}, // filled in below with a path outside the rootfs
		{"relative climb", "../../../../../../etc/ssh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mnt := fakeRootfs(t)
			outside := filepath.Join(t.TempDir(), "host-etc-ssh")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			target := tc.target
			if target == "" {
				target = outside
			}
			if err := os.MkdirAll(filepath.Join(mnt, "home", "sparky"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(mnt, "home", "sparky", ".ssh")); err != nil {
				t.Fatal(err)
			}

			if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
				t.Fatalf("writeAuthorizedKey: %v", err)
			}

			// Nothing outside the rootfs was created or touched.
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Fatalf("escaped the rootfs: %v entries=%v", err, entries)
			}
			// The planted symlink was replaced by a real directory holding the key.
			sshDir := filepath.Join(mnt, "home", "sparky", ".ssh")
			fi, err := os.Lstat(sshDir)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				t.Fatalf(".ssh mode = %v, want a real directory", fi.Mode())
			}
			got, err := os.ReadFile(filepath.Join(sshDir, "authorized_keys"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != testGuestKey {
				t.Fatalf("authorized_keys = %q", got)
			}
		})
	}
}

// Create is re-run on an existing rootfs whenever a resume fails, so keys the
// user added inside their own sandbox have to survive the cold boot.
func TestWriteAuthorizedKeyPreservesUserKeys(t *testing.T) {
	mnt := fakeRootfs(t)
	sshDir := filepath.Join(mnt, "home", "sparky", ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const laptop = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIP3Yd0ZGPdmZLQAKk8xLmqL/9Zr5rWQNqjRLYh7lHcVn laptop"
	// The gateway key is already present under a different comment — it must be
	// recognised as the same key and not duplicated.
	prior := laptop + "\n" + strings.TrimSuffix(testGuestKey, " gateway") + " stale-comment\n"
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(sshDir, "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	want := laptop + "\n" + testGuestKey + "\n"
	if string(got) != want {
		t.Fatalf("authorized_keys =\n%q\nwant\n%q", got, want)
	}
}

// A rootfs whose login user has no home yet must come out with one the user
// owns; sshd's StrictModes rejects pubkey auth on a root-owned home, and the
// only symptom is an SSH attach that hangs.
func TestWriteAuthorizedKeyCreatesOwnedHome(t *testing.T) {
	mnt := fakeRootfs(t)
	if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		perm os.FileMode
	}{
		{filepath.Join(mnt, "home", "sparky"), 0o755},
		{filepath.Join(mnt, "home", "sparky", ".ssh"), 0o700},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.IsDir() || fi.Mode().Perm() != tc.perm {
			t.Fatalf("%s mode = %v, want dir %v", tc.path, fi.Mode(), tc.perm)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("%s: no stat", tc.path)
		}
		if int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
			t.Fatalf("%s owned by %d:%d, want the login user %d:%d",
				tc.path, st.Uid, st.Gid, os.Getuid(), os.Getgid())
		}
	}
}

func TestDiskUsageReadsExt4UsedBlocks(t *testing.T) {
	d := newTestDriver(t)
	dir := d.vmDir("box")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := d.rootfsPath("box")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Match the live macOS PoC geometry: a 25 GiB filesystem with only about
	// 2.9 GiB in use. Materialize the outer file sparsely to prove the result
	// comes from ext4 counters rather than host allocation.
	const blocks = uint32(6_553_600)
	const free = uint32(5_789_327)
	const overhead = uint32(146_887)
	if err := f.Truncate(int64(blocks) * 4096); err != nil {
		t.Fatal(err)
	}
	sb := make([]byte, 1024)
	binary.LittleEndian.PutUint32(sb[0x04:0x08], blocks)
	binary.LittleEndian.PutUint32(sb[0x0c:0x10], free)
	binary.LittleEndian.PutUint32(sb[0x18:0x1c], 2) // 1024 << 2 = 4096
	binary.LittleEndian.PutUint16(sb[0x38:0x3a], 0xef53)
	binary.LittleEndian.PutUint32(sb[0x248:0x24c], overhead)
	if _, err := f.WriteAt(sb, 1024); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := d.DiskUsageMB(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	const want = int64(2_411)
	if got != want {
		t.Fatalf("DiskUsageMB = %d, want %d", got, want)
	}
	// Capacity excludes the same metadata overhead usage does, so the meter's
	// two halves share a basis: 25 GiB of image, minus 146887 overhead blocks,
	// is the 25026 MiB `df` reports inside the guest — not the raw 25600.
	capacity, err := d.DiskCapacityMB(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if capacity != 25_026 {
		t.Fatalf("DiskCapacityMB = %d, want 25026", capacity)
	}
	if got > capacity {
		t.Fatalf("usage %d exceeds capacity %d", got, capacity)
	}
}

// A full filesystem has to read as full: with no free blocks, usage and
// capacity must land on the same number or the console meter can never reach
// 100%.
func TestDiskUsageReachesCapacityWhenFull(t *testing.T) {
	d := newTestDriver(t)
	if err := os.MkdirAll(d.vmDir("box"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExt4Superblock(t, d.rootfsPath("box"), 6_553_600, 0, 146_887)

	used, err := d.DiskUsageMB(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := d.DiskCapacityMB(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if used != capacity {
		t.Fatalf("full filesystem reports %d/%d MB, want them equal", used, capacity)
	}
}

func TestExt4DiskRejectsInvalidSuperblock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-ext4")
	if err := os.WriteFile(path, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ext4DiskMB(path); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("ext4DiskMB error = %v, want invalid-magic diagnostic", err)
	}
}

// TestTemplateUsageMB pins the three properties the pooled-disk baseline rests
// on: the template figure is produced by the same superblock read DiskUsageMB
// uses (so the manager may subtract one from the other), a template that isn't
// there is an error rather than a silent zero (so a deleted snapshot keeps every
// fork's stored baseline instead of spiking its charge), and a name out of a
// persisted record cannot walk out of ImageDir.
func TestTemplateUsageMB(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	imageDir := filepath.Join(parent, "images")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	d := newTestDriver(t)
	d.opts.ImageDir = imageDir

	// Identical geometry on both sides: a template in the image dir and a
	// sandbox rootfs. If the two ever stopped sharing ext4DiskMB, subtracting
	// them would silently start comparing different bases.
	const blocks, free, overhead = uint32(6_553_600), uint32(5_789_327), uint32(146_887)
	writeExt4Superblock(t, filepath.Join(imageDir, "snap-alice-cuda.ext4"), blocks, free, overhead)
	if err := os.MkdirAll(d.vmDir("box"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExt4Superblock(t, d.rootfsPath("box"), blocks, free, overhead)

	base, err := d.TemplateUsageMB(ctx, "snap-alice-cuda")
	if err != nil {
		t.Fatalf("TemplateUsageMB: %v", err)
	}
	used, err := d.DiskUsageMB(ctx, "box")
	if err != nil {
		t.Fatal(err)
	}
	if base != used || base == 0 {
		t.Fatalf("TemplateUsageMB = %d, DiskUsageMB = %d; want the same non-zero figure", base, used)
	}

	if _, err := d.TemplateUsageMB(ctx, "snap-alice-gone"); err == nil {
		t.Fatal("a missing template must be an error, not 0 — the manager keeps its last baseline on error")
	}

	// A traversal name is refused before any filesystem access: plant the file a
	// naive join would reach and prove its figure never comes back.
	writeExt4Superblock(t, filepath.Join(parent, "escape.ext4"), blocks, free, overhead)
	got, err := d.TemplateUsageMB(ctx, "../escape")
	if err == nil || !strings.Contains(err.Error(), "invalid template image name") {
		t.Fatalf("TemplateUsageMB(%q) = %d, %v; want an image-name refusal", "../escape", got, err)
	}
}

// writeExt4Superblock lays down a sparse image carrying just the superblock
// fields the disk accounting reads, with 4 KiB blocks.
func writeExt4Superblock(t *testing.T, path string, blocks, free, overhead uint32) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	if err := f.Truncate(int64(blocks) * 4096); err != nil {
		t.Fatal(err)
	}
	sb := make([]byte, 1024)
	binary.LittleEndian.PutUint32(sb[0x04:0x08], blocks)
	binary.LittleEndian.PutUint32(sb[0x0c:0x10], free)
	binary.LittleEndian.PutUint32(sb[0x18:0x1c], 2) // 1024 << 2 = 4096
	binary.LittleEndian.PutUint16(sb[0x38:0x3a], 0xef53)
	binary.LittleEndian.PutUint32(sb[0x248:0x24c], overhead)
	if _, err := f.WriteAt(sb, 1024); err != nil {
		t.Fatal(err)
	}
}

func TestDropSnapshots(t *testing.T) {
	d := newTestDriver(t)
	touch(t, d, "box", "rootfs.ext4", "mem.snap", "state.snap")
	d.vms["box"] = &vmState{idx: 1, paused: true}

	if err := d.DropSnapshots("box"); err != nil {
		t.Fatal(err)
	}
	dir := d.vmDir("box")
	for _, f := range []string{"mem.snap", "state.snap"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s still present: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "rootfs.ext4")); err != nil {
		t.Errorf("rootfs removed: %v", err)
	}
	// The paused record must be gone so resumeOrRecreate's Create cold-boots
	// instead of tripping the already-exists check.
	if _, ok := d.vms["box"]; ok {
		t.Error("driver record not dropped")
	}

	// Idempotent: no snapshots, no record — still fine.
	if err := d.DropSnapshots("box"); err != nil {
		t.Fatal(err)
	}
}

func TestDropSnapshotsRefusesRunning(t *testing.T) {
	d := newTestDriver(t)
	touch(t, d, "box", "mem.snap", "state.snap")
	d.vms["box"] = &vmState{idx: 1, machine: &sdk.Machine{}}
	if err := d.DropSnapshots("box"); err == nil {
		t.Fatal("expected error on running vm")
	}
}

func TestRenameVMRefusesWithSnapshot(t *testing.T) {
	d := newTestDriver(t)
	// state.snap embeds absolute paths into the old dir; a rename would leave a
	// resume pointing at a dir that no longer exists.
	touch(t, d, "old", "rootfs.ext4", "state.snap")
	d.vms["old"] = &vmState{idx: 1, paused: true}
	err := d.RenameVM("old", "new")
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("expected snapshot-refusal error, got %v", err)
	}
	if _, err := os.Stat(d.vmDir("old")); err != nil {
		t.Fatalf("old dir moved despite refusal: %v", err)
	}
}

func TestRenameVM(t *testing.T) {
	d := newTestDriver(t)
	touch(t, d, "old", "rootfs.ext4")
	d.vms["old"] = &vmState{idx: 1, paused: true}

	if err := d.RenameVM("old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.vmDir("old")); !os.IsNotExist(err) {
		t.Errorf("old dir still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.vmDir("new"), "rootfs.ext4")); err != nil {
		t.Errorf("rootfs missing after rename: %v", err)
	}
	if _, ok := d.vms["old"]; ok {
		t.Error("old driver record not dropped")
	}
	if _, ok := d.vms["new"]; ok {
		t.Error("rename must leave the post-restart shape, not rekey the record")
	}
}

func TestRenameVMRefusals(t *testing.T) {
	d := newTestDriver(t)
	touch(t, d, "old", "rootfs.ext4")
	d.vms["old"] = &vmState{idx: 1, machine: &sdk.Machine{}}
	if err := d.RenameVM("old", "new"); err == nil {
		t.Fatal("expected error renaming a running vm")
	}
	d.vms["old"].machine = nil
	d.vms["old"].paused = true

	touch(t, d, "taken", "rootfs.ext4")
	if err := d.RenameVM("old", "taken"); err == nil {
		t.Fatal("expected error renaming onto an existing vm dir")
	}
	d.vms["live"] = &vmState{idx: 2, paused: true}
	if err := d.RenameVM("old", "live"); err == nil {
		t.Fatal("expected error renaming onto an existing vm record")
	}
}

// TestFreeSlotReuse checks that slot allocation reclaims indices released by
// dropped records — Destroy, DropSnapshots (every reboot), and RenameVM all
// delete the vmState, and a monotonic counter would burn a slot each time
// until Create failed with an address outside the configured prefix.
func TestFreeSlotReuse(t *testing.T) {
	d := newTestDriver(t)
	d.vms["a"] = &vmState{idx: 0, paused: true}
	d.vms["c"] = &vmState{idx: 2, paused: true}

	// Lowest free slot, including gaps between live records.
	if idx, err := d.freeSlot(); err != nil || idx != 1 {
		t.Fatalf("freeSlot = %d, %v; want 1", idx, err)
	}

	// Dropping a record (here via DropSnapshots, the reboot path) releases its
	// slot for the next Create.
	touch(t, d, "a", "rootfs.ext4")
	if err := d.DropSnapshots("a"); err != nil {
		t.Fatal(err)
	}
	if idx, err := d.freeSlot(); err != nil || idx != 0 {
		t.Fatalf("freeSlot after DropSnapshots = %d, %v; want 0", idx, err)
	}

	// RenameVM likewise.
	touch(t, d, "c", "rootfs.ext4")
	if err := d.RenameVM("c", "c2"); err != nil {
		t.Fatal(err)
	}
	d.vms["a"] = &vmState{idx: 0, paused: true}
	d.vms["b"] = &vmState{idx: 1, paused: true}
	if idx, err := d.freeSlot(); err != nil || idx != 2 {
		t.Fatalf("freeSlot after RenameVM = %d, %v; want 2", idx, err)
	}

	// Exhaustion must error rather than mint an out-of-range address.
	d.guestNet = guestnet.MustParse("192.0.2.0/28")
	d.vms = map[string]*vmState{}
	for i := 0; i < d.guestNet.Capacity(); i++ {
		d.vms[fmt.Sprintf("v%d", i)] = &vmState{idx: i, paused: true}
	}
	if _, err := d.freeSlot(); err == nil {
		t.Fatal("expected error with every slot in use")
	}
}

func TestFreeSlotSkipsHostServiceReservation(t *testing.T) {
	d := newTestDriver(t)
	d.guestNet = guestnet.MustParse("10.44.16.0/20")
	d.reservedSlots = map[int]bool{0: true, 2: true}
	d.vms["middle"] = &vmState{idx: 1, paused: true}

	if idx, err := d.freeSlot(); err != nil || idx != 3 {
		t.Fatalf("freeSlot = %d, %v; want first unreserved slot 3", idx, err)
	}
}

func TestProcStatCPUTicks(t *testing.T) {
	// comm ("fire cr) acker") contains a space and a ')': fields must be
	// counted from the LAST ')'. utime=150, stime=25 (fields 14/15).
	line := "1234 (fire cr) acker) S 10 10 10 0 -1 4194560 500 0 0 0 150 25 12 3 20 0 4 0 100000 0 0"
	got, err := procStatCPUTicks(line)
	if err != nil {
		t.Fatal(err)
	}
	if got != 175 {
		t.Errorf("ticks = %d, want 175", got)
	}

	for _, bad := range []string{
		"no closing paren",
		"1234 (fc) S 10 10", // too few fields after comm
	} {
		if _, err := procStatCPUTicks(bad); err == nil {
			t.Errorf("procStatCPUTicks(%q) accepted malformed input", bad)
		}
	}
}

// A guest may legitimately keep a 0750 home; installing the gateway key is not
// a reason to widen it. ~/.ssh is the deliberate exception — sshd ignores
// authorized_keys unless that directory is the login user's and private.
func TestWriteAuthorizedKeyLeavesExistingHomeModeAlone(t *testing.T) {
	mnt := fakeRootfs(t)
	home := filepath.Join(mnt, "home", "sparky")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 {
		t.Fatalf("home mode = %v, want the guest's own 0750 untouched", fi.Mode().Perm())
	}
	if fi, err = os.Stat(filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf(".ssh mode = %v, want 0700 for sshd StrictModes", fi.Mode().Perm())
	}
}
