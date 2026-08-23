// Package ext4 builds ext4 filesystem images from a directory tree without
// mounting anything.
//
// The historical way to turn a flattened container image into a sandbox rootfs
// was: truncate a file, mkfs it, loop-mount it, untar into the mountpoint,
// umount. That needs a loop device and CAP_SYS_ADMIN, which on Kubernetes means
// a device-plugin resource (sparkbox.dev/loop) and an init container running
// with SYS_ADMIN and an unconfined AppArmor/seccomp profile — a lot of trusted
// surface to create a filesystem.
//
// mke2fs has done this natively since e2fsprogs 1.43: `-d <dir>` populates the
// filesystem at creation time by writing inodes directly into the image file.
// No mount, no loop device, no SYS_ADMIN. Everything a rootfs depends on
// survives the trip — verified against e2fsprogs 1.47.0:
//
//   - ownership (uid/gid) and modes, including setuid/setgid and 0600;
//   - extended attributes, which matters more than it sounds: the base image
//     does `setcap cap_net_raw=+ep /usr/bin/ping`, and that capability lives in
//     a security.capability xattr. Lose xattrs and ping silently needs sudo;
//   - hardlinks, which share an inode rather than being duplicated — an Ubuntu
//     rootfs has thousands.
//
// The caller still needs to be able to *create* that tree faithfully, which
// means CHOWN/FOWNER/DAC_OVERRIDE/MKNOD when unpacking an image as root. This
// package removes SYS_ADMIN from that list, not root itself; unpacking inside a
// user namespace is the separate step that would remove root too.
package ext4

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MiB is a mebibyte, the unit every size in this package is expressed in.
const MiB = 1 << 20

// blockSize is the ext4 block size we build with. 4 KiB is mke2fs's own choice
// for anything above a few hundred MiB; naming it here keeps the size estimate
// below honest about what it is rounding to.
const blockSize = 4096

// metadataOverhead is the fraction of a filesystem that is not file data:
// the journal, inode tables, group descriptors, block bitmaps. 8% is
// comfortably above what a 4 KiB-block ext4 actually spends (~5% with the
// default 16 KiB bytes-per-inode ratio) and this number only ever makes
// EstimateMinMB refuse a size that would have failed anyway.
const metadataOverhead = 1.08

