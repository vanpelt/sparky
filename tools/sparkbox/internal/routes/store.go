// Package routes is the sqlite-backed store of HTTP proxy routes: each row
// maps a subdomain (e.g. "myvm" under hivemind.tools) to a sandbox and the guest
// port to forward to. The proxy edge (internal/proxy) reads it to route
// requests; the host manager writes a default route per sandbox and deletes
// them on destroy.
//
// State lives in a single sqlite file so it survives restarts alongside the
// sandbox records. The driver is modernc.org/sqlite — pure Go, no cgo — so the
// single-binary build is preserved.
package routes

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultPort is forwarded to when a route doesn't specify one.
const DefaultPort = 8000

// Visibility governs whether the proxy edge gates a route behind a login.
// Private is the default: a visitor must present a valid session identifying a
// handle that owns the sandbox (or an operator). Public restores the old
// unauthenticated web-preview behaviour, opt-in via `ctl@ share <name> public`.
const (
	VisibilityPrivate = "private"
	VisibilityPublic  = "public"
)

// ValidVisibility reports whether v is a known visibility value.
func ValidVisibility(v string) bool {
	return v == VisibilityPrivate || v == VisibilityPublic
}

// ErrSubdomainTaken is returned when a subdomain is already bound to a
// different sandbox.
var ErrSubdomainTaken = errors.New("subdomain already in use by another sandbox")

// ErrNoSuchRoute is returned when an operation targets a subdomain with no row.
var ErrNoSuchRoute = errors.New("no such route")

// subdomainRe allows one or more dash-separated DNS labels (so both "myvm" and
// "web-myvm" or "api.myvm" work as subdomains).
var subdomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// Route maps <Subdomain>.<domain> to Sandbox:Port.
type Route struct {
	Subdomain  string    `json:"subdomain"`
	Sandbox    string    `json:"sandbox"`
	Owner      string    `json:"owner"`
	Port       int       `json:"port"`
	Visibility string    `json:"visibility"`
	CreatedAt  time.Time `json:"created_at"`
}

// ValidSubdomain reports whether s is a usable subdomain label.
func ValidSubdomain(s string) bool {
	return len(s) <= 253 && subdomainRe.MatchString(s)
}

type Store struct {
	mu sync.Mutex // serialises writes (sqlite is single-writer)
	db *sql.DB
}

// Open opens (creating if needed) the sqlite database at path and applies the
// schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
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
		CREATE TABLE IF NOT EXISTS routes (
			subdomain  TEXT PRIMARY KEY,
			sandbox    TEXT NOT NULL,
			owner      TEXT NOT NULL,
			port       INTEGER NOT NULL,
			created_at TIMESTAMP NOT NULL
		);
		CREATE INDEX IF NOT EXISTS routes_sandbox ON routes(sandbox);
		CREATE INDEX IF NOT EXISTS routes_owner   ON routes(owner);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	// Migration: routes predating authenticated forwarding have no visibility
	// column. New routes default to private (secure by default); existing rows
	// inherit that too, so a fresh deploy gates previews until `share … public`.
	if err := addColumnIfMissing(db, "routes", "visibility",
		"TEXT NOT NULL DEFAULT '"+VisibilityPrivate+"'"); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Store{db: db}, nil
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN unless the column already
// exists. sqlite has no ADD COLUMN IF NOT EXISTS, and a bare ALTER errors on
// the second boot, so we consult table_info first.
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
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// Upsert creates or updates a route. It refuses to move a subdomain that is
// already bound to a different sandbox (ErrSubdomainTaken); the owning sandbox
// can freely change its own port.
func (s *Store) Upsert(r Route) error {
	if !ValidSubdomain(r.Subdomain) {
		return fmt.Errorf("invalid subdomain %q", r.Subdomain)
	}
	if r.Sandbox == "" || r.Owner == "" {
		return fmt.Errorf("route needs a sandbox and owner")
	}
	if r.Port <= 0 || r.Port > 65535 {
		return fmt.Errorf("invalid port %d", r.Port)
	}
	if r.Visibility == "" {
		r.Visibility = VisibilityPrivate
	}
	if !ValidVisibility(r.Visibility) {
		return fmt.Errorf("invalid visibility %q", r.Visibility)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var existingSandbox string
	err = tx.QueryRow(`SELECT sandbox FROM routes WHERE subdomain = ?`, r.Subdomain).Scan(&existingSandbox)
	switch {
	case err == sql.ErrNoRows:
		// new route
	case err != nil:
		return err
	case existingSandbox != r.Sandbox:
		return ErrSubdomainTaken
	}

	// Only the port is updated on conflict: visibility is set once at creation
	// and thereafter changed only through SetVisibility, so the host manager
	// re-writing the default route on every resume can't silently re-privatise a
	// subdomain the owner deliberately made public.
	if _, err := tx.Exec(`
		INSERT INTO routes (subdomain, sandbox, owner, port, visibility, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(subdomain) DO UPDATE SET port = excluded.port`,
		r.Subdomain, r.Sandbox, r.Owner, r.Port, r.Visibility, r.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// SetVisibility flips a route between public and private. It returns
// ErrNoSuchRoute if the subdomain has no row.
func (s *Store) SetVisibility(subdomain, visibility string) error {
	if !ValidVisibility(visibility) {
		return fmt.Errorf("invalid visibility %q", visibility)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE routes SET visibility = ? WHERE subdomain = ?`, visibility, subdomain)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchRoute
	}
	return nil
}

// GetBySubdomain returns the route for a subdomain, if any.
func (s *Store) GetBySubdomain(subdomain string) (Route, bool, error) {
	var r Route
	err := s.db.QueryRow(
		`SELECT subdomain, sandbox, owner, port, visibility, created_at FROM routes WHERE subdomain = ?`,
		subdomain,
	).Scan(&r.Subdomain, &r.Sandbox, &r.Owner, &r.Port, &r.Visibility, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return Route{}, false, nil
	}
	if err != nil {
		return Route{}, false, err
	}
	return r, true, nil
}

// ListBySandbox returns all routes pointing at a sandbox.
func (s *Store) ListBySandbox(sandbox string) ([]Route, error) {
	return s.query(`SELECT subdomain, sandbox, owner, port, visibility, created_at FROM routes WHERE sandbox = ? ORDER BY subdomain`, sandbox)
}

// ListByOwner returns all routes owned by a user.
func (s *Store) ListByOwner(owner string) ([]Route, error) {
	return s.query(`SELECT subdomain, sandbox, owner, port, visibility, created_at FROM routes WHERE owner = ? ORDER BY subdomain`, owner)
}

// List returns every route.
func (s *Store) List() ([]Route, error) {
	return s.query(`SELECT subdomain, sandbox, owner, port, visibility, created_at FROM routes ORDER BY subdomain`)
}

func (s *Store) query(q string, args ...any) ([]Route, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var r Route
		if err := rows.Scan(&r.Subdomain, &r.Sandbox, &r.Owner, &r.Port, &r.Visibility, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes a single route by subdomain.
func (s *Store) Delete(subdomain string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM routes WHERE subdomain = ?`, subdomain)
	return err
}

// DeleteBySandbox removes every route for a sandbox (called on destroy).
func (s *Store) DeleteBySandbox(sandbox string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM routes WHERE sandbox = ?`, sandbox)
	return err
}
