// Package envs is the sqlite-backed store of environments: the user-facing
// object that gathers the repositories, secrets, plain env vars, egress rules
// and base image a sandbox should come up holding, under one name a person can
// say out loud. It is the fifth reader of the shared sandbox_tags table, after
// internal/secrets, internal/repos, internal/netrules and internal/templates,
// and it mirrors them exactly (same DB file, own connection, WAL, pure-Go
// driver, the tag grammar asked of internal/secrets rather than copied) minus
// any secret material: a name, a description and a setup script are
// configuration, so they are stored in plaintext.
//
// An environment OWNS EXACTLY ONE TAG, and its name IS that tag. That is the
// whole design, and it is what keeps this package small: nothing about the four
// existing tag joins changes, because an environment invents no new join key.
// `env create web` is a promise that the tag `web` means something, and every
// store that already reads tags keeps answering the same question it always
// did. The alternative — an environment owning a SET of tags — would have
// needed a second indirection table, a precedence rule for two environments
// that disagree about the base image, and a story for what happens when
// somebody hand-tags a sandbox with half of one. One name, one tag, and the
// composition is the union the other stores already compute.
//
// The sandbox_tags table is shared with internal/secrets; envs only reads it.
// Whichever package opens the DB first creates the table (all five use CREATE
// TABLE IF NOT EXISTS with a byte-identical DDL), and secrets owns the tag
// mutations (SetTags/DeleteBySandbox/RenameSandbox). If this package ever
// writes a sandbox_tags row, secrets' in-transaction cross-owner refusal is
// bypassed and the invariant that a tag belongs to exactly one handle is gone.
//
// Because the name is the tag, `default` cannot be an environment name. Every
// sandbox its owner creates is stamped with that tag (see secrets.DefaultTag),
// so an environment called `default` would not be one environment among several
// — it would be the base image, the repo list and the secret selector for every
// box the person ever makes, chosen by a word they typed once. An environment
// binds a template, which REPLACES rather than adds, so this is the same
// refusal internal/netrules and internal/templates already make, for the same
// reason and in the same words.
//
// Owner scoping is structural, not handler discipline: EnvironmentsForSandbox
// carries the owner on BOTH sides of the tag join (bt.owner = e.owner AND
// e.owner = ?). The first term looks redundant next to the second and is not —
// without it, any tag name two people happen to share (web, ci, dev) joins
// their rows together, and what an environment names is a private repository
// list and a secret set. TestEnvironmentsForSandboxScopesByOwner exists to keep
// it that way.
//
// The build columns (setup_sh, setup_from, build_state, build_box, build_error,
// built_at) are carried by the schema and round-tripped by SetScript/SetState,
// but nothing in this package orchestrates a build. That is deliberate: the
// builder sandbox, the metadata endpoints and the guest unit that captures
// .sparkbox/setup.sh are a later phase, and they must be able to land as
// behavior on a schema that already exists rather than as a migration.
package envs

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

// State is where an environment is in its build. An environment is useful in
// every one of these states — the tag composes secrets, repos and rules from
// the moment it exists — so `draft` means "no snapshot bound yet", not
// "unusable".
type State string

const (
	// StateDraft is a freshly created environment: composition only, no
	// build has ever run.
	StateDraft State = "draft"
	// StateBuilding means a builder sandbox exists (BuildBox names it) and
	// the reconciler is responsible for it across a restart.
	StateBuilding State = "building"
	// StateReady means a snapshot was captured and bound; BuiltAt says when.
	StateReady State = "ready"
	// StateFailed means the last build gave up. BuildError says why, and it
	// is the only state in which that field is meaningful.
	StateFailed State = "failed"
)

// SetupFrom values, naming where a setup script came from. The distinction is
// worth persisting because it is what a later run has to decide from: a script
// the repository shipped is re-read on every build, one an agent wrote is
// re-generated only when asked, and one a person typed is never overwritten.
const (
	SetupFromRepo   = "repo"
	SetupFromAgent  = "agent"
	SetupFromManual = "manual"
)

