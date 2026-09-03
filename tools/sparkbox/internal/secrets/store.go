// Package secrets is the sqlite-backed store of sandbox tags and encrypted
// user secrets: a secret is an env var owned by a handle, a tag connects
// secrets to sandboxes, and EnvForSandbox computes the environment a sandbox
// should see — owner scoping is structural in that query, not handler
// discipline.
//
// State lives in the same sqlite file as the proxy routes (internal/routes),
// on its own connection — WAL makes that safe and keeps the packages
// decoupled. The driver is modernc.org/sqlite — pure Go, no cgo — so the
// single-binary build is preserved.
//
// Values are encrypted at rest with AES-256-GCM under a KEK derived from the
// OIDC signing key (DeriveKEK), so a stolen database file alone is unreadable.
// The flip side: rotating the OIDC key orphans every stored value. That is
// handled loudly, never silently — a keycheck sentinel verified at Open
// detects the wholesale mismatch, logs the rotation once at Error level, and
// rewrites the sentinel under the current KEK so the store heals as users
// re-enter values: listing, deletion, and PutSecret (which re-seals under the
// current key) keep working, while orphaned rows fail delivery with
// ErrUndecryptable — aborting the entire env computation rather than pushing
// a partial set — until re-entered or deleted. Only if the sentinel cannot be
// rewritten do the delivery paths answer ErrNotEnabled (tags stay functional
// throughout). The per-row key_id column reserves the wrapped-DEK
// re-encryption migration without building it.
//
// This package also holds plain, declared-non-sensitive environment variables
// (the env_vars table, PutVar and friends). They live here rather than in a
// package of their own for two reasons: this package composes /etc/environment,
// so keeping vars beside secrets means EnvForSandbox stays the single delivery
// query and internal/envsync needs no change at all; and this package is the
// only writer of sandbox_tags, so a var joins to a sandbox through exactly the
// machinery a secret already uses. Var values are stored in PLAINTEXT on
// purpose — they are declared non-sensitive (NODE_ENV, LOG_LEVEL, a service
// URL), and nothing on the var path touches the AEAD or the keycheck sentinel,
// so an undecryptable-secrets state never disables vars.
package secrets

import (
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	// ErrUndecryptable is returned when a stored ciphertext fails
	// authentication — wrong KEK, tampered blob, or a row spliced from
	// another owner. Callers must treat the whole computation as failed.
	ErrUndecryptable = errors.New("secret cannot be decrypted (key rotated or data tampered)")
	// ErrNoSuchSecret is returned when an operation targets an env name the
	// owner has no secret for.
	ErrNoSuchSecret = errors.New("no such secret")
	// ErrNoSuchVar is the env_vars counterpart of ErrNoSuchSecret: DeleteVar
	// targeted a (tag, name) the owner has no plain var for. It maps to 404 via
	// the console's statusFor convention.
	ErrNoSuchVar = errors.New("no such env var")
	// ErrNotEnabled is returned by the delivery paths (EnvForSandbox,
	// SandboxesForSecret) when the keycheck sentinel failed at Open and could
	// not be rewritten under the current KEK. Writes and listing are never
	// gated — recovery is users re-entering values. The "not enabled" wording
	// maps to 501 via the console's statusFor convention.
	ErrNotEnabled = errors.New("secrets are not enabled (encryption key does not match stored data)")
	// ErrSecretNameInUse is returned by RenameSecret when the owner already
	// holds a secret under the new name. Renaming onto it would have to either
	// destroy that value or merge two rows, and neither is something a rename
	// should decide on its own.
	ErrSecretNameInUse = errors.New("a secret with that name already exists")
)

// envNameRe bounds env names to the portable shell-identifier form.
var envNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// reservedEnvNames are refused at PutSecret and PutVar alike (via checkEnvRow):
// delivering them via /etc/environment would poison every guest session (and a
// pushed PATH breaks the SSH delivery pipeline itself). Mirrored in envsync's
// render-time gate.
var reservedEnvNames = map[string]bool{
	"PATH":            true,
	"LD_PRELOAD":      true,
	"LD_LIBRARY_PATH": true,
}

