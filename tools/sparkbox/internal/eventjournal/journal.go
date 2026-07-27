// Package eventjournal persists the monotonic revision behind node inventory
// events. A reconnecting gateway can resume from its last applied revision; if
// that revision has fallen out of the bounded replay window, the journal says
// so explicitly and the gateway must fetch a full inventory.
package eventjournal

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const DefaultRetain = 4096

var ErrInvalid = errors.New("invalid event journal input")

// GapError says after_revision can no longer be resumed from the replay
// window. Oldest and Current let a client log the exact missing interval.
type GapError struct {
	After   uint64
	Oldest  uint64
	Current uint64
}

func (e *GapError) Error() string {
	return fmt.Sprintf("event revision %d is no longer available (oldest %d, current %d)",
		e.After, e.Oldest, e.Current)
}

// Event is one opaque, revisioned inventory event. Kind is a bounded protocol
// token; Payload belongs to the transport adapter.
type Event struct {
	Revision uint64
	Kind     string
	Payload  []byte
	Created  time.Time
}

type Journal struct {
	mu      sync.Mutex
	db      *sql.DB
	retain  int
	watchMu sync.Mutex
	watches map[chan struct{}]struct{}
}

// Open opens or creates an event journal. retain <= 0 selects DefaultRetain.
func Open(path string, retain int) (*Journal, error) {
	if path == "" {
		return nil, ErrInvalid
	}
	if retain <= 0 {
		retain = DefaultRetain
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
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
		CREATE TABLE IF NOT EXISTS node_event_state (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			revision  INTEGER NOT NULL
		);
		INSERT OR IGNORE INTO node_event_state (singleton, revision) VALUES (1, 0);
		CREATE TABLE IF NOT EXISTS node_events (
			revision   INTEGER PRIMARY KEY,
			kind       TEXT NOT NULL,
			payload    BLOB NOT NULL,
			created_at TIMESTAMP NOT NULL
		);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Journal{db: db, retain: retain, watches: map[chan struct{}]struct{}{}}, nil
}

func (j *Journal) Close() error { return j.db.Close() }

// Current returns the last allocated revision. It remains monotonic even when
// old replay rows have been pruned.
func (j *Journal) Current(ctx context.Context) (uint64, error) {
	var revision uint64
	err := j.db.QueryRowContext(ctx,
		`SELECT revision FROM node_event_state WHERE singleton = 1`).Scan(&revision)
	return revision, err
}

// Append allocates and commits the next revision with the event in one
// transaction, then prunes the oldest replay rows.
func (j *Journal) Append(ctx context.Context, kind string, payload []byte) (Event, error) {
	if kind == "" {
		return Event{}, ErrInvalid
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var revision uint64
	if err := tx.QueryRowContext(ctx,
		`UPDATE node_event_state SET revision = revision + 1
		 WHERE singleton = 1 RETURNING revision`).Scan(&revision); err != nil {
		return Event{}, err
	}
	event := Event{
		Revision: revision,
		Kind:     kind,
		Payload:  bytes.Clone(payload),
		Created:  time.Now().UTC(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO node_events (revision, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		event.Revision, event.Kind, event.Payload, event.Created); err != nil {
		return Event{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM node_events
		 WHERE revision <= (SELECT max(revision) - ? FROM node_events)`, j.retain); err != nil {
		return Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return Event{}, err
	}
	j.notify()
	return event, nil
}

// After returns events strictly newer than after in revision order. A limit <=
// zero returns the whole retained suffix. A GapError is returned instead of a
// partial history when the requested next revision has already been pruned.
func (j *Journal) After(ctx context.Context, after uint64, limit int) ([]Event, error) {
	current, oldest, err := j.bounds(ctx)
	if err != nil {
		return nil, err
	}
	if after < current && oldest > 0 && after+1 < oldest {
		return nil, &GapError{After: after, Oldest: oldest, Current: current}
	}
	query := `SELECT revision, kind, payload, created_at
		FROM node_events WHERE revision > ? ORDER BY revision`
	args := []any{after}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := j.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.Revision, &event.Kind, &event.Payload, &event.Created); err != nil {
			return nil, err
		}
		event.Payload = bytes.Clone(event.Payload)
		events = append(events, event)
	}
	return events, rows.Err()
}

// Watch delivers retained and newly appended events after a revision. A gap or
// database error is sent on errs and closes both channels.
func (j *Journal) Watch(ctx context.Context, after uint64) (<-chan Event, <-chan error) {
	events := make(chan Event, 16)
	errs := make(chan error, 1)
	wake := make(chan struct{}, 1)
	unsubscribe := j.subscribe(wake)
	go func() {
		defer close(events)
		defer close(errs)
		defer unsubscribe()
		for {
			batch, err := j.After(ctx, after, 256)
			if err != nil {
				errs <- err
				return
			}
			for _, event := range batch {
				select {
				case events <- event:
					after = event.Revision
				case <-ctx.Done():
					return
				}
			}
			if len(batch) == 256 {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
		}
	}()
	return events, errs
}

func (j *Journal) bounds(ctx context.Context) (current, oldest uint64, err error) {
	err = j.db.QueryRowContext(ctx, `
		SELECT s.revision, coalesce(min(e.revision), 0)
		FROM node_event_state s LEFT JOIN node_events e
		WHERE s.singleton = 1`).Scan(&current, &oldest)
	return current, oldest, err
}

func (j *Journal) subscribe(wake chan struct{}) func() {
	j.watchMu.Lock()
	j.watches[wake] = struct{}{}
	j.watchMu.Unlock()
	return func() {
		j.watchMu.Lock()
		delete(j.watches, wake)
		j.watchMu.Unlock()
	}
}

func (j *Journal) notify() {
	j.watchMu.Lock()
	defer j.watchMu.Unlock()
	for wake := range j.watches {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}
