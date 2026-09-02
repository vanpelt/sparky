//go:build !unix

package ociimage

import (
	"archive/tar"
	"errors"
	"os"
)

// Sparkbox's host side runs on Unix. These stubs exist so the package still
// compiles for cross-platform tooling, and they report the honest thing — that
// this platform cannot reproduce a Unix rootfs — rather than silently building
// a wrong one.
var errXattrUnsupported = errors.New("extended attributes unsupported")

func lchown(string, int, int) error          { return os.ErrPermission }
func lsetxattr(string, string, []byte) error { return errXattrUnsupported }
func mknod(string, *tar.Header) error        { return os.ErrPermission }
