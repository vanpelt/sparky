// Package objstore is a thin rclone-shell-out object store for sandbox
// archives. It deliberately reuses the fleet's existing rclone conventions (the
// same remote + bucket that hack/build-artifacts.sh publishes release artifacts
// to), so a host that can already fetch releases can archive with one extra
// config: write credentials in its rclone.conf.
//
// Unlike release artifacts, archives are *user data* — a sandbox's whole rootfs
// — so they are written with the default (private) ACL, never public-read.
//
// The store deals only in object keys (bucket-relative paths) and local files;
// the manager decides key layout (archives/<owner>/<name>...) and what to store.
package objstore

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Store is an rclone-backed object store. A nil *Store is a valid "archiving
// disabled" value: callers gate on `store != nil`. Construct with New.
type Store struct {
	bin    string // rclone binary (default: "rclone" on PATH)
	remote string // configured rclone remote name, e.g. "sparkbox-artifacts"
	bucket string // bucket within the remote
}

// New builds a Store from an rclone remote + bucket. It returns nil when remote
// is empty, so `objstore.New(...)` threads straight into an optional field and
// an unconfigured host simply has archiving disabled.
func New(remote, bucket string) *Store {
	if remote == "" || bucket == "" {
		return nil
	}
	return &Store{bin: "rclone", remote: remote, bucket: bucket}
}

// ref builds the "<remote>:<bucket>/<key>" address rclone addresses objects by.
func (s *Store) ref(key string) string {
	return s.remote + ":" + s.bucket + "/" + strings.TrimPrefix(key, "/")
}

// Put uploads localPath to key. Fat multipart settings (64M chunks × 16 in
// flight) keep a high-RTT pipe full for the multi-GB rootfs, matching the
// tuning hack/build-artifacts.sh landed on for the same fr-par bucket.
func (s *Store) Put(ctx context.Context, key, localPath string) error {
	return s.run(ctx, "copyto", localPath, s.ref(key),
		"--s3-chunk-size", "64M", "--s3-upload-concurrency", "16")
}

// Get downloads key to localPath.
func (s *Store) Get(ctx context.Context, key, localPath string) error {
	return s.run(ctx, "copyto", s.ref(key), localPath)
}

// Delete removes key. A missing object is not an error — deletion is meant to be
// idempotent (a torn/never-completed archive should still let the record go).
func (s *Store) Delete(ctx context.Context, key string) error {
	err := s.run(ctx, "deletefile", s.ref(key))
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}

// Exists reports whether key is present. Used to guard restore against a
// half-finished archive.
func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	out, err := s.output(ctx, "lsf", s.ref(key))
	if err != nil {
		// lsf on a missing object exits non-zero; treat that as "absent", not an
		// error, so a genuine credential/network failure is distinguishable.
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "directory not found") {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func (s *Store) run(ctx context.Context, args ...string) error {
	_, err := s.output(ctx, args...)
	return err
}

func (s *Store) output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.bin, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("rclone %s: %v: %s",
			strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}
