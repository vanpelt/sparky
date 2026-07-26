// Package placement is the gateway's durable answer to "which machine holds
// this sandbox name". The PRIMARY KEY on name IS the fleet-wide name
// allocation: Reserve is a single INSERT, so a key conflict is the taken-name
// answer and no read-then-write window exists.
//
// sandboxes.json stays NODE-LOCAL truth. This table's owner and node columns
// are gateway-authored and are the only authorization inputs; its state column
// is a cache, and a stale one is a display artifact, never an authorization
// input.
//
// The store lives in sparkbox.db alongside routes, users and secrets, and is
// built to the same template: the same DSN, its own *sql.DB, and a mutex
// serialising writes because sqlite is single-writer.
package placement

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// A row's State says whether the gateway's picture of a name still matches the
// fleet's. Nothing authorizes on it. It is written by exactly one thing —
// reconciliation, the pass that compares a node's inventory against the rows
// placed on it (internal/fleet) — and it is a marker, never a cache of the
// sandbox's vmm state: a machine's own record is the only source of that, and
// writing "running" here would be the gateway inventing a state nobody
// observed. Orphaned rows are still served, flagged unreachable, and refused
// for mutation; quarantined rows are served to nobody.
const (
	StateOK         = ""           // the owning node has it and agrees
	StateOrphaned   = "orphaned"   // the node is up and does not have it
	StateQuarantine = "quarantine" // two nodes claim the name
)

// ErrTaken is returned when a name is already placed. It is the answer to
// "may I create this sandbox", so callers render it as a name collision
// rather than an internal failure.
var ErrTaken = errors.New("that sandbox name is already placed")

// ErrNoSuchRow is returned when an operation targets a name with no row.
var ErrNoSuchRow = errors.New("no such placement")

// Row is one name's placement. Owner and Node are gateway-authored; a node
// never gets to write either, which is what makes "a node lying can only
// affect its own sandboxes" structural rather than a review rule.
type Row struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	Node      string    `json:"node"`
	Image     string    `json:"image"`
	Arch      string    `json:"arch"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Store struct {
	mu sync.Mutex // serialises writes (sqlite is single-writer)
	db *sql.DB
}

// Open opens (creating if needed) the sqlite database at path and applies the
// schema.
func Open(path string) (*Store, error) {
	// DSN pragmas run on every pooled connection (a db.Exec pragma binds to
	// just one), and _txlock=immediate takes the write lock at Begin, where
	// busy_timeout applies — every transaction in this store writes. See the
	// fuller rationale in secrets.Open; the stores share sparkbox.db.
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// Redundant with the DSN, but an unsupported pragma fails Open loudly.
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
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS placements (
			name       TEXT PRIMARY KEY,
			owner      TEXT NOT NULL,
			node       TEXT NOT NULL,
			image      TEXT NOT NULL DEFAULT '',
			arch       TEXT NOT NULL DEFAULT '',
			state      TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		);
		CREATE INDEX IF NOT EXISTS placements_node  ON placements(node);
		CREATE INDEX IF NOT EXISTS placements_owner ON placements(owner);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const rowCols = `name, owner, node, image, arch, state, created_at, updated_at`

// Reserve claims a name for a node. It is the fleet's name allocator: the
// INSERT either wins the PRIMARY KEY or it does not, so two gateways racing
// the same name cannot both be told yes. DO NOTHING rather than a bare INSERT
// so the taken answer is a row count instead of a driver-specific error
// string.
func (s *Store) Reserve(name, owner, node, image, arch string) error {
	if name == "" || owner == "" || node == "" {
		return fmt.Errorf("placement needs a name, an owner and a node")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`
		INSERT INTO placements (`+rowCols+`)
		VALUES (?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(name) DO NOTHING`,
		name, owner, node, image, arch, now, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTaken
	}
	return nil
}

// Release drops a name so it can be reserved again. Releasing a name with no
// row succeeds: the caller wanted the name gone and it is, and a destroy that
// crashed halfway must be safe to re-run.
func (s *Store) Release(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM placements WHERE name = ?`, name)
	return err
}

