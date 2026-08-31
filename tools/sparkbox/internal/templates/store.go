// Package templates is the sqlite-backed store of per-tag base images: a
// binding says "a sandbox carrying this tag boots from this snapshot", a tag
// connects bindings to sandboxes, and BindingsForTags answers the one question
// the create path asks — which disk does this sandbox start from. It mirrors
// internal/repos and internal/netrules (same DB file, own connection, WAL,
// pure-Go driver, tagRe copied verbatim) minus any secret material: a snapshot
// name is a pointer to a file in this machine's image directory, not a
// credential, so it is stored in plaintext.
//
// The sandbox_tags table is shared with internal/secrets; templates only reads
// it. Whichever package opens the DB first creates the table (all four use
// CREATE TABLE IF NOT EXISTS with a byte-identical DDL), and secrets owns the
// tag mutations (SetTags/DeleteBySandbox/RenameSandbox). If this package ever
// writes a sandbox_tags row, secrets' in-transaction cross-owner refusal is
// bypassed and the invariant that a tag belongs to exactly one handle is gone.
//
// What makes this store different from the other three readers is that a
// binding REPLACES. Two tags on a sandbox mean the union of two secret sets and
// the union of two repo lists; internal/netrules is the exception that
// subtracts. A sandbox has exactly one rootfs, so two tags binding two
// snapshots have no answer that is not a coin flip. That is why the tag IS the
// primary key here — an owner cannot accumulate two templates on one tag — and
// why the ambiguity refusal for two DIFFERENT tags lives in ctlops rather than
// in a precedence rule in this file: a precedence rule means somebody gets a
// sandbox with the wrong CUDA in it and finds out twenty minutes later.
package templates

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

// ErrNoSuchBinding is returned when an operation targets a tag the owner has no
// binding for. It maps to 404 via the console's statusFor convention, and it is
// deliberately the same answer for another owner's tag as for a tag nobody has
// bound — the query carries the owner, so the two are indistinguishable here as
// well as on the wire.
var ErrNoSuchBinding = errors.New("no such template binding")

// ErrInvalidBinding wraps every validation failure (bad tag, snapshot name, a
// port outside 1-65535, the `default` refusal, or a full binding list) so
// callers can map the whole family to 400 without message matching.
var ErrInvalidBinding = errors.New("invalid template binding")

// tagRe matches internal/secrets so the tag namespaces align exactly.
var tagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// snapNameRe is a copy of host.snapNameRe (internal/host/snapshot.go:16), the
// grammar of the user-facing snapshot name a binding points at. It is one
// character longer than tagRe and that difference is deliberate rather than a
// typo waiting to be tidied: the two namespaces are separate, a snapshot name
// becomes snap-<owner>-<name>.ext4 on disk while a tag never becomes anything,
// and folding them into one regexp here would silently start refusing a
// 41-character snapshot the host is perfectly willing to hold.
var snapNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// maxTemplatesPerOwner caps an owner's binding list. Bindings are a routing
// table from tag to base image, not an inventory of snapshots — an owner with
// sixty-four distinct boot disks has a naming problem, not a quota problem —
// and the cap keeps a runaway script from turning the create path's tag lookup
// into a scan.
const maxTemplatesPerOwner = 64

// Binding is the listable shape of a tag-to-template binding. None of it is
// sensitive: the snapshot name is already printed by `snapshot ls` to the owner
// who took it, and a binding is only ever read back under its own owner.
type Binding struct {
	Owner     string    `json:"owner"`    // sparkbox handle
	Tag       string    `json:"tag"`      // the sandbox_tags word that selects it
	Snapshot  string    `json:"snapshot"` // the owner's snapshot name, not the on-disk image
	CreatedAt time.Time `json:"created_at"`
}

// Store is the template-bindings database handle.
type Store struct {
	mu  sync.Mutex // serialises writes (sqlite is single-writer)
	db  *sql.DB
	log *slog.Logger
}