// ErrNoSuchEnvironment is returned when an operation targets a name the owner
// has no environment for. It maps to 404 via the console's statusFor
// convention, and it is deliberately the same answer for another owner's
// environment as for one nobody has created — every query carries the owner, so
// the two are indistinguishable here as well as on the wire.
var ErrNoSuchEnvironment = errors.New("no such environment")

// ErrInvalidName wraps every validation refusal that is not the reserved word
// or the cap: a name outside the tag grammar, a missing owner, an unknown state
// or an unknown setup-script origin. Callers map the whole family to 400
// without matching on messages.
var ErrInvalidName = errors.New("invalid environment name")

// ErrReservedName is the `default` refusal, kept separate from ErrInvalidName
// because it is the one validation failure whose answer is not "spell it
// differently" but "you do not want what you asked for". ctlops maps it to its
// own code so the explanation survives to the terminal.
var ErrReservedName = errors.New("reserved environment name")

// ErrTooManyEnvironments is the per-owner cap, reported only on the insert path.
var ErrTooManyEnvironments = errors.New("too many environments")

// maxEnvironmentsPerOwner caps an owner's environment list. An environment is a
// named way of working — a repo plus its toolchain — not a per-branch artifact,
// and each one can own a snapshot on this machine's disk, so the ceiling is low
// on purpose.
const maxEnvironmentsPerOwner = 32