// tagRe bounds tags to short DNS-label-ish strings safe in URLs and UIs.
var tagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ValidTag reports whether a tag is one this store can hold. It is the same
// filter tagRe applies on the way in, asked as a question.
//
// Exported for one caller with a hard ordering problem. ctlops.NormalizeTags
// deliberately does NOT validate the charset (ctlops/parse.go says why), so a
// malformed tag is normally discovered by the store at write time — which is
// fine for `ctl tags` and wrong for a guest-initiated capture, where the write
// happens two minutes after the sandbox was paused and there is nobody left in
// the session to read the refusal. PlanSelfSnapshot asks here instead, while
// the VM is still running and the terminal still exists.
func ValidTag(tag string) bool { return tagRe.MatchString(tag) }

// DefaultTag is the tag a secret gets when its owner names none, and the tag
// EVERY sandbox is stamped with as it is created (ctlops.Create, which adds it
// alongside whatever tags the creator asked for).
//
// It exists because the untagged case was a silent failure, and silent is the
// operative word. EnvForSandbox is an inner join on tags, so a first-time user
// who saves CLAUDE_CODE_OAUTH_TOKEN and then runs `ssh new@<gateway>` gets an
// empty environment, an agent that asks them to log in, and nothing anywhere
// saying why. Both halves have to default to the same string for the join to
// find anything, which is why one constant serves both.
//
// The create side is unconditional because narrowing it to untagged creates
// moved the same silent failure one step along rather than fixing it: `ssh
// new@<gateway> --tag hm` then opted the sandbox out of the owner's secrets,
// which is not what anyone naming a tag means, and it failed the same silent
// way minutes later inside the guest.
//
// It is still not a wildcard. A secret tagged `default` reaches exactly the
// sandboxes tagged `default`, and either side can be retagged to opt out —
// ctlops.SetTags replaces the whole set and never re-adds this, so `ctl tags
// <name> hm` drops it permanently.
//
// Three packages read sandbox_tags, and only two of them may safely see this
// tag. Secrets and internal/repos are additive: a `default` row means an
// environment variable or a checkout appears everywhere. internal/netrules is
// subtractive — a governed sandbox is filtered to exactly its allow list where
// an ungoverned one has open egress — so a rule-set tagged `default` would cut
// the entire fleet down to that list. netrules.PutRule therefore refuses this
// tag outright, and that refusal is what makes stamping it on every sandbox
// safe.
const DefaultTag = "default"

// maxValueLen caps a secret value at 4 KiB — env vars, not blobs.
const maxValueLen = 4096

// keycheck sentinel: a fixed plaintext encrypted under the current KEK at
// first Open and verified on every subsequent Open, so a wholesale key
// mismatch is detected before any per-row decrypt fails.
const (
	keycheckKey   = "keycheck"
	keycheckPlain = "sparkbox-secrets-keycheck"
	keycheckAAD   = aadPrefix + "|keycheck"
)