// Open opens (creating if needed) the sqlite database at path and applies the
// schema. It shares the file with internal/secrets, internal/netrules,
// internal/repos and internal/routes on its own connection; WAL keeps that
// safe. See internal/secrets for the DSN rationale — the short version is that
// the _pragma DSN params run on every pooled connection where a db.Exec pragma
// binds to only one, and _txlock=immediate takes the write lock up front so the
// read-then-write upsert in Bind cannot hit SQLITE_BUSY_SNAPSHOT, which the
// busy handler never waits on. Every Begin in this store writes.
func Open(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// The explicit Execs are redundant with the DSN but make an unsupported
	// pragma fail Open loudly instead of surfacing later.
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close() //nolint:errcheck
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	// The sandbox_tags block is a literal copy of the one in
	// internal/secrets, internal/netrules and internal/repos. Four packages
	// now race to create it and no coordination is needed — but if the
	// copies ever drift, the loser of the race silently gets the other's
	// schema, so this must stay byte-identical rather than merely
	// equivalent.
	//
	// template_tags has no id column, unlike every other table in this
	// family. A repo or a secret is a thing an owner accumulates and a row
	// id names one of them; a binding is a function from tag to snapshot,
	// so (owner, tag) IS its identity and a second row for the same tag is
	// the state this design exists to make unrepresentable.
	//
	// snapshot stores the user-facing name rather than the snap-<owner>-<name>
	// image basename. The image name is derived from the pair and can be
	// recomputed; the name is what the owner typed, what `snapshot ls`
	// prints, and what an error has to say back to them.
	//
	// snapshot_ports is keyed on the snapshot rather than the tag, and that is
	// the whole reason it is a second table instead of a column on the first.
	// The port is a fact about the CAPTURE — it is read off the source
	// sandbox's route in the same breath as the rootfs is compacted — while a
	// binding is made later, possibly much later, by which time the source box
	// may be gone or serving something else. Hanging it off the tag would mean
	// `snapshot create` had nowhere to put it and `snapshot bind` had to guess.
	// It also has no created_at for the same reason: it is a property of a
	// snapshot, not an event in its life.
	//
	// A row is written only when the source's port differs from the stock
	// default, so absence means "whatever this host serves by default" rather
	// than "unknown". The two are indistinguishable in effect — a box that
	// explicitly serves 8000 and a box nobody touched both want 8000 — which is
	// what makes the sparse encoding lossless.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS sandbox_tags (
			sandbox    TEXT NOT NULL,
			owner      TEXT NOT NULL,
			tag        TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			PRIMARY KEY (sandbox, tag)
		);
		CREATE INDEX IF NOT EXISTS sandbox_tags_owner ON sandbox_tags(owner);
		CREATE INDEX IF NOT EXISTS sandbox_tags_tag   ON sandbox_tags(owner, tag);

		CREATE TABLE IF NOT EXISTS template_tags (
			owner      TEXT NOT NULL,
			tag        TEXT NOT NULL,
			snapshot   TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			PRIMARY KEY (owner, tag)
		);
		CREATE INDEX IF NOT EXISTS template_tags_snapshot ON template_tags(owner, snapshot);

		CREATE TABLE IF NOT EXISTS snapshot_ports (
			owner    TEXT NOT NULL,
			snapshot TEXT NOT NULL,
			port     INTEGER NOT NULL,
			PRIMARY KEY (owner, snapshot)
		);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Store{db: db, log: log}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Bind points the owner's tag at a snapshot, replacing whatever it pointed at
