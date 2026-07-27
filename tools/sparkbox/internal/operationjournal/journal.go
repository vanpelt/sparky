// Package operationjournal persists node-side mutation identities and results.
//
// A gateway can lose a reply, restart, and submit the same operation again
// without making the node perform the mutation twice. Operation IDs and
// idempotency keys are both unique; either one finds the durable original, and
// a different immutable request hash is rejected instead of being mistaken for
// a retry.
package operationjournal

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

// State is the durable lifecycle of an operation.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

var (
	// ErrNotFound means no operation has the requested ID.
	ErrNotFound = errors.New("operation not found")
	// ErrIdentityConflict means an operation ID or idempotency key was reused
	// with different immutable request fields.
	ErrIdentityConflict = errors.New("operation identity was reused for a different request")
	// ErrTerminal means a caller attempted to mutate a completed operation.
	ErrTerminal = errors.New("operation is already terminal")
	// ErrInvalid means the operation identity is incomplete.
	ErrInvalid = errors.New("invalid operation identity")
)

// Spec is the immutable identity stored when an operation is first claimed.
type Spec struct {
	ID             string
	IdempotencyKey string
	RequestHash    []byte
	Kind           string
	Target         string
	Initiator      string
	CreatedAt      time.Time
}

// Failure is the durable, transport-neutral error projection of a mutation.
type Failure struct {
	Code      string
	Message   string
	Retryable bool
}

// Operation is one journal row. Result is an opaque protobuf or JSON payload
// owned by the adapter above this package.
type Operation struct {
	Spec
	State     State
	Sequence  uint64
	UpdatedAt time.Time
	Result    []byte
	Failure   *Failure
}

// Journal is a SQLite-backed operation journal. The notification map is only
// an optimization for live watchers; SQLite remains authoritative across a
// process restart.
type Journal struct {
	mu      sync.Mutex // serializes claims and transitions (SQLite is one-writer)
	db      *sql.DB
	watchMu sync.Mutex
	watches map[string]map[chan struct{}]struct{}
}

