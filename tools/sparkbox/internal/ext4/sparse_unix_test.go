//go:build unix

package ext4

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The image must be sparse. A 25 GiB template that occupies 25 GiB on the host
// defeats the reflink model — every sandbox would pay the ceiling instead of
// what it writes.
func TestBuildImageIsSparse(t *testing.T) {
	requireTools(t)
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	const sizeMB = 512
	if err := Build(context.Background(), tree(t), img, sizeMB); err != nil {
		t.Fatalf("Build: %v", err)
	}
	st, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != sizeMB*MiB {
		t.Errorf("apparent size = %d, want %d", st.Size(), int64(sizeMB)*MiB)
	}
	if onDisk := diskUsageBytes(t, img); onDisk >= sizeMB*MiB/2 {
		t.Errorf("image occupies %d bytes of a %d-byte filesystem — not sparse", onDisk, sizeMB*MiB)
	}
}

// diskUsageBytes reports what the file actually occupies, as opposed to its
// apparent length. st_blocks is in 512-byte units.
func diskUsageBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("skipping: no Unix stat available")
	}
	return int64(st.Blocks) * 512
}