// before, and returns the new binding along with the snapshot it replaced ("" if
// the tag was unbound). The previous snapshot is returned rather than discarded
// because a re-point is otherwise completely silent: the person typing `bind`
// gets the same success line whether they created a binding or quietly changed
// what every future box on that tag boots from, and the caller needs the old
// value to say which.
//
// created_at is refreshed on a re-point — deliberately unlike repos.PutRepo,
// which preserves it because the attachment is the same attachment. Here the
// question the column answers is "since when has this tag booted from THIS
// snapshot", and the moment the snapshot changes the old answer is wrong.
//
// The store does not check that the snapshot exists or that the owner may fork
// it. That is ctlops' ownedSnapshot gate, which has the sandbox store; here it
// would be a second, weaker copy of an ownership check. What this refuses is a
// binding that could never be correct no matter who asked for it.
func (s *Store) Bind(owner, tag, snapshot string) (Binding, string, error) {
	if owner == "" {
		return Binding{}, "", fmt.Errorf("%w: binding needs an owner", ErrInvalidBinding)
	}
	tag = strings.TrimSpace(tag)
	snapshot = strings.TrimSpace(snapshot)
	// The `default` check folds case and runs BEFORE the grammar check.
	// "DEFAULT" would be refused either way — tagRe has no uppercase — but
	// by a message about the character set, which says nothing about why
	// this particular word is the one that cannot be bound. ctlops
	// lowercases tags before they reach a store (parse.go:152); a transport
	// that forgets to still gets the explanation rather than a spelling
	// lecture.
	//
	// internal/netrules refuses the same word for the same shape of reason
	// (store.go:204-210), and this refusal is likewise what keeps
	// ctlops.Create free to keep stamping `default` on every sandbox it
	// makes. The blast radius is narrower than netrules': template_tags is
	// keyed (owner, tag) and every sandbox_tags join in this tree carries
	// the owner on both sides, so a `default` binding by alice could never
	// have reached bob's boxes. It reaches all of alice's, forever, which is
	// quite bad enough. The legitimate version of "change the base image for
	// everyone" already exists and is an operator knob: ctlops' DefaultImage.
	if strings.EqualFold(tag, secrets.DefaultTag) {
		return Binding{}, "", fmt.Errorf(
			"%w: a template cannot be bound to the %q tag — every sandbox you create carries "+
				"that tag, so this snapshot would silently become the base image for all of "+
				"them, including ones you make months from now. Bind it to a name you also "+
				"put on the sandboxes you mean to boot from it.",
			ErrInvalidBinding, secrets.DefaultTag)
	}
	if !tagRe.MatchString(tag) {
		return Binding{}, "", fmt.Errorf("%w: tag %q (want [a-z0-9][a-z0-9-]*, max 40 chars)", ErrInvalidBinding, tag)
	}
	if !snapNameRe.MatchString(snapshot) {
		return Binding{}, "", fmt.Errorf("%w: snapshot %q (want [a-z0-9][a-z0-9-]*, max 41 chars)", ErrInvalidBinding, snapshot)
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Binding{}, "", err
	}
	defer tx.Rollback() //nolint:errcheck

	var prev string
	err = tx.QueryRow(`SELECT snapshot FROM template_tags WHERE owner = ? AND tag = ?`, owner, tag).Scan(&prev)
	switch {
	case err == sql.ErrNoRows:
		// The cap is checked inside the transaction, on the insert path
		// only: a re-point must never start failing because the list is
		// full, or an owner at the cap could not move a tag off a
		// snapshot they had just deleted.
		prev = ""
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM template_tags WHERE owner = ?`, owner).Scan(&n); err != nil {
			return Binding{}, "", err
		}
		if n >= maxTemplatesPerOwner {
			return Binding{}, "", fmt.Errorf("%w: too many template bindings (%d, max %d)", ErrInvalidBinding, n, maxTemplatesPerOwner)
		}
		if _, err := tx.Exec(`
			INSERT INTO template_tags (owner, tag, snapshot, created_at)
			VALUES (?, ?, ?, ?)`,
			owner, tag, snapshot, now); err != nil {
			return Binding{}, "", err
		}
	case err != nil:
		return Binding{}, "", err
	default:
		if _, err := tx.Exec(`UPDATE template_tags SET snapshot = ?, created_at = ? WHERE owner = ? AND tag = ?`,
			snapshot, now, owner, tag); err != nil {
			return Binding{}, "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return Binding{}, "", err
	}
	return Binding{Owner: owner, Tag: tag, Snapshot: snapshot, CreatedAt: now}, prev, nil
}

// Unbind removes the owner's binding for tag and returns what it was, or
// ErrNoSuchBinding. The binding comes back because the caller has to be able to
// print which snapshot the tag stopped pointing at — an unbind that only says
// "ok" is indistinguishable from an unbind of the wrong tag.
//
// Unbinding touches no sandbox and no snapshot. Boxes already created from the
// template keep running on their own reflinked rootfs; the next create on that
// tag falls back to the operator default image.
func (s *Store) Unbind(owner, tag string) (Binding, error) {
	if owner == "" {
		return Binding{}, fmt.Errorf("%w: binding needs an owner", ErrInvalidBinding)
	}
	tag = strings.TrimSpace(tag)
	if !tagRe.MatchString(tag) {
		return Binding{}, fmt.Errorf("%w: tag %q (want [a-z0-9][a-z0-9-]*, max 40 chars)", ErrInvalidBinding, tag)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Binding{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var b Binding
	err = tx.QueryRow(`SELECT owner, tag, snapshot, created_at FROM template_tags WHERE owner = ? AND tag = ?`,
		owner, tag).Scan(&b.Owner, &b.Tag, &b.Snapshot, &b.CreatedAt)
	if err == sql.ErrNoRows {
		return Binding{}, ErrNoSuchBinding
	}
	if err != nil {
		return Binding{}, err
	}
	if _, err := tx.Exec(`DELETE FROM template_tags WHERE owner = ? AND tag = ?`, owner, tag); err != nil {
		return Binding{}, err
	}
	if err := tx.Commit(); err != nil {
		return Binding{}, err
	}
	return b, nil
}

// BindingsForOwner returns every binding the owner holds, ordered by tag — the
// listing behind `snapshot ls`'s tags column and the source of the bound-tags
// set a delete has to refuse against.
func (s *Store) BindingsForOwner(owner string) ([]Binding, error) {
	rows, err := s.db.Query(`
		SELECT owner, tag, snapshot, created_at
		FROM template_tags WHERE owner = ? ORDER BY tag`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBindings(rows)
}

// BindingsForTags returns the owner's bindings for the given tags, ordered by
// tag. This is the create path's entry point: ctlops hands it the tags it has
// already computed for a sandbox that does not exist yet, which is why this is
// a plain lookup and not a sandbox_tags join — stampTags has not run and must
// not run before the refusal, so a join would answer "no template" for every
// create.
//
// The ordering is not cosmetic. When two tags resolve to two different
// snapshots the caller refuses the create and names both, and that message has
// to read the same way every time it is printed or the same mistake produces
// two different-looking errors.
//
// An empty tag list answers nil without touching the database — the overwhelmingly
// common case is a create with no tags at all, and an untagged create must not
// pay a query to be told it has no template.
func (s *Store) BindingsForTags(owner string, tags []string) ([]Binding, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	// Tags are not validated here: an unbindable tag simply matches no row,
	// and a create whose tag happens to be spelled oddly must fall back to
	// the default image rather than fail. The list is already bounded by
	// ctlops.MaxTagsPerSandbox, so the placeholder expansion cannot approach
	// sqlite's parameter limit.
	args := make([]any, 0, len(tags)+1)
	args = append(args, owner)
	holes := make([]string, 0, len(tags))
	for _, tag := range tags {
		holes = append(holes, "?")
		args = append(args, tag)
	}
	rows, err := s.db.Query(`
		SELECT owner, tag, snapshot, created_at
		FROM template_tags WHERE owner = ? AND tag IN (`+strings.Join(holes, ",")+`)
		ORDER BY tag`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBindings(rows)
}

// TemplatesForSandbox returns the bindings that a sandbox's own tags reach —
// "which template would this box boot from if it were created now". It is the
// display and debug view; the create path uses BindingsForTags, because the
// sandbox does not exist yet at the moment the image is chosen.
//
// Owner scoping is structural: the join requires bt.owner = tt.owner AND
// tt.owner = ?. The first term looks redundant next to the second and is not.
// Without it, any tag name two people happen to share (ci, prod, dev) joins
// their rows together, and what comes back is another handle's snapshot name —
// which is both an information leak and, if anything downstream ever trusted
// it, a boot from a disk the owner has no claim on. No caller-side check
// replaces this; it has to be in the SQL, which is what
// TestTemplatesForSandboxJoinsOnTagsAndScopesByOwner exists to keep true.
func (s *Store) TemplatesForSandbox(sandbox, owner string) ([]Binding, error) {
	rows, err := s.db.Query(`
		SELECT tt.owner, tt.tag, tt.snapshot, tt.created_at
		FROM template_tags tt
		JOIN sandbox_tags bt ON bt.tag = tt.tag AND bt.owner = tt.owner
		WHERE bt.sandbox = ? AND tt.owner = ?
		ORDER BY tt.tag`, sandbox, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBindings(rows)
}

// ---------------------------------------------------------------------------
// Snapshot ports
// ---------------------------------------------------------------------------
//
// The port a snapshot's source sandbox served on, so a sandbox booted from the
// template lands on it too. A snapshot is a rootfs and the default port is a
// row in internal/routes, so without this the two halves of "it worked on the
// box I captured" cannot travel together: the disk carries the dev server, the
// route carries the 8000 nobody is listening on.
//
// It lives on the GATEWAY, next to the bindings, and not on host.Snapshot. On a
// fleet the snapshot record belongs to the node that holds the template file —
// the gateway caches it and re-reads it from the node on every reconnect (see
// fleet.remoteNode.Snapshotter) — so a field stamped here would evaporate, and
// making it survive would mean carrying a port through nodelink and the node
// proto. Routes are gateway state even for a sandbox running on another
// machine, so the fact never needs to leave this process.

// SetSnapshotPort records the default port a snapshot was captured on,
// replacing any port already recorded for it. Callers record only a port that
// differs from the host default; see the schema comment in Open.
//
// Re-capturing under a name that already exists overwrites rather than
// accumulates, which is what keeps a deleted-and-retaken name from inheriting
// the port of the template it replaced.
func (s *Store) SetSnapshotPort(owner, snapshot string, port int) error {
	if owner == "" {
		return fmt.Errorf("%w: a snapshot port needs an owner", ErrInvalidBinding)
	}
	snapshot = strings.TrimSpace(snapshot)
	if !snapNameRe.MatchString(snapshot) {
		return fmt.Errorf("%w: snapshot %q (want [a-z0-9][a-z0-9-]*, max 41 chars)", ErrInvalidBinding, snapshot)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: port %d (want 1-65535)", ErrInvalidBinding, port)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO snapshot_ports (owner, snapshot, port) VALUES (?, ?, ?)
		ON CONFLICT(owner, snapshot) DO UPDATE SET port = excluded.port`,
		owner, snapshot, port)
	return err
}

