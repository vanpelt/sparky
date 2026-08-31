package envsync

// Tests run against the mock driver's real per-VM SSH servers (the
// e2e_test.go stack, minus the gateway): a push is a genuine SSH exec of the
// base64-transported rewrite script against a genuine file, so block
// rewriting, empty-set clearing, hostile-value inertness, and paused-skip are
// all exercised KVM-free.

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// bakedEnv stands in for the toolchain PATH the image bakes into
// /etc/environment; every rewrite must leave it byte-for-byte intact.
const bakedEnv = "# baked by the image\nPATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin\nGOPATH=/home/sparky/go\n"

type testEnv struct {
	syncer *Syncer
	mgr    *host.Manager
	store  *secrets.Store
	dir    string
	dbPath string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	hostKey, err := sshgw.LoadOrCreateKey(dir, "host_key")
	if err != nil {
		t.Fatal(err)
	}
	upstreamKey, err := sshgw.LoadOrCreateKey(dir, "upstream_key")
	if err != nil {
		t.Fatal(err)
	}
	driver := mock.New(dir, hostKey)
	t.Cleanup(func() { driver.Close() })

	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver,
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "secrets.db")
	store, err := secrets.Open(dbPath, secrets.DeriveKEK([]byte("test-ikm")), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	s := New(store, mgr, upstreamKey, log)
	// The mock's fake VMs run /bin/sh unprivileged with cwd = the sandbox
	// workdir: use a relative env file and drop the sudo.
	s.envPath = "environment"
	s.shell = "sh"
	return &testEnv{syncer: s, mgr: mgr, store: store, dir: dir, dbPath: dbPath}
}

