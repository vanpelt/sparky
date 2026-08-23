// Package ociimage materializes sandbox rootfs templates from OCI container
// images, without a container runtime.
//
// The old pipeline built one monolithic ext4 in CI, published it as a ~750 MB
// release asset, and then patched fast-moving agent CLIs into it on every host
// with deploy/refresh-agent-tools.sh — a second, divergent rootfs build with its
// own staleness protocol. See docs/oci-rootfs-and-durable-volumes-design.md for
// why that shape kept producing sandboxes whose tool versions were a function of
// when they were created.
//
// Here the image is the artifact. A host resolves a reference to a digest, pulls
// the layers it does not already have, unpacks the flattened filesystem, applies
// the guest overlay, and builds an ext4 with internal/ext4 — no daemon, no loop
// device, no CAP_SYS_ADMIN.
package ociimage

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Result reports what an Unpack did, including the things it could not do.
//
// The skip counters exist because unpacking faithfully needs privileges the
// caller may not have, and the two reasonable responses to that — refuse, or
// proceed knowingly — are the caller's decision, not this function's. Unpack is
// the mechanism; Materialize applies the policy.
type Result struct {
	Entries int   // tar entries written
	Bytes   int64 // regular-file bytes written

	// SkippedOwnership counts entries whose uid/gid could not be applied, which
	// happens when running unprivileged. A rootfs built with these skipped has
	// every file owned by the building user, so the guest's own accounts do not
	// own their homes — usable for a smoke test, not for a real sandbox.
	SkippedOwnership int
	// SkippedDevices counts device nodes that could not be created (no
	// CAP_MKNOD). Usually harmless: OCI images ship an empty /dev and the guest
	// mounts devtmpfs at boot.
	SkippedDevices int
	// SkippedXattrs counts extended attributes that could not be set. This one
	// is quietly load-bearing — file capabilities live in security.capability,
	// so a skipped xattr is a binary that lost its setcap.
	SkippedXattrs int
}

// Unpack writes a flattened image tar stream into dir.
//
// The stream is expected to be already flattened, with whiteouts resolved — one
// filesystem, not a stack of layers. Unpack does not interpret .wh. entries; it
// treats them as ordinary files, which would be wrong for a raw layer tar and is
// correct for a flattened one.
//
// dir must exist and should be empty. Everything below is written relative to
// it, and nothing is ever written outside it: see resolve for how entries that
// try are refused.
func Unpack(ctx context.Context, r io.Reader, dir string) (*Result, error) {
	if st, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("ociimage: destination: %w", err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("ociimage: destination %s is not a directory", dir)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("ociimage: destination: %w", err)
	}

	res := &Result{}
	// Directory modes and times are applied after the whole tree is written.
	// A directory that arrives read-only (0555 is common under /usr) cannot have
	// children created in it, and every file written into a directory bumps its
	// mtime — so both have to be a fixup pass rather than done inline.
	var dirs []deferredDir

	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, fmt.Errorf("ociimage: read tar: %w", err)
		}

		target, err := resolve(root, hdr.Name)
		if err != nil {
			return res, err
		}
		// Everything but a directory is replaced outright. A flattened stream
		// should not repeat a path, but an existing entry here would otherwise
		// be written *through* — appending to a file, or worse, following a
		// symlink we just created.
		if hdr.Typeflag != tar.TypeDir {
			if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return res, fmt.Errorf("ociimage: replace %s: %w", hdr.Name, err)
			}
		}

		if err := writeEntry(tr, hdr, target, root, res); err != nil {
			return res, err
		}
		if hdr.Typeflag == tar.TypeDir {
			dirs = append(dirs, deferredDir{path: target, mode: hdr.FileInfo().Mode(), mtime: hdr.ModTime})
		}
		res.Entries++
	}

	// Deepest first, so a parent's mode is tightened only after its children
	// are in place and its mtime is not bumped again by a later write.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		if err := os.Chmod(d.path, chmodBits(d.mode)); err != nil {
			return res, fmt.Errorf("ociimage: chmod dir: %w", err)
		}
		if !d.mtime.IsZero() {
			// Best-effort: a wrong directory mtime is cosmetic, and refusing the
			// whole image over one is not a trade anyone wants.
			_ = os.Chtimes(d.path, d.mtime, d.mtime)
		}
	}
	return res, nil
}

