package ociimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ext4"
)

// Overlay applies sparkbox's guest payload to an unpacked rootfs tree before it
// becomes an ext4: the gateway's authorized_keys, the sshd drop-in, the boot
// hooks, the workload-identity units.
//
// It is a function rather than a method so the payload can live with the rest
// of the guest-side code and this package stays about images. img is the
// resolved image, so an overlay can ask it things — most usefully LoginUser,
// which the image declares about itself.
type Overlay func(ctx context.Context, rootDir string, img *Image) error

// Spec describes one template to materialize.
type Spec struct {
	// Ref is the image to build from, e.g. ghcr.io/vanpelt/sparkbox-rootfs:edge.
	Ref string
	// SizeMB is the guest's root filesystem ceiling. The image is a thin
	// template — hosts reflink it per sandbox — so this is a ceiling the guest
	// cannot grow past, not an allocation.
	SizeMB int64
	// Overlay applies the guest payload. May be nil, which produces a template
	// that is exactly the image (useful for bring-up and for tests).
	Overlay Overlay
	// OverlayRevision changes whenever the Overlay's payload changes, so a host
	// rebuilds templates it already has. This is the same job
	// refresh-agent-tools.sh does with IDENTITY_REV and AGENT_ENV_REV, except
	// that here it is a cache-key input rather than a stamp to be trusted.
	OverlayRevision string
	// OverlayInputs are the host-specific bytes the Overlay bakes in — above
	// all the gateway's public key. Two hosts running the same image do not get
	// the same template, so these have to be part of the cache key or a host
	// will happily reuse a template holding another host's gateway key.
	OverlayInputs [][]byte
	// AllowUnownedFiles permits a template whose ownership could not be applied
	// (an unprivileged unpack). Every file then belongs to the building user, so
	// the guest's own accounts do not own their homes. Useful for a smoke test
	// on a laptop; never right for a host that serves sandboxes.
	AllowUnownedFiles bool
}

// Cache is a directory of materialized ext4 templates, addressed by what went
// into them.
//
// It sits above internal/vmm: a template here is an ordinary <name>.ext4 in the
// image directory, and the driver resolves it exactly as it always has. Nothing
// in the driver learns what a registry is.
type Cache struct {
	dir string

	// building serializes materialization per key so two concurrent sandbox
	// creates on a cold cache do the pull once rather than racing to build the
	// same template twice. One sparkbox owns an image directory, so an
	// in-process lock is the whole requirement.
	mu       sync.Mutex
	building map[string]*sync.Mutex
}

// NewCache returns a Cache over an image directory. The directory is created if
// it does not exist.
func NewCache(dir string) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("ociimage: cache directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ociimage: cache directory: %w", err)
	}
	return &Cache{dir: dir, building: map[string]*sync.Mutex{}}, nil
}

// TemplateName returns the driver-facing image name for a resolved digest and
// spec — the value to put in vmm.Config.Image.
//
// The name is a hash of everything that went into the template, not just the
// image digest: the overlay's revision, the host-specific bytes it bakes in, and
// the size ceiling. Change any of them and you get a different template rather
// than a stale one that happens to have the right digest in its name.
func TemplateName(digest string, spec Spec) string {
	h := sha256.New()
	// Length-prefixed so no two different field splits can hash the same. The
	// alternative — joining with a separator — collides as soon as a field can
	// contain the separator, and a digest contains a colon.
	for _, field := range [][]byte{
		[]byte(digest),
		[]byte(spec.OverlayRevision),
		[]byte(strconv.FormatInt(spec.SizeMB, 10)),
	} {
		fmt.Fprintf(h, "%d:", len(field))
		h.Write(field)
	}
	for _, in := range spec.OverlayInputs {
		fmt.Fprintf(h, "%d:", len(in))
		h.Write(in)
	}
	// 12 bytes is far past collision-relevant for the handful of templates a
	// host holds, and keeps the name readable in a directory listing.
	return "oci-" + hex.EncodeToString(h.Sum(nil)[:12])
}

// Path returns where a template name lives on disk.
func (c *Cache) Path(name string) string { return filepath.Join(c.dir, name+".ext4") }