// Open opens or creates the journal at path.
func Open(path string) (*Journal, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: empty database path", ErrInvalid)
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
		CREATE TABLE IF NOT EXISTS node_operations (
			operation_id    TEXT PRIMARY KEY,
			idempotency_key TEXT NOT NULL UNIQUE,
			request_hash    BLOB NOT NULL,
			kind            TEXT NOT NULL,
			target          TEXT NOT NULL,
			initiator       TEXT NOT NULL,
			created_at      TIMESTAMP NOT NULL,
			updated_at      TIMESTAMP NOT NULL,
			state           TEXT NOT NULL,
			sequence        INTEGER NOT NULL,
			result          BLOB,
			error_code      TEXT NOT NULL DEFAULT '',
			error_message   TEXT NOT NULL DEFAULT '',
			error_retryable INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS node_operations_updated_at
			ON node_operations(updated_at);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Journal{db: db, watches: map[string]map[chan struct{}]struct{}{}}, nil
}

// Close releases the database. Live Watch calls should be cancelled first.
func (j *Journal) Close() error { return j.db.Close() }

// Claim atomically creates a pending operation or returns the durable original.
// Existing is true when either the operation ID or idempotency key was already
// present with the same request hash.
func (j *Journal) Claim(ctx context.Context, spec Spec) (op Operation, existing bool, err error) {
	spec, err = normalizeSpec(spec)
	if err != nil {
		return Operation{}, false, err
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	byID, idErr := getQuery(ctx, tx, `WHERE operation_id = ?`, spec.ID)
	if idErr != nil && !errors.Is(idErr, ErrNotFound) {
		return Operation{}, false, idErr
	}
	byKey, keyErr := getQuery(ctx, tx, `WHERE idempotency_key = ?`, spec.IdempotencyKey)
	if keyErr != nil && !errors.Is(keyErr, ErrNotFound) {
		return Operation{}, false, keyErr
	}

	switch {
	case idErr == nil && keyErr == nil && byID.ID != byKey.ID:
		return Operation{}, false, ErrIdentityConflict
	case idErr == nil:
		if !sameRequest(byID, spec) {
			return Operation{}, false, ErrIdentityConflict
		}
		return byID, true, tx.Commit()
	case keyErr == nil:
		if !sameRequest(byKey, spec) {
			return Operation{}, false, ErrIdentityConflict
		}
		return byKey, true, tx.Commit()
	}

	op = Operation{
		Spec:      spec,
		State:     StatePending,
		Sequence:  1,
		UpdatedAt: spec.CreatedAt,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_operations (
			operation_id, idempotency_key, request_hash, kind, target, initiator,
			created_at, updated_at, state, sequence
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.IdempotencyKey, op.RequestHash, op.Kind, op.Target, op.Initiator,
		op.CreatedAt, op.UpdatedAt, op.State, op.Sequence); err != nil {
		return Operation{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, false, err
	}
	j.notify(op.ID)
	return op, false, nil
}

// Get returns the current durable state of operation id.
func (j *Journal) Get(ctx context.Context, id string) (Operation, error) {
	return getQuery(ctx, j.db, `WHERE operation_id = ?`, id)
}

// Start moves a pending operation to running.
func (j *Journal) Start(ctx context.Context, id string) (Operation, error) {
	return j.transition(ctx, id, StateRunning, nil, nil)
}

// Succeed completes an operation with an opaque result.
func (j *Journal) Succeed(ctx context.Context, id string, result []byte) (Operation, error) {
	return j.transition(ctx, id, StateSucceeded, result, nil)
}

// Fail completes an operation with a stable error projection.
func (j *Journal) Fail(ctx context.Context, id string, failure Failure) (Operation, error) {
	return j.transition(ctx, id, StateFailed, nil, &failure)
}

// Cancel records cancellation as a terminal outcome.
func (j *Journal) Cancel(ctx context.Context, id string, failure Failure) (Operation, error) {
	return j.transition(ctx, id, StateCancelled, nil, &failure)
}

func (j *Journal) transition(ctx context.Context, id string, next State, result []byte, failure *Failure) (Operation, error) {
	if id == "" || !validState(next) {
		return Operation{}, ErrInvalid
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	current, err := getQuery(ctx, tx, `WHERE operation_id = ?`, id)
	if err != nil {
		return Operation{}, err
	}
	if terminal(current.State) {
		return Operation{}, ErrTerminal
	}
	if !allowedTransition(current.State, next) {
		return Operation{}, fmt.Errorf("%w: %s to %s", ErrInvalid, current.State, next)
	}

	now := time.Now().UTC()
	code, message, retryable := "", "", false
	if failure != nil {
		code, message, retryable = failure.Code, failure.Message, failure.Retryable
	}
	seq := current.Sequence + 1
	if _, err := tx.ExecContext(ctx, `
		UPDATE node_operations
		SET updated_at = ?, state = ?, sequence = ?, result = ?,
		    error_code = ?, error_message = ?, error_retryable = ?
		WHERE operation_id = ?`,
		now, next, seq, bytes.Clone(result), code, message, retryable, id); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, err
	}
	op, err := j.Get(ctx, id)
	if err == nil {
		j.notify(id)
	}
	return op, err
}

// Watch reports each state whose sequence is greater than after. It always
// re-reads SQLite after a notification, so coalesced notifications cannot skip
// a durable terminal state. The channel closes on cancellation or database
// failure.
func (j *Journal) Watch(ctx context.Context, id string, after uint64) <-chan Operation {
	out := make(chan Operation, 1)
	wake := make(chan struct{}, 1)
	unsubscribe := j.subscribe(id, wake)
	go func() {
		defer close(out)
		defer unsubscribe()
		for {
			op, err := j.Get(ctx, id)
			if err != nil {
				return
			}
			if op.Sequence > after {
				select {
				case out <- op:
					after = op.Sequence
				case <-ctx.Done():
					return
				}
				if terminal(op.State) {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
		}
	}()
	return out
}

func (j *Journal) subscribe(id string, wake chan struct{}) func() {
	j.watchMu.Lock()
	if j.watches[id] == nil {
		j.watches[id] = map[chan struct{}]struct{}{}
	}
	j.watches[id][wake] = struct{}{}
	j.watchMu.Unlock()
	return func() {
		j.watchMu.Lock()
		delete(j.watches[id], wake)
		if len(j.watches[id]) == 0 {
			delete(j.watches, id)
		}
		j.watchMu.Unlock()
	}
}

func (j *Journal) notify(id string) {
	j.watchMu.Lock()
	defer j.watchMu.Unlock()
	for wake := range j.watches[id] {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const selectColumns = `
	SELECT operation_id, idempotency_key, request_hash, kind, target, initiator,
	       created_at, updated_at, state, sequence, result,
	       error_code, error_message, error_retryable
	FROM node_operations `

func getQuery(ctx context.Context, q queryer, where string, arg any) (Operation, error) {
	var (
		op        Operation
		state     string
		result    []byte
		code      string
		message   string
		retryable bool
	)
	err := q.QueryRowContext(ctx, selectColumns+where, arg).Scan(
		&op.ID, &op.IdempotencyKey, &op.RequestHash, &op.Kind, &op.Target,
		&op.Initiator, &op.CreatedAt, &op.UpdatedAt, &state, &op.Sequence,
		&result, &code, &message, &retryable,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	op.State = State(state)
	op.RequestHash = bytes.Clone(op.RequestHash)
	op.Result = bytes.Clone(result)
	if code != "" || message != "" {
		op.Failure = &Failure{Code: code, Message: message, Retryable: retryable}
	}
	return op, nil
}

func normalizeSpec(spec Spec) (Spec, error) {
	if spec.ID == "" || spec.IdempotencyKey == "" || len(spec.RequestHash) == 0 ||
		spec.Kind == "" || spec.Initiator == "" {
		return Spec{}, ErrInvalid
	}
	spec.RequestHash = bytes.Clone(spec.RequestHash)
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	} else {
		spec.CreatedAt = spec.CreatedAt.UTC()
	}
	return spec, nil
}

func sameRequest(op Operation, spec Spec) bool {
	return op.ID == spec.ID &&
		op.IdempotencyKey == spec.IdempotencyKey &&
		bytes.Equal(op.RequestHash, spec.RequestHash) &&
		op.Kind == spec.Kind &&
		op.Target == spec.Target &&
		op.Initiator == spec.Initiator
}

func validState(state State) bool {
	switch state {
	case StatePending, StateRunning, StateSucceeded, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

func terminal(state State) bool {
	return state == StateSucceeded || state == StateFailed || state == StateCancelled
}

func allowedTransition(from, to State) bool {
	switch from {
	case StatePending:
		return to == StateRunning || to == StateFailed || to == StateCancelled
	case StateRunning:
		return to == StateSucceeded || to == StateFailed || to == StateCancelled
	default:
		return false
	}
}
