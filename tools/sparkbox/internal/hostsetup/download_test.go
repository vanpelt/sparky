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
	"runtime"
	"syscall"
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

// TestDownloadVerifyDecompresses covers the streaming-decompress path: the sha
// is verified over the compressed wire bytes, only the decompressed file lands
// on disk, and an existing decompressed file short-circuits the re-download.
func TestDownloadVerifyDecompresses(t *testing.T) {
	// Model a sparse ext4 image: some real data, a large free-space run, more
	// data, then free space through EOF. The trailing run also proves finish()
	// preserves the apparent size after its final seek.
	raw := bytes.Repeat([]byte("ext4-blocks"), 1000)
	raw = append(raw, make([]byte, 4<<20)...)
	raw = append(raw, bytes.Repeat([]byte("more-blocks"), 1000)...)
	raw = append(raw, make([]byte, 2<<20)...)
	zbytes := compressZstd(t, raw)
	gbytes := compressGzip(t, raw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rootfs.zst":
			w.Write(zbytes)
		case "/rootfs.gz":
			w.Write(gbytes)
		}
	}))
	defer srv.Close()
	f := NewHTTPFetcher()
	dir := t.TempDir()

	// zstd: decompressed dest, no compressed file left behind.
	a := Artifact{Name: "rootfs", URL: srv.URL + "/rootfs.zst", SHA256: sha(zbytes), Dest: filepath.Join(dir, "universal.ext4.zst")}
	dl, err := downloadVerify(context.Background(), f, a)
	if err != nil || !dl {
		t.Fatalf("zstd download: dl=%v err=%v", dl, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "universal.ext4")); !bytes.Equal(got, raw) {
		t.Fatal("zstd content mismatch")
	}
	fi, err := os.Stat(filepath.Join(dir, "universal.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	// The setup binary and destination are Linux/XFS even for a macOS host.
	// APFS eagerly allocates an intervening seek range once a later extent is
	// written, so only assert physical sparseness on the target OS.
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && runtime.GOOS == "linux" {
		allocated := st.Blocks * 512
		if allocated >= fi.Size()/2 {
			t.Fatalf("decompressed rootfs was materialized: %d allocated bytes for %d apparent bytes",
				allocated, fi.Size())
		}
	}
	if _, err := os.Stat(a.Dest); !os.IsNotExist(err) {
		t.Fatal("compressed form must never land on disk")
	}

	// Second call skips on the decompressed file (partial re-runs must not
	// re-download the multi-GB rootfs).
	dl, err = downloadVerify(context.Background(), f, a)
	if err != nil || dl {
		t.Fatalf("second download should skip: dl=%v err=%v", dl, err)
	}

	// gzip path.
	g := Artifact{Name: "rootfs", URL: srv.URL + "/rootfs.gz", SHA256: sha(gbytes), Dest: filepath.Join(dir, "ubuntu.ext4.gz")}
	if dl, err := downloadVerify(context.Background(), f, g); err != nil || !dl {
		t.Fatalf("gzip download: dl=%v err=%v", dl, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "ubuntu.ext4")); !bytes.Equal(got, raw) {
		t.Fatal("gzip content mismatch")
	}

	// A sha mismatch leaves no decompressed file behind.
	bad := Artifact{Name: "rootfs", URL: srv.URL + "/rootfs.zst", SHA256: sha([]byte("other")), Dest: filepath.Join(dir, "bad.ext4.zst")}
	if _, err := downloadVerify(context.Background(), f, bad); err == nil {
		t.Fatal("expected sha mismatch error")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.ext4")); !os.IsNotExist(err) {
		t.Fatal("mismatched download must not leave a partial file")
	}
}

func TestSparseFileWriterCreatesHoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := &sparseFileWriter{f: f}
	data := bytes.Repeat([]byte("data"), 32*1024)
	zeroes := make([]byte, 4<<20)
	for _, part := range [][]byte{data, zeroes, data, zeroes} {
		if _, err := w.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Join([][]byte{data, zeroes, data, zeroes}, nil)
	if !bytes.Equal(got, want) {
		t.Fatal("sparse writer changed file content")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && runtime.GOOS == "linux" {
		allocated := st.Blocks * 512
		if allocated >= fi.Size()/2 {
			t.Fatalf("sparse writer materialized zero runs: %d allocated bytes for %d apparent bytes",
				allocated, fi.Size())
		}
	}
}

func compressZstd(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := zstd.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func compressGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}
