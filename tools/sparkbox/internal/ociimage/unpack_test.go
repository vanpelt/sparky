package ociimage

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// entry is a tar entry to be written by tarball. Only the fields the tests
// actually vary are here; everything else takes a sane default.
type entry struct {
	name     string
	typ      byte
	mode     int64
	body     string
	link     string
	uid, gid int
	xattrs   map[string]string
	devmajor int64
	devminor int64
	mtime    time.Time
}

func tarStream(t *testing.T, entries ...entry) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			if typ == tar.TypeDir {
				mode = 0o755
			} else {
				mode = 0o644
			}
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: typ,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Linkname: e.link,
			Uid:      e.uid,
			Gid:      e.gid,
			Devmajor: e.devmajor,
			Devminor: e.devminor,
			ModTime:  e.mtime,
			Format:   tar.FormatPAX,
		}
		if len(e.xattrs) > 0 {
			hdr.PAXRecords = map[string]string{}
			for k, v := range e.xattrs {
				hdr.PAXRecords["SCHILY.xattr."+k] = v
			}
		}
		if typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func unpackInto(t *testing.T, entries ...entry) (string, *Result) {
	t.Helper()
	dir := t.TempDir()
	res, err := Unpack(context.Background(), tarStream(t, entries...), dir)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	return dir, res
}

func TestUnpackWritesTheTree(t *testing.T) {
	dir, res := unpackInto(t,
		entry{name: "etc/", typ: tar.TypeDir},
		entry{name: "etc/hostname", body: "sparky\n"},
		entry{name: "usr/local/sbin/", typ: tar.TypeDir},
		entry{name: "usr/local/sbin/sparkbox-netcfg", body: "#!/bin/sh\n", mode: 0o755},
	)

	got, err := os.ReadFile(filepath.Join(dir, "etc/hostname"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sparky\n" {
		t.Errorf("content = %q", got)
	}
	if res.Entries != 4 {
		t.Errorf("Entries = %d, want 4", res.Entries)
	}
	if res.Bytes != int64(len("sparky\n")+len("#!/bin/sh\n")) {
		t.Errorf("Bytes = %d", res.Bytes)
	}
}

// O_CREATE's mode is masked by umask, so a setuid binary (/usr/bin/sudo is
// 4755) only keeps its bit if the unpacker chmods explicitly. Losing it makes
// sudo fail inside the guest with a message that points nowhere near here.
func TestUnpackPreservesSetuidAndExactModes(t *testing.T) {
	dir, _ := unpackInto(t,
		entry{name: "usr/bin/sudo", mode: 0o4755},
		entry{name: "root/.ssh/authorized_keys", mode: 0o600},
		entry{name: "tmp/", typ: tar.TypeDir, mode: 0o1777},
	)
	for name, want := range map[string]fs.FileMode{
		"usr/bin/sudo":              0o755 | fs.ModeSetuid,
		"root/.ssh/authorized_keys": 0o600,
		"tmp":                       0o777 | fs.ModeDir | fs.ModeSticky,
	} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want {
			t.Errorf("%s mode = %v, want %v", name, info.Mode(), want)
		}
	}
}

// A directory that arrives read-only must still receive its children. Applying
// 0555 inline would make every subsequent write into it fail with EACCES.
func TestUnpackFillsReadOnlyDirectories(t *testing.T) {
	dir, _ := unpackInto(t,
		entry{name: "usr/share/doc/", typ: tar.TypeDir, mode: 0o555},
		entry{name: "usr/share/doc/copyright", body: "MIT\n"},
	)
	if _, err := os.ReadFile(filepath.Join(dir, "usr/share/doc/copyright")); err != nil {
		t.Fatalf("child of a read-only directory: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "usr/share/doc"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Errorf("directory mode = %v, want 0555 after the fixup pass", info.Mode().Perm())
	}
}

func TestUnpackPreservesSymlinksAndHardlinks(t *testing.T) {
	dir, _ := unpackInto(t,
		entry{name: "bin/", typ: tar.TypeDir},
		entry{name: "bin/ping", body: "elf", mode: 0o755},
		entry{name: "bin/ping6", typ: tar.TypeLink, link: "bin/ping"},
		entry{name: "etc/alternatives/awk", typ: tar.TypeSymlink, link: "/usr/bin/mawk"},
	)

	link, err := os.Readlink(filepath.Join(dir, "etc/alternatives/awk"))
	if err != nil {
		t.Fatal(err)
	}
	// An absolute symlink pointing outside the tree is normal and must be kept
	// verbatim — it resolves inside the guest, not here.
	if link != "/usr/bin/mawk" {
		t.Errorf("symlink target = %q", link)
	}

	a, err := os.Stat(filepath.Join(dir, "bin/ping"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(dir, "bin/ping6"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Error("hardlink was expanded into a separate file")
	}
}

func TestUnpackRefusesPathTraversal(t *testing.T) {
	for _, name := range []string{
		"../escaped",
		"etc/../../escaped",
		"/../escaped",
	} {
		dir := t.TempDir()
		_, err := Unpack(context.Background(), tarStream(t, entry{name: name, body: "x"}), dir)
		if err == nil {
			// Cleaning may render some of these harmless-but-inside; what must
			// never happen is a file appearing outside the destination.
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "escaped")); statErr == nil {
				t.Errorf("%q wrote outside the destination", name)
			}
			continue
		}
		if !errors.Is(err, errEscape) {
			t.Errorf("%q: got %v, want an escape error", name, err)
		}
	}
}

// The attack lexical cleaning misses: a symlink placed earlier in the same
// archive, then an entry written through it. "etc/passwd" is innocent on its
// face and lands on the host's /etc/passwd if the unpacker just opens it.
func TestUnpackRefusesWritesThroughASymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	_, err := Unpack(context.Background(), tarStream(t,
		entry{name: "etc", typ: tar.TypeSymlink, link: "/"},
		entry{name: "etc/passwd", body: "pwned\n"},
	), dir)

	if err == nil {
		t.Fatal("Unpack accepted a write through a symlinked parent")
	}
	if !errors.Is(err, errEscape) {
		t.Errorf("got %v, want an escape error", err)
	}
	// And the symlink must not have been followed on the way to failing.
	if got, readErr := os.ReadFile("/etc/passwd"); readErr == nil && strings.Contains(string(got), "pwned") {
		t.Fatal("host /etc/passwd was written through the symlink")
	}
}