// Environment is the listable shape of the object. None of it is sensitive: the
// secrets an environment composes live in internal/secrets and are never read
// through this struct, so it is returned in full for display, for the ctl
// surface and for the console.
type Environment struct {
	Owner       string     `json:"owner"`
	Name        string     `json:"name"` // IS the tag
	Description string     `json:"description,omitempty"`
	SetupScript string     `json:"setup_script,omitempty"` // captured .sparkbox/setup.sh; "" until a build runs
	SetupFrom   string     `json:"setup_from,omitempty"`   // "repo" | "agent" | "manual" | ""
	State       State      `json:"state"`
	BuildBox    string     `json:"build_box,omitempty"` // builder sandbox name while one exists
	BuildError  string     `json:"build_error,omitempty"`
	BuiltAt     *time.Time `json:"built_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Store is the environments database handle.
type Store struct {
	mu  sync.Mutex // serialises writes (sqlite is single-writer)
	db  *sql.DB
	log *slog.Logger
}

// Open opens (creating if needed) the sqlite database at path and applies the
// schema. It shares the file with internal/secrets, internal/netrules,
// internal/repos, internal/templates and internal/routes on its own connection;
// WAL keeps that safe. See internal/secrets for the DSN rationale — the short
// version is that the _pragma DSN params run on every pooled connection where a
// db.Exec pragma binds to only one, and _txlock=immediate takes the write lock
// up front so the read-then-write upsert in Put cannot hit
// SQLITE_BUSY_SNAPSHOT, which the busy handler never waits on. Every Begin in
// this store writes.
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
	// internal/secrets, internal/netrules, internal/repos and
	// internal/templates. Five packages now race to create it and no
	// coordination is needed — but if the copies ever drift, the loser of
	// the race silently gets the other's schema, so this must stay
	// byte-identical rather than merely equivalent. This package only ever
	// SELECTs from it; secrets owns every mutation.
	//
	// environments has an id column even though (owner, name) is already
	// unique and is what every query here selects on. The id is for the
	// rows a later phase hangs off an environment — a build attempt, a
	// captured artifact — which need something stable to reference, and a
	// name is not that: renaming an environment means renaming its tag,
	// which is a thing people will want and which must not orphan history.
	//
	// setup_sh, setup_from, build_state, build_box, build_error and built_at
	// are all NOT NULL DEFAULT '' (built_at nullable) rather than absent
	// until the build phase needs them. The empty string already means "no
	// script yet" and "no builder right now"; a NULL would be a second
	// spelling of the same state that every Scan would have to unwrap
	// through sql.NullString. built_at is the exception because "never
	// built" is genuinely not a time, and pretending it is the zero time
	// makes every ordering query wrong.
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

		CREATE TABLE IF NOT EXISTS environments (
			id          TEXT PRIMARY KEY,
			owner       TEXT NOT NULL,
			name        TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			setup_sh    TEXT NOT NULL DEFAULT '',
			setup_from  TEXT NOT NULL DEFAULT '',
			build_state TEXT NOT NULL DEFAULT 'draft',
			build_box   TEXT NOT NULL DEFAULT '',
			build_error TEXT NOT NULL DEFAULT '',
			built_at    TIMESTAMP,
			created_at  TIMESTAMP NOT NULL,
			updated_at  TIMESTAMP NOT NULL,
			UNIQUE (owner, name)
		);
		CREATE INDEX IF NOT EXISTS environments_owner ON environments(owner);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Store{db: db, log: log}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Put creates the owner's environment, or updates the description of one that
// already exists, and returns the row as it now stands.
//
// An update deliberately touches description and updated_at ONLY. The build
// state, the builder sandbox and the captured script are the property of the
// build that produced them, and `env create web --description "..."` typed a
// second time to fix a typo must not silently reset an environment somebody is
// using back to draft. That also makes Put safe to call from a create path that
// does not know whether the environment is new.
//
// The tag rows are not touched either, in either direction. Creating an
// environment does not tag anything — the tag exists the moment a sandbox
// carries it, which is ctlops' business — and this store never writes
// sandbox_tags at all.
func (s *Store) Put(owner, name, description string) (Environment, error) {
	if owner == "" {
		return Environment{}, fmt.Errorf("%w: an environment needs an owner", ErrInvalidName)
	}
	name = strings.TrimSpace(name)
	// The `default` check folds case and runs BEFORE the grammar check.
	// "DEFAULT" would be refused either way — the tag grammar has no
	// uppercase — but by a message about the character set, which says
	// nothing about why this particular word is the one that cannot be an
	// environment. ctlops lowercases tags before they reach a store
	// (parse.go); a transport that forgets to still gets the explanation
	// rather than a spelling lecture.
	//
	// internal/netrules (store.go:204-210) and internal/templates
	// (store.go:228-235) refuse the same word for the same shape of reason.
	// An environment is the strongest case of the three: it binds a
	// template, so it REPLACES rather than adds, and it also carries the
	// repo list and the secret selector along with it.
	if strings.EqualFold(name, secrets.DefaultTag) {
		return Environment{}, fmt.Errorf(
			"%w: an environment cannot be named %q — an environment's name IS its tag, and every "+
				"sandbox you create carries that tag, so this environment would silently become "+
				"the base image and the secret and repository selector for all of them, including "+
				"ones you make months from now. Name it for the work it does and put that name on "+
				"the sandboxes you mean to run it in.",
			ErrReservedName, secrets.DefaultTag)
	}
	if !secrets.ValidTag(name) {
		return Environment{}, fmt.Errorf("%w: name %q (want [a-z0-9][a-z0-9-]*, max 40 chars)", ErrInvalidName, name)
	}
	description = strings.TrimSpace(description)
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Environment{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var id string
	err = tx.QueryRow(`SELECT id FROM environments WHERE owner = ? AND name = ?`, owner, name).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		// The cap is checked inside the transaction, on the insert path
		// only: an update must never start failing because the list is
		// full, or an owner at the cap could not fix a description or
		// record the outcome of a build.
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM environments WHERE owner = ?`, owner).Scan(&n); err != nil {
			return Environment{}, err
		}
		if n >= maxEnvironmentsPerOwner {
			return Environment{}, fmt.Errorf("%w (%d, max %d)", ErrTooManyEnvironments, n, maxEnvironmentsPerOwner)
		}
		id = newID()
		if _, err := tx.Exec(`
			INSERT INTO environments (id, owner, name, description, build_state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, owner, name, description, string(StateDraft), now, now); err != nil {
			return Environment{}, err
		}
	case err != nil:
		return Environment{}, err
	default:
		// created_at is not touched: it is the same environment, and its
		// age is the answer to "how long have I been working this way".
		if _, err := tx.Exec(`UPDATE environments SET description = ?, updated_at = ? WHERE id = ?`,
			description, now, id); err != nil {
			return Environment{}, err
		}
	}
	env, err := getTx(tx, owner, name)
	if err != nil {
		return Environment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Environment{}, err
	}
	return env, nil
}

// Get returns one environment, or ErrNoSuchEnvironment.
func (s *Store) Get(owner, name string) (Environment, error) {
	row := s.db.QueryRow(selectCols+` WHERE owner = ? AND name = ?`, owner, strings.TrimSpace(name))
	return scanEnv(row)
}

// List returns the owner's environments, sorted by name. It is owner-scoped in
// the query rather than filtered afterwards: two people may hold the same
// environment name, because a tag namespace is per-handle, and the only thing
// keeping their rows apart is this WHERE clause.
func (s *Store) List(owner string) ([]Environment, error) {
	rows, err := s.db.Query(selectCols+` WHERE owner = ? ORDER BY name`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEnvs(rows)
}

// Delete removes the environment row, or returns ErrNoSuchEnvironment.
//
// It removes only the row. The secrets, repos, rules and template binding that
// share its tag are separate objects in separate stores, and whether deleting
// an environment should take them with it is a policy decision that belongs to
// ctlops, where the whole fan-out is visible and can be reported. Deleting them
// from here would make `env rm` a silently destructive command with no way to
// preview it.
func (s *Store) Delete(owner, name string) error {
	name = strings.TrimSpace(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM environments WHERE owner = ? AND name = ?`, owner, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchEnvironment
	}
	return nil
}

