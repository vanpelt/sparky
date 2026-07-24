// Package nodes is the fleet's node roster: an SSH public key, a node name and
// an approval status, in the same sqlite file as the users and routes stores.
//
// It is deliberately separate from internal/users. A node is a machine, not an
// account: it owns no sandboxes, has no invites and must never satisfy a user
// lookup, and a user key must never open the node door. Two tables with two
// lookups keep that true by construction rather than by a status column
// everybody has to remember to check.
//
// A row is created by the node itself on first contact (Enroll) and stays
// pending until an operator approves it out of band, having compared the
// fingerprint. That is the whole trust ceremony: the key is the identity, the
// name is a label, and approval is a human saying yes.
//
// Approval is keyed on the fingerprint (ApproveFP) and never on the name, which
// is what makes the sentence above true rather than aspirational. The name in a
// row is whatever the machine asked to be called; approving one would trust a
// string a stranger chose, and a stranger who enrols a name before the machine
// an operator is expecting gets blessed in its place.
package nodes

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	xssh "golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/reserved"
)

// Approval statuses. A row is born pending; approving it is the only thing that
// lets a link carry traffic, and disabling it is the only deprovisioning
// mechanism short of Remove.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusDisabled = "disabled"
)

// MaxPending bounds self-enrolment the way an invite quota bounds signup:
// anybody who can reach the SSH listener can offer a key, so without a ceiling
// the roster is an unauthenticated write endpoint.
const MaxPending = 32

var (
	// ErrNoSuchNode is returned when an operation targets a name with no row.
	ErrNoSuchNode = errors.New("no such node")
	// ErrNameTaken is returned when a name is already registered to a different
	// key. Names are labels; keys are identities, so the key wins.
	ErrNameTaken = errors.New("that node name is registered to a different key")
	// ErrTooManyPending is returned once MaxPending rows await approval.
	ErrTooManyPending = errors.New("too many nodes are awaiting approval")
	// ErrBadName is returned for a name that fails ValidName.
	ErrBadName = errors.New("invalid node name")
)

// ValidName reports whether s is usable as a node name. It is the platform's
// one label rule: node names appear in the gateway's synthetic sandbox
// addresses (<sandbox>.<node>.…), so anything that is not label-safe there
// would produce an address nothing can parse — the same reason a sandbox name
// is bounded the same way, which is why both ask internal/reserved.
func ValidName(s string) bool { return reserved.ValidLabel(s) }

// Node is one machine in the fleet.
//
// Wire is unexported to JSON on purpose: it is the credential the node proves
// possession of, and no console, API or ctl@ rendering has any use for it. FP
// is what an operator compares out of band before approving.
type Node struct {
	Name       string     `json:"name"`
	Wire       string     `json:"-"`
	FP         string     `json:"fp"`
	Status     string     `json:"status"`
	Arch       string     `json:"arch,omitempty"`
	Release    string     `json:"release,omitempty"`
	ApprovedBy string     `json:"approved_by,omitempty"`
	FirstSeen  time.Time  `json:"first_seen"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
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
		CREATE TABLE IF NOT EXISTS nodes (
			name        TEXT PRIMARY KEY,
			wire        TEXT NOT NULL,
			fp          TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'pending',
			arch        TEXT NOT NULL DEFAULT '',
			release     TEXT NOT NULL DEFAULT '',
			approved_by TEXT NOT NULL DEFAULT '',
			first_seen  TIMESTAMP NOT NULL,
			approved_at TIMESTAMP,
			last_seen   TIMESTAMP
		);
		CREATE UNIQUE INDEX IF NOT EXISTS nodes_wire ON nodes(wire);
		CREATE UNIQUE INDEX IF NOT EXISTS nodes_fp ON nodes(fp);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// wireOf is the exact-match lookup key for a public key: its SSH wire
// encoding, which is what the client proves possession of. A private copy of
// the same helper internal/users keeps, so the two key spaces cannot be
// accidentally joined through a shared function.
func wireOf(key xssh.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key.Marshal())
}

// Lookup returns the row for a key whatever its status — unlike users.Lookup,
// which filters to active accounts. The node door must be able to tell a
// pending node that it is pending, and a disabled one that it is disabled,
// which it cannot do if the store collapses both into "unknown". Status is the
// caller's decision.
func (s *Store) Lookup(key xssh.PublicKey) (Node, bool) {
	n, err := s.get(`SELECT `+cols+` FROM nodes WHERE wire = ?`, wireOf(key))
	if err != nil {
		return Node{}, false
	}
	return n, true
}

// PendingCount is how many rows are awaiting approval, i.e. what counts
// against MaxPending.
func (s *Store) PendingCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM nodes WHERE status = ?`, StatusPending).Scan(&n)
	return n, err
}