func TestUnpackRefusesAHardlinkEscapingTheTree(t *testing.T) {
	dir := t.TempDir()
	_, err := Unpack(context.Background(), tarStream(t,
		entry{name: "etc", typ: tar.TypeSymlink, link: "/"},
		entry{name: "stolen", typ: tar.TypeLink, link: "etc/shadow"},
	), dir)
	if err == nil {
		t.Fatal("Unpack accepted a hardlink whose source escapes the tree")
	}
}

// Extended attributes carry file capabilities; a dropped security.capability is
// a binary that quietly needs root. Skips are counted rather than fatal because
// some filesystems have no xattrs at all.
func TestUnpackAppliesExtendedAttributes(t *testing.T) {
	dir, res := unpackInto(t, entry{
		name:   "bin/ping",
		mode:   0o755,
		body:   "elf",
		xattrs: map[string]string{"user.sparkbox-test": "present"},
	})
	if res.SkippedXattrs > 0 {
		t.Skipf("skipping: filesystem does not support xattrs (%d skipped)", res.SkippedXattrs)
	}
	got, err := getxattrForTest(filepath.Join(dir, "bin/ping"), "user.sparkbox-test")
	if err != nil {
		t.Fatalf("read back xattr: %v", err)
	}
	if got != "present" {
		t.Errorf("xattr = %q, want %q", got, "present")
	}
}

func TestUnpackRecordsOwnershipItCouldNotApply(t *testing.T) {
	_, res := unpackInto(t, entry{name: "home/sparky/.bashrc", uid: 1000, gid: 1000})
	if os.Geteuid() == 0 {
		if res.SkippedOwnership != 0 {
			t.Errorf("running as root but skipped %d chowns", res.SkippedOwnership)
		}
		return
	}
	// Unprivileged: the skip must be *counted*, not silently swallowed, so the
	// caller can refuse to ship a rootfs whose ownership is wrong.
	if res.SkippedOwnership == 0 {
		t.Error("unprivileged unpack reported no skipped ownership")
	}
}

func TestUnpackSkipsDeviceNodesItCannotCreate(t *testing.T) {
	dir, res := unpackInto(t,
		entry{name: "dev/", typ: tar.TypeDir},
		entry{name: "dev/null", typ: tar.TypeChar, mode: 0o666, devmajor: 1, devminor: 3},
	)
	if res.SkippedDevices > 0 {
		if _, err := os.Lstat(filepath.Join(dir, "dev/null")); !errors.Is(err, fs.ErrNotExist) {
			t.Error("device counted as skipped but something was created")
		}
		return
	}
	info, err := os.Lstat(filepath.Join(dir, "dev/null"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeCharDevice == 0 {
		t.Errorf("dev/null is not a character device: %v", info.Mode())
	}
}

func TestUnpackIgnoresEntryTypesWithNoContent(t *testing.T) {
	dir, _ := unpackInto(t,
		entry{name: "volume-header", typ: 'V'},
		entry{name: "etc/hostname", body: "sparky\n"},
	)
	if _, err := os.Stat(filepath.Join(dir, "etc/hostname")); err != nil {
		t.Errorf("real entry after an ignored one: %v", err)
	}
}

func TestUnpackRejectsABadDestination(t *testing.T) {
	if _, err := Unpack(context.Background(), tarStream(t), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("want error for a missing destination")
	}
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(context.Background(), tarStream(t), file); err == nil {
		t.Error("want error when the destination is a regular file")
	}
}

func TestUnpackHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Unpack(ctx, tarStream(t, entry{name: "etc/hostname", body: "x"}), t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}
