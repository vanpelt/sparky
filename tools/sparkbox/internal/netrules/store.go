// Package netrules is the sqlite-backed store of per-tag network rule-sets: a
// rule-set is a named list of allowed egress domains owned by a handle, a tag
// connects rule-sets to sandboxes, and RulesForSandbox computes the rules a
// sandbox should be governed by — owner scoping is structural in that query,
// not handler discipline. It mirrors internal/secrets (same DB file, own
// connection, WAL, pure-Go driver) minus the crypto: domain patterns are policy
// config, not credentials, so the spec is stored in plaintext.
//
// The sandbox_tags table is shared with internal/secrets; netrules only reads
// it. Whichever package opens the DB first creates the table (both use
// CREATE TABLE IF NOT EXISTS), and secrets owns the tag mutations
// (SetTags/DeleteBySandbox/RenameSandbox).
package netrules

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNoSuchRule is returned when an operation targets a name the owner has no
// rule-set for. It maps to 404 via the console's statusFor convention.
var ErrNoSuchRule = errors.New("no such network rule")

// ErrInvalidRule wraps every validation failure (bad name, pattern, or tag) so
// callers can map the whole family to 400 without message matching.
var ErrInvalidRule = errors.New("invalid network rule")

// ruleNameRe bounds a rule-set name to a short, UI/URL-safe label. Slightly
// friendlier than a tag (allows spaces and capitals) since it is a display name.
var ruleNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]{0,63}$`)

// tagRe matches internal/secrets so the tag namespaces align exactly.
var tagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// domainRe validates a bare domain label sequence (no scheme, no wildcard).
var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// maxAllow caps a rule-set at a sane number of patterns — a policy, not a feed.
const maxAllow = 256

// RuleSpec is the body of a rule-set. v1 is an egress allowlist only (the
// chosen model: allow + enforce, no bandwidth caps); the struct leaves room to
// grow without a schema change since it is stored as JSON.
type RuleSpec struct {
	// Allow is the list of permitted egress domain patterns. Each is either a
	// bare domain ("github.com" — apex and subdomains) or a "*.domain" wildcard
	// (subdomains only), matching sluice's allowlist syntax.
	Allow []string `json:"allow"`
}

// RuleMeta is the listable shape of a rule-set: unlike a secret, the spec is not
// sensitive, so it is returned in full for display and for the policy pusher.
type RuleMeta struct {
	Name      string    `json:"name"`
	Tags      []string  `json:"tags"`
	Spec      RuleSpec  `json:"spec"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is the netrules database handle.
type Store struct {
	mu  sync.Mutex // serialises writes (sqlite is single-writer)
	db  *sql.DB
	log *slog.Logger
}