// chmodBits narrows a mode to what os.Chmod applies: the 0777 permissions plus
// setuid, setgid, and sticky.
//
// FileMode.Perm() returns *only* the 0777 bits, so reaching for it here silently
// drops all three — which shows up as /usr/bin/sudo losing its setuid (sudo then
// fails inside the guest with a message pointing nowhere near the unpacker) and
// /tmp losing its sticky bit (any user can delete any other user's files).
func chmodBits(m fs.FileMode) fs.FileMode {
	return m & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

type deferredDir struct {
	path  string
	mode  fs.FileMode
	mtime time.Time
}

// writeEntry materializes one tar entry that has already been resolved to a
// safe host path.
func writeEntry(tr *tar.Reader, hdr *tar.Header, target, root string, res *Result) error {
	mode := hdr.FileInfo().Mode()

	switch hdr.Typeflag {
	case tar.TypeDir:
		// Created permissive; the deferred pass applies the real mode. Without
		// this a read-only directory arriving before its children makes every
		// child fail with EACCES.
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("ociimage: mkdir %s: %w", hdr.Name, err)
		}

	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("ociimage: mkdir parent of %s: %w", hdr.Name, err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode.Perm())
		if err != nil {
			return fmt.Errorf("ociimage: create %s: %w", hdr.Name, err)
		}
		n, err := io.Copy(f, tr)
		if err != nil {
			f.Close()
			return fmt.Errorf("ociimage: write %s: %w", hdr.Name, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("ociimage: close %s: %w", hdr.Name, err)
		}
		res.Bytes += n

	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("ociimage: mkdir parent of %s: %w", hdr.Name, err)
		}
		// The link target is not validated: a rootfs is full of absolute
		// symlinks that are correct inside the guest and meaningless here
		// (/usr/bin/awk -> /etc/alternatives/awk). They are only ever dangerous
		// if something later writes *through* them, and resolve refuses that.
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return fmt.Errorf("ociimage: symlink %s: %w", hdr.Name, err)
		}

	case tar.TypeLink:
		source, err := resolve(root, hdr.Linkname)
		if err != nil {
			return fmt.Errorf("ociimage: hardlink %s: %w", hdr.Name, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("ociimage: mkdir parent of %s: %w", hdr.Name, err)
		}
		if err := os.Link(source, target); err != nil {
			return fmt.Errorf("ociimage: hardlink %s -> %s: %w", hdr.Name, hdr.Linkname, err)
		}

	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("ociimage: mkdir parent of %s: %w", hdr.Name, err)
		}
		switch err := mknod(target, hdr); {
		case err == nil:
		case errors.Is(err, os.ErrPermission):
			res.SkippedDevices++
			return nil // nothing further to apply to a node that does not exist
		default:
			return fmt.Errorf("ociimage: mknod %s: %w", hdr.Name, err)
		}

	default:
		// TypeXGlobalHeader and friends carry no filesystem content. Ignoring an
		// unknown type is right for a flattened image: the alternative is
		// refusing an image over a header we have no opinion about.
		return nil
	}

	return applyMetadata(hdr, target, res)
}

