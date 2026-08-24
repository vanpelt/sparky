// Package repos is the sqlite-backed store of per-tag repository attachments: an
// attachment says "this GitHub repository belongs in any sandbox carrying this
// tag", a tag connects attachments to sandboxes, and ReposForSandbox computes
// the checkout manifest a guest should have — owner scoping is structural in
// that query, not handler discipline. It mirrors internal/netrules (same DB
// file, own connection, WAL, pure-Go driver, tagRe copied verbatim) minus the
// rule spec: a repo slug is configuration, not a credential, so it is stored in
// plaintext. The credential that clones the repository is never stored at all —
// it is minted per request from the GitHub App and expires in an hour.
//
// The sandbox_tags table is shared with internal/secrets; repos only reads it.
// Whichever package opens the DB first creates the table (all three use
// CREATE TABLE IF NOT EXISTS with a byte-identical DDL), and secrets owns the
// tag mutations (SetTags/DeleteBySandbox/RenameSandbox). If this package ever
// writes a sandbox_tags row, secrets' in-transaction cross-owner refusal is
// bypassed and the invariant that a tag belongs to exactly one handle is gone.
//
// The blast radius of a mistake here is larger than it is in netrules: a join
// that loses its owner term leaks another user's private repository into a
// sandbox, not merely an extra egress domain. Every *ForSandbox query therefore
// carries the owner on BOTH sides of the join, and TestReposForSandboxScopesByOwner
// exists to keep it that way.
package repos

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"

	_ "modernc.org/sqlite"
)

// Access levels. Read is what a checkout needs; write is what a push needs, and
// it is what the credential minter turns into "contents: write" on the
// installation token, so it is deliberately a separate, opt-in word rather than
// a flag somebody sets once and forgets.
const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// ErrNoSuchRepo is returned when an operation targets a (host, slug) the owner
// has no attachment for. It maps to 404 via the console's statusFor convention.
var ErrNoSuchRepo = errors.New("no such repo")

// ErrInvalidRepo wraps every validation failure (bad slug, ref, path, access, or
// tag) so callers can map the whole family to 400 without message matching.
var ErrInvalidRepo = errors.New("invalid repo")

// tagRe matches internal/secrets so the tag namespaces align exactly.
var tagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// repoNameRe is deliberately NOT the login grammar users.ValidGitHubOrg checks.
// A GitHub login is letters, digits and interior hyphens; a repository name
// additionally admits '.' and '_', may start with a digit or a dot, and runs to
// 100 characters. Checking the name half against the login grammar would reject
// perfectly ordinary repositories (node.js, my_repo, 2fa). The forms this
// regexp cannot express — "." and ".." as names, an all-dots name, a trailing
// ".git" — are rejected by validRepoName below.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// refRe bounds a branch/tag name. The leading character must be alphanumeric,
// which is the load-bearing part: the ref reaches the guest as the argument of
// `git clone --branch <ref>`, where a value starting with '-' is an option
// rather than a ref. Interior '/' is allowed because feature/x is how people
// name branches.
var refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)

// pathRe bounds a clone destination. It is relative by construction (no leading
// '/', '~' or '-') because the guest resolves it against the login user's home
// directory; validRelPath additionally refuses a ".." segment, so a stored path
// cannot walk out of the home directory the person thought they were
// configuring.
var pathRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)

// maxReposPerOwner caps an owner's attachment list. This is a working set — the
// repositories a person wants their sandboxes to come up holding — not a mirror
// of their GitHub account, and an unbounded list turns first boot into a clone
// storm.
const maxReposPerOwner = 128

// defaultHost is the only host this store accepts today. GHES is a deliberate
// non-feature rather than an oversight: another host needs its own clone
// domains in DomainsForSandbox and its own App installation, and accepting the
// string now would let a row exist that no credential path can serve.
const defaultHost = "github.com"

