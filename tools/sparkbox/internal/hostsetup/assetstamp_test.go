package hostsetup

// The stamp is what lets an EXISTING host notice a new guest rootfs. The sha in
// the manifest covers the compressed asset, which is streamed straight into a
// 25GB ext4 image and never kept, so before the stamp the only check available
// was "a file is there" — and a release that changed only the guest image
// (agent tools, guest packages, images/Dockerfile) never reached a host that
// already had a template.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// stampServer serves one compressed payload and counts how many times it was
// actually fetched, which is the only way to tell "skipped" from "re-downloaded
// with identical bytes".
func stampServer(t *testing.T, body []byte) (url string, hits *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write(body) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/rootfs.zst", &n
}

func TestDownloadVerifyStampsAndSkipsAnUnchangedRootfs(t *testing.T) {
	raw := bytes.Repeat([]byte("ext4-blocks"), 500)
	zbytes := compressZstd(t, raw)
	url, hits := stampServer(t, zbytes)
	dir := t.TempDir()
	dest := filepath.Join(dir, "universal.ext4")
	a := Artifact{Name: "rootfs", URL: url, SHA256: sha(zbytes), Dest: dest + ".zst"}

	dl, err := downloadVerify(context.Background(), NewHTTPFetcher(), a)
	if err != nil || !dl {
		t.Fatalf("first download: dl=%v err=%v", dl, err)
	}
	stamp, err := os.ReadFile(assetStampPath(dest))
	if err != nil {
		t.Fatalf("no stamp written: %v", err)
	}
	if got := string(bytes.TrimSpace(stamp)); got != sha(zbytes) {
		t.Errorf("stamp = %q, want the compressed asset's sha %q", got, sha(zbytes))
	}

	// Same release again: the stamp matches, so nothing is refetched.
	dl, err = downloadVerify(context.Background(), NewHTTPFetcher(), a)
	if err != nil || dl {
		t.Fatalf("second download: dl=%v err=%v, want a skip", dl, err)
	}
	if *hits != 1 {
		t.Errorf("server hit %d times, want 1 — an unchanged rootfs was refetched", *hits)
	}
}

func TestDownloadVerifyRefetchesAStaleRootfs(t *testing.T) {
	// THE BUG. The file on disk is a real template and is exactly the right
	// size; only the release it came from has moved on. Before the stamp this
	// case was indistinguishable from "up to date".
	oldRaw := bytes.Repeat([]byte("old-guest-image"), 500)
	newRaw := bytes.Repeat([]byte("new-guest-image"), 500)
	newZ := compressZstd(t, newRaw)
	url, hits := stampServer(t, newZ)
	dir := t.TempDir()
	dest := filepath.Join(dir, "universal.ext4")

	// A host holding the previous release, stamped as such.
	if err := os.WriteFile(dest, oldRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assetStampPath(dest), []byte(sha(compressZstd(t, oldRaw))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := Artifact{Name: "rootfs", URL: url, SHA256: sha(newZ), Dest: dest + ".zst"}
	dl, err := downloadVerify(context.Background(), NewHTTPFetcher(), a)
	if err != nil || !dl {
		t.Fatalf("stale rootfs: dl=%v err=%v, want a refetch", dl, err)
	}
	if *hits != 1 {
		t.Fatalf("server hit %d times, want 1", *hits)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, newRaw) {
		t.Fatal("the new guest image did not land")
	}
	if s, _ := os.ReadFile(assetStampPath(dest)); string(bytes.TrimSpace(s)) != sha(newZ) {
		t.Error("stamp was not moved to the new release")
	}
}

func TestDownloadVerifyRefetchesAnUnstampedRootfs(t *testing.T) {
	// A host provisioned before stamping existed. Its template could be any
	// release, so it is refetched rather than trusted: being wrong that way
	// costs a download, the other way is the silent staleness being fixed.
	raw := bytes.Repeat([]byte("ext4-blocks"), 500)
	zbytes := compressZstd(t, raw)
	url, hits := stampServer(t, zbytes)
	dir := t.TempDir()
	dest := filepath.Join(dir, "universal.ext4")
	if err := os.WriteFile(dest, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	a := Artifact{Name: "rootfs", URL: url, SHA256: sha(zbytes), Dest: dest + ".zst"}
	dl, err := downloadVerify(context.Background(), NewHTTPFetcher(), a)
	if err != nil || !dl {
		t.Fatalf("unstamped rootfs: dl=%v err=%v, want a refetch", dl, err)
	}
	if *hits != 1 {
		t.Errorf("server hit %d times, want 1", *hits)
	}
	if !assetStampMatches(dest, sha(zbytes)) {
		t.Error("the refetch left no stamp, so the next run would refetch again")
	}
}

func TestDownloadVerifyKeepsExistenceOnlyWithoutASha(t *testing.T) {
	// An unpinned manifest has nothing to compare, so existence is genuinely
	// all there is and the old behaviour must survive — otherwise every run
	// re-downloads a multi-GB rootfs forever.
	raw := bytes.Repeat([]byte("ext4-blocks"), 500)
	url, hits := stampServer(t, compressZstd(t, raw))
	dir := t.TempDir()
	dest := filepath.Join(dir, "universal.ext4")
	if err := os.WriteFile(dest, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	a := Artifact{Name: "rootfs", URL: url, Dest: dest + ".zst"} // no SHA256
	dl, err := downloadVerify(context.Background(), NewHTTPFetcher(), a)
	if err != nil || dl {
		t.Fatalf("unpinned rootfs: dl=%v err=%v, want a skip", dl, err)
	}
	if *hits != 0 {
		t.Errorf("server hit %d times, want 0", *hits)
	}
}

func TestAssetStampMatches(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "universal.ext4")
	if assetStampMatches(dest, "abc") {
		t.Error("a missing stamp must not match")
	}
	if err := os.WriteFile(assetStampPath(dest), []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !assetStampMatches(dest, "abc") {
		t.Error("trailing newline should be tolerated — the stamp is written with one")
	}
	if assetStampMatches(dest, "def") {
		t.Error("a different release must not match")
	}
	if assetStampMatches(dest, "") {
		t.Error("an empty expected sha must never match, or an unpinned manifest would look verified")
	}
}