// applyMetadata sets ownership, mode, extended attributes, and mtime on an
// entry that has just been written. Directories get their mode and mtime in the
// deferred pass instead; everything else is final here.
//
// The order is load-bearing: chown BEFORE chmod. Linux clears setuid and setgid
// on chown — deliberately, so you cannot hand someone else a setuid binary — so
// chmod'ing first and chowning after silently strips those bits. That is how
// /usr/bin/sudo arrives in the guest as 0755 and fails with a message pointing
// nowhere near this function.
func applyMetadata(hdr *tar.Header, target string, res *Result) error {
	// Lchown, not Chown: a symlink's own ownership is what the image recorded,
	// and following it here would retarget the change onto whatever it points
	// at — which for an absolute symlink is a path on the *host*.
	switch err := lchown(target, hdr.Uid, hdr.Gid); {
	case err == nil:
	case errors.Is(err, os.ErrPermission):
		res.SkippedOwnership++
	default:
		return fmt.Errorf("ociimage: chown %s: %w", hdr.Name, err)
	}

	for name, value := range xattrsOf(hdr) {
		switch err := lsetxattr(target, name, []byte(value)); {
		case err == nil:
		case errors.Is(err, os.ErrPermission), errors.Is(err, errXattrUnsupported):
			res.SkippedXattrs++
		default:
			return fmt.Errorf("ociimage: setxattr %s on %s: %w", name, hdr.Name, err)
		}
	}

	// A symlink has no mode of its own, and a directory's is applied in the
	// deferred pass once its children are in place. O_CREATE's mode was masked
	// by umask, so this is also where a regular file gets its real permissions.
	if hdr.Typeflag != tar.TypeDir && hdr.Typeflag != tar.TypeSymlink {
		if err := os.Chmod(target, chmodBits(hdr.FileInfo().Mode())); err != nil {
			return fmt.Errorf("ociimage: chmod %s: %w", hdr.Name, err)
		}
	}

	if hdr.Typeflag != tar.TypeDir && hdr.Typeflag != tar.TypeSymlink && !hdr.ModTime.IsZero() {
		_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
	}
	return nil
}

// xattrsOf pulls extended attributes out of a tar header. GNU/PAX archives
// record them as SCHILY.xattr.<name> PAX records; Go also surfaces them in the
// deprecated Xattrs field, which some writers still populate exclusively.
func xattrsOf(hdr *tar.Header) map[string]string {
	out := map[string]string{}
	for k, v := range hdr.PAXRecords {
		if name, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
			out[name] = v
		}
	}
	//nolint:staticcheck // Xattrs is deprecated but still the only source for some writers.
	for k, v := range hdr.Xattrs {
		if _, dup := out[k]; !dup {
			out[k] = v
		}
	}
	return out
}

// errEscape is returned for any tar entry that would write outside the
// destination directory.
var errEscape = errors.New("ociimage: tar entry escapes the destination")

// resolve maps a tar entry name to a host path under root, refusing anything
// that would leave root.
//
// Two distinct attacks are in scope, and lexical cleaning only stops the first:
//
//  1. "../../etc/shadow" — cleaning the path catches this.
//  2. A symlink "etc" -> "/" earlier in the same archive, followed by an entry
//     "etc/shadow". That path is lexically innocent; it escapes at open() time
//     because a component of it is a symlink. So every parent component is
//     lstat'd and a symlinked one is refused outright.
//
// The final component is deliberately not checked — it is what we are about to
// create, and the caller unlinks it first.
func resolve(root, name string) (string, error) {
	clean := path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))
	target := filepath.Join(root, filepath.FromSlash(clean))
	// Belt and braces: Join already cleans, but an explicit prefix check makes
	// the invariant checkable rather than inferred.
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", errEscape, name)
	}

	parts := strings.Split(strings.Trim(clean, "/"), "/")
	at := root
	for _, part := range parts[:max(len(parts)-1, 0)] {
		if part == "" {
			continue
		}
		at = filepath.Join(at, part)
		info, err := os.Lstat(at)
		if errors.Is(err, fs.ErrNotExist) {
			break // not created yet; nothing below it can be a symlink either
		}
		if err != nil {
			return "", fmt.Errorf("ociimage: resolve %q: %w", name, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %q traverses symlink %q", errEscape, name, at)
		}
	}
	return target, nil
}
