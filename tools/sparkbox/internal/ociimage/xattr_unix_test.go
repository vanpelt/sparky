//go:build unix

package ociimage

import "golang.org/x/sys/unix"

// getxattrForTest reads one extended attribute back off an unpacked entry.
func getxattrForTest(path, name string) (string, error) {
	buf := make([]byte, 256)
	n, err := unix.Lgetxattr(path, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
