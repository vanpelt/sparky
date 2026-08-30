package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// ToolCache is the agent-CLI cache THIS machine holds, served to the guests on
// its own taps. deploy/refresh-agent-tools.sh fills the directory and publishes
// manifest.json describing what it verified; this is the read side of that.
//
// It is a SIBLING of RepoAccess for the reason repos.go:34 gives, and it is a
// capability with its own Options field so it gets to be absent: a host built
// without a tool cache answers 501 on these two routes and keeps minting id
// tokens.
//
// IT MUST NEVER GROW A RELAY IMPLEMENTATION. Identity relays to the gateway
// because a fleet has exactly one signing key; RepoAccess relays because a
// fleet has exactly one attachment ledger. A tool cache is neither: every host
// runs its own refresher on its own timer, so a node already holds the same
// artifacts the gateway does. Relaying would drag ~150MB per guest across the
// fleet link to deliver bytes that were already sitting on the node's disk,
// which is the whole argument for a pull in the first place — see the header of
// deploy/refresh-agent-tools.sh.
//
// Neither method takes a context or a sandbox, unlike every other capability
// here. Both are local file operations and the answer is identical for every
// guest on the machine: which sandbox is asking decides only whether it is
// allowed to ask (caller) and how often (allowRepoCall), never what it gets.
type ToolCache interface {
	Manifest() (ToolManifest, error)
	Open(name string) (io.ReadSeekCloser, ToolFile, error)
}

// ToolManifest is the document a guest's `sparkbox update-tools` reads: what
// this host has verified, and everything needed to install it.
//
// Tools is an ARRAY of objects each carrying its own name, not a map keyed by
// name, and that is forced by the reader. The guest payload holds to curl/awk
// with no jq and no python (install-guest-identity.sh explains why), and its
// flattener splits the document on `{` — which puts a map's key on the previous
// line, where nothing can recover it.
type ToolManifest struct {
	Arch        string      `json:"arch"`
	Rev         string      `json:"rev"`
	GeneratedAt string      `json:"generated_at"`
	Tools       []ToolEntry `json:"tools"`
}

// ToolEntry is one artifact and the shape it installs in.
//
// Size is `,string` — quoted on the wire — for the same guest parser: its
// pair-walk only recognises `"key": "value"`, so an unquoted number reads back
// as empty and the guest loses the length check that tells a truncated download
// from a tampered one. Nothing here is omitempty either: the awk reads absent
// and empty the same way, and a stable set of keys is easier to eyeball in a
// boot log than a document whose shape changes per tool.
//
// The layout fields (Kind/Bin/Dir/Exec/Link/KeepOnly/Drop) are DATA rather than
// something the guest derives, because two of the five tools are bundles: pi
// and agent-browser resolve their own runtime assets relative to the real
// executable, so a guest that copied the executable to /usr/local/bin would get
// a CLI whose every `skills` subcommand fails. refresh-agent-tools.sh writes
// these beside the patch loop that performs the same install, so the two cannot
// drift.
type ToolEntry struct {
	Name     string `json:"name"`
	Key      string `json:"key"`
	Version  string `json:"version"`
	File     string `json:"file"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size,string"`
	Kind     string `json:"kind"`
	Bin      string `json:"bin"`
	Dir      string `json:"dir"`
	Exec     string `json:"exec"`
	Link     string `json:"link"`
	KeepOnly string `json:"keep_only"`
	Drop     string `json:"drop"`
}

// ToolFile is what the handler needs about an opened artifact that the reader
// itself will not tell it: how long it is and when it last changed, both for
// http.ServeContent's conditional and Range handling.
type ToolFile struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// ErrNoTools is a host that serves no cache: no --tools-dir, or a directory the
// refresher has never published a manifest into. Answered 501 like
// ErrNotEnabled, and for the same reason — it is a statement about the
// deployment, and no amount of retrying changes it.
var ErrNoTools = errors.New("this host does not serve an agent tool cache")

// ErrNoSuchTool is a request for a name this host's manifest does not carry.
// Answered 404. It is deliberately also the answer for an artifact that
// vanished from underneath the manifest between the listing and the read: the
// guest's repair is the same either way, which is to come back after the next
// refresher run.
var ErrNoSuchTool = errors.New("this host publishes no such tool")

