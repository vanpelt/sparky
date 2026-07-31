package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Filesystem is an immutable object store rooted at a mounted filesystem.
// It is intended for durable shared storage such as a VAST PVC: callers still
// address objects by slash-separated keys, while Put publishes each object
// only after its complete contents have landed beside the final path.
type Filesystem struct {
	root string
}

// NewFilesystem opens a filesystem-backed object store rooted at dir.
func NewFilesystem(dir string) (*Filesystem, error) {
	if dir == "" {
		return nil, errors.New("filesystem object store directory is empty")
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve filesystem object store: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create filesystem object store: %w", err)
	}
	return &Filesystem{root: root}, nil
}

func (s *Filesystem) objectPath(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || path.Clean(key) != key {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid object key %q", key)
		}
	}
	return filepath.Join(s.root, filepath.FromSlash(key)), nil
}

// Put copies localPath to a new immutable object. A temporary file is filled
// and synced first, then linked into its final name without replacing an
// existing object.
func (s *Filesystem) Put(ctx context.Context, key, localPath string) error {
	dst, err := s.objectPath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if _, err := copyContext(ctx, tmp, src); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, dst); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("object %q already exists", key)
		}
		return fmt.Errorf("publish object %q: %w", key, err)
	}
	return nil
}

// Get copies an immutable object to localPath. It uses a sibling temporary
// file so a failed or canceled read never leaves a partial destination.
func (s *Filesystem) Get(ctx context.Context, key, localPath string) error {
	srcPath, err := s.objectPath(key)
	if err != nil {
		return err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(localPath), "."+filepath.Base(localPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if _, err := copyContext(ctx, tmp, src); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, localPath)
}

// Delete removes an object. It is idempotent.
func (s *Filesystem) Delete(_ context.Context, key string) error {
	dst, err := s.objectPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Filesystem) Exists(_ context.Context, key string) (bool, error) {
	dst, err := s.objectPath(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(dst)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 1024*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			w, writeErr := dst.Write(buf[:n])
			written += int64(w)
			if writeErr != nil {
				return written, writeErr
			}
			if w != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}