// SetScript records the setup script and where it came from. An empty script is
// legal and means "there isn't one" — that is how a capture that found no
// .sparkbox/setup.sh reports its result, and it must be distinguishable from
// never having looked, which is what SetupFrom carries.
func (s *Store) SetScript(owner, name, script, from string) error {
	from = strings.TrimSpace(from)
	switch from {
	case "", SetupFromRepo, SetupFromAgent, SetupFromManual:
	default:
		return fmt.Errorf("%w: setup source %q (want %q, %q or %q)",
			ErrInvalidName, from, SetupFromRepo, SetupFromAgent, SetupFromManual)
	}
	name = strings.TrimSpace(name)
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE environments SET setup_sh = ?, setup_from = ?, updated_at = ? WHERE owner = ? AND name = ?`,
		script, from, now, owner, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchEnvironment
	}
	return nil
}

// SetState moves an environment through its build lifecycle.
//
// Two of the rules are in the SQL rather than in the caller on purpose. The
// build error is CLEARED whenever the new state is not StateFailed: a stale
// "npm install exited 1" hanging off a ready environment is worse than no
// message, because it is read as the reason for something that succeeded, and
// leaving that to every caller means one of them eventually forgets. And
// built_at is stamped only on the transition INTO StateReady, so it keeps
// meaning "when the bound snapshot was captured" — an environment that fails a
// later rebuild still boots from the disk it built in March, and the column has
// to keep saying March.
//
// box is written unconditionally, "" included: it names the builder sandbox
// that exists right now, so clearing it is how a finished build says the box is
// gone.
func (s *Store) SetState(owner, name string, st State, box, buildErr string) error {
	switch st {
	case StateDraft, StateBuilding, StateReady, StateFailed:
	default:
		return fmt.Errorf("%w: state %q (want %q, %q, %q or %q)",
			ErrInvalidName, st, StateDraft, StateBuilding, StateReady, StateFailed)
	}
	if st != StateFailed {
		buildErr = ""
	}
	name = strings.TrimSpace(name)
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	var res sql.Result
	var err error
	if st == StateReady {
		res, err = s.db.Exec(`
			UPDATE environments SET build_state = ?, build_box = ?, build_error = ?, built_at = ?, updated_at = ?
			WHERE owner = ? AND name = ?`,
			string(st), box, buildErr, now, now, owner, name)
	} else {
		res, err = s.db.Exec(`
			UPDATE environments SET build_state = ?, build_box = ?, build_error = ?, updated_at = ?
			WHERE owner = ? AND name = ?`,
			string(st), box, buildErr, now, owner, name)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchEnvironment
	}
	return nil
}

// Building returns every environment in StateBuilding, across ALL owners, for
// the reconciler that runs at startup.
//
// It is the one query in this package with no owner term, and that is what it is
// for: a build is a sandbox holding a slot on this machine, and a restart in the
// middle of one leaves a builder box nobody is waiting on. The process that
// cleans those up is not acting on behalf of a user and cannot be, so asking it
// for a handle would either be a lie or a loop over the user table. Every other
// caller must go through List.
func (s *Store) Building() ([]Environment, error) {
	rows, err := s.db.Query(selectCols+` WHERE build_state = ? ORDER BY owner, name`, string(StateBuilding))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEnvs(rows)
}

// EnvironmentsForSandbox returns the environments whose name is one of the
// sandbox's tags — in practice zero or one, because ctlops refuses to create a
// sandbox carrying two environments for the same reason internal/templates
// refuses two base images, but the query answers honestly rather than picking.
//
// Owner scoping is structural: the join requires bt.owner = e.owner AND
// e.owner = ?. The first term looks redundant next to the second and is not.
// Without it, any tag name two people happen to share (web, ci, dev) joins their
// rows together, and an environment is precisely the object that names somebody's
// private repository list and secret set. No caller-side check replaces this; it
// has to be in the SQL.
func (s *Store) EnvironmentsForSandbox(sandbox, owner string) ([]Environment, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT e.owner, e.name, e.description, e.setup_sh, e.setup_from,
		       e.build_state, e.build_box, e.build_error, e.built_at, e.created_at, e.updated_at
		FROM environments e
		JOIN sandbox_tags bt ON bt.tag = e.name AND bt.owner = e.owner
		WHERE bt.sandbox = ? AND e.owner = ?
		ORDER BY e.name`, sandbox, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEnvs(rows)
}