// toolNameRe is the shape a tool name may have. Names reach us out of the URL
// and out of the manifest, and both are checked against it: the URL because
// PathValue hands back a percent-decoded segment that can still contain a
// separator, and the manifest because a name is what a guest asks for by, so a
// name it cannot express is a name that could never be served.
var toolNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// toolBodyBudget is how long one artifact may take to reach one guest.
//
// The metadata server gives every handler a 10s WriteTimeout (server.go:440),
// which is right for a JWT and a JSON document and truncates agent-browser's
// ~92MB tarball long before it lands — over a tap whose egress sluice may be
// metering the guest. A truncated body is not reported as a timeout anywhere:
// the guest sees short bytes, and without its own length check it reads a
// sha256 mismatch, which sends whoever is holding it looking for a tampered
// artifact. So the deadline is raised PER HANDLER and never on the server:
// /token and /github/credential want the short budget they have (githubBudget
// in repos.go says why), and a server-wide raise would quietly take it away.
const toolBodyBudget = 10 * time.Minute

// LocalTools serves the directory refresh-agent-tools.sh writes. It is the only
// implementation of ToolCache there is, or should ever be — see the interface.
//
// Dir is the same path the refresher unit gets as TOOLS_DIR: hostsetup renders
// both from Config.toolsDir(), and the CKS entrypoint hands both $data_dir/tools,
// so the host that fills the cache and the server that serves it cannot name
// different directories.
type LocalTools struct {
	Dir string

	// The parse is memoised on manifest.json's (size, mtime) because a fleet
	// running `update-tools` is N guests asking within seconds of each other,
	// and re-reading and re-validating the document per request buys nothing —
	// it changes only when the refresher renames a new one into place. The
	// artifact stats below are NOT memoised: five stats are cheap, and they are
	// what keeps a manifest from naming a file the prune has since deleted.
	mu         sync.Mutex
	cached     ToolManifest
	cachedOK   bool
	cachedSize int64
	cachedMod  time.Time
}

// Manifest reads, validates and filters the published document.
//
// The two halves are deliberately different in severity. A MALFORMED entry
// rejects the WHOLE file: it means the producer and this reader disagree about
// the contract, and serving the entries that happened to parse would install a
// half-understood payload. A MISSING or SHORT artifact only drops that entry:
// that is an ordinary transient — a refresher part-way through a run, or a
// prune that raced a read — and the other four tools are still installable.
func (l *LocalTools) Manifest() (ToolManifest, error) {
	if strings.TrimSpace(l.Dir) == "" {
		return ToolManifest{}, ErrNoTools
	}
	path := filepath.Join(l.Dir, "manifest.json")
	st, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// The refresher has never completed a run here. This is 501 and NEVER
		// an empty tool list: an empty list reads to the guest as "you are
		// already current", which is the exact lie the refresher rewrote itself
		// to stop telling (see its "WHICH TEMPLATES ARE CURRENT" header).
		return ToolManifest{}, fmt.Errorf("%w: no manifest has been published in %s", ErrNoTools, l.Dir)
	case err != nil:
		return ToolManifest{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.cachedOK || l.cachedSize != st.Size() || !l.cachedMod.Equal(st.ModTime()) {
		parsed, err := parseToolManifest(path)
		if err != nil {
			return ToolManifest{}, err
		}
		l.cached, l.cachedOK = parsed, true
		l.cachedSize, l.cachedMod = st.Size(), st.ModTime()
	}

	out := l.cached
	out.Tools = make([]ToolEntry, 0, len(l.cached.Tools))
	for _, e := range l.cached.Tools {
		fi, err := os.Stat(filepath.Join(l.Dir, e.File))
		if err != nil || fi.Size() != e.Size {
			continue
		}
		out.Tools = append(out.Tools, e)
	}
	if len(out.Tools) == 0 {
		// A manifest naming nothing this host actually holds. Not 501 — the
		// cache is configured and the document exists — and not an empty 200,
		// for the reason above.
		return ToolManifest{}, fmt.Errorf("the published manifest names %d tools and none of their artifacts are in %s", len(l.cached.Tools), l.Dir)
	}
	return out, nil
}

// Open returns one artifact's bytes.
//
// PATH TRAVERSAL: the requested name is LOOKED UP in the manifest's own entries
// and the file is opened by the basename that entry stored — the request never
// reaches filepath.Join. A name the manifest does not carry is a 404 before any
// filesystem call happens, so `..%2f..%2fetc%2fpasswd` fails as an unknown tool
// rather than as anything more interesting.
func (l *LocalTools) Open(name string) (io.ReadSeekCloser, ToolFile, error) {
	m, err := l.Manifest()
	if err != nil {
		return nil, ToolFile{}, err
	}
	for _, e := range m.Tools {
		if e.Name != name {
			continue
		}
		path := filepath.Join(l.Dir, e.File)
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ToolFile{}, fmt.Errorf("%w: %s", ErrNoSuchTool, name)
			}
			return nil, ToolFile{}, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close() //nolint:errcheck
			return nil, ToolFile{}, err
		}
		if fi.Size() != e.Size {
			// Lost a race with the refresher's rename. Refuse rather than serve
			// bytes the manifest's digest does not describe: the guest checks
			// both and would refuse anyway, and it should not have to spend
			// 92MB of a metered tap finding out.
			f.Close() //nolint:errcheck
			return nil, ToolFile{}, fmt.Errorf("%w: %s changed underneath the manifest", ErrNoSuchTool, name)
		}
		return f, ToolFile{Name: e.File, Size: fi.Size(), ModTime: fi.ModTime()}, nil
	}
	return nil, ToolFile{}, fmt.Errorf("%w: %s", ErrNoSuchTool, name)
}