// Open opens (creating if needed) the sqlite database at path and applies the
// schema. It shares the file with internal/secrets/routes on its own connection;
// WAL keeps that safe. See internal/secrets for the DSN rationale.
func Open(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
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
		CREATE TABLE IF NOT EXISTS sandbox_tags (
			sandbox    TEXT NOT NULL,
			owner      TEXT NOT NULL,
			tag        TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			PRIMARY KEY (sandbox, tag)
		);
		CREATE INDEX IF NOT EXISTS sandbox_tags_owner ON sandbox_tags(owner);
		CREATE INDEX IF NOT EXISTS sandbox_tags_tag   ON sandbox_tags(owner, tag);

		CREATE TABLE IF NOT EXISTS network_rules (
			id         TEXT PRIMARY KEY,
			owner      TEXT NOT NULL,
			name       TEXT NOT NULL,
			spec       TEXT NOT NULL,
			version    INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			UNIQUE (owner, name)
		);

		CREATE TABLE IF NOT EXISTS network_rule_tags (
			rule_id TEXT NOT NULL REFERENCES network_rules(id) ON DELETE CASCADE,
			tag     TEXT NOT NULL,
			PRIMARY KEY (rule_id, tag)
		);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Store{db: db, log: log}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// PutRule creates or updates the owner's rule-set for name, replacing its tag
// set. An update bumps version. The spec's allow patterns are validated and
// normalised; the tags are validated against tagRe so they match the shared
// sandbox_tags namespace.
func (s *Store) PutRule(owner, name string, spec RuleSpec, tags []string) error {
	if owner == "" {
		return fmt.Errorf("rule needs an owner")
	}
	if !ruleNameRe.MatchString(name) {
		return fmt.Errorf("%w: name %q (want a short label, max 64 chars)", ErrInvalidRule, name)
	}
	norm, err := normSpec(spec)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRule, err)
	}
	tags, err = normTags(tags)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRule, err)
	}
	blob, err := json.Marshal(norm)
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

	var id string
	var version int
	err = tx.QueryRow(`SELECT id, version FROM network_rules WHERE owner = ? AND name = ?`, owner, name).Scan(&id, &version)
	switch {
	case err == sql.ErrNoRows:
		id = newID()
		if _, err := tx.Exec(`
			INSERT INTO network_rules (id, owner, name, spec, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, 1, ?, ?)`,
			id, owner, name, string(blob), now, now); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err := tx.Exec(`UPDATE network_rules SET spec = ?, version = ?, updated_at = ? WHERE id = ?`,
			string(blob), version+1, now, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM network_rule_tags WHERE rule_id = ?`, id); err != nil {
			return err
		}
	}
	for _, tag := range tags {
		if _, err := tx.Exec(`INSERT INTO network_rule_tags (rule_id, tag) VALUES (?, ?)`, id, tag); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteRule removes the owner's rule-set for name, or ErrNoSuchRule. Tag rows
// are deleted explicitly in the same transaction; the ON DELETE CASCADE stays a
// schema-level backstop.
func (s *Store) DeleteRule(owner, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`
		DELETE FROM network_rule_tags WHERE rule_id IN (SELECT id FROM network_rules WHERE owner = ? AND name = ?)`,
		owner, name); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM network_rules WHERE owner = ? AND name = ?`, owner, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchRule
	}
	return tx.Commit()
}