// cloneDomains is what an HTTPS `git clone` of a github.com repository actually
// talks to, sorted. github.com serves the smart-HTTP refs advertisement,
// codeload.github.com serves archive and pack downloads, and
// objects.githubusercontent.com serves the LFS/blob redirects a partial clone
// follows. Allowing only github.com produces a clone that resolves refs and
// then stalls on the pack, which reads as a hang rather than as a policy
// refusal — which is precisely why the egress overlay unions all three.
var cloneDomains = []string{
	"codeload.github.com",
	"github.com",
	"objects.githubusercontent.com",
}

// Repo is the listable shape of an attachment. Unlike a secret, none of it is
// sensitive — the design's own argument is that a repo slug is configuration —
// so it is returned in full for display, for the ctl surface, and for the
// manifest the guest reads.
type Repo struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`          // sparkbox handle
	Host      string    `json:"host"`           // always "github.com" for now
	Slug      string    `json:"slug"`           // "wandb/hivemind"
	Ref       string    `json:"ref,omitempty"`  // "" = the repo's default branch
	Path      string    `json:"path,omitempty"` // "" = default layout
	Access    string    `json:"access"`         // AccessRead | AccessWrite
	Tags      []string  `json:"tags"`           // never nil
	CreatedAt time.Time `json:"created_at"`
}

// Store is the repos database handle.
type Store struct {
	mu  sync.Mutex // serialises writes (sqlite is single-writer)
	db  *sql.DB
	log *slog.Logger
}

// Open opens (creating if needed) the sqlite database at path and applies the
// schema. It shares the file with internal/secrets, internal/netrules and
// internal/routes on its own connection; WAL keeps that safe. See
// internal/secrets for the DSN rationale — the short version is that the
// _pragma DSN params run on every pooled connection where a db.Exec pragma
// binds to only one, and _txlock=immediate takes the write lock up front so the
// read-then-write upsert in PutRepo cannot hit SQLITE_BUSY_SNAPSHOT, which the
// busy handler never waits on. Every Begin in this store writes.
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
	// internal/secrets and internal/netrules. Three packages now race to
	// create it and no coordination is needed — but if the copies ever
	// drift, the loser of the race silently gets the other's schema, so
	// this must stay byte-identical rather than merely equivalent.
	//
	// ref and path are NOT NULL DEFAULT '' where the design doc had them
	// nullable: "" already means "the default" for both, and a NULL would
	// be a second spelling of the same state that every Scan would have to
	// unwrap through sql.NullString.
	//
	// slug carries COLLATE NOCASE because github.com is case-insensitive on
	// both halves. Without it wandb/Hivemind and wandb/hivemind are two rows
	// for one repository, with two tag sets and two answers to "is it
	// attached", while the case a person typed is still worth keeping for
	// display — so the column stores what they typed and compares without
	// regard to case.
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

		CREATE TABLE IF NOT EXISTS repos (
			id         TEXT PRIMARY KEY,
			owner      TEXT NOT NULL,
			host       TEXT NOT NULL,
			slug       TEXT NOT NULL COLLATE NOCASE,
			ref        TEXT NOT NULL DEFAULT '',
			path       TEXT NOT NULL DEFAULT '',
			access     TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			UNIQUE (owner, host, slug)
		);
		CREATE INDEX IF NOT EXISTS repos_owner ON repos(owner);

		CREATE TABLE IF NOT EXISTS repo_tags (
			repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
			tag     TEXT NOT NULL,
			PRIMARY KEY (repo_id, tag)
		);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Store{db: db, log: log}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// PutRepo creates or updates the owner's attachment for (host, slug), replacing
// its tag set wholesale. r.Owner must be empty or equal to owner — the argument
// is the authority, and a mismatch is a caller bug worth refusing rather than
// silently writing under the wrong handle. r.ID and r.Tags are ignored: the id
// is the store's, and the tag set is the tags argument.
//
// The store never stamps secrets.DefaultTag on an untagged attachment, unlike
// PutSecret. That asymmetry is deliberate. An untagged secret that lands in
// every future sandbox is an environment variable; an untagged repository that
// lands in every future sandbox is a checkout somebody did not ask for, in a
// home directory they are using. A repo with no tags is a configured, unreached
// attachment, and the ctl surface is where the choice to say `--tag default` is
// made and printed.
func (s *Store) PutRepo(owner string, r Repo, tags []string) error {
	if owner == "" {
		return fmt.Errorf("%w: repo needs an owner", ErrInvalidRepo)
	}
	if r.Owner != "" && r.Owner != owner {
		return fmt.Errorf("%w: repo carries owner %q but is being written for %q", ErrInvalidRepo, r.Owner, owner)
	}
	host, err := normHost(r.Host)
	if err != nil {
		return err
	}
	slug, err := normSlug(r.Slug)
	if err != nil {
		return err
	}
	ref := strings.TrimSpace(r.Ref)
	if ref != "" && !refRe.MatchString(ref) {
		return fmt.Errorf("%w: ref %q (want a branch or tag name, no leading '-')", ErrInvalidRepo, r.Ref)
	}
	if strings.Contains(ref, "..") {
		return fmt.Errorf("%w: ref %q (\"..\" is not a ref)", ErrInvalidRepo, r.Ref)
	}
	path, err := normPath(r.Path)
	if err != nil {
		return err
	}
	access, err := NormalizeAccess(r.Access)
	if err != nil {
		return err
	}
	tags, err = normTags(tags)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRepo, err)
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
	err = tx.QueryRow(`SELECT id FROM repos WHERE owner = ? AND host = ? AND slug = ?`, owner, host, slug).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		// The cap is checked inside the transaction, on the insert path
		// only: an update must never start failing because the list is
		// full, or an owner at the cap could not fix a bad ref.
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM repos WHERE owner = ?`, owner).Scan(&n); err != nil {
			return err
		}
		if n >= maxReposPerOwner {
			return fmt.Errorf("%w: too many repos (%d, max %d)", ErrInvalidRepo, n, maxReposPerOwner)
		}
		id = newID()
		if _, err := tx.Exec(`
			INSERT INTO repos (id, owner, host, slug, ref, path, access, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, owner, host, slug, ref, path, access, now); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		// created_at is not touched: the attachment is the same one, and
		// its age is the answer to "how long has this been on my boxes".
		// The slug is rewritten so a case correction sticks.
		if _, err := tx.Exec(`UPDATE repos SET slug = ?, ref = ?, path = ?, access = ? WHERE id = ?`,
			slug, ref, path, access, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM repo_tags WHERE repo_id = ?`, id); err != nil {
			return err
		}
	}
	for _, tag := range tags {
		if _, err := tx.Exec(`INSERT INTO repo_tags (repo_id, tag) VALUES (?, ?)`, id, tag); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteRepo removes the owner's attachment for (host, slug), or ErrNoSuchRepo.
// Tag rows are deleted explicitly in the same transaction; the ON DELETE CASCADE
// stays a schema-level backstop.
//
// Detaching does not touch any guest. A repository already cloned into a running
// sandbox stays cloned — this removes it from the manifest, and from the egress
// overlay the next push computes.
func (s *Store) DeleteRepo(owner, host, slug string) error {
	host, err := normHost(host)
	if err != nil {
		return err
	}
	slug, err = normSlug(slug)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`
		DELETE FROM repo_tags WHERE repo_id IN (SELECT id FROM repos WHERE owner = ? AND host = ? AND slug = ?)`,
		owner, host, slug); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM repos WHERE owner = ? AND host = ? AND slug = ?`, owner, host, slug)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchRepo
	}
	return tx.Commit()
}

// ListRepos returns the owner's attachments in full, sorted by host then slug.
func (s *Store) ListRepos(owner string) ([]Repo, error) {
	rows, err := s.db.Query(`
		SELECT id, owner, host, slug, ref, path, access, created_at
		FROM repos WHERE owner = ? ORDER BY host, slug`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, ids, err := scanRepos(rows)
	if err != nil {
		return nil, err
	}
	return s.attachTags(owner, out, ids)
}

// ReposForSandbox returns every attachment of the sandbox's owner that shares a
// tag with the sandbox — the checkout manifest for that guest.
//
// Owner scoping is structural: the join requires bt.owner = r.owner AND
// r.owner = ?. The first term looks redundant next to the second and is not.
// Without it, any tag name two people happen to share (ci, prod, dev) joins
// their rows together, and what leaks is a private repository — the slug in the
// manifest, and then a credential minted for it. No caller-side check replaces
// this; it has to be in the SQL.
func (s *Store) ReposForSandbox(sandbox, owner string) ([]Repo, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT r.id, r.owner, r.host, r.slug, r.ref, r.path, r.access, r.created_at
		FROM repos r
		JOIN repo_tags rt ON r.id = rt.repo_id
		JOIN sandbox_tags bt ON bt.tag = rt.tag AND bt.owner = r.owner
		WHERE bt.sandbox = ? AND r.owner = ?
		ORDER BY r.host, r.slug`, sandbox, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, ids, err := scanRepos(rows)
	if err != nil {
		return nil, err
	}
	return s.attachTags(owner, out, ids)
}

