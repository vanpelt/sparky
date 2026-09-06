//go:build linux

// Package guestdisk owns the guest disk image: cloning a template, measuring it,
// compacting it, injecting the gateway's SSH key into it, and stripping a
// sandbox's secrets out of it before it becomes a template.
//
// None of this is VMM-specific. It is cp --reflink, an ext4 superblock read,
// loop mounts, e2fsck/zerofree/zstd and a rewrite of one authorized_keys file,
// and every VMM driver in this repository needs all of it. It lives here
// because it previously lived in the firecracker driver as unexported helpers,
// and the QEMU driver's first move was to copy ~360 lines of it verbatim —
// including the parts whose correctness is a security property.
//
// That copy is the reason this package exists rather than a style preference:
//
//   - writeAuthorizedKey refuses a symlink escape out of the mounted guest
//     filesystem. There is one test for that behaviour, and a second copy of
//     the code it does not run against.
//   - Sanitize is the fix for a real incident: snapshots taken on a fleet node
//     carried the owner's plaintext secrets into every fork. The next file
//     added to its strip list has to be added to every copy or one backend
//     starts leaking again.
//   - DiskMB's two return values are subtracted from each other by
//     host.Manager to compute pooled per-owner quota. Two implementations of
//     the same superblock read is two chances for that arithmetic to disagree.
//
// The precedent is internal/guestnet, which was extracted from this same
// driver for this same reason.
package guestdisk

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	xssh "golang.org/x/crypto/ssh"
)

