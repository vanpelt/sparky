package objstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystemRoundTripIsImmutable(t *testing.T) {
	ctx := context.Background()
	store, err := NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	const key = "checkpoints/alice/id/generation.ext4.zst"
	if err := store.Put(ctx, key, src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, key, src); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("immutable Put = %v, want already exists", err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if err := store.Get(ctx, key, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "first" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	exists, err := store.Exists(ctx, key)
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
}

func TestFilesystemRejectsKeysOutsideRoot(t *testing.T) {
	store, err := NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "/absolute", "../escape", "a/../../escape", "a//b"} {
		if err := store.Put(context.Background(), key, src); err == nil {
			t.Errorf("Put accepted invalid key %q", key)
		}
	}
}