// SandboxesForRepo returns which of the owner's sandboxes carry the attachment —
// the fan-out set to re-push (or to name in "detaching this affects…") after it
// changes. Same owner-on-both-sides join as ReposForSandbox, for the same
// reason.
func (s *Store) SandboxesForRepo(owner, host, slug string) ([]string, error) {
	host, err := normHost(host)
	if err != nil {
		return nil, err
	}
	slug, err = normSlug(slug)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT DISTINCT bt.sandbox
		FROM repos r
		JOIN repo_tags rt ON r.id = rt.repo_id
		JOIN sandbox_tags bt ON bt.tag = rt.tag AND bt.owner = r.owner
		WHERE r.owner = ? AND r.host = ? AND r.slug = ?
		ORDER BY bt.sandbox`, owner, host, slug)
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

// DomainsForSandbox returns the egress domains the sandbox's attachments imply,
// sorted, and an empty slice when it has none. The netrules overlay unions these
// into the effective allow-set rather than writing them into anybody's stored
// rule spec — a detached repo must not leave its holes behind, and a rule the
// user did not write is not a rule they should have to delete.
//
// The list is per-sandbox rather than per-repo because it does not vary: every
// github.com clone over HTTPS reaches the same three hosts. What varies is
// whether the sandbox has an attachment at all, which is exactly the question
// the join answers.
func (s *Store) DomainsForSandbox(sandbox, owner string) ([]string, error) {
	var n int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT 1
			FROM repos r
			JOIN repo_tags rt ON r.id = rt.repo_id
			JOIN sandbox_tags bt ON bt.tag = rt.tag AND bt.owner = r.owner
			WHERE bt.sandbox = ? AND r.owner = ?
			LIMIT 1
		)`, sandbox, owner).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return []string{}, nil
	}
	return append([]string{}, cloneDomains...), nil
}