// Enroll records a node's first contact and returns its row.
//
// It is idempotent for the same key: a node that reconnects while still
// pending gets its own row back, unchanged and still pending, so a reconnect
// loop cannot burn through the MaxPending budget. The row's name is
// authoritative — a key that comes back asking for a different name is told
// what it is actually called rather than being renamed, because the operator
// approved a name they read off the roster.
func (s *Store) Enroll(name string, key xssh.PublicKey) (Node, error) {
	if !ValidName(name) {
		return Node{}, ErrBadName
	}
	wire := wireOf(key)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Node{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var existing string
	err = tx.QueryRow(`SELECT name FROM nodes WHERE wire = ?`, wire).Scan(&existing)
	switch {
	case err == nil:
		n, err := s.getTx(tx, `SELECT `+cols+` FROM nodes WHERE name = ?`, existing)
		if err != nil {
			return Node{}, err
		}
		return n, tx.Commit()
	case err != sql.ErrNoRows:
		return Node{}, err
	}

	var taken int
	err = tx.QueryRow(`SELECT 1 FROM nodes WHERE name = ?`, name).Scan(&taken)
	switch {
	case err == nil:
		return Node{}, ErrNameTaken
	case err != sql.ErrNoRows:
		return Node{}, err
	}

	// Counted inside the transaction: _txlock=immediate means the write lock is
	// already held, so two nodes enrolling at once cannot both read 31.
	var pending int
	if err := tx.QueryRow(`SELECT count(*) FROM nodes WHERE status = ?`, StatusPending).Scan(&pending); err != nil {
		return Node{}, err
	}
	if pending >= MaxPending {
		return Node{}, ErrTooManyPending
	}

	n := Node{
		Name:      name,
		Wire:      wire,
		FP:        xssh.FingerprintSHA256(key),
		Status:    StatusPending,
		FirstSeen: time.Now().UTC(),
	}
	if _, err := tx.Exec(
		`INSERT INTO nodes (name, wire, fp, status, first_seen) VALUES (?, ?, ?, ?, ?)`,
		n.Name, n.Wire, n.FP, n.Status, n.FirstSeen); err != nil {
		return Node{}, err
	}
	return n, tx.Commit()
}

// ApproveFP blesses a pending node, keyed on the fingerprint of the key that
// node proves possession of. by is the operator's handle, recorded so the
// roster answers "who let this machine in" months later.
//
// The fingerprint rather than the name is the whole security of the ceremony. A
// name is chosen by the machine enrolling — the gateway has no way to check it
// against anything — so `approve gpu-01` asks an operator to trust a string a
// stranger picked, and a stranger who enrols the name first is the one who gets
// blessed. A fingerprint is derived from the key itself and cannot be claimed:
// the operator reads it off the machine's own console, compares it against the
// roster, and what they type can only ever approve the machine holding that key.
//
// An empty fp is refused rather than run as a query. The gateway's own entry in
// a listing carries no fingerprint, and a WHERE that matched the empty string
// would let a malformed call bless whatever row happened to have one.
func (s *Store) ApproveFP(fp, by string) error {
	if fp == "" {
		return ErrNoSuchNode
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`UPDATE nodes SET status = ?, approved_by = ?, approved_at = ? WHERE fp = ?`,
		StatusApproved, by, time.Now().UTC(), fp)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchNode
	}
	return nil
}

// GetByFP returns the row whose key has this fingerprint.
func (s *Store) GetByFP(fp string) (Node, error) {
	if fp == "" {
		return Node{}, ErrNoSuchNode
	}
	n, err := s.get(`SELECT `+cols+` FROM nodes WHERE fp = ?`, fp)
	if err == sql.ErrNoRows {
		return Node{}, ErrNoSuchNode
	}
	return n, err
}

// Remove deletes a row. The node may enrol again afterwards — removal revokes
// the approval, it does not blacklist the key.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM nodes WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchNode
	}
	return nil
}

// Seen records what a node told us about itself on its last connection. These
// columns are node-authored and display-only: nothing authorizes on them.
func (s *Store) Seen(name, arch, release string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`UPDATE nodes SET arch = ?, release = ?, last_seen = ? WHERE name = ?`,
		arch, release, at.UTC(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchNode
	}
	return nil
}

// Get returns one node row.
func (s *Store) Get(name string) (Node, error) {
	n, err := s.get(`SELECT `+cols+` FROM nodes WHERE name = ?`, name)
	if err == sql.ErrNoRows {
		return Node{}, ErrNoSuchNode
	}
	return n, err
}

// List returns every node, name-sorted.
func (s *Store) List() ([]Node, error) {
	rows, err := s.db.Query(`SELECT ` + cols + ` FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// cols is the column list every read shares, so scan can be written once.
const cols = `name, wire, fp, status, arch, release, approved_by, first_seen, approved_at, last_seen`

func (s *Store) get(q string, args ...any) (Node, error) {
	return scan(s.db.QueryRow(q, args...))
}

func (s *Store) getTx(tx *sql.Tx, q string, args ...any) (Node, error) {
	return scan(tx.QueryRow(q, args...))
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scan(row scanner) (Node, error) {
	var n Node
	var approvedAt, lastSeen sql.NullTime
	if err := row.Scan(&n.Name, &n.Wire, &n.FP, &n.Status, &n.Arch, &n.Release,
		&n.ApprovedBy, &n.FirstSeen, &approvedAt, &lastSeen); err != nil {
		return Node{}, err
	}
	if approvedAt.Valid {
		t := approvedAt.Time
		n.ApprovedAt = &t
	}
	if lastSeen.Valid {
		t := lastSeen.Time
		n.LastSeen = &t
	}
	return n, nil
}
