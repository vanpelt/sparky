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

// ErrSubdomainTaken is returned when a subdomain is already bound to a
// different sandbox.
var ErrSubdomainTaken = errors.New("subdomain already in use by another sandbox")

// subdomainRe allows one or more dash-separated DNS labels (so both "myvm" and
// "web-myvm" or "api.myvm" work as subdomains).
var subdomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// Route maps <Subdomain>.<domain> to Sandbox:Port.
type Route struct {
	Subdomain string    `json:"subdomain"`
	Sandbox   string    `json:"sandbox"`
	Owner     string    `json:"owner"`
	Port      int       `json:"port"`
	CreatedAt time.Time `json:"created_at"`
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
	return &Store{db: db}, nil
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

	if _, err := tx.Exec(`
		INSERT INTO routes (subdomain, sandbox, owner, port, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(subdomain) DO UPDATE SET port = excluded.port`,
		r.Subdomain, r.Sandbox, r.Owner, r.Port, r.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// GetBySubdomain returns the route for a subdomain, if any.
func (s *Store) GetBySubdomain(subdomain string) (Route, bool, error) {
	var r Route
	err := s.db.QueryRow(
		`SELECT subdomain, sandbox, owner, port, created_at FROM routes WHERE subdomain = ?`,
		subdomain,
	).Scan(&r.Subdomain, &r.Sandbox, &r.Owner, &r.Port, &r.CreatedAt)
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
	return s.query(`SELECT subdomain, sandbox, owner, port, created_at FROM routes WHERE sandbox = ? ORDER BY subdomain`, sandbox)
}

// ListByOwner returns all routes owned by a user.
func (s *Store) ListByOwner(owner string) ([]Route, error) {
	return s.query(`SELECT subdomain, sandbox, owner, port, created_at FROM routes WHERE owner = ? ORDER BY subdomain`, owner)
}

// List returns every route.
func (s *Store) List() ([]Route, error) {
	return s.query(`SELECT subdomain, sandbox, owner, port, created_at FROM routes ORDER BY subdomain`)
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
		if err := rows.Scan(&r.Subdomain, &r.Sandbox, &r.Owner, &r.Port, &r.CreatedAt); err != nil {
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