// scanRepos drains a repo query into metas plus the parallel id slice attachTags
// needs. Both list queries select the same columns in the same order precisely so
// this can be one function.
func scanRepos(rows *sql.Rows) ([]Repo, []string, error) {
	var out []Repo
	var ids []string
	for rows.Next() {
		var r Repo
		if err := rows.Scan(&r.ID, &r.Owner, &r.Host, &r.Slug, &r.Ref, &r.Path, &r.Access, &r.CreatedAt); err != nil {
			return nil, nil, err
		}
		r.Tags = []string{}
		out = append(out, r)
		ids = append(ids, r.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return out, ids, nil
}

// attachTags fills each repo's Tags by a single joined query, matching the id
// order of out via ids — one query rather than N+1, and scoped to the owner so
// the join cannot pull a tag row from somebody else's attachment.
func (s *Store) attachTags(owner string, out []Repo, ids []string) ([]Repo, error) {
	if len(out) == 0 {
		return out, nil
	}
	trows, err := s.db.Query(`
		SELECT rt.repo_id, rt.tag FROM repo_tags rt
		JOIN repos r ON r.id = rt.repo_id
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

// ValidSlug reports whether slug is an "owner/name" GitHub could have issued.
func ValidSlug(slug string) bool {
	_, _, ok := SplitSlug(slug)
	return ok
}

// SplitSlug splits "owner/name" into its halves, reporting ok only when both
// pass their own grammar. The two halves are NOT the same grammar: the owner is
// a login (letters, digits, interior hyphens, 39 chars) and the name is a
// repository name (also '.' and '_', may lead with a digit or a dot, 100
// chars). Checking both against the login grammar is the mistake that rejects
// node.js; checking both against the name grammar is the one that accepts an
// owner GitHub could never have issued.
func SplitSlug(slug string) (owner, name string, ok bool) {
	o, n, found := strings.Cut(strings.TrimSpace(slug), "/")
	if !found || strings.Contains(n, "/") {
		return "", "", false
	}
	if !users.ValidGitHubOrg(o) || !validRepoName(n) {
		return "", "", false
	}
	return o, n, true
}

// validRepoName holds the parts of the repository-name grammar a regexp cannot
// state. GitHub refuses "." and ".." (they are directory entries, not names),
// refuses a name that is nothing but dots for the same reason, and refuses a
// trailing ".git" because that is the suffix a clone URL already carries — a
// repository named foo.git and the clone URL for foo are indistinguishable.
// Accepting any of them here would store a slug whose clone can only fail, and
// the ".git" case in particular is what a pasted URL leaves behind.
func validRepoName(name string) bool {
	if !repoNameRe.MatchString(name) {
		return false
	}
	if strings.Trim(name, ".") == "" {
		return false
	}
	return !strings.HasSuffix(strings.ToLower(name), ".git")
}

// NormalizeAccess folds an access word to its canonical form; "" means read,
// because read is what a checkout needs and write is a thing somebody should
// have to ask for.
func NormalizeAccess(in string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "", AccessRead:
		return AccessRead, nil
	case AccessWrite:
		return AccessWrite, nil
	default:
		return "", fmt.Errorf("%w: access %q (want %q or %q)", ErrInvalidRepo, in, AccessRead, AccessWrite)
	}
}

// normHost defaults an empty host to github.com and refuses everything else.
// See defaultHost for why a GHES hostname is refused rather than stored.
func normHost(host string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" || h == defaultHost {
		return defaultHost, nil
	}
	return "", fmt.Errorf("%w: host %q (only %s is supported)", ErrInvalidRepo, host, defaultHost)
}

// normSlug validates a slug and returns it in its canonical "owner/name" form,
// with the case the caller typed preserved — github.com renders it that way and
// the column compares NOCASE, so keeping it costs nothing and losing it makes
// every list look subtly wrong.
func normSlug(slug string) (string, error) {
	owner, name, ok := SplitSlug(slug)
	if !ok {
		return "", fmt.Errorf("%w: slug %q (want owner/name, e.g. wandb/hivemind, with no scheme and no trailing .git)", ErrInvalidRepo, slug)
	}
	return owner + "/" + name, nil
}

// normPath validates a clone destination. It must stay relative and inside the
// login user's home: the guest joins it onto $HOME and clones as that user, so
// an absolute path or a ".." segment writes somewhere nobody asked for, and a
// leading '-' becomes an option to git rather than a directory. A leading '/'
// is refused rather than trimmed — quietly turning /etc/ssh into ~/etc/ssh
// would store a path the person did not ask for and never tell them.
func normPath(path string) (string, error) {
	p := strings.TrimRight(strings.TrimSpace(path), "/")
	if p == "" {
		return "", nil
	}
	if !pathRe.MatchString(p) {
		return "", fmt.Errorf("%w: path %q (want a relative path under the home directory)", ErrInvalidRepo, path)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: path %q (no empty, \".\" or \"..\" segments)", ErrInvalidRepo, path)
		}
	}
	return p, nil
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

// newID returns a short random hex id for a repo row. math/rand/v2 is seeded by
// the runtime and needs no crypto strength — this is a row id, not a secret.
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