// Rename moves a placement to a new name, keeping everything else — the
// sandbox has not moved machines, only changed what it is called. It is a
// DELETE plus an INSERT rather than an UPDATE of the primary key so the
// target's availability is decided by the same INSERT that claims it, inside
// the tx that frees the old name.
func (s *Store) Rename(old, new string) error {
	if new == "" {
		return fmt.Errorf("placement needs a name")
	}
	if old == new {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var r Row
	err = tx.QueryRow(`SELECT `+rowCols+` FROM placements WHERE name = ?`, old).
		Scan(&r.Name, &r.Owner, &r.Node, &r.Image, &r.Arch, &r.State, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return ErrNoSuchRow
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM placements WHERE name = ?`, old); err != nil {
		return err
	}
	res, err := tx.Exec(`
		INSERT INTO placements (`+rowCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO NOTHING`,
		new, r.Owner, r.Node, r.Image, r.Arch, r.State, r.CreatedAt, time.Now().UTC())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The rollback leaves the old row alone: a rename that lost the race
		// must not cost the caller the name it already had.
		return ErrTaken
	}
	return tx.Commit()
}

// SetNode repoints a name at another machine. Migration only — a sandbox's
// rootfs does not move on its own, so this is an operator moving it.
//
// Reconciliation deliberately does not call it. A machine reporting a name the
// ledger places elsewhere is exactly what an operator's half-finished migration
// looks like AND exactly what a wrong or compromised machine looks like, and
// moving the placement on that evidence would hand one machine another's
// sandbox. It marks the contradiction instead; deciding it is this, run by
// somebody who knows which disk has the data.
func (s *Store) SetNode(name, node string) error {
	if node == "" {
		return fmt.Errorf("placement needs a node")
	}
	return s.update(`UPDATE placements SET node = ?, updated_at = ? WHERE name = ?`, node, time.Now().UTC(), name)
}

// SetRowState records what reconciliation last found. It is a compare-and-set:
// the row is written only if it is still on the machine the caller read it on
// AND still in the state the caller read, and ErrNoSuchRow otherwise.
//
// The guard is not defensive tidiness, it is the whole correctness of the
// caller. Reconciliation reads a machine's rows once and then walks them,
// writing one statement per disagreement, with no lock held across the pass —
// it cannot hold one, since a pass over a recovered machine is up to
// MaxSandboxesPerNode separate transactions and dispatch runs on other
// goroutines throughout. So between the SELECT and any given UPDATE, a `ctl rm`
// can release a name and a create can re-reserve it on ANOTHER machine, for
// ANOTHER user. An update keyed on name alone would then stamp "orphaned" onto
// that brand-new placement, which refuses every mutation on it — resume
// included — and only clears when its own machine happens to send a full
// inventory, which may be never. Comparing node and state makes the stale write
// a no-op instead: zero rows affected means "that is not the row I read", which
// is exactly the wanted answer.
//
// A missing row is likewise a lost race with a destroy rather than an error
// worth escalating — but the caller is told, because it is also how a typo
// surfaces.
func (s *Store) SetRowState(name, node, want, state string) error {
	return s.update(
		`UPDATE placements SET state = ?, updated_at = ? WHERE name = ? AND node = ? AND state = ?`,
		state, time.Now().UTC(), name, node, want)
}

func (s *Store) update(q string, args ...any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchRow
	}
	return nil
}

// Get returns the placement for a name, if any.
func (s *Store) Get(name string) (Row, bool, error) {
	var r Row
	err := s.db.QueryRow(`SELECT `+rowCols+` FROM placements WHERE name = ?`, name).
		Scan(&r.Name, &r.Owner, &r.Node, &r.Image, &r.Arch, &r.State, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, err
	}
	return r, true, nil
}

// List returns every placement.
func (s *Store) List() ([]Row, error) {
	return s.query(`SELECT ` + rowCols + ` FROM placements ORDER BY name`)
}

// ByNode returns every placement on one machine. This is what a node's
// inventory is reconciled against.
func (s *Store) ByNode(node string) ([]Row, error) {
	return s.query(`SELECT `+rowCols+` FROM placements WHERE node = ? ORDER BY name`, node)
}

// ByOwner returns every placement belonging to a handle.
func (s *Store) ByOwner(owner string) ([]Row, error) {
	return s.query(`SELECT `+rowCols+` FROM placements WHERE owner = ? ORDER BY name`, owner)
}

func (s *Store) query(q string, args ...any) ([]Row, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Name, &r.Owner, &r.Node, &r.Image, &r.Arch, &r.State, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
