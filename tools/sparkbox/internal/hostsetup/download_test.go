package hostsetup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestDownloadVerify(t *testing.T) {
	payload := []byte("hello firecracker")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()
	f := NewHTTPFetcher()
	dir := t.TempDir()

	// Good sha downloads and lands the file.
	dest := filepath.Join(dir, "vmlinux")
	a := Artifact{Name: "vmlinux", URL: srv.URL + "/ok", SHA256: sha(payload), Dest: dest}
	dl, err := downloadVerify(context.Background(), f, a)
	if err != nil || !dl {
		t.Fatalf("first download: dl=%v err=%v", dl, err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, payload) {
		t.Fatal("content mismatch")
	}

	// Second call is a no-op (present + verified).
	dl, err = downloadVerify(context.Background(), f, a)
	if err != nil || dl {
		t.Fatalf("second download should skip: dl=%v err=%v", dl, err)
	}

	// Bad sha leaves no file behind.
	dest2 := filepath.Join(dir, "bad")
	_, err = downloadVerify(context.Background(), f, Artifact{Name: "bad", URL: srv.URL + "/ok", SHA256: sha([]byte("other")), Dest: dest2})
	if err == nil {
		t.Fatal("expected sha mismatch error")
	}
	if _, statErr := os.Stat(dest2); !os.IsNotExist(statErr) {
		t.Fatal("mismatched download must not leave a partial file")
	}

	// Executable mode is honored.
	dest3 := filepath.Join(dir, "fc")
	if _, err := downloadVerify(context.Background(), f, Artifact{Name: "fc", URL: srv.URL + "/ok", SHA256: sha(payload), Dest: dest3, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dest3); fi.Mode().Perm()&0o100 == 0 {
		t.Fatal("firecracker should be executable")
	}
}

func TestDecompressInPlace(t *testing.T) {
	dir := t.TempDir()
	raw := bytes.Repeat([]byte("ext4-blocks"), 1000)

	// zstd
	zpath := filepath.Join(dir, "universal.ext4.zst")
	writeZstd(t, zpath, raw)
	out, err := decompressInPlace(zpath)
	if err != nil {
		t.Fatal(err)
	}
	if out != filepath.Join(dir, "universal.ext4") {
		t.Fatalf("out path = %q", out)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, raw) {
		t.Fatal("zstd content mismatch")
	}
	if _, err := os.Stat(zpath); !os.IsNotExist(err) {
		t.Fatal("compressed source should be removed")
	}

	// gzip
	gpath := filepath.Join(dir, "ubuntu.ext4.gz")
	writeGzip(t, gpath, raw)
	out, err = decompressInPlace(gpath)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, raw) {
		t.Fatal("gzip content mismatch")
	}

	// plain path is a no-op
	plain := filepath.Join(dir, "already.ext4")
	os.WriteFile(plain, raw, 0o644)
	if out, err := decompressInPlace(plain); err != nil || out != plain {
		t.Fatalf("plain path should be a no-op: %q %v", out, err)
	}
}

func writeZstd(t *testing.T, path string, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	w, _ := zstd.NewWriter(&buf)
	w.Write(data)
	w.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGzip(t *testing.T, path string, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(data)
	w.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