// parseToolManifest reads and validates the document, rejecting the whole file
// on the first entry that does not satisfy the contract. Every rule here is one
// the guest installer would otherwise have to enforce with a shell `case` after
// it had already spent the download.
func parseToolManifest(path string) (ToolManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ToolManifest{}, err
	}
	var m ToolManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return ToolManifest{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(m.Tools) == 0 {
		return ToolManifest{}, fmt.Errorf("%s publishes no tools", path)
	}
	seen := make(map[string]bool, len(m.Tools))
	for _, e := range m.Tools {
		switch {
		case !toolNameRe.MatchString(e.Name):
			return ToolManifest{}, fmt.Errorf("%s: %q is not a tool name a guest could ask for", path, e.Name)
		case e.Name == "manifest":
			// Handler() registers the literal /tools/manifest, which wins over
			// /tools/{name} for that exact segment, so such a tool would be
			// unreachable. Said here rather than left to be discovered.
			return ToolManifest{}, fmt.Errorf("%s: a tool may not be named %q — that path is the manifest itself", path, e.Name)
		case seen[e.Name]:
			return ToolManifest{}, fmt.Errorf("%s: two tools are named %q", path, e.Name)
		case !bareFilename(e.File):
			return ToolManifest{}, fmt.Errorf("%s: %s names artifact %q, which is not a bare filename", path, e.Name, e.File)
		case !isHex64(e.SHA256):
			return ToolManifest{}, fmt.Errorf("%s: %s has no sha256 digest", path, e.Name)
		case e.Size <= 0:
			return ToolManifest{}, fmt.Errorf("%s: %s has size %d", path, e.Name, e.Size)
		case e.Kind != "binary" && e.Kind != "bundle":
			return ToolManifest{}, fmt.Errorf("%s: %s has unknown kind %q", path, e.Name, e.Kind)
		case !strings.HasPrefix(e.Bin, "/"):
			return ToolManifest{}, fmt.Errorf("%s: %s installs to %q, which is not an absolute path", path, e.Name, e.Bin)
		case e.Kind == "bundle" && (!strings.HasPrefix(e.Dir, "/") || e.Exec == "" || e.Link == ""):
			// A bundle without all three is the pi/agent-browser breakage the
			// layout fields exist to prevent: no directory to unpack into, no
			// executable inside it, or no relative symlink from PATH back to
			// the real binary that resolves its own skill data.
			return ToolManifest{}, fmt.Errorf("%s: bundle %s does not name a directory, an executable and a link", path, e.Name)
		}
		seen[e.Name] = true
	}
	return m, nil
}