// SnapshotPort returns the port recorded for the owner's snapshot, or 0 when
// none is — which is every snapshot captured from a sandbox on the stock port,
// and every snapshot that predates this table.
//
// Zero rather than ErrNoSuchBinding: "no recorded port" is the ordinary case
// and not a condition any caller wants to branch on twice, and 0 is not a port
// SetSnapshotPort will store.
func (s *Store) SnapshotPort(owner, snapshot string) (int, error) {
	var port int
	err := s.db.QueryRow(`SELECT port FROM snapshot_ports WHERE owner = ? AND snapshot = ?`,
		owner, snapshot).Scan(&port)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return port, nil
}

// SnapshotPorts returns every port this owner has recorded, keyed by snapshot
// name — the listing counterpart of SnapshotPort, so `snapshot ls` reads the
// table once instead of once per row.
//
// Only snapshots WITH a port appear. A caller indexing the map gets 0 for the
// ordinary snapshot that has none, which is the same answer SnapshotPort gives
// it, so the two can be swapped without a caller changing how it reads them.
func (s *Store) SnapshotPorts(owner string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT snapshot, port FROM snapshot_ports WHERE owner = ?`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var port int
		if err := rows.Scan(&name, &port); err != nil {
			return nil, err
		}
		out[name] = port
	}
	return out, rows.Err()
}

// ForgetSnapshotPort drops the row for a snapshot that is being deleted. A row
// left behind would be inherited by the next snapshot to take that name, which
// is a create landing on a port from a template its owner already threw away.
//
// Deleting a port nobody recorded is not an error: the overwhelmingly common
// snapshot has no row, and a delete path that had to know which kind it was
// deleting would have to ask first.
func (s *Store) ForgetSnapshotPort(owner, snapshot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM snapshot_ports WHERE owner = ? AND snapshot = ?`, owner, snapshot)
	return err
}

// scanBindings drains a binding query. Every query in this store selects the
// same four columns in the same order precisely so this can be one function.
func scanBindings(rows *sql.Rows) ([]Binding, error) {
	var out []Binding
	for rows.Next() {
		var b Binding
		if err := rows.Scan(&b.Owner, &b.Tag, &b.Snapshot, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN unless the column already
// exists. sqlite has no ADD COLUMN IF NOT EXISTS, and a bare ALTER errors on
// the second boot, so we consult table_info first. Unused until the first
// schema migration; kept as this store's own copy per the deliberate
// per-package duplication convention.
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

// Keeps the migration helper compiled until the first schema change needs it.
var _ = addColumnIfMissing
