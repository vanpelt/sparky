package hostsetup

import (
	"bytes"
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

// sparseFileWriter preserves long zero ranges as holes while a compressed
// rootfs is streamed to disk. os.Create + io.Copy materializes every decoded
// zero byte, turning a mostly-empty 25 GiB ext4 image into a 25 GiB allocation.
// That is especially painful on the macOS PoC's deliberately small XFS volume,
// but wastes space on every host.
//
// We only seek over 64 KiB blocks that are entirely zero: mixed blocks are
// written normally, keeping this cheap while recovering the large free-space
// runs in an ext4 image. finish truncates after a trailing hole because seek
// alone does not extend a regular file.
type sparseFileWriter struct {
	f           *os.File
	zeros       []byte
	logicalSize int64
}

// readerOnly hides an optional io.WriterTo implementation on a decompressor.
// Some decoders otherwise hand the destination a whole frame in one Write;
// one non-zero byte would then materialize all of its free-space zeroes.
type readerOnly struct{ io.Reader }

func (w *sparseFileWriter) Write(p []byte) (int, error) {
	const blockSize = 64 * 1024
	isZero := func(block []byte) bool {
		if len(w.zeros) < len(block) {
			w.zeros = make([]byte, len(block))
		}
		return bytes.Equal(block, w.zeros[:len(block)])
	}

	consumed := 0
	for consumed < len(p) {
		firstEnd := min(consumed+blockSize, len(p))
		zeroRun := isZero(p[consumed:firstEnd])
		runEnd := firstEnd
		for runEnd < len(p) {
			nextEnd := min(runEnd+blockSize, len(p))
			if isZero(p[runEnd:nextEnd]) != zeroRun {
				break
			}
			runEnd = nextEnd
		}
		runLen := runEnd - consumed
		if zeroRun {
			if _, err := w.f.Seek(int64(runLen), io.SeekCurrent); err != nil {
				return consumed, err
			}
			w.logicalSize += int64(runLen)
			consumed = runEnd
			continue
		}
		n, err := w.f.Write(p[consumed:runEnd])
		w.logicalSize += int64(n)
		consumed += n
		if err != nil {
			return consumed, err
		}
		if n != runLen {
			return consumed, io.ErrShortWrite
		}
	}
	return consumed, nil
}

func (w *sparseFileWriter) finish() error {
	return w.f.Truncate(w.logicalSize)
}

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

// decompressor maps an artifact path to its on-disk destination and, when the
// path carries a compression suffix (.zst / .gz), a reader-wrapper that
// decompresses the stream. wrap == nil means the artifact is stored as-is.
func decompressor(p string) (dest string, wrap func(io.Reader) (io.ReadCloser, error)) {
	switch {
	case strings.HasSuffix(p, ".zst"):
		return strings.TrimSuffix(p, ".zst"), func(r io.Reader) (io.ReadCloser, error) {
			zr, err := zstd.NewReader(r)
			if err != nil {
				return nil, err
			}
			return zr.IOReadCloser(), nil
		}
	case strings.HasSuffix(p, ".gz"):
		return strings.TrimSuffix(p, ".gz"), func(r io.Reader) (io.ReadCloser, error) {
			return gzip.NewReader(r)
		}
	default:
		return p, nil
	}
}

// downloadVerify fetches a.URL, verifying a.SHA256 over the wire bytes as it
// streams (never buffering the body). A compressed a.Dest is decompressed
// during the same pass, so the multi-GB rootfs costs one disk write and its
// compressed form never lands on disk. Idempotency: an uncompressed artifact
// is skipped when a.Dest already matches the sha; a compressed one is skipped
// when its decompressed target exists (the sha covers bytes that are never
// kept, so existence is the strongest cheap check). A failure — including a
// sha mismatch discovered only after the stream ends — leaves no partial file.
func downloadVerify(ctx context.Context, f Fetcher, a Artifact) (downloaded bool, err error) {
	dest, wrap := decompressor(a.Dest)
	if wrap == nil {
		if a.SHA256 != "" {
			if have, herr := sha256File(dest); herr == nil && have == a.SHA256 {
				return false, nil
			}
		}
	} else if _, serr := os.Stat(dest); serr == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}
	body, err := f.Get(ctx, a.URL)
	if err != nil {
		return false, err
	}
	defer body.Close()

	// The hash sees exactly the wire bytes the (optional) decompressor pulls
	// through the tee; our artifacts are single-frame, so that is all of them.
	h := sha256.New()
	var src io.Reader = io.TeeReader(body, h)
	if wrap != nil {
		zr, werr := wrap(src)
		if werr != nil {
			return false, werr
		}
		defer zr.Close()
		src = zr
	}
	tmp := dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	if wrap != nil {
		sparse := &sparseFileWriter{f: out}
		_, err = io.CopyBuffer(sparse, readerOnly{src}, make([]byte, 1024*1024))
		if err == nil {
			err = sparse.finish()
		}
	} else {
		_, err = io.Copy(out, src)
	}
	if err != nil {
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
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}