// ListRules returns the owner's rule-sets in full (name, spec, tags), sorted by
// name.
func (s *Store) ListRules(owner string) ([]RuleMeta, error) {
	rows, err := s.db.Query(`
		SELECT id, name, spec, version, created_at, updated_at FROM network_rules WHERE owner = ? ORDER BY name`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RuleMeta
	var ids []string
	for rows.Next() {
		var m RuleMeta
		var id, blob string
		if err := rows.Scan(&id, &m.Name, &blob, &m.Version, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(blob), &m.Spec); err != nil {
			return nil, fmt.Errorf("rule %q: bad spec: %w", m.Name, err)
		}
		if m.Spec.Allow == nil {
			m.Spec.Allow = []string{}
		}
		m.Tags = []string{}
		out = append(out, m)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.attachTags(owner, out, ids)
}

// attachTags fills each rule's Tags by a single joined query, matching the id
// order of out via ids.
func (s *Store) attachTags(owner string, out []RuleMeta, ids []string) ([]RuleMeta, error) {
	if len(out) == 0 {
		return out, nil
	}
	trows, err := s.db.Query(`
		SELECT rt.rule_id, rt.tag FROM network_rule_tags rt
		JOIN network_rules r ON r.id = rt.rule_id
		WHERE r.owner = ? ORDER BY rt.tag`, owner)
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

// RulesForSandbox returns every rule-set of the sandbox's owner that shares a
// tag with the sandbox. Owner scoping is structural — the join requires
// bt.owner = r.owner AND r.owner = ? — so no caller mistake can pull another
// owner's rules. The policy pusher merges these specs into one effective
// allow-set for the VM.
func (s *Store) RulesForSandbox(sandbox, owner string) ([]RuleMeta, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT r.id, r.name, r.spec, r.version, r.created_at, r.updated_at
		FROM network_rules r
		JOIN network_rule_tags rt ON r.id = rt.rule_id
		JOIN sandbox_tags bt ON bt.tag = rt.tag AND bt.owner = r.owner
		WHERE bt.sandbox = ? AND r.owner = ?
		ORDER BY r.name`, sandbox, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RuleMeta
	var ids []string
	for rows.Next() {
		var m RuleMeta
		var id, blob string
		if err := rows.Scan(&id, &m.Name, &blob, &m.Version, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(blob), &m.Spec); err != nil {
			return nil, fmt.Errorf("rule %q: bad spec: %w", m.Name, err)
		}
		if m.Spec.Allow == nil {
			m.Spec.Allow = []string{}
		}
		m.Tags = []string{}
		out = append(out, m)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.attachTags(owner, out, ids)
}

// SandboxesForRule returns which of the owner's sandboxes are governed by the
// named rule-set — the fan-out set to re-push after a rule changes.
func (s *Store) SandboxesForRule(owner, name string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT bt.sandbox
		FROM network_rules r
		JOIN network_rule_tags rt ON r.id = rt.rule_id
		JOIN sandbox_tags bt ON bt.tag = rt.tag AND bt.owner = r.owner
		WHERE r.owner = ? AND r.name = ?
		ORDER BY bt.sandbox`, owner, name)
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

// AllowForSandbox is a convenience that merges every rule-set governing the
// sandbox into one de-duplicated, sorted allow-pattern list — the exact payload
// the policy pusher sends to sluice for the VM's tap. The governed return
// reports whether any rule-set applies at all: a sandbox with no tag bound to a
// rule is ungoverned, and the pusher omits it so sluice leaves its egress
// unrestricted (an empty allow-list, by contrast, is a deliberate deny-all on a
// governed sandbox). governed is therefore distinct from len(allow) == 0.
func (s *Store) AllowForSandbox(sandbox, owner string) (allow []string, governed bool, err error) {
	rules, err := s.RulesForSandbox(sandbox, owner)
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range rules {
		for _, p := range r.Spec.Allow {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out, len(rules) > 0, nil
}

// normSpec validates and normalises a rule spec: each allow pattern is
// lower-cased, de-duplicated, and checked against the "domain" / "*.domain"
// forms sluice accepts, so a bad rule is rejected here rather than by sluice.
func normSpec(spec RuleSpec) (RuleSpec, error) {
	if len(spec.Allow) > maxAllow {
		return RuleSpec{}, fmt.Errorf("too many allow patterns (%d > %d)", len(spec.Allow), maxAllow)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(spec.Allow))
	for _, raw := range spec.Allow {
		p := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), ".")))
		if p == "" {
			continue
		}
		if err := validateAllowPattern(p); err != nil {
			return RuleSpec{}, fmt.Errorf("allow %q: %w", raw, err)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return RuleSpec{Allow: out}, nil
}

// validateAllowPattern accepts "github.com" (apex + subdomains) or
// "*.github.com" (subdomains only), matching sluice's allowlist.add.
func validateAllowPattern(p string) error {
	if rest, ok := strings.CutPrefix(p, "*."); ok {
		if rest == "" || strings.ContainsRune(rest, '*') || !domainRe.MatchString(rest) {
			return fmt.Errorf("wildcard must be of the form *.example.com")
		}
		return nil
	}
	if strings.ContainsRune(p, '*') {
		return fmt.Errorf("'*' is only allowed as a leading *. label")
	}
	if !domainRe.MatchString(p) {
		return fmt.Errorf("not a domain")
	}
	return nil
}

// normTags validates, sorts, and de-duplicates a tag list against tagRe.
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

// newID returns a short random hex id for a rule row. math/rand/v2 is seeded by
// the runtime and needs no crypto strength — this is a row id, not a secret.
func newID() string {
	var b [5]byte
	for i := range b {
		b[i] = byte(rand.UintN(256))
	}
	return hex.EncodeToString(b[:])
}