func (te *testEnv) create(t *testing.T, name, owner string) *host.Sandbox {
	t.Helper()
	box, err := te.mgr.Create(context.Background(), name, owner, "ubuntu", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// envFile is the guest env file's host-side path (the mock VM's "disk" is its
// workdir).
func (te *testEnv) envFile(name string) string {
	return filepath.Join(te.dir, "mock-vms", name, "environment")
}

func (te *testEnv) seedEnv(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(te.envFile(name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (te *testEnv) readEnv(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(te.envFile(name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tagAndPut wires owner's secret env=value to every sandbox tagged tag.
func (te *testEnv) tagAndPut(t *testing.T, owner, sandbox, tag, env, value string) {
	t.Helper()
	if err := te.store.SetTags(sandbox, owner, []string{tag}); err != nil {
		t.Fatal(err)
	}
	if err := te.store.PutSecret(owner, env, value, []string{tag}); err != nil {
		t.Fatal(err)
	}
}

func TestPushEnvRewritesOnlyManagedBlock(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")

	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("PushEnv: %v", err)
	}
	got := te.readEnv(t, "boxa")
	if !strings.HasPrefix(got, bakedEnv) {
		t.Fatalf("baked content not preserved verbatim:\n%s", got)
	}
	want := bakedEnv + BlockBegin + "\n" + `API_KEY="s3kr3t"` + "\n" + BlockEnd + "\n"
	if got != want {
		t.Fatalf("env file = %q, want %q", got, want)
	}
	fi, err := os.Stat(te.envFile("boxa"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("env file mode = %o, want 0644", fi.Mode().Perm())
	}

	// A second push replaces the block instead of accumulating markers.
	if err := te.store.PutSecret("alice", "API_KEY", "rotated", []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("second PushEnv: %v", err)
	}
	got = te.readEnv(t, "boxa")
	if n := strings.Count(got, BlockBegin); n != 1 {
		t.Fatalf("want exactly 1 begin marker after re-push, got %d:\n%s", n, got)
	}
	if strings.Contains(got, "s3kr3t") || !strings.Contains(got, `API_KEY="rotated"`) {
		t.Fatalf("old value survived re-push:\n%s", got)
	}
	if !strings.HasPrefix(got, bakedEnv) {
		t.Fatalf("baked content lost on re-push:\n%s", got)
	}
}

func TestPushEnvEmptySetWritesEmptyBlock(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatal(err)
	}

	// Untag the box: the next push must clear the secret, not skip the write.
	if err := te.store.SetTags("boxa", "alice", nil); err != nil {
		t.Fatal(err)
	}
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("PushEnv with empty set: %v", err)
	}
	got := te.readEnv(t, "boxa")
	want := bakedEnv + BlockBegin + "\n" + BlockEnd + "\n"
	if got != want {
		t.Fatalf("env file = %q, want empty managed block %q", got, want)
	}
}

func TestPushEnvHostileValuesAreInert(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)

	// A value equal to a marker line cannot appear here: the markers contain
	// '#', which sanitizeEnv rejects outright (see TestSanitizeEnv).
	hostile := map[string]string{
		"CMD_SUB":  `$(touch pwned-sub)`,
		"BACKTICK": "`touch pwned-tick`",
		"QUOTES":   `it's "quoted" \and\ back\slashed`,
		"SEMI":     `; touch pwned-semi ;`,
	}
	if err := te.store.SetTags("boxa", "alice", []string{"web"}); err != nil {
		t.Fatal(err)
	}
	for name, val := range hostile {
		if err := te.store.PutSecret("alice", name, val, []string{"web"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("PushEnv: %v", err)
	}

	got := te.readEnv(t, "boxa")
	for name, val := range hostile {
		if !strings.Contains(got, name+`="`+val+`"`+"\n") {
			t.Fatalf("value for %s not rendered verbatim:\n%s", name, got)
		}
	}
	for _, f := range []string{"pwned-sub", "pwned-tick", "pwned-semi"} {
		if _, err := os.Stat(filepath.Join(te.dir, "mock-vms", "boxa", f)); err == nil {
			t.Fatalf("hostile value executed: %s exists", f)
		}
	}

	// A re-push must still strip and rewrite one clean block.
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("re-push: %v", err)
	}
	got = te.readEnv(t, "boxa")
	if n := strings.Count(got, BlockBegin); n != 1 {
		t.Fatalf("want exactly 1 begin marker after re-push, got %d:\n%s", n, got)
	}
	if !strings.HasPrefix(got, bakedEnv) {
		t.Fatalf("baked content lost:\n%s", got)
	}
}

func TestPushEnvDanglingBeginStripsToEOFAndHeals(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	// A hand-edited file: begin marker with no end. Everything from the
	// marker to EOF is stripped — over-removal beats leaking the stale
	// secret line — and the appended fresh block leaves the file balanced.
	broken := bakedEnv + BlockBegin + "\n" + `STALE="old"` + "\n" + "USER_EDIT=lost\n"
	te.seedEnv(t, "boxa", broken)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "fresh")

	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("PushEnv: %v", err)
	}
	got := te.readEnv(t, "boxa")
	want := bakedEnv + BlockBegin + "\n" + `API_KEY="fresh"` + "\n" + BlockEnd + "\n"
	if got != want {
		t.Fatalf("env file = %q, want stale block stripped + fresh block %q", got, want)
	}

	// The file healed, so re-pushes must not accumulate markers.
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("re-push: %v", err)
	}
	got = te.readEnv(t, "boxa")
	if n := strings.Count(got, BlockBegin); n != 1 {
		t.Fatalf("want exactly 1 begin marker after re-push, got %d:\n%s", n, got)
	}
}

func TestPushEnvOrphanEndMarkerKeptWithoutGrowth(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	// Begin marker hand-deleted: the lines before the orphan end marker may
	// be orphaned secret values or user edits — ambiguous — so a push keeps
	// them and the marker verbatim (StripEnv fails closed on this shape).
	broken := bakedEnv + `MAYBE_SECRET="old"` + "\n" + BlockEnd + "\n"
	te.seedEnv(t, "boxa", broken)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "fresh")

	for i := 0; i < 3; i++ {
		if err := te.syncer.PushEnv(context.Background(), box); err != nil {
			t.Fatalf("PushEnv #%d: %v", i+1, err)
		}
	}
	got := te.readEnv(t, "boxa")
	want := broken + BlockBegin + "\n" + `API_KEY="fresh"` + "\n" + BlockEnd + "\n"
	if got != want {
		t.Fatalf("env file after 3 pushes = %q, want orphan kept + exactly one fresh block %q", got, want)
	}
}

func TestPushEnvSkipsPausedBox(t *testing.T) {
	te := newTestEnv(t)
	te.create(t, "boxa", "alice")
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")
	if err := te.mgr.Pause(context.Background(), "boxa"); err != nil {
		t.Fatal(err)
	}
	box, ok := te.mgr.Get("boxa")
	if !ok || box.State != vmm.StatePaused {
		t.Fatalf("want paused box, got %+v", box)
	}

	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("PushEnv on paused box: %v", err)
	}
	if _, err := os.Stat(te.envFile("boxa")); !os.IsNotExist(err) {
		t.Fatalf("paused box's env file was touched (stat err = %v)", err)
	}
	if box, _ := te.mgr.Get("boxa"); box.State != vmm.StatePaused {
		t.Fatalf("push woke the box: state = %s", box.State)
	}
}

func TestPushEnvUndecryptablePushesNothing(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "GOOD", "value-a")
	if err := te.store.PutSecret("alice", "ALSO_GOOD", "value-b", []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatal(err)
	}
	before := te.readEnv(t, "boxa")

	// Corrupt one row out-of-band: decryption is all-or-nothing, so the push
	// must fail entirely — the intact sibling must not be re-pushed alone.
	db, err := sql.Open("sqlite", te.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE secrets SET ciphertext = X'DEADBEEF' WHERE env_name = 'GOOD'`); err != nil {
		t.Fatal(err)
	}

	err = te.syncer.PushEnv(context.Background(), box)
	if !errors.Is(err, secrets.ErrUndecryptable) {
		t.Fatalf("want ErrUndecryptable, got %v", err)
	}
	if got := te.readEnv(t, "boxa"); got != before {
		t.Fatalf("guest env changed despite undecryptable row:\nbefore %q\nafter  %q", before, got)
	}
}

func TestStripEnvClearsManagedBlock(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatal(err)
	}

	// Corrupt the row out-of-band: unlike PushEnv, StripEnv must still clear
	// the block — hygiene cannot depend on the store being decryptable.
	db, err := sql.Open("sqlite", te.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE secrets SET ciphertext = X'DEADBEEF'`); err != nil {
		t.Fatal(err)
	}

	if err := te.syncer.StripEnv(context.Background(), box); err != nil {
		t.Fatalf("StripEnv: %v", err)
	}
	got := te.readEnv(t, "boxa")
	want := bakedEnv + BlockBegin + "\n" + BlockEnd + "\n"
	if got != want {
		t.Fatalf("env file after strip = %q, want baked content + empty block %q", got, want)
	}
	if strings.Contains(got, "s3kr3t") {
		t.Fatalf("secret value survived strip:\n%s", got)
	}
}

