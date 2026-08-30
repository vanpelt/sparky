//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

// Captures go to a writable directory that is NOT the operator's image dir.
//
// This is not a tidiness split. On a hardened node /var/lib/sparkbox is mounted
// read-only to the VM controller — the base images are laid down by a
// privileged one-shot that exits before any guest runs — so a capture written
// there fails with EROFS, which is exactly how this was found:
//
//	cp: cannot create regular file '/var/lib/sparkbox/images/.snap-…tmp':
//	Read-only file system
//
// Keeping that mount read-only is the point: it stops a compromised controller
// substituting the rootfs every future sandbox boots from.
func TestCapturesAreWrittenToTheWritableTemplateDir(t *testing.T) {
	images, templates := t.TempDir(), t.TempDir()
	d := &Driver{opts: Options{ImageDir: images, TemplateDir: templates}}

	if got := d.captureDir(); got != templates {
		t.Errorf("captureDir = %q, want the writable template dir %q", got, templates)
	}
	// The single-machine shape, and every host that predates the split: one
	// directory, and the behaviour has to be byte-identical to what it was.
	same := &Driver{opts: Options{ImageDir: images}}
	if got := same.captureDir(); got != images {
		t.Errorf("captureDir with no TemplateDir = %q, want ImageDir %q", got, images)
	}
	if got := same.templatePath("universal"); got != filepath.Join(images, "universal.ext4") {
		t.Errorf("templatePath with no TemplateDir = %q, want it under ImageDir", got)
	}
}

// A file in the writable dir must never shadow a base image in the read-only
// one.
//
// The whole value of the read-only mount is that `universal` — the rootfs every
// fresh sandbox is created from — cannot be substituted. Resolving the writable
// directory first would hand that substitution back to anything able to write a
// file there, which is the one process the mount exists to constrain.
func TestABaseImageCannotBeShadowedFromTheWritableDir(t *testing.T) {
	images, templates := t.TempDir(), t.TempDir()
	d := &Driver{opts: Options{ImageDir: images, TemplateDir: templates}}

	trusted := filepath.Join(images, "universal.ext4")
	if err := os.WriteFile(trusted, []byte("OPERATOR IMAGE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templates, "universal.ext4"), []byte("IMPOSTOR"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := d.templatePath("universal"); got != trusted {
		t.Fatalf("templatePath resolved %q; a writable-dir file shadowed the operator's base image, which is the substitution the read-only mount exists to prevent", got)
	}

	// And the capture it was split out for still resolves, since nothing in the
	// read-only dir claims that name.
	capture := filepath.Join(templates, "snap-alice-gold.ext4")
	if err := os.WriteFile(capture, []byte("USER TEMPLATE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := d.templatePath("snap-alice-gold"); got != capture {
		t.Errorf("templatePath(snap-alice-gold) = %q, want the capture %q", got, capture)
	}
}

// RemoveTemplate deletes from the capture dir only.
//
// Its one caller is `snapshot rm`, which is a user deleting something they
// made. An operator base image is not the control plane's to remove, and a
// name that resolves into ImageDir must therefore come back not-found rather
// than succeeding.
func TestRemoveTemplateWillNotDeleteAnOperatorBaseImage(t *testing.T) {
	images, templates := t.TempDir(), t.TempDir()
	d := &Driver{opts: Options{ImageDir: images, TemplateDir: templates}}

	trusted := filepath.Join(images, "universal.ext4")
	if err := os.WriteFile(trusted, []byte("OPERATOR IMAGE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveTemplate(t.Context(), "universal"); err != nil {
		t.Fatalf("RemoveTemplate on a name it does not hold: %v", err)
	}
	if _, err := os.Stat(trusted); err != nil {
		t.Fatalf("the operator's base image was deleted by a control-plane verb: %v", err)
	}
}