// Materialize ensures a template exists for spec and returns its driver-facing
// image name.
//
// The reference is resolved first, which costs one registry round trip and no
// layer data. If the resulting template is already on disk, that is the whole
// cost — a warm host pays a manifest fetch per sandbox create, not a pull.
//
// Resolving on every call is deliberate: a tag moves, and a host that skipped
// the check would serve a stale template indefinitely with no way to notice.
// That is precisely the failure refresh-agent-tools.sh was written to work
// around and then reproduced in its own stamp file.
func (c *Cache) Materialize(ctx context.Context, p *Puller, spec Spec) (string, error) {
	if spec.SizeMB <= 0 {
		return "", fmt.Errorf("ociimage: SizeMB must be positive, got %d", spec.SizeMB)
	}
	if err := ext4.Available(); err != nil {
		return "", err
	}

	img, err := p.Resolve(ctx, spec.Ref)
	if err != nil {
		return "", err
	}
	name := TemplateName(img.Digest, spec)

	unlock := c.lock(name)
	defer unlock()

	// Re-check under the lock: the goroutine we queued behind may have just
	// built the very template we are about to build.
	if _, err := os.Stat(c.Path(name)); err == nil {
		return name, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("ociimage: stat template: %w", err)
	}

	if err := c.build(ctx, img, spec, name); err != nil {
		return "", err
	}
	return name, nil
}

// lock returns the per-key mutex's unlock function, creating the mutex on first
// use. Keys are never removed: a host holds a handful of templates, and the
// bookkeeping to reclaim a mutex safely costs more than the mutex.
func (c *Cache) lock(name string) func() {
	c.mu.Lock()
	m, ok := c.building[name]
	if !ok {
		m = &sync.Mutex{}
		c.building[name] = m
	}
	c.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// build does the work Materialize decided was necessary: stream the flattened
// image into a staging tree, apply the overlay, and publish an ext4.
func (c *Cache) build(ctx context.Context, img *Image, spec Spec, name string) error {
	// Staged next to the templates rather than in TMPDIR: an Ubuntu rootfs is
	// several GB, and the image directory is the volume an operator sized for
	// exactly this. A /tmp on tmpfs would try to hold it in RAM.
	staging, err := os.MkdirTemp(c.dir, ".staging-"+name+"-")
	if err != nil {
		return fmt.Errorf("ociimage: staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	fsys := img.Filesystem()
	res, unpackErr := Unpack(ctx, fsys, staging)
	closeErr := fsys.Close()
	if unpackErr != nil {
		return fmt.Errorf("ociimage: unpack %s: %w", img.Ref, unpackErr)
	}
	if closeErr != nil {
		return fmt.Errorf("ociimage: read %s: %w", img.Ref, closeErr)
	}
	if res.SkippedOwnership > 0 && !spec.AllowUnownedFiles {
		return fmt.Errorf("ociimage: %s: could not set ownership on %d entries — "+
			"run as root (CAP_CHOWN) or set AllowUnownedFiles for a throwaway build",
			img.Ref, res.SkippedOwnership)
	}
	if res.SkippedXattrs > 0 {
		// Not fatal — plenty of images have no security-relevant xattrs — but a
		// silently de-setcap'd binary is a bad way to find out, so say it.
		return fmt.Errorf("ociimage: %s: could not set %d extended attributes; "+
			"file capabilities (setcap) would be lost — check the filesystem under %s",
			img.Ref, res.SkippedXattrs, c.dir)
	}

	if spec.Overlay != nil {
		if err := spec.Overlay(ctx, staging, img); err != nil {
			return fmt.Errorf("ociimage: overlay %s: %w", img.Ref, err)
		}
	}

	// Fail with the number an operator can act on, rather than letting mke2fs
	// report ENOSPC a few gigabytes in.
	min, err := ext4.EstimateMinMB(staging)
	if err != nil {
		return err
	}
	if spec.SizeMB < min {
		return fmt.Errorf("ociimage: %s needs at least %d MB but SizeMB is %d",
			img.Ref, min, spec.SizeMB)
	}

	if err := ext4.BuildAtomic(ctx, staging, c.Path(name), spec.SizeMB); err != nil {
		return err
	}
	return nil
}

// Prune removes cached templates that are not in keep.
//
// Templates are immutable and named by their inputs, so an image bump leaves the
// previous one behind forever. The caller decides what is live — a template is
// still in use by any sandbox that has not been recreated since — so this takes
// the keep set rather than guessing from mtimes.
func (c *Cache) Prune(keep map[string]bool) (removed []string, err error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, fmt.Errorf("ociimage: read cache: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := strings.CutSuffix(e.Name(), ".ext4")
		// Only ever touch our own: an image directory also holds hand-managed
		// templates and snapshot templates, and neither is ours to reclaim.
		if !ok || !strings.HasPrefix(name, "oci-") || keep[name] {
			continue
		}
		if rmErr := os.Remove(filepath.Join(c.dir, e.Name())); rmErr != nil {
			return removed, fmt.Errorf("ociimage: prune %s: %w", e.Name(), rmErr)
		}
		removed = append(removed, name)
	}
	return removed, nil
}