func TestStripEnvClearsCapturedGuestIdentityAndJournal(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	guestRoot := filepath.Join(te.dir, "mock-vms", "boxa")
	writeGuest := func(rel, content string) {
		t.Helper()
		path := filepath.Join(guestRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeGuest("etc/machine-id", "parent-machine-id\n")
	writeGuest("var/lib/dbus/machine-id", "parent-machine-id\n")
	writeGuest("var/log/journal/parent-machine-id/system.journal", "parent logs")
	writeGuest("var/log/journal/parent-machine-id/user-1000.journal", "parent user logs")

	if err := te.syncer.StripEnv(context.Background(), box); err != nil {
		t.Fatalf("StripEnv: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(guestRoot, "etc/machine-id")); err != nil || len(got) != 0 {
		t.Fatalf("machine-id after strip = %q, %v; want an existing empty file", got, err)
	}
	if _, err := os.Stat(filepath.Join(guestRoot, "var/lib/dbus/machine-id")); !os.IsNotExist(err) {
		t.Fatalf("dbus machine-id survived strip (stat err = %v)", err)
	}
	entries, err := os.ReadDir(filepath.Join(guestRoot, "var/log/journal"))
	if err != nil {
		t.Fatalf("read journal directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("journal directory contains %v after strip, want empty", entries)
	}
}

func TestPushEnvDoesNotClearLiveGuestIdentityOrJournal(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	guestRoot := filepath.Join(te.dir, "mock-vms", "boxa")
	journal := filepath.Join(guestRoot, "var/log/journal/machine/system.journal")
	if err := os.MkdirAll(filepath.Dir(journal), 0o755); err != nil {
		t.Fatal(err)
	}
	machineID := filepath.Join(guestRoot, "etc/machine-id")
	if err := os.MkdirAll(filepath.Dir(machineID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(machineID, []byte("live-machine-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("live logs"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("PushEnv: %v", err)
	}
	if got, err := os.ReadFile(machineID); err != nil || string(got) != "live-machine-id\n" {
		t.Fatalf("ordinary push changed machine-id to %q, %v", got, err)
	}
	if got, err := os.ReadFile(journal); err != nil || string(got) != "live logs" {
		t.Fatalf("ordinary push changed journal to %q, %v", got, err)
	}
}

func TestStripEnvUnreachableBoxIsAnError(t *testing.T) {
	te := newTestEnv(t)
	te.create(t, "boxa", "alice")
	if err := te.mgr.Pause(context.Background(), "boxa"); err != nil {
		t.Fatal(err)
	}
	box, ok := te.mgr.Get("boxa")
	if !ok || box.State != vmm.StatePaused {
		t.Fatalf("want paused box, got %+v", box)
	}

	// A skip here would let Archive/Snapshot pack an uncleared rootfs, so an
	// unreachable box must fail loudly (the manager wakes boxes before calling).
	if err := te.syncer.StripEnv(context.Background(), box); err == nil {
		t.Fatal("StripEnv on paused box returned nil, want error")
	}
	if _, err := os.Stat(te.envFile("boxa")); !os.IsNotExist(err) {
		t.Fatalf("paused box's env file was touched (stat err = %v)", err)
	}
}

func TestStripEnvDanglingBeginStripsToEOF(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatal(err)
	}
	// Hand-delete the end marker: the value lines now dangle after the begin
	// marker. Strip-to-EOF provably removes them, so the strip succeeds.
	corrupted := strings.Replace(te.readEnv(t, "boxa"), BlockEnd+"\n", "", 1)
	te.seedEnv(t, "boxa", corrupted)

	if err := te.syncer.StripEnv(context.Background(), box); err != nil {
		t.Fatalf("StripEnv: %v", err)
	}
	got := te.readEnv(t, "boxa")
	want := bakedEnv + BlockBegin + "\n" + BlockEnd + "\n"
	if got != want {
		t.Fatalf("env file after strip = %q, want %q", got, want)
	}
}

func TestStripEnvFailsClosedOnOrphanEndMarker(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatal(err)
	}
	// Hand-delete the begin marker: the secret line before the orphan end
	// marker is invisible to the rewrite, so a strip cannot prove the block
	// is gone and must fail — Archive/Snapshot abort instead of packing a
	// rootfs that still carries the value.
	corrupted := strings.Replace(te.readEnv(t, "boxa"), BlockBegin+"\n", "", 1)
	te.seedEnv(t, "boxa", corrupted)

	if err := te.syncer.StripEnv(context.Background(), box); err == nil {
		t.Fatal("StripEnv on orphan end marker returned nil, want error")
	}
	if got := te.readEnv(t, "boxa"); got != corrupted {
		t.Fatalf("failed strip modified the file:\nbefore %q\nafter  %q", corrupted, got)
	}
	// The trap must have removed the partially written tmp file.
	strays, err := filepath.Glob(filepath.Join(te.dir, "mock-vms", "boxa", "environment.sparkbox.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(strays) != 0 {
		t.Fatalf("tmp files left behind after failed strip: %v", strays)
	}
}

func TestPushEnvSweepsStaleTmpFiles(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")
	// A tmp file stranded by a past crash (SIGKILL, power loss) — no trap
	// can clean those, so each push sweeps before writing.
	stray := filepath.Join(te.dir, "mock-vms", "boxa", "environment.sparkbox.12345")
	if err := os.WriteFile(stray, []byte(`LEAKED="s3kr3t"`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("PushEnv: %v", err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("stale tmp file not swept (stat err = %v)", err)
	}
}

func TestStripEnvQuiescesSyncOwnerUntilLifecyclePush(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")
	stripped := bakedEnv + BlockBegin + "\n" + BlockEnd + "\n"

	if err := te.syncer.StripEnv(context.Background(), box); err != nil {
		t.Fatalf("StripEnv: %v", err)
	}
	if got := te.readEnv(t, "boxa"); got != stripped {
		t.Fatalf("env file after strip = %q, want %q", got, stripped)
	}

	// A change-time push after the strip must skip the quiesced box: an
	// in-flight SyncOwner delivery landing between strip and pack would put
	// plaintext secrets into the archive/template.
	te.syncer.SyncOwner(context.Background(), "alice")
	te.syncer.wg.Wait()
	if got := te.readEnv(t, "boxa"); got != stripped {
		t.Fatalf("SyncOwner pushed into a quiesced box:\n%s", got)
	}

	// The manager's lifecycle push (fired on the next transition to running)
	// lifts the quiesce and delivers.
	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("PushEnv: %v", err)
	}
	if got := te.readEnv(t, "boxa"); !strings.Contains(got, `API_KEY="s3kr3t"`) {
		t.Fatalf("lifecycle push did not deliver:\n%s", got)
	}
	te.syncer.SyncOwner(context.Background(), "alice")
	te.syncer.wg.Wait()
	if got := te.readEnv(t, "boxa"); !strings.Contains(got, `API_KEY="s3kr3t"`) {
		t.Fatalf("SyncOwner still skipping after lifecycle push:\n%s", got)
	}
}

func TestStripEnvWinsOverConcurrentSyncOwner(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")

	// Whatever the interleaving, per-box serialization plus the quiesce flag
	// guarantee the strip's empty block is the final content: an in-flight
	// push either completes before the strip or observes quiesced and skips.
	te.syncer.SyncOwner(context.Background(), "alice")
	if err := te.syncer.StripEnv(context.Background(), box); err != nil {
		t.Fatalf("StripEnv: %v", err)
	}
	te.syncer.wg.Wait()

	got := te.readEnv(t, "boxa")
	want := bakedEnv + BlockBegin + "\n" + BlockEnd + "\n"
	if got != want {
		t.Fatalf("env file after concurrent push+strip = %q, want %q", got, want)
	}
}

func TestDeliverBoundedByContextOnWedgedGuest(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")
	// A guest whose exec never exits: the ctx deadline must cover the exec,
	// not just the dial, or the push goroutine leaks forever.
	te.syncer.shell = "sleep 15"

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	start := time.Now()
	err := te.syncer.PushEnv(ctx, box)
	if err == nil {
		t.Fatal("PushEnv against a wedged guest returned nil, want error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("PushEnv not bounded by ctx: took %s", elapsed)
	}
}

func TestDeliverCapsGuestOutput(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")
	// A guest that floods stdout before failing: only a bounded prefix may
	// reach the error message (and host memory).
	te.syncer.shell = "sh -c 'head -c 262144 /dev/zero; exit 3'"

	err := te.syncer.PushEnv(context.Background(), box)
	if err == nil {
		t.Fatal("PushEnv returned nil, want exec error")
	}
	if len(err.Error()) > 2*maxExecOutput {
		t.Fatalf("error message not capped: %d bytes", len(err.Error()))
	}
}

func TestSanitizeEnv(t *testing.T) {
	te := newTestEnv(t)
	env := map[string]string{
		"PATH":            "/evil/bin",
		"LD_PRELOAD":      "/evil/hook.so",
		"LD_LIBRARY_PATH": "/evil/lib",
		"API_KEY":         "s3kr3t",
	}
	got, err := te.syncer.sanitizeEnv("boxa", env)
	if err != nil {
		t.Fatalf("sanitizeEnv: %v", err)
	}
	if len(got) != 1 || got["API_KEY"] != "s3kr3t" {
		t.Fatalf("reserved names not dropped: %v", got)
	}

	// pam_env truncates at '#' even inside quotes; delivering a truncated
	// value would be silent corruption, so the whole push must fail.
	if _, err := te.syncer.sanitizeEnv("boxa", map[string]string{"DB_PASS": "abc#def"}); err == nil {
		t.Fatal("sanitizeEnv accepted a value containing '#', want error")
	}
}

func TestSyncOwnerPushesRunningBoxesOnly(t *testing.T) {
	te := newTestEnv(t)
	te.create(t, "runa", "alice")
	te.create(t, "pausa", "alice")
	te.create(t, "bobbox", "bob")
	for _, name := range []string{"runa", "pausa", "bobbox"} {
		te.seedEnv(t, name, bakedEnv)
	}
	te.tagAndPut(t, "alice", "runa", "web", "API_KEY", "s3kr3t")
	if err := te.store.SetTags("pausa", "alice", []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if err := te.mgr.Pause(context.Background(), "pausa"); err != nil {
		t.Fatal(err)
	}

	te.syncer.SyncOwner(context.Background(), "alice")
	te.syncer.wg.Wait()

	if got := te.readEnv(t, "runa"); !strings.Contains(got, `API_KEY="s3kr3t"`) {
		t.Fatalf("running box not pushed:\n%s", got)
	}
	if got := te.readEnv(t, "pausa"); got != bakedEnv {
		t.Fatalf("paused box was pushed:\n%s", got)
	}
	if box, _ := te.mgr.Get("pausa"); box.State != vmm.StatePaused {
		t.Fatalf("SyncOwner woke a paused box: state = %s", box.State)
	}
	if got := te.readEnv(t, "bobbox"); got != bakedEnv {
		t.Fatalf("another owner's box was pushed:\n%s", got)
	}
}
