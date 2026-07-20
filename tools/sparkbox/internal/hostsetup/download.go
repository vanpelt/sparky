package hostsetup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Fetcher retrieves a URL's body. The real implementation is an http.Client;
// tests use an httptest server or a canned in-memory fetcher.
type Fetcher interface {
	Get(ctx context.Context, url string) (io.ReadCloser, error)
}

// httpFetcher is the production Fetcher.
type httpFetcher struct{ c *http.Client }

// NewHTTPFetcher returns a Fetcher backed by the default http client.
func NewHTTPFetcher() Fetcher { return httpFetcher{c: http.DefaultClient} }

func (h httpFetcher) Get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// sha256File streams a file through sha256 without buffering it (the rootfs is
// tens of GB). Returns "" if the file does not exist.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// downloadVerify fetches a.URL to a.Dest, verifying a.SHA256 as it streams
// (never buffering the body). It is idempotent: if a.Dest already exists with a
// matching sha, it is left untouched and (false, nil) is returned. A sha
// mismatch leaves no partial file at a.Dest.
func downloadVerify(ctx context.Context, f Fetcher, a Artifact) (downloaded bool, err error) {
	if a.SHA256 != "" {
		if have, herr := sha256File(a.Dest); herr == nil && have == a.SHA256 {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(a.Dest), 0o755); err != nil {
		return false, err
	}
	body, err := f.Get(ctx, a.URL)
	if err != nil {
		return false, err
	}
	defer body.Close()

	tmp := a.Dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), body); err != nil {
		out.Close()
		os.Remove(tmp)
		return false, err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); a.SHA256 != "" && got != a.SHA256 {
		os.Remove(tmp)
		return false, fmt.Errorf("%s: sha256 mismatch (want %s, got %s)", a.Name, a.SHA256, got)
	}
	mode := os.FileMode(0o644)
	if a.Mode != 0 {
		mode = os.FileMode(a.Mode)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, a.Dest); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// decompressInPlace expands a .zst or .gz artifact to the path with the
// compression suffix stripped, then removes the compressed source (matching the
// shell's `zstd -d --rm`). A path without a known suffix is a no-op. It returns
// the resulting (decompressed) path.
func decompressInPlace(path string) (string, error) {
	switch {
	case strings.HasSuffix(path, ".zst"):
		return decompress(path, strings.TrimSuffix(path, ".zst"), func(r io.Reader) (io.ReadCloser, error) {
			zr, err := zstd.NewReader(r)
			if err != nil {
				return nil, err
			}
			return zr.IOReadCloser(), nil
		})
	case strings.HasSuffix(path, ".gz"):
		return decompress(path, strings.TrimSuffix(path, ".gz"), func(r io.Reader) (io.ReadCloser, error) {
			return gzip.NewReader(r)
		})
	default:
		return path, nil
	}
}

func decompress(src, dst string, wrap func(io.Reader) (io.ReadCloser, error)) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	zr, err := wrap(in)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, zr); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	os.Remove(src)
	return dst, nil
}
