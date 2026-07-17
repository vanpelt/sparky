// Package schedule is the platform scheduler: the honest answer to background
// work in a scale-to-zero world. A paused sandbox runs no code, so in-guest
// cron and systemd timers silently stop (resource-model design, Part 3). Rather
// than hide that, we move the schedule *outside* the VM: an entry here says
// "every 30m, run <cmd> in <sandbox>", owned by the control plane. A host-side
// loop (scheduler.go) resumes the sandbox when a job is due, runs it, and lets
// it idle again — the sandbox stays cheap and the job stays reliable.
//
// State lives in its own sqlite file alongside the route store, using the same
// pure-Go modernc.org/sqlite driver so the single-binary build is preserved.
package schedule

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	cron "github.com/robfig/cron/v3"
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when no entry matches an ID.
var ErrNotFound = errors.New("schedule not found")

// Entry is one scheduled command bound to a sandbox. Spec is a standard 5-field
// cron expression (also accepting descriptors like "@hourly" and "@every 30m").
// NextRun is not stored — it's always derived from Spec so a config change or a
// missed window can never leave a stale timestamp on disk.
type Entry struct {
	ID        string    `json:"id"`
	Sandbox   string    `json:"sandbox"`
	Owner     string    `json:"owner"`
	Spec      string    `json:"spec"`
	Command   string    `json:"command"`
	CreatedAt time.Time `json:"created_at"`
	LastRun   time.Time `json:"last_run,omitempty"`
	LastExit  int       `json:"last_exit"`
	LastError string    `json:"last_error,omitempty"`
}

// Parse compiles a cron spec, accepting the standard 5-field form plus the
// common descriptors (@hourly, @daily, @every 30m). It's the single source of
// truth for what a valid spec is, used by both validation and next-run.
func Parse(spec string) (cron.Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("empty schedule spec")
	}
	return cron.ParseStandard(spec)
}

// NextRun returns the first activation of spec strictly after `after`. It's the
// upcoming-wake time surfaced to users; callers pass time.Now() for "when does
// this next fire".
func NextRun(spec string, after time.Time) (time.Time, error) {
	sched, err := Parse(spec)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after), nil
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
		CREATE TABLE IF NOT EXISTS schedules (
			id         TEXT PRIMARY KEY,
			sandbox    TEXT NOT NULL,
			owner      TEXT NOT NULL,
			spec       TEXT NOT NULL,
			command    TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			last_run   TIMESTAMP,
			last_exit  INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS schedules_sandbox ON schedules(sandbox);
		CREATE INDEX IF NOT EXISTS schedules_owner   ON schedules(owner);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Add validates and persists a new entry, assigning it an ID and CreatedAt.
// The spec must parse; the sandbox, owner, and command must be non-empty.
func (s *Store) Add(e Entry) (Entry, error) {
	if _, err := Parse(e.Spec); err != nil {
		return Entry{}, fmt.Errorf("invalid schedule %q: %w", e.Spec, err)
	}
	if e.Sandbox == "" || e.Owner == "" {
		return Entry{}, errors.New("schedule needs a sandbox and owner")
	}
	if strings.TrimSpace(e.Command) == "" {
		return Entry{}, errors.New("schedule needs a command to run")
	}
	e.ID = newID()
	e.CreatedAt = time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`
		INSERT INTO schedules (id, sandbox, owner, spec, command, created_at, last_exit, last_error)
		VALUES (?, ?, ?, ?, ?, ?, 0, '')`,
		e.ID, e.Sandbox, e.Owner, e.Spec, e.Command, e.CreatedAt); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Get returns one entry by ID.
func (s *Store) Get(id string) (Entry, error) {
	e, err := s.scanOne(s.db.QueryRow(selectCols+` WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Entry{}, ErrNotFound
	}
	return e, err
}

// List returns every entry (scheduler loop).
func (s *Store) List() ([]Entry, error) {
	return s.query(selectCols + ` ORDER BY sandbox, created_at`)
}

// ListByOwner returns a user's entries (ctl@ schedule list).
func (s *Store) ListByOwner(owner string) ([]Entry, error) {
	return s.query(selectCols+` WHERE owner = ? ORDER BY sandbox, created_at`, owner)
}

// ListBySandbox returns the entries bound to a sandbox (console next-wake).
func (s *Store) ListBySandbox(sandbox string) ([]Entry, error) {
	return s.query(selectCols+` WHERE sandbox = ? ORDER BY created_at`, sandbox)
}

// RecordRun stamps the outcome of a job: when it ran, its exit code, and any
// error (transport failure or a non-zero exit's output tail). Advancing
// last_run is what stops the next tick from re-firing the same window.
func (s *Store) RecordRun(id string, at time.Time, exit int, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE schedules SET last_run = ?, last_exit = ?, last_error = ? WHERE id = ?`,
		at.UTC(), exit, errMsg, id)
	return err
}

// Delete removes one entry by ID.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteBySandbox removes every schedule for a sandbox (called on destroy so a
// deleted sandbox leaves no orphaned jobs that would fail to resume forever).
func (s *Store) DeleteBySandbox(sandbox string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM schedules WHERE sandbox = ?`, sandbox)
	return err
}

const selectCols = `SELECT id, sandbox, owner, spec, command, created_at, last_run, last_exit, last_error FROM schedules`

func (s *Store) query(q string, args ...any) ([]Entry, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := s.scanOne(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// scanner is the shared shape of *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanOne(sc scanner) (Entry, error) {
	var e Entry
	var lastRun sql.NullTime
	if err := sc.Scan(&e.ID, &e.Sandbox, &e.Owner, &e.Spec, &e.Command,
		&e.CreatedAt, &lastRun, &e.LastExit, &e.LastError); err != nil {
		return Entry{}, err
	}
	if lastRun.Valid {
		e.LastRun = lastRun.Time
	}
	return e, nil
}

// newID returns a short random hex id for a schedule entry.
func newID() string {
	var b [5]byte
	rand.Read(b[:]) //nolint:errcheck // crypto/rand never fails on these platforms
	return hex.EncodeToString(b[:])
}
