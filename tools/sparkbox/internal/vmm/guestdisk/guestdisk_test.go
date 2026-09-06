//go:build linux

package guestdisk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReflinkCloneRequiresAlways(t *testing.T) {
	binDir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "args")
	fakeCP := filepath.Join(binDir, "cp")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SPARKBOX_CP_ARGS\"\n: > \"$3\"\n"
	if err := os.WriteFile(fakeCP, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("SPARKBOX_CP_ARGS", argsPath)

	source := filepath.Join(t.TempDir(), "source.ext4")
	destination := filepath.Join(t.TempDir(), "destination.ext4")
	if err := Clone(context.Background(), source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "--reflink=always\n" + source + "\n" + destination + "\n"
	if string(got) != want {
		t.Fatalf("cp args = %q, want %q", got, want)
	}

	// cp may create its destination before discovering that FICLONE cannot work.
	// A retry must not mistake that torn file for a complete pre-existing disk.
	failureScript := "#!/bin/sh\nprintf partial > \"$3\"\necho no-reflink >&2\nexit 1\n"
	if err := os.WriteFile(fakeCP, []byte(failureScript), 0o755); err != nil {
		t.Fatal(err)
	}
	err = Clone(context.Background(), source, destination)
	if err == nil || !strings.Contains(err.Error(), "no-reflink") {
		t.Fatalf("failed clone error = %v, want cp diagnostic", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed clone left destination behind: %v", err)
	}
}

func TestRootfsLoginIdentity(t *testing.T) {
	passwd := []byte("root:x:0:0:root:/root:/bin/bash\nsparky:x:1000:1001::/home/sparky:/bin/bash\n")

	got, err := rootfsLoginIdentity(passwd, "sparky")
	if err != nil {
		t.Fatal(err)
	}
	if got.home != "/home/sparky" || got.uid != 1000 || got.gid != 1001 {
		t.Fatalf("identity = %+v", got)
	}

	got, err = rootfsLoginIdentity(passwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.home != "/root" || got.uid != 0 || got.gid != 0 {
		t.Fatalf("default identity = %+v", got)
	}

	for _, tc := range []struct {
		name   string
		passwd string
		user   string
	}{
		{"missing user", string(passwd), "nobody"},
		{"bad uid", "sparky:x:nope:1000::/home/sparky:/bin/sh\n", "sparky"},
		{"unsafe home", "sparky:x:1000:1000::/:/bin/sh\n", "sparky"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := rootfsLoginIdentity([]byte(tc.passwd), tc.user); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestInstallAuthorizedKeyReportsParseFailure(t *testing.T) {
	err := InstallAuthorizedKey(context.Background(), "/not-mounted", "sparky", "not-an-ssh-key")
	if err == nil {
		t.Fatal("expected invalid gateway key to fail before mounting")
	}
	if !strings.Contains(err.Error(), "ssh:") {
		t.Fatalf("error %q does not preserve the SSH parser reason", err)
	}
}

// fakeRootfs builds an unmounted stand-in for a guest rootfs: just the
// /etc/passwd that writeAuthorizedKey reads, with the login user owned by
// whoever runs the test so the chowns succeed unprivileged.
func fakeRootfs(t *testing.T) string {
	t.Helper()
	mnt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mnt, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("sparky:x:%d:%d::/home/sparky:/bin/bash\n", os.Getuid(), os.Getgid())
	if err := os.WriteFile(filepath.Join(mnt, "etc", "passwd"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return mnt
}

const testGuestKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHqQ0kZ3vJZmZ1TzjJ4mUq8kMuQm/kZPq6ZQ0mVUOwvE gateway"

// A guest owns its own rootfs, so it can replace ~/.ssh with a symlink and try
// to steer the root-owned gateway's next cold-boot write onto the host. It must
// not land outside the image.
func TestWriteAuthorizedKeyRefusesSymlinkEscape(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"absolute", ""}, // filled in below with a path outside the rootfs
		{"relative climb", "../../../../../../etc/ssh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mnt := fakeRootfs(t)
			outside := filepath.Join(t.TempDir(), "host-etc-ssh")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			target := tc.target
			if target == "" {
				target = outside
			}
			if err := os.MkdirAll(filepath.Join(mnt, "home", "sparky"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(mnt, "home", "sparky", ".ssh")); err != nil {
				t.Fatal(err)
			}

			if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
				t.Fatalf("writeAuthorizedKey: %v", err)
			}

			// Nothing outside the rootfs was created or touched.
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Fatalf("escaped the rootfs: %v entries=%v", err, entries)
			}
			// The planted symlink was replaced by a real directory holding the key.
			sshDir := filepath.Join(mnt, "home", "sparky", ".ssh")
			fi, err := os.Lstat(sshDir)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				t.Fatalf(".ssh mode = %v, want a real directory", fi.Mode())
			}
			got, err := os.ReadFile(filepath.Join(sshDir, "authorized_keys"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != testGuestKey {
				t.Fatalf("authorized_keys = %q", got)
			}
		})
	}
}

// Create is re-run on an existing rootfs whenever a resume fails, so keys the
// user added inside their own sandbox have to survive the cold boot.
func TestWriteAuthorizedKeyPreservesUserKeys(t *testing.T) {
	mnt := fakeRootfs(t)
	sshDir := filepath.Join(mnt, "home", "sparky", ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const laptop = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIP3Yd0ZGPdmZLQAKk8xLmqL/9Zr5rWQNqjRLYh7lHcVn laptop"
	// The gateway key is already present under a different comment — it must be
	// recognised as the same key and not duplicated.
	prior := laptop + "\n" + strings.TrimSuffix(testGuestKey, " gateway") + " stale-comment\n"
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(sshDir, "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	want := laptop + "\n" + testGuestKey + "\n"
	if string(got) != want {
		t.Fatalf("authorized_keys =\n%q\nwant\n%q", got, want)
	}
}

// A rootfs whose login user has no home yet must come out with one the user
// owns; sshd's StrictModes rejects pubkey auth on a root-owned home, and the
// only symptom is an SSH attach that hangs.
func TestWriteAuthorizedKeyCreatesOwnedHome(t *testing.T) {
	mnt := fakeRootfs(t)
	if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		perm os.FileMode
	}{
		{filepath.Join(mnt, "home", "sparky"), 0o755},
		{filepath.Join(mnt, "home", "sparky", ".ssh"), 0o700},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.IsDir() || fi.Mode().Perm() != tc.perm {
			t.Fatalf("%s mode = %v, want dir %v", tc.path, fi.Mode(), tc.perm)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("%s: no stat", tc.path)
		}
		if int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
			t.Fatalf("%s owned by %d:%d, want the login user %d:%d",
				tc.path, st.Uid, st.Gid, os.Getuid(), os.Getgid())
		}
	}
}

func TestExt4DiskRejectsInvalidSuperblock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-ext4")
	if err := os.WriteFile(path, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DiskMB(path); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("DiskMB error = %v, want invalid-magic diagnostic", err)
	}
}

// A guest may legitimately keep a 0750 home; installing the gateway key is not
// a reason to widen it. ~/.ssh is the deliberate exception — sshd ignores
// authorized_keys unless that directory is the login user's and private.
func TestWriteAuthorizedKeyLeavesExistingHomeModeAlone(t *testing.T) {
	mnt := fakeRootfs(t)
	home := filepath.Join(mnt, "home", "sparky")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 {
		t.Fatalf("home mode = %v, want the guest's own 0750 untouched", fi.Mode().Perm())
	}
	if fi, err = os.Stat(filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf(".ssh mode = %v, want 0700 for sshd StrictModes", fi.Mode().Perm())
	}
}
