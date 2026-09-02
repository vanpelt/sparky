//go:build unix

package ociimage

import (
	"archive/tar"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// errXattrUnsupported marks a filesystem that has no extended attributes at all
// (tmpfs without user_xattr, some overlay configurations). Distinct from a
// permission failure so Result can report it, and so a caller that cares can
// tell "this host cannot do it" from "this process may not".
var errXattrUnsupported = errors.New("extended attributes unsupported")

// lchown sets ownership without following a symlink. Following one would apply
// the change to the link's target, which for the absolute symlinks a rootfs is
// full of means a path on the host.
func lchown(path string, uid, gid int) error {
	if err := os.Lchown(path, uid, gid); err != nil {
		return err
	}
	return nil
}

// lsetxattr sets an extended attribute on the entry itself rather than on a
// symlink's target. security.capability travels this way, so this is how a
// setcap'd binary keeps its capability through an unpack.
func lsetxattr(path, name string, value []byte) error {
	err := unix.Lsetxattr(path, name, value, 0)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOTSUP):
		return errXattrUnsupported
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		return os.ErrPermission
	default:
		return err
	}
}

// mknod creates a device node or fifo. Device nodes need CAP_MKNOD; the caller
// treats a permission failure as a skip, because OCI images almost always ship
// an empty /dev and the guest mounts devtmpfs at boot anyway.
func mknod(path string, hdr *tar.Header) error {
	var mode uint32
	switch hdr.Typeflag {
	case tar.TypeChar:
		mode = unix.S_IFCHR
	case tar.TypeBlock:
		mode = unix.S_IFBLK
	case tar.TypeFifo:
		mode = unix.S_IFIFO
	default:
		return errors.New("not a device or fifo")
	}
	mode |= uint32(hdr.FileInfo().Mode().Perm())
	err := unix.Mknod(path, mode, int(unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))))
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		return os.ErrPermission
	}
	return err
}