// Build creates an ext4 filesystem of sizeMB at outPath, populated from the
// directory tree rooted at rootDir.
//
// outPath is created (or truncated) and left in place on success. On any
// failure it is removed, because a partially written filesystem is not a thing
// any caller wants to find on disk — use BuildAtomic when a concurrent reader
// might be looking at an existing image at that path.
//
// The root directory of the resulting filesystem is owned by 0:0 regardless of
// who runs this. Without -E root_owner, mke2fs stamps the *invoking* user onto
// the root inode, which produces a rootfs whose / is owned by whichever uid the
// build happened to run as — a difference that shows up much later, as a guest
// that cannot write to /.
func Build(ctx context.Context, rootDir, outPath string, sizeMB int64) error {
	if sizeMB <= 0 {
		return fmt.Errorf("ext4: size must be positive, got %d", sizeMB)
	}
	st, err := os.Stat(rootDir)
	if err != nil {
		return fmt.Errorf("ext4: source tree: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("ext4: source tree %s is not a directory", rootDir)
	}

	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("ext4: create image: %w", err)
	}
	// Truncate rather than write: the image is sparse until mke2fs fills it, so
	// a 25 GiB ceiling costs only the blocks the filesystem actually uses. That
	// sparseness is what makes the template thin and the per-sandbox reflink
	// copy cheap.
	if err := f.Truncate(sizeMB * MiB); err != nil {
		f.Close()
		os.Remove(outPath)
		return fmt.Errorf("ext4: size image: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(outPath)
		return fmt.Errorf("ext4: close image: %w", err)
	}

	args := []string{
		"-q",         // quiet: we surface stderr ourselves on failure
		"-F",         // the target is a regular file, not a block device
		"-t", "ext4", //
		"-E", "root_owner=0:0", // see the doc comment
		"-d", rootDir, // populate at creation — the whole point
		outPath,
	}
	cmd := exec.CommandContext(ctx, "mke2fs", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(outPath)
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("ext4: mke2fs not found — install e2fsprogs")
		}
		return fmt.Errorf("ext4: mke2fs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// BuildAtomic builds into a sibling temp file and renames it over outPath, so a
// reader either sees the previous image or the new one and never a half-written
// filesystem.
//
// This is the same discipline firecracker.Snapshot uses when it publishes a
// template: a sandbox create that reflinks the template while a refresh is in
// flight must not observe the intermediate state. The temp file is dot-prefixed
// so a directory listing of templates skips it.
func BuildAtomic(ctx context.Context, rootDir, outPath string, sizeMB int64) error {
	dir, base := filepath.Split(outPath)
	tmp := filepath.Join(dir, "."+base+".tmp")
	// A previous run killed mid-build leaves this behind; it is ours to reuse.
	os.Remove(tmp)
	if err := Build(ctx, rootDir, tmp, sizeMB); err != nil {
		return err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ext4: publish image: %w", err)
	}
	return nil
}

// TreeSize reports what rootDir will occupy in an ext4: the number of 4 KiB
// blocks its file data rounds up to, and the number of inodes it needs.
//
// It counts allocated blocks, not apparent size, so a sparse file is charged
// what it costs. Hardlinks are charged once — they share an inode in the image
// exactly as they do on disk. Symlinks under 60 bytes are charged no blocks at
// all, because ext4 stores those targets inline in the inode.
func TreeSize(rootDir string) (blocks, inodes int64, err error) {
	seen := make(map[[2]uint64]bool) // (device, inode) of hardlinked files already counted
	err = filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		inodes++
		switch {
		case d.IsDir():
			// A directory's own data block(s); one is right for all but huge dirs.
			blocks++
			return nil
		case info.Mode()&fs.ModeSymlink != 0:
			if len(linkTargetOf(path)) > 59 {
				blocks++ // too long for an inline "fast symlink"
			}
			return nil
		case !info.Mode().IsRegular():
			return nil // device nodes, fifos, sockets: inode only, no data
		}
		if key, nlink, ok := hardlinkKey(info); ok {
			if nlink > 1 {
				if seen[key] {
					inodes-- // same inode as one we already counted
					return nil
				}
				seen[key] = true
			}
		}
		blocks += allocatedBlocks(info)
		return nil
	})
	return blocks, inodes, err
}

// linkTargetOf reads a symlink, returning "" when it cannot be read. The only
// caller uses the length to decide whether ext4 can inline it, and a symlink we
// cannot read is not worth failing an estimate over.
func linkTargetOf(path string) string {
	t, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return t
}

// EstimateMinMB reports the smallest filesystem size, in MiB, that rootDir is
// expected to fit in.
//
// This exists to fail fast. Handing mke2fs a tree that does not fit produces a
// confusing error after it has already done most of the work, and the failure a
// caller actually cares about — "the 25 GiB ceiling you configured is smaller
// than the image you asked to put in it" — is answerable up front. The estimate
// is deliberately generous; it gates a refusal, so erring high only makes us
// ask for a bigger ceiling than strictly required.
func EstimateMinMB(rootDir string) (int64, error) {
	blocks, inodes, err := TreeSize(rootDir)
	if err != nil {
		return 0, fmt.Errorf("ext4: measure tree: %w", err)
	}
	dataMB := float64(blocks) * blockSize / MiB
	// mke2fs's default bytes-per-inode is 16 KiB, so an inode table sized for
	// N inodes costs about N*256 bytes. A container rootfs is inode-dense
	// (many tiny files), so this can exceed the block estimate on small trees.
	inodeMB := float64(inodes) * 256 / MiB
	min := int64((dataMB+inodeMB)*metadataOverhead) + 1
	// A filesystem below ~16 MiB cannot hold a journal plus useful data, and
	// nothing we build is ever that small; round the floor up rather than
	// returning an estimate that would fail for a reason we did not compute.
	if min < 16 {
		min = 16
	}
	return min, nil
}

// ReadFile extracts a single file from an unmounted ext4 image.
//
// debugfs reads the filesystem directly, so this needs neither a mount nor root
// — which is exactly why refresh-agent-tools.sh already reads its staleness
// stamp this way. Having it here means the OCI cache can verify a template it
// did not build (checking that the overlay revision inside matches the one the
// cache key claims) without growing a second way to look inside an image.
//
// A missing file is reported as fs.ErrNotExist so callers can errors.Is it.
func ReadFile(ctx context.Context, imagePath, guestPath string) ([]byte, error) {
	if !strings.HasPrefix(guestPath, "/") {
		return nil, fmt.Errorf("ext4: guest path %q must be absolute", guestPath)
	}
	cmd := exec.CommandContext(ctx, "debugfs", "-R", "cat "+guestPath, imagePath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("ext4: debugfs not found — install e2fsprogs")
		}
		return nil, fmt.Errorf("ext4: debugfs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	// debugfs reports a missing file on stderr and still exits 0, so the exit
	// code alone cannot be trusted to mean "found".
	if msg := stderr.String(); strings.Contains(msg, "File not found") {
		return nil, fmt.Errorf("ext4: %s not in %s: %w", guestPath, imagePath, fs.ErrNotExist)
	}
	return stdout.Bytes(), nil
}

// Available reports whether the e2fsprogs tools this package shells out to are
// on PATH. Callers use it to choose a code path or to fail with a clear
// prerequisite message instead of an exec error three layers down.
func Available() error {
	for _, bin := range []string{"mke2fs", "debugfs"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("ext4: %s not found on PATH — install e2fsprogs", bin)
		}
	}
	return nil
}

// apparentBlocks rounds a file's logical size up to whole ext4 blocks. It is
// the fallback for platforms (and filesystems) that do not report allocation,
// and it always reports at least as much as the file really needs.
func apparentBlocks(info fs.FileInfo) int64 {
	return (info.Size() + blockSize - 1) / blockSize
}