// imageNameRe bounds a snapshot/template basename so Snapshot can't be tricked
// into writing outside ImageDir. Mirrors the manager's sandbox-name rules but
// also allows the '.' and uppercase we use in derived template names.
var imageNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,126}$`)

// Clone makes the no-full-copy policy common to fresh VM disks and
// snapshot staging. Keeping the exact cp invocation here also gives tests one
// seam for proving that neither path can silently regress to --reflink=auto.
func Clone(ctx context.Context, source, destination string) error {
	if out, err := exec.CommandContext(
		ctx, "cp", "--reflink=always", source, destination,
	).CombinedOutput(); err != nil {
		os.Remove(destination) //nolint:errcheck // never let a torn clone pass Create's exists check
		// Trimmed, because cp's diagnostic ends in a newline and this sentence
		// has to survive a trip across the node link. ctlops.wireSentence
		// blanks any message containing a control character — a correct
		// defence against a peer forging terminal output — and replaces it
		// with "the remote host reported a failure it could not describe". So
		// an untrimmed cp error reaches the operator as no error at all: this
		// exact newline once turned "cannot open ... Permission denied" into a
		// sentence that named nothing and cost an afternoon.
		return fmt.Errorf("copy rootfs: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DiskMB reads the primary ext4 superblock directly and follows statfs(2):
// capacity = total blocks - filesystem metadata overhead, used = capacity -
// free. Both come from the same superblock read so the console's meter has a
// numerator and denominator on the same basis — measuring used against the raw
// image size instead would leave a genuinely full guest short of 100% by
// however much metadata the filesystem holds. Firecracker owns the image and
// may have it mounted in a guest, so invoking e2fsck/debugfs here would be
// unsafe; a fixed-size read is passive and gives a sufficiently fresh
// best-effort counter for the periodic console measurement.
func DiskMB(path string) (usedMB, capacityMB int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	const (
		superOffset      = 1024
		superSize        = 1024
		ext4Magic        = 0xef53
		incompat64Bit    = 0x80
		roCompatBigalloc = 0x200
		maxLogBlockSize  = 6 // 1024 << 6 == ext4's 64 KiB maximum
		bytesPerMiB      = 1024 * 1024
		maxSignedInt64   = uint64(1<<63 - 1)
	)
	sb := make([]byte, superSize)
	if _, err := f.ReadAt(sb, superOffset); err != nil {
		return 0, 0, fmt.Errorf("read ext4 superblock: %w", err)
	}
	if magic := binary.LittleEndian.Uint16(sb[0x38:0x3a]); magic != ext4Magic {
		return 0, 0, fmt.Errorf("rootfs has invalid ext4 magic %#x", magic)
	}
	logBlockSize := binary.LittleEndian.Uint32(sb[0x18:0x1c])
	if logBlockSize > maxLogBlockSize {
		return 0, 0, fmt.Errorf("rootfs has invalid ext4 block-size shift %d", logBlockSize)
	}
	blocks := uint64(binary.LittleEndian.Uint32(sb[0x04:0x08]))
	free := uint64(binary.LittleEndian.Uint32(sb[0x0c:0x10]))
	incompat := binary.LittleEndian.Uint32(sb[0x60:0x64])
	roCompat := binary.LittleEndian.Uint32(sb[0x64:0x68])
	if roCompat&roCompatBigalloc != 0 {
		return 0, 0, fmt.Errorf("rootfs ext4 bigalloc is not supported for disk accounting")
	}
	if incompat&incompat64Bit != 0 {
		blocks |= uint64(binary.LittleEndian.Uint32(sb[0x150:0x154])) << 32
		free |= uint64(binary.LittleEndian.Uint32(sb[0x158:0x15c])) << 32
	}
	if free > blocks {
		return 0, 0, fmt.Errorf("rootfs ext4 free blocks %d exceed total blocks %d", free, blocks)
	}
	blockSize := uint64(1024) << logBlockSize
	usedBlocks := blocks - free
	// s_overhead_last is the number of filesystem-metadata blocks excluded
	// from statfs.f_blocks, and therefore from both `df`'s used figure and its
	// total.
	overhead := uint64(binary.LittleEndian.Uint32(sb[0x248:0x24c]))
	if overhead > usedBlocks {
		return 0, 0, fmt.Errorf("rootfs ext4 overhead blocks %d exceed occupied blocks %d",
			overhead, usedBlocks)
	}
	usedBlocks -= overhead
	capacityBlocks := blocks - overhead
	if capacityBlocks > maxSignedInt64/blockSize {
		return 0, 0, fmt.Errorf("rootfs ext4 block count overflows int64")
	}
	return int64(usedBlocks * blockSize / bytesPerMiB),
		int64(capacityBlocks * blockSize / bytesPerMiB), nil
}

// ExitCode extracts a process exit status, or -1 if err isn't an exit error.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// Compact fscks then zeroes the free space of an unmounted ext4 image so a
// following zstd/reflink only carries used blocks. e2fsck -fy is mandatory
// before zerofree (which refuses a dirty fs) and repairs the unclean state a
// killed VMM leaves the disk in.
func Compact(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, "e2fsck", "-fy", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		// e2fsck exits 1/2 when it *corrected* errors — success for us; only >= 4
		// (uncorrected or operational error) is fatal.
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() >= 4 {
			return fmt.Errorf("e2fsck %s: %v: %s", path, err, out)
		}
	}
	if o, err := exec.CommandContext(ctx, "zerofree", path).CombinedOutput(); err != nil {
		return fmt.Errorf("zerofree %s: %v: %s", path, err, o)
	}
	return nil
}

type loginIdentity struct {
	home     string
	uid, gid int
}

func rootfsLoginIdentity(passwd []byte, user string) (loginIdentity, error) {
	if user == "" {
		user = "root"
	}
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 7 || fields[0] != user {
			continue
		}
		uid, uerr := strconv.Atoi(fields[2])
		gid, gerr := strconv.Atoi(fields[3])
		home := filepath.Clean(fields[5])
		if uerr != nil || gerr != nil || !filepath.IsAbs(home) || home == "/" {
			return loginIdentity{}, fmt.Errorf("invalid passwd entry for %q", user)
		}
		return loginIdentity{home: home, uid: uid, gid: gid}, nil
	}
	return loginIdentity{}, fmt.Errorf("login user %q not found in guest /etc/passwd", user)
}

func InstallAuthorizedKey(ctx context.Context, rootfs, loginUser, key string) (retErr error) {
	if key == "" {
		return nil
	}
	publicKey, _, _, rest, err := xssh.ParseAuthorizedKey([]byte(key))
	if err != nil {
		return fmt.Errorf("gateway upstream public key is invalid: %w", err)
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return errors.New("gateway upstream public key is invalid: trailing data")
	}
	key = strings.TrimSpace(string(xssh.MarshalAuthorizedKey(publicKey)))
	mnt, err := os.MkdirTemp("", "sparkbox-key-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt) //nolint:errcheck
	if out, err := exec.CommandContext(ctx, "mount", "-o", "loop", rootfs, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s: %v: %s", rootfs, err, out)
	}
	defer func() {
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil && retErr == nil {
			retErr = fmt.Errorf("umount %s: %v: %s", mnt, err, out)
		}
	}()
	return writeAuthorizedKey(mnt, loginUser, key)
}

// writeAuthorizedKey puts key in the login user's authorized_keys inside an
// already-mounted guest rootfs.
//
// Everything here touches a filesystem the guest owns and can have rewritten
// arbitrarily, so it goes through os.Root: every path resolves beneath mnt,
// and a symlink that would escape it (any absolute one, or a relative one
// climbing past the root) is refused rather than followed onto the host.
// Without that a guest could point ~/.ssh at /etc/ssh and have the root-owned
// gateway chown and write *host* files on its next cold boot.
func writeAuthorizedKey(mnt, loginUser, key string) error {
	root, err := os.OpenRoot(mnt)
	if err != nil {
		return fmt.Errorf("open rootfs %s: %w", mnt, err)
	}
	defer root.Close() //nolint:errcheck

	passwd, err := root.ReadFile("etc/passwd")
	if err != nil {
		return err
	}
	identity, err := rootfsLoginIdentity(passwd, loginUser)
	if err != nil {
		return err
	}
	home := strings.TrimPrefix(identity.home, "/")
	if err := ensureGuestDir(root, home, 0o755, identity); err != nil {
		return err
	}
	// ~/.ssh is the exception to ensureGuestDir's leave-it-alone rule: sshd's
	// StrictModes ignores authorized_keys in a directory the login user does
	// not own or that anyone else can write, so these two are enforced even on
	// a directory the guest already had.
	sshDir := path.Join(home, ".ssh")
	if err := ensureGuestDir(root, sshDir, 0o700, identity); err != nil {
		return err
	}
	if err := root.Chmod(sshDir, 0o700); err != nil {
		return err
	}
	if err := root.Lchown(sshDir, identity.uid, identity.gid); err != nil {
		return err
	}

	// A read failure is not fatal: absent is the common case, and a dangling
	// symlink or a directory sitting in authorized_keys' place is the guest's
	// own mess — either way the gateway key still has to land. Replace whatever
	// is there rather than writing through it.
	authorizedKeys := path.Join(sshDir, "authorized_keys")
	existing, _ := root.ReadFile(authorizedKeys) //nolint:errcheck
	if err := root.RemoveAll(authorizedKeys); err != nil {
		return err
	}
	if err := root.WriteFile(authorizedKeys, mergeAuthorizedKeys(existing, key), 0o600); err != nil {
		return err
	}
	return root.Lchown(authorizedKeys, identity.uid, identity.gid)
}

// ensureGuestDir makes name a real directory inside the guest rootfs.
//
// A directory that is already there is left exactly as it is — mode and
// ownership included, since a guest may legitimately run a 0750 home and it is
// not our place to widen it. Anything else is replaced: a regular file, or the
// symlink a guest plants to aim our writes somewhere it prefers. A directory we
// create gets perm and the login user, because a root-owned home is precisely
// what makes sshd's StrictModes refuse the account later.
func ensureGuestDir(root *os.Root, name string, perm os.FileMode, identity loginIdentity) error {
	switch fi, err := root.Lstat(name); {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		if err := root.Remove(name); err != nil {
			return err
		}
	case !os.IsNotExist(err):
		return err
	}
	if err := root.MkdirAll(name, perm); err != nil {
		return err
	}
	if err := root.Chmod(name, perm); err != nil {
		return err
	}
	return root.Lchown(name, identity.uid, identity.gid)
}

// mergeAuthorizedKeys adds the gateway key to a guest's existing
// authorized_keys instead of replacing the file. Create is not only the
// first-boot path — the manager re-runs it whenever a resume fails — so
// overwriting would silently drop keys the user added inside their own
// sandbox. Duplicates of the gateway key itself are collapsed, comparing the
// parsed key so a differing comment or option prefix is not mistaken for a
// second key.
func mergeAuthorizedKeys(existing []byte, key string) []byte {
	var out []string
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if sameAuthorizedKey(line, key) {
			continue
		}
		out = append(out, line)
	}
	out = append(out, key)
	return []byte(strings.Join(out, "\n") + "\n")
}

// sameAuthorizedKey reports whether two authorized_keys lines carry the same
// public key. Both sides are reduced to the bare type-and-blob form so a
// differing comment or option prefix is not read as a second key — the gateway
// key arrives with a comment on it, and a line that already holds it will have
// whatever comment the last write left behind.
func sameAuthorizedKey(a, b string) bool {
	normalize := func(line string) (string, bool) {
		pub, _, _, _, err := xssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(xssh.MarshalAuthorizedKey(pub))), true
	}
	na, aok := normalize(a)
	nb, bok := normalize(b)
	return aok && bok && na == nb
}

// Sanitize strips a rootfs of its per-guest identity so every fork gets
// a fresh one — the same end state hack/build-rootfs.sh gives a freshly built
// template (blank machine id and hostname, no journal or SSH host keys; the
// sparkbox-netcfg boot hook regenerates the keys via ssh-keygen -A). Best-effort
// per file: a template missing any of these is still valid.
func Sanitize(ctx context.Context, imagePath string) (retErr error) {
	mnt, err := os.MkdirTemp("", "sparkbox-snap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt) //nolint:errcheck
	if o, err := exec.CommandContext(ctx, "mount", "-o", "loop", imagePath, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s: %v: %s", imagePath, err, o)
	}
	defer func() {
		if o, err := exec.Command("umount", mnt).CombinedOutput(); err != nil && retErr == nil {
			retErr = fmt.Errorf("umount %s: %v: %s", mnt, err, o)
		}
	}()

	// Snapshot images came from a guest and every directory entry in them is
	// attacker-controlled. os.Root gives the sanitization pass openat-style
	// beneath-root resolution, so an absolute /etc/hostname symlink (or a
	// relative chain containing ..) cannot redirect these root operations into
	// the Sparkbox container's filesystem.
	root, err := os.OpenRoot(mnt)
	if err != nil {
		return fmt.Errorf("open snapshot rootfs: %w", err)
	}
	defer root.Close() //nolint:errcheck
	for _, rel := range []string{"var/lib/dbus/machine-id", "etc/resolv.conf"} {
		root.RemoveAll(rel) //nolint:errcheck
	}
	// Keep /etc/machine-id present but empty. Besides being systemd's documented
	// image-builder state, the file is needed if /etc is ever read-only: PID 1
	// can bind-mount a transient id over an existing empty file, not an absent
	// path.
	if err := root.WriteFile("etc/machine-id", nil, 0o644); err != nil && !os.IsNotExist(err) {
		return err
	}
	if journal, err := root.Open("var/log/journal"); err == nil {
		if entries, err := journal.ReadDir(-1); err == nil {
			for _, entry := range entries {
				root.RemoveAll(path.Join("var/log/journal", entry.Name())) //nolint:errcheck
			}
		}
		journal.Close() //nolint:errcheck
	}
	root.RemoveAll("etc/hostname") //nolint:errcheck
	if err := root.WriteFile("etc/hostname", nil, 0o644); err != nil && !os.IsNotExist(err) {
		return err
	}
	if sshDir, err := root.Open("etc/ssh"); err == nil {
		if entries, err := sshDir.ReadDir(-1); err == nil {
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "ssh_host_") {
					root.RemoveAll(path.Join("etc/ssh", entry.Name())) //nolint:errcheck
				}
			}
		}
		sshDir.Close() //nolint:errcheck
	}
	root.RemoveAll("var/run/secrets/hivemind") //nolint:errcheck
	root.RemoveAll("run/secrets/hivemind")     //nolint:errcheck
	return nil
}

// ValidImageName reports whether name is safe to use as a template or snapshot
// basename. It bounds the name so a caller cannot be tricked into writing
// outside the image directory; it mirrors the manager's sandbox-name rules but
// also allows the '.' and uppercase that derived template names use.
func ValidImageName(name string) bool { return imageNameRe.MatchString(name) }
