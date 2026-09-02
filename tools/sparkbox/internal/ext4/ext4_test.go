package ext4

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireTools skips a test that needs e2fsprogs rather than failing it, so the
// package still tests cleanly on a workstation without it. The build path this
// package exists for always has e2fsprogs.
func requireTools(t *testing.T) {
	t.Helper()
	if err := Available(); err != nil {
		t.Skipf("skipping: %v", err)
	}
}

// tree builds a small rootfs-shaped directory and returns its path. The shapes
// here are the ones a real image depends on surviving the build.
func tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdirAll(t, root, "etc", "usr/local/sbin", "home/sparky/.ssh")
	write(t, filepath.Join(root, "etc/hostname"), "", 0o644)
	write(t, filepath.Join(root, "usr/local/sbin/sparkbox-netcfg"), "#!/bin/sh\nexit 0\n", 0o755)
	write(t, filepath.Join(root, "home/sparky/.ssh/authorized_keys"), "ssh-ed25519 AAAA test\n", 0o600)
	if err := os.Chmod(filepath.Join(root, "home/sparky/.ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/local/sbin/sparkbox-netcfg", filepath.Join(root, "etc/netcfg")); err != nil {
		t.Fatal(err)
	}
	return root
}

func mkdirAll(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func write(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is masked by umask; the tests below assert on exact
	// modes, so set it explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// debugfsStat returns debugfs's `stat` output for a guest path, which the
// assertions below grep. Reading the image with debugfs rather than mounting it
// is the same trick the package itself uses, and keeps the tests runnable
// without root.
func debugfsStat(t *testing.T, image, guestPath string) string {
	t.Helper()
	out, err := exec.Command("debugfs", "-R", "stat "+guestPath, image).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs stat %s: %v: %s", guestPath, err, out)
	}
	return string(out)
}

func TestBuildPopulatesTheTree(t *testing.T) {
	requireTools(t)
	root := tree(t)
	img := filepath.Join(t.TempDir(), "rootfs.ext4")

	if err := Build(context.Background(), root, img, 64); err != nil {
		t.Fatalf("Build: %v", err)
	}

	got, err := ReadFile(context.Background(), img, "/usr/local/sbin/sparkbox-netcfg")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "#!/bin/sh\nexit 0\n"; string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

// Modes have to survive verbatim: an authorized_keys that arrives as 0644
// makes sshd refuse the key, which presents as a sandbox the gateway cannot
// log into rather than as a build failure.
func TestBuildPreservesModes(t *testing.T) {
	requireTools(t)
	root := tree(t)
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := Build(context.Background(), root, img, 64); err != nil {
		t.Fatalf("Build: %v", err)
	}

	for path, want := range map[string]string{
		"/home/sparky/.ssh/authorized_keys": "0600",
		"/usr/local/sbin/sparkbox-netcfg":   "0755",
	} {
		if out := debugfsStat(t, img, path); !strings.Contains(out, "Mode:  "+want) {
			t.Errorf("%s: want mode %s, got:\n%s", path, want, out)
		}
	}
}

// The root inode must be owned by 0:0 no matter who runs the build. Without
// -E root_owner mke2fs stamps the invoking uid onto /, which produces a guest
// whose / belongs to a uid that does not exist in it.
func TestBuildRootIsOwnedByRoot(t *testing.T) {
	requireTools(t)
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := Build(context.Background(), tree(t), img, 64); err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := debugfsStat(t, img, "/")
	if !strings.Contains(out, "User:     0") || !strings.Contains(out, "Group:     0") {
		t.Errorf("root inode not owned by 0:0:\n%s", out)
	}
}

// Extended attributes carry file capabilities. The base image does
// `setcap cap_net_raw=+ep /usr/bin/ping`; if the build drops xattrs, ping
// silently stops working for non-root and nothing else reports a problem.
func TestBuildPreservesExtendedAttributes(t *testing.T) {
	requireTools(t)
	if _, err := exec.LookPath("setcap"); err != nil {
		t.Skip("skipping: setcap not available")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	ping := filepath.Join(root, "bin/ping")
	write(t, ping, "#!/bin/sh\n", 0o755)
	if out, err := exec.Command("setcap", "cap_net_raw=+ep", ping).CombinedOutput(); err != nil {
		t.Skipf("skipping: setcap failed (needs privilege): %v: %s", err, out)
	}

	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := Build(context.Background(), root, img, 64); err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := exec.Command("debugfs", "-R", "ea_list /bin/ping", img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs ea_list: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "security.capability") {
		t.Errorf("security.capability xattr lost in build:\n%s", out)
	}
}

// Hardlinks have to stay hardlinks: an Ubuntu rootfs has thousands, and
// expanding them into copies would inflate the image and break the identity
// checks that depend on two paths being the same inode.
func TestBuildPreservesHardlinks(t *testing.T) {
	requireTools(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(root, "bin/ping")
	write(t, a, "#!/bin/sh\n", 0o755)
	if err := os.Link(a, filepath.Join(root, "bin/ping6")); err != nil {
		t.Fatal(err)
	}

	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := Build(context.Background(), root, img, 64); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := inodeOf(t, img, "/bin/ping"); got != inodeOf(t, img, "/bin/ping6") {
		t.Errorf("hardlinks got separate inodes (%s vs %s)", got, inodeOf(t, img, "/bin/ping6"))
	}
	if out := debugfsStat(t, img, "/bin/ping"); !strings.Contains(out, "Links: 2") {
		t.Errorf("want link count 2:\n%s", out)
	}
}

func inodeOf(t *testing.T, image, guestPath string) string {
	t.Helper()
	out := debugfsStat(t, image, guestPath)
	_, rest, ok := strings.Cut(out, "Inode: ")
	if !ok {
		t.Fatalf("no inode in debugfs output for %s:\n%s", guestPath, out)
	}
	return strings.TrimSpace(strings.SplitN(rest, " ", 2)[0])
}

func TestBuildRejectsATreeThatDoesNotFit(t *testing.T) {
	requireTools(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "big"), strings.Repeat("x", 4*MiB), 0o644)

	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	err := Build(context.Background(), root, img, 1)
	if err == nil {
		t.Fatal("Build succeeded with a 1MB filesystem for a 4MB tree")
	}
	// A failed build must not leave an image behind for a caller to mistake
	// for a usable template.
	if _, statErr := os.Stat(img); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("failed Build left %s on disk (stat err: %v)", img, statErr)
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	requireTools(t)
	out := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := Build(context.Background(), tree(t), out, 0); err == nil {
		t.Error("want error for zero size")
	}
	if err := Build(context.Background(), filepath.Join(t.TempDir(), "nope"), out, 64); err == nil {
		t.Error("want error for a missing source tree")
	}
	file := filepath.Join(t.TempDir(), "afile")
	write(t, file, "x", 0o644)
	if err := Build(context.Background(), file, out, 64); err == nil {
		t.Error("want error when the source tree is a regular file")
	}
}

func TestBuildAtomicPublishesByRename(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "universal.ext4")
	if err := os.WriteFile(img, []byte("previous template"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := BuildAtomic(context.Background(), tree(t), img, 64); err != nil {
		t.Fatalf("BuildAtomic: %v", err)
	}
	if _, err := ReadFile(context.Background(), img, "/etc/hostname"); err != nil {
		t.Errorf("published image is not the new one: %v", err)
	}
	// No temp file may survive a successful publish, or the next directory
	// listing of templates has a stray entry in it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temp file %s left behind after publish", e.Name())
		}
	}
}

// A failed atomic build must leave the previous template exactly as it was:
// a host mid-refresh keeps serving sandbox creates from the old image.
func TestBuildAtomicLeavesTheOldImageOnFailure(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "universal.ext4")
	const previous = "previous template"
	if err := os.WriteFile(img, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	write(t, filepath.Join(root, "big"), strings.Repeat("x", 4*MiB), 0o644)
	if err := BuildAtomic(context.Background(), root, img, 1); err == nil {
		t.Fatal("BuildAtomic succeeded with a filesystem too small for the tree")
	}

	got, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != previous {
		t.Errorf("previous template was disturbed by a failed build: %q", got)
	}
}

func TestReadFileReportsAMissingFile(t *testing.T) {
	requireTools(t)
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := Build(context.Background(), tree(t), img, 64); err != nil {
		t.Fatalf("Build: %v", err)
	}
	_, err := ReadFile(context.Background(), img, "/etc/sparkbox/tools-rev")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("want fs.ErrNotExist for a missing file, got %v", err)
	}
}

func TestReadFileRequiresAnAbsolutePath(t *testing.T) {
	if _, err := ReadFile(context.Background(), "unused.ext4", "etc/hostname"); err == nil {
		t.Error("want error for a relative guest path")
	}
}

func TestEstimateMinMBAdmitsTheTreeItMeasured(t *testing.T) {
	requireTools(t)
	root := tree(t)
	// A few MB of real data, so the estimate is measuring something.
	write(t, filepath.Join(root, "payload"), strings.Repeat("x", 6*MiB), 0o644)

	min, err := EstimateMinMB(root)
	if err != nil {
		t.Fatalf("EstimateMinMB: %v", err)
	}
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := Build(context.Background(), root, img, min); err != nil {
		t.Errorf("Build failed at the estimated minimum %dMB: %v", min, err)
	}
}

// Hardlinks share an inode and their data is stored once, so a tree of N links
// to one file must not be charged N times.
func TestTreeSizeChargesHardlinksOnce(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a")
	write(t, target, strings.Repeat("x", 1*MiB), 0o644)
	singleBlocks, singleInodes, err := TreeSize(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b", "c", "d"} {
		if err := os.Link(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	blocks, inodes, err := TreeSize(root)
	if err != nil {
		t.Fatal(err)
	}
	if blocks != singleBlocks {
		t.Errorf("blocks grew from %d to %d after adding hardlinks", singleBlocks, blocks)
	}
	if inodes != singleInodes {
		t.Errorf("inodes grew from %d to %d after adding hardlinks", singleInodes, inodes)
	}
}

func TestEstimateMinMBHasAFloor(t *testing.T) {
	min, err := EstimateMinMB(t.TempDir())
	if err != nil {
		t.Fatalf("EstimateMinMB: %v", err)
	}
	if min < 16 {
		t.Errorf("empty tree estimated at %dMB, below the 16MB floor", min)
	}
}