// selectCols is the one column list every read shares, in the order scanEnv
// expects. EnvironmentsForSandbox restates it with an alias rather than
// interpolating this constant, because a joined query needs the prefix on every
// column and a half-qualified list is the kind of thing that compiles until the
// day a second table grows a `name`.
const selectCols = `
	SELECT owner, name, description, setup_sh, setup_from,
	       build_state, build_box, build_error, built_at, created_at, updated_at
	FROM environments`

// scanner is satisfied by both *sql.Row and *sql.Rows, so the column list has
// exactly one Scan call behind it.
type scanner interface {
	Scan(dest ...any) error
}

// scanEnv reads one row, translating sql.ErrNoRows into the package sentinel and
// a NULL built_at into a nil pointer.
func scanEnv(sc scanner) (Environment, error) {
	var e Environment
	var state string
	var builtAt sql.NullTime
	if err := sc.Scan(&e.Owner, &e.Name, &e.Description, &e.SetupScript, &e.SetupFrom,
		&state, &e.BuildBox, &e.BuildError, &builtAt, &e.CreatedAt, &e.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return Environment{}, ErrNoSuchEnvironment
		}
		return Environment{}, err
	}
	e.State = State(state)
	if builtAt.Valid {
		t := builtAt.Time.UTC()
		e.BuiltAt = &t
	}
	return e, nil
}

// scanEnvs drains a multi-row query. It returns a non-nil empty slice for no
// rows so a JSON encoding of the listing is [] rather than null.
func scanEnvs(rows *sql.Rows) ([]Environment, error) {
	out := []Environment{}
	for rows.Next() {
		e, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// getTx re-reads a row inside the writing transaction so Put returns exactly
// what it wrote, including the fields it deliberately left alone.
func getTx(tx *sql.Tx, owner, name string) (Environment, error) {
	return scanEnv(tx.QueryRow(selectCols+` WHERE owner = ? AND name = ?`, owner, name))
}

// newID returns a short random hex id for an environment row. math/rand/v2 is
// seeded by the runtime and needs no crypto strength — this is a row id, not a
// secret.
func newID() string {
	var b [5]byte
	for i := range b {
		b[i] = byte(rand.UintN(256))
	}
	return hex.EncodeToString(b[:])
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