// bareFilename reports whether f names a file inside the cache directory and
// nothing else. Belt and braces: Open never joins a request, only this value.
func bareFilename(f string) bool {
	if f == "" || f == "." || f == ".." || strings.ContainsAny(f, `/\`) {
		return false
	}
	return filepath.Base(f) == f
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// toolManifest answers GET /tools/manifest: what this host has verified and how
// to install it. Nothing here is secret — it is versions, paths and digests of
// public artifacts — but it does name every tool the box will serve, so it goes
// through the same caller gate as everything else on this port.
func (s *Server) toolManifest(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	if s.tools == nil {
		http.Error(w, "sparkbox: "+ErrNoTools.Error(), http.StatusNotImplemented)
		return
	}
	if !s.allowRepoCall(box.Name + " tools") {
		http.Error(w, "sparkbox: too many tool requests", http.StatusTooManyRequests)
		return
	}
	m, err := s.tools.Manifest()
	if err != nil {
		s.failTools(w, "manifest", box, err)
		return
	}
	s.writeJSON(w, m)
}

// toolFile answers GET /tools/{name}: one artifact, whole or by range.
func (s *Server) toolFile(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	if s.tools == nil {
		http.Error(w, "sparkbox: "+ErrNoTools.Error(), http.StatusNotImplemented)
		return
	}
	name := r.PathValue("name")
	if !toolNameRe.MatchString(name) {
		// Refused before the cache is consulted, and answered as an unknown
		// tool rather than a distinct status: a guest asking for a separator is
		// asking for something this host does not publish, and telling it apart
		// from an ordinary miss only helps somebody probing.
		http.Error(w, "sparkbox: "+ErrNoSuchTool.Error(), http.StatusNotFound)
		return
	}
	// Taken before the body, like the mint's and the credential's: past here
	// this handler occupies a reader and up to toolBodyBudget of the tap.
	if !s.allowRepoCall(box.Name + " tools") {
		http.Error(w, "sparkbox: too many tool requests", http.StatusTooManyRequests)
		return
	}
	f, info, err := s.tools.Open(name)
	if err != nil {
		s.failTools(w, "download", box, err)
		return
	}
	defer f.Close() //nolint:errcheck

	// See toolBodyBudget. The error is deliberately ignored: the only writer
	// that refuses a deadline here is httptest.ResponseRecorder, which returns
	// http.ErrNotSupported, and treating that as a failure would 500 every unit
	// test in this package while changing nothing about a real connection.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(toolBodyBudget))

	// Set explicitly so ServeContent neither sniffs the body nor guesses from a
	// name: these are binaries and tarballs, and the guest checks a digest, not
	// a content type. no-store matches every other answer on this port.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	s.log.Info("serving agent tool", "sandbox", box.Name, "owner", box.Owner,
		"tool", name, "file", info.Name, "bytes", info.Size)
	// ServeContent, not io.Copy: it honours Range, so a guest whose 92MB
	// download died at 80MB resumes instead of pulling the whole tarball again.
	// The empty name keeps it from re-deriving a content type from an extension.
	http.ServeContent(w, r, "", info.ModTime, f)
}

// failTools maps a ToolCache error onto a status. A sibling of failRepos for
// the reason that one is a sibling of fail: the guest reads these sentences in
// a boot log, and "could not establish this sandbox's identity" would send
// whoever is holding a failed `update-tools` to the wrong subsystem.
func (s *Server) failTools(w http.ResponseWriter, what string, box *host.Sandbox, err error) {
	switch {
	case errors.Is(err, ErrNoSuchTool):
		http.Error(w, "sparkbox: "+err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrNoTools):
		http.Error(w, "sparkbox: "+err.Error(), http.StatusNotImplemented)
	default:
		s.log.Error("tool cache failed", "op", what, "sandbox", box.Name, "err", err)
		http.Error(w, "sparkbox: could not answer this sandbox's tool request", http.StatusInternalServerError)
	}
}