// SecretMeta is the listable shape of a secret: everything but the value.
// There is deliberately no read path for values — they are write-only from
// the API's point of view and only ever decrypted for delivery to a sandbox.
type SecretMeta struct {
	Name      string    `json:"name"`
	Tags      []string  `json:"tags"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Var is a plain, declared-non-sensitive environment variable attached to a
// tag. Unlike SecretMeta it carries its Value: there is nothing to hide, which
// is the whole point of declaring a var instead of a secret — a console can
// show it, an environment definition can round-trip it, and an edit does not
// have to re-type it.
type Var struct {
	Tag       string    `json:"tag"`
	Name      string    `json:"name"`
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Store struct {
	mu       sync.Mutex // serialises writes (sqlite is single-writer)
	db       *sql.DB
	aead     cipher.AEAD
	log      *slog.Logger
	disabled bool // keycheck sentinel could not be rewritten; delivery answers ErrNotEnabled
}

// Open opens (creating if needed) the sqlite database at path, applies the
// schema, and verifies the keycheck sentinel under kek (from DeriveKEK). A
// sentinel mismatch does not fail Open: it is logged loudly and the sentinel
// is rewritten under the current KEK so the store heals as values are
// re-entered; existing rows fail delivery with ErrUndecryptable until then.
func Open(path string, kek []byte, log *slog.Logger) (*Store, error) {
	aead, err := newAEAD(kek)
	if err != nil {
		return nil, fmt.Errorf("secrets kek: %w", err)
	}
	// The _pragma DSN params run on every pooled connection — a db.Exec
	// pragma binds to only the one connection that happens to execute it, so
	// the pool would otherwise grow connections with busy_timeout=0 that fail
	// instantly with SQLITE_BUSY under cross-store write contention.
	// _txlock=immediate makes Begin take the write lock up front, where
	// busy_timeout applies; a deferred read-then-write transaction (PutSecret
	// SELECTs before it INSERTs) hits SQLITE_BUSY_SNAPSHOT on upgrade, which
	// the busy handler never waits on. Every Begin in this store writes.
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

		CREATE TABLE IF NOT EXISTS secrets (
			id         TEXT PRIMARY KEY,
			owner      TEXT NOT NULL,
			env_name   TEXT NOT NULL,
			ciphertext BLOB NOT NULL,
			key_id     TEXT NOT NULL,
			version    INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			UNIQUE (owner, env_name)
		);

		CREATE TABLE IF NOT EXISTS secret_tags (
			secret_id  TEXT NOT NULL REFERENCES secrets(id) ON DELETE CASCADE,
			tag        TEXT NOT NULL,
			PRIMARY KEY (secret_id, tag)
		);

		CREATE TABLE IF NOT EXISTS secrets_meta (k TEXT PRIMARY KEY, v BLOB NOT NULL);

		-- env_vars holds plaintext, declared-non-sensitive variables. Note the
		-- deliberate key difference from secrets: a secret is UNIQUE (owner,
		-- env_name) — one value per owner, and its tags only choose where that
		-- one value lands — whereas a var is keyed by TAG as well, so NODE_ENV
		-- can be "development" under one environment and "staging" under
		-- another for the same person. That per-tag value is the reason this
		-- table exists instead of a plaintext column on secrets.
		CREATE TABLE IF NOT EXISTS env_vars (
			owner      TEXT NOT NULL,
			tag        TEXT NOT NULL,
			name       TEXT NOT NULL,
			value      TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (owner, tag, name)
		);
		CREATE INDEX IF NOT EXISTS env_vars_owner ON env_vars(owner);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	s := &Store{db: db, aead: aead, log: log}
	if err := s.keycheck(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return s, nil
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
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

var _ = addColumnIfMissing // referenced by the first future migration

func (s *Store) Close() error { return s.db.Close() }

// keycheck writes the sentinel on first open and verifies it on every later
// open. A verification failure means every stored ciphertext is orphaned
// (OIDC key rotation) — flag it once, loudly, instead of failing one row at a
// time, then rewrite the sentinel under the current KEK so the store heals as
// users re-enter values: writes and listing keep working, and orphaned rows
// surface as ErrUndecryptable through EnvForSandbox until re-entered or
// deleted. Only a failed sentinel rewrite leaves delivery disabled.
func (s *Store) keycheck() error {
	var blob []byte
	err := s.db.QueryRow(`SELECT v FROM secrets_meta WHERE k = ?`, keycheckKey).Scan(&blob)
	switch {
	case err == sql.ErrNoRows:
		sealed, err := seal(s.aead, []byte(keycheckAAD), []byte(keycheckPlain))
		if err != nil {
			return err
		}
		_, err = s.db.Exec(`INSERT INTO secrets_meta (k, v) VALUES (?, ?)`, keycheckKey, sealed)
		return err
	case err != nil:
		return err
	}
	if pt, err := unseal(s.aead, []byte(keycheckAAD), blob); err == nil && string(pt) == keycheckPlain {
		return nil
	}
	s.log.Error("secrets keycheck failed: stored secrets were encrypted under a different key (OIDC key rotated?); " +
		"existing values are undecryptable until re-entered or deleted — re-saving a secret re-encrypts it " +
		"under the current key; tags are unaffected")
	sealed, err := seal(s.aead, []byte(keycheckAAD), []byte(keycheckPlain))
	if err == nil {
		_, err = s.db.Exec(`UPDATE secrets_meta SET v = ? WHERE k = ?`, sealed, keycheckKey)
	}
	if err != nil {
		s.disabled = true
		s.log.Error("secrets keycheck sentinel could not be rewritten; secret delivery disabled", "err", err)
	}
	return nil
}

// TagsFor returns a sandbox's tags, sorted.
func (s *Store) TagsFor(sandbox string) ([]string, error) {
	rows, err := s.db.Query(`SELECT tag FROM sandbox_tags WHERE sandbox = ? ORDER BY tag`, sandbox)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}

// SetTags replaces a sandbox's tag set wholesale (the console sends the full
// set, so replace is the natural primitive and removal needs no separate op).
//
// It refuses a sandbox whose existing rows belong to somebody else. Tag rows
// are keyed by NAME, and a name is the one argument a caller supplies before
// anything has decided whether it is theirs — `new+<their-box>@` over SSH, or
// POST /v1/sandboxes with their name — so an unscoped replace let any user
// delete a stranger's tags. That is not cosmetic: tags select which of the
// owner's secrets are pushed into the guest env, and internal/netrules picks
// the per-tag egress allowlist from them, so a de-tagged VM comes back
// unfiltered. The owner gate above this one (ctlops resolves the name before
// stamping) is the first line of defence; this is the one no future caller can
// forget.
func (s *Store) SetTags(sandbox, owner string, tags []string) error {
	if sandbox == "" || owner == "" {
		return fmt.Errorf("tags need a sandbox and owner")
	}
	tags, err := normTags(tags)
	if err != nil {
		return err
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	// Inside the transaction, so the check and the replace cannot be separated.
	var other string
	switch err := tx.QueryRow(
		`SELECT owner FROM sandbox_tags WHERE sandbox = ? AND owner <> ? LIMIT 1`,
		sandbox, owner).Scan(&other); {
	case err == nil:
		return fmt.Errorf("sandbox %q is tagged by another owner", sandbox)
	case errors.Is(err, sql.ErrNoRows):
	default:
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sandbox_tags WHERE sandbox = ? AND owner = ?`,
		sandbox, owner); err != nil {
		return err
	}
	for _, tag := range tags {
		if _, err := tx.Exec(`INSERT INTO sandbox_tags (sandbox, owner, tag, created_at) VALUES (?, ?, ?, ?)`,
			sandbox, owner, tag, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteBySandbox removes every tag row for a sandbox (called on destroy).
func (s *Store) DeleteBySandbox(sandbox string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM sandbox_tags WHERE sandbox = ?`, sandbox)
	return err
}

// RenameSandbox moves a sandbox's tag rows to its new name. The manager
// guarantees no live sandbox owns the new name, but stale rows for it can
// survive a best-effort destroy/rename cleanup — those are dropped first, in
// the same transaction, so the move cannot trip the (sandbox, tag) primary
// key. Delete-then-move rather than UPDATE OR REPLACE: stale rows (possibly
// another owner's) must never merge into the renamed sandbox's tag set.
func (s *Store) RenameSandbox(old, new string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`DELETE FROM sandbox_tags WHERE sandbox = ?`, new); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE sandbox_tags SET sandbox = ? WHERE sandbox = ?`, new, old); err != nil {
		return err
	}
	return tx.Commit()
}

// checkEnvRow applies the rules every row this store may deliver has to
// satisfy, whether it is an encrypted secret or a plaintext var: a portable
// shell-identifier name, none of the reserved loader/PATH knobs, ≤ 4 KiB, and
// no '\r', '\n', NUL or '#'. The two writers share one implementation on
// purpose — a var must never be able to carry a payload a secret could not,
// since both end up as lines in the same /etc/environment block. kind names the
// row type in the message and is the only difference between the callers;
// PutSecret's wording is unchanged.
func checkEnvRow(kind, envName, value string) error {
	if !envNameRe.MatchString(envName) {
		return fmt.Errorf("invalid env name %q (want [A-Z_][A-Z0-9_]*, max 64 chars)", envName)
	}
	if reservedEnvNames[envName] {
		return fmt.Errorf("env name %s is reserved: it would break the guest's session environment", envName)
	}
	if len(value) > maxValueLen {
		return fmt.Errorf("%s value for %s exceeds %d bytes", kind, envName, maxValueLen)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s value for %s contains a newline or NUL and cannot be an env var", kind, envName)
	}
	if strings.Contains(value, "#") {
		return fmt.Errorf("%s value for %s contains '#', which /etc/environment treats as a comment even inside quotes", kind, envName)
	}
	return nil
}

// ValidateVar reports whether a plain env var could be written and delivered,
// without writing it.
//
// It exists so a caller composing several stores in one command can refuse a
// bad var BEFORE its first write, rather than discovering the rule from PutVar
// after an environment row and a retagged secret are already committed. The
// rules are the same ones PutVar enforces, from the same implementation, so the
// two cannot drift.
func ValidateVar(name, value string) error { return checkEnvRow("var", name, value) }

// PutSecret creates or updates the owner's secret for envName, replacing its
// tag set. An update bumps version and re-encrypts under a fresh nonce. The
// value must be a deliverable env var: ≤ 4 KiB, no newlines, no NULs, and no
// '#' — pam_env truncates an /etc/environment line at the first '#' before
// any quote handling, so such a value would be silently corrupted in the
// guest. Reserved names (PATH and the LD_* loader knobs) are refused too: a
// pushed PATH overrides sshd's default via pam_env and breaks the delivery
// channel itself. envsync enforces both again at render time as a backstop
// for pre-existing rows.
func (s *Store) PutSecret(owner, envName, value string, tags []string) error {
	if owner == "" {
		return fmt.Errorf("secret needs an owner")
	}
	if err := checkEnvRow("secret", envName, value); err != nil {
		return err
	}
	tags, err := normTags(tags)
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		// A secret with no tags reaches no sandbox at all, so an empty set is
		// never what the caller wanted — it is either a UI that did not ask or
		// a user who did not know they had to. Defaulting here rather than in
		// one caller keeps the console, the ssh channel and the REST API from
		// disagreeing about what "no tags" means.
		tags = []string{DefaultTag}
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var id string
	var version int
	err = tx.QueryRow(`SELECT id, version FROM secrets WHERE owner = ? AND env_name = ?`, owner, envName).Scan(&id, &version)
	switch {
	case err == sql.ErrNoRows:
		id = newID()
		ciphertext, err := seal(s.aead, aadFor(owner, envName, id), []byte(value))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO secrets (id, owner, env_name, ciphertext, key_id, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
			id, owner, envName, ciphertext, keyID, now, now); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		ciphertext, err := seal(s.aead, aadFor(owner, envName, id), []byte(value))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE secrets SET ciphertext = ?, key_id = ?, version = ?, updated_at = ? WHERE id = ?`,
			ciphertext, keyID, version+1, now, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM secret_tags WHERE secret_id = ?`, id); err != nil {
			return err
		}
	}
	for _, tag := range tags {
		if _, err := tx.Exec(`INSERT INTO secret_tags (secret_id, tag) VALUES (?, ?)`, id, tag); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RetagSecret replaces the tag set of an existing secret WITHOUT touching its
// value, its version or its key id, or returns ErrNoSuchSecret.
//
// It exists because PutSecret is the only other way to change a tag set and it
// demands the value, which no surface can supply: values are write-only from
// every API's point of view (there is deliberately no read-a-value method), so
// "also give this secret the tag `web`" was previously expressible only by
// asking the user to paste their token again. That is not a small annoyance —
// re-pasting a credential to change a label is exactly the sort of step that
// gets skipped, and the environment then reaches a sandbox without it.
//
// The version is deliberately NOT bumped: version counts changes to the secret
// MATERIAL, which is what a guest re-reads, and a tag change alters who
// receives the row rather than what it says. Callers that need the delivery
// refreshed re-push the affected sandboxes, exactly as they do after a
// PutSecret.
//
// An empty tag set defaults to DefaultTag for PutSecret's reason: a secret
// tagged with nothing reaches no sandbox at all, and that failure is silent
// until somebody's agent asks them to log in.
func (s *Store) RetagSecret(owner, envName string, tags []string) error {
	if owner == "" {
		return fmt.Errorf("secret needs an owner")
	}
	tags, err := normTags(tags)
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		tags = []string{DefaultTag}
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var id string
	// Owner-scoped in the SELECT, so another owner's secret of the same name is
	// simply not found — the same answer as a name nobody holds.
	if err := tx.QueryRow(`SELECT id FROM secrets WHERE owner = ? AND env_name = ?`, owner, envName).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return ErrNoSuchSecret
		}
		return err
	}
	if _, err := tx.Exec(`UPDATE secrets SET updated_at = ? WHERE id = ?`, now, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM secret_tags WHERE secret_id = ?`, id); err != nil {
		return err
	}
	for _, tag := range tags {
		if _, err := tx.Exec(`INSERT INTO secret_tags (secret_id, tag) VALUES (?, ?)`, id, tag); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RenameSecret changes the env name of an existing secret, keeping its value,
// its tags, its version and its row id, or returns ErrNoSuchSecret.
//
// It has to touch the ciphertext even though the value does not change: the
// AAD binds every blob to (owner, env name, id), which is exactly what stops a
// row being re-homed under another name by editing the database. So a rename
// is an unseal under the old name followed by a seal under the new one — the
// same move Resealer makes for an owner rename, and the reason a rename cannot
// be done without the key.
//
// The version is deliberately NOT bumped, for RetagSecret's reason: version
// counts changes to the secret MATERIAL, and this is the same value under a
// different name. Callers re-push the affected sandboxes.
func (s *Store) RenameSecret(owner, oldName, newName string) error {
	if owner == "" {
		return fmt.Errorf("secret needs an owner")
	}
	// The value rules do not apply to a rename (nothing is being written), but
	// the NAME rules do, and they are the same ones PutSecret would enforce.
	if err := checkEnvRow("secret", newName, ""); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	now := time.Now().UTC()

	// No s.disabled gate: unlike the delivery paths this is a write, and a
	// store whose key does not match answers through the unseal below with
	// ErrUndecryptable — which says the specific thing that went wrong.
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var id string
	var blob []byte
	if err := tx.QueryRow(`SELECT id, ciphertext FROM secrets WHERE owner = ? AND env_name = ?`,
		owner, oldName).Scan(&id, &blob); err != nil {
		if err == sql.ErrNoRows {
			return ErrNoSuchSecret
		}
		return err
	}
	var taken string
	switch err := tx.QueryRow(`SELECT id FROM secrets WHERE owner = ? AND env_name = ?`,
		owner, newName).Scan(&taken); {
	case err == nil:
		return ErrSecretNameInUse
	case err != sql.ErrNoRows:
		return err
	}

	plaintext, err := unseal(s.aead, aadFor(owner, oldName, id), blob)
	if err != nil {
		return err
	}
	ciphertext, err := seal(s.aead, aadFor(owner, newName, id), plaintext)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE secrets SET env_name = ?, ciphertext = ?, key_id = ?, updated_at = ? WHERE id = ?`,
		newName, ciphertext, keyID, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteSecret removes the owner's secret for envName, or ErrNoSuchSecret.
// Tag rows are deleted explicitly in the same transaction: the ON DELETE
// CASCADE (enforced on every pooled connection via the DSN foreign_keys
// pragma) stays a schema-level backstop.
func (s *Store) DeleteSecret(owner, envName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`
		DELETE FROM secret_tags WHERE secret_id IN (SELECT id FROM secrets WHERE owner = ? AND env_name = ?)`,
		owner, envName); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM secrets WHERE owner = ? AND env_name = ?`, owner, envName)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchSecret
	}
	return tx.Commit()
}

// ListSecrets returns the owner's secrets as metadata only — values are
// write-only and never listed.
func (s *Store) ListSecrets(owner string) ([]SecretMeta, error) {
	rows, err := s.db.Query(`
		SELECT id, env_name, version, created_at, updated_at FROM secrets WHERE owner = ? ORDER BY env_name`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretMeta
	var ids []string
	for rows.Next() {
		var m SecretMeta
		var id string
		if err := rows.Scan(&id, &m.Name, &m.Version, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Tags = []string{}
		out = append(out, m)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	trows, err := s.db.Query(`
		SELECT st.secret_id, st.tag FROM secret_tags st
		JOIN secrets s ON s.id = st.secret_id
		WHERE s.owner = ? ORDER BY st.tag`, owner)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	byID := make(map[string]int, len(ids))
	for i, id := range ids {
		byID[id] = i
	}
	for trows.Next() {
		var id, tag string
		if err := trows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		if i, ok := byID[id]; ok {
			out[i].Tags = append(out[i].Tags, tag)
		}
	}
	return out, trows.Err()
}

// EnvForSandbox computes the environment a sandbox should see: every plain var
// and every secret of the sandbox's owner sharing a tag with the sandbox. Owner
// scoping is structural in both queries — each join requires bt.owner = x.owner
// AND x.owner = ?, so no caller mistake can pull another owner's values.
// Decryption is all-or-nothing: one undecryptable row fails the whole call with
// ErrUndecryptable, never a partial environment (and never the vars alone).
//
// The merge order is load-bearing: vars are read FIRST and secrets are written
// over them, so on a name collision the SECRET wins. The reverse would let a
// plaintext row shadow an encrypted one — a var named the same as a secret
// would silently replace it in the guest, which is both a wrong value and a
// downgrade a user never asked for. Collisions are otherwise legal and quiet;
// nothing refuses a var whose name matches a secret, because the two are
// written by different surfaces at different times and refusing at write time
// would only move the surprise.
//
// This is the single delivery query. internal/envsync calls it and knows
// nothing about vars.
func (s *Store) EnvForSandbox(sandbox, owner string) (map[string]string, error) {
	if s.disabled { // delivery-only gate: writes/listing recover the store
		// Unchanged from before vars existed: a store that cannot verify its
		// own key refuses to compute an environment at all rather than deliver
		// the half of it that happens to be plaintext. Vars remain reachable
		// through VarsForSandbox, which is never gated.
		return nil, ErrNotEnabled
	}
	env, err := s.VarsForSandbox(sandbox, owner)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT DISTINCT s.id, s.env_name, s.ciphertext
		FROM secrets s
		JOIN secret_tags st ON s.id = st.secret_id
		JOIN sandbox_tags bt ON bt.tag = st.tag AND bt.owner = s.owner
		WHERE bt.sandbox = ? AND s.owner = ?`, sandbox, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		id, name string
		blob     []byte
	}
	var sealedRows []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.blob); err != nil {
			return nil, err
		}
		sealedRows = append(sealedRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, r := range sealedRows {
		pt, err := unseal(s.aead, aadFor(owner, r.name, r.id), r.blob)
		if err != nil {
			// All-or-nothing: drop the vars already collected too, so a caller
			// can never mistake a partial environment for a whole one.
			return nil, fmt.Errorf("secret %s: %w", r.name, ErrUndecryptable)
		}
		env[r.name] = string(pt) // secrets overwrite vars, never the other way
	}
	return env, nil
}

// SandboxesForSecret returns which of the owner's sandboxes receive envName —
// the fan-out set to re-push after a secret changes.
func (s *Store) SandboxesForSecret(owner, envName string) ([]string, error) {
	if s.disabled { // delivery-only gate: writes/listing recover the store
		return nil, ErrNotEnabled
	}
	rows, err := s.db.Query(`
		SELECT DISTINCT bt.sandbox
		FROM secrets s
		JOIN secret_tags st ON s.id = st.secret_id
		JOIN sandbox_tags bt ON bt.tag = st.tag AND bt.owner = s.owner
		WHERE s.owner = ? AND s.env_name = ?
		ORDER BY bt.sandbox`, owner, envName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sandbox string
		if err := rows.Scan(&sandbox); err != nil {
			return nil, err
		}
		out = append(out, sandbox)
	}
	return out, rows.Err()
}

// --- plain env vars ------------------------------------------------------
//
// Everything from here to normTags operates on env_vars. None of it touches
// s.aead, s.disabled, or the keycheck sentinel: vars are plaintext by
// declaration, so an OIDC key rotation that orphans every secret must still
// leave vars listable, writable, and deliverable. The one place the two meet is
// EnvForSandbox, which merges them.

// PutVar creates or updates the owner's plain env var name under tag. The name
// and value rules are exactly a secret's — same helper, same refusals — because
// both end up as lines in the same /etc/environment block. The value is stored
// as given, in plaintext: a var is a declaration that this string is not
// sensitive. Anything that needs encrypting is a secret, not a var.
//
// Unlike a secret, which is UNIQUE (owner, env_name), a var is keyed by tag as
// well, so the same name under two tags is two independent rows with two
// values.
func (s *Store) PutVar(owner, tag, name, value string) error {
	if owner == "" {
		return fmt.Errorf("env var needs an owner")
	}
	if err := checkEnvRow("env var", name, value); err != nil {
		return err
	}
	tag, err := normTag(tag)
	if err != nil {
		return err
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	// Upsert rather than select-then-write: the primary key is the whole
	// identity of the row, so there is no version to bump and nothing to
	// re-seal, and created_at survives an update.
	_, err = s.db.Exec(`
		INSERT INTO env_vars (owner, tag, name, value, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner, tag, name)
		DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		owner, tag, name, value, now, now)
	return err
}

// DeleteVar removes one of the owner's vars, or ErrNoSuchVar. It is scoped to
// (owner, tag, name) — the same var name under another of the owner's tags is
// a different row and is left alone.
func (s *Store) DeleteVar(owner, tag, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM env_vars WHERE owner = ? AND tag = ? AND name = ?`, owner, tag, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchVar
	}
	return nil
}

// ListVars returns every var the owner has, across all tags, sorted by tag then
// name. Values are included — see Var.
func (s *Store) ListVars(owner string) ([]Var, error) {
	return s.queryVars(`
		SELECT tag, name, value, created_at, updated_at FROM env_vars
		WHERE owner = ? ORDER BY tag, name`, owner)
}

// VarsForTag returns the owner's vars carrying exactly tag, sorted by name.
func (s *Store) VarsForTag(owner, tag string) ([]Var, error) {
	return s.queryVars(`
		SELECT tag, name, value, created_at, updated_at FROM env_vars
		WHERE owner = ? AND tag = ? ORDER BY name`, owner, tag)
}

func (s *Store) queryVars(query string, args ...any) ([]Var, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Var
	for rows.Next() {
		var v Var
		if err := rows.Scan(&v.Tag, &v.Name, &v.Value, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VarsForSandbox computes the plain env vars a sandbox should see: every var of
// the sandbox's owner sharing a tag with the sandbox. Owner scoping is
// structural, exactly as in EnvForSandbox — the join requires bt.owner =
// v.owner AND v.owner = ?, so no caller mistake can pull another owner's rows.
//
// A sandbox carrying two tags that both define the same name is resolved
// deterministically rather than by whatever order sqlite felt like: rows arrive
// ordered by tag, and later assignment wins, so the alphabetically last tag
// supplies the value.
func (s *Store) VarsForSandbox(sandbox, owner string) (map[string]string, error) {
	rows, err := s.db.Query(`
		SELECT v.name, v.value
		FROM env_vars v
		JOIN sandbox_tags bt ON bt.tag = v.tag AND bt.owner = v.owner
		WHERE bt.sandbox = ? AND v.owner = ?
		ORDER BY v.tag`, sandbox, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	env := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		env[name] = value
	}
	return env, rows.Err()
}

// DeleteVarsForTag removes every var the owner holds under tag — the cleanup an
// environment's deletion performs. It is scoped to one owner: an identical tag
// string belonging to somebody else is untouched.
func (s *Store) DeleteVarsForTag(owner, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM env_vars WHERE owner = ? AND tag = ?`, owner, tag)
	return err
}

// normTag validates a single tag through the same filter normTags applies to a
// list, and supplies DefaultTag when the caller names none — for the reason
// spelled out on DefaultTag: a row attached to no tag reaches no sandbox, and
// that failure is silent minutes later inside a guest rather than loud here.
func normTag(tag string) (string, error) {
	if tag == "" {
		return DefaultTag, nil
	}
	tags, err := normTags([]string{tag})
	if err != nil {
		return "", err
	}
	return tags[0], nil
}

// normTags validates, sorts, and de-duplicates a tag list.
func normTags(tags []string) ([]string, error) {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if !tagRe.MatchString(tag) {
			return nil, fmt.Errorf("invalid tag %q (want [a-z0-9][a-z0-9-]*, max 40 chars)", tag)
		}
		if !seen[tag] {
			seen[tag] = true
			out = append(out, tag)
		}
	}
	sort.Strings(out)
	return out, nil
}

// newID returns a short random hex id for a secret row.
func newID() string {
	var b [5]byte
	rand.Read(b[:]) //nolint:errcheck // crypto/rand never fails on these platforms
	return hex.EncodeToString(b[:])
}
