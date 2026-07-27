package ctlops

import "time"

// ---------------------------------------------------------------------------
// Result types
//
// Deliberately not *host.Sandbox: SSHAddr, HostIP, GuestV6 and KeyFP are
// internal topology, and api.<domain> is a documented public contract that must
// not move when an internal struct does.
// ---------------------------------------------------------------------------

type SandboxInfo struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	State string `json:"state"`
	// Node names the machine whose driver runs this VM. It is the first
	// internal-topology field info() deliberately does NOT drop: a user needs to
	// know which machine their sandbox is on to reason about its arch, its
	// accelerators and its outages, and unlike a guest address a node name is
	// not dialable. omitempty keeps a single-box payload byte-identical.
	Node string `json:"node,omitempty"`
	// Unreachable reports that the node holding this sandbox is not answering
	// the control plane. The sandbox is very likely still running.
	Unreachable bool      `json:"unreachable,omitempty"`
	Pinned      bool      `json:"pinned"`
	Ballooned   bool      `json:"ballooned,omitempty"`
	Tags        []string  `json:"tags"` // never nil
	VCPUs       int64     `json:"vcpus"`
	MemMB       int64     `json:"mem_mb"`
	DiskMB      int64     `json:"disk_mb,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	LastActive  time.Time `json:"last_active"`
	URL         string    `json:"url,omitempty"`          // https://<name>.<domain>
	TerminalURL string    `json:"terminal_url,omitempty"` // https://<name>-xterm.<domain>
}

type SnapshotInfo struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	FromBox   string    `json:"from_sandbox"`
	CreatedAt time.Time `json:"created_at"`
}

type ScheduleInfo struct {
	ID        string     `json:"id"`
	Sandbox   string     `json:"sandbox"`
	Spec      string     `json:"spec"`
	Command   string     `json:"command"`
	NextRun   *time.Time `json:"next_run,omitempty"` // nil when the spec no longer parses
	LastRun   *time.Time `json:"last_run,omitempty"`
	LastExit  int        `json:"last_exit"`
	LastError string     `json:"last_error,omitempty"`
}

type RouteInfo struct {
	Subdomain  string `json:"subdomain"`
	Sandbox    string `json:"sandbox"`
	Port       int    `json:"port"`
	Visibility string `json:"visibility"`
	URL        string `json:"url,omitempty"`
}

// NodeInfo is one machine in the fleet as an operator sees it: the roster row
// joined to the live link.
//
// The node's public key itself is deliberately absent. FP is what an operator
// compares out of band before approving, and it is the only part of the
// credential anybody has a use for — the key is the roster's alone.
type NodeInfo struct {
	Name    string `json:"name"`
	Status  string `json:"status"`          // pending | approved | disabled
	Online  bool   `json:"online"`          // a link is attached right now
	Local   bool   `json:"local,omitempty"` // the gateway's own machine
	FP      string `json:"fingerprint"`     // SHA256:… of the node's key
	Arch    string `json:"arch,omitempty"`  // node-authored, display only
	Release string `json:"release,omitempty"`
	// Sandboxes is how many placements this machine still holds. It is the one
	// number that decides whether removing the node is safe, so it is part of
	// the listing rather than something a client has to derive by cross-
	// referencing every sandbox.
	Sandboxes int `json:"sandboxes"`
	// Egress is whether this machine runs an egress gateway. A fleet can be
	// half-metered — a gateway with sluice and a node without is a working
	// deployment — and a bandwidth panel of zeroes is the only other place that
	// shows, where it reads as an idle VM rather than an unmeasured one.
	Egress     bool       `json:"egress,omitempty"`
	ApprovedBy string     `json:"approved_by,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
}

type Whoami struct {
	Handle           string     `json:"handle"`
	Status           string     `json:"status"`
	Operator         bool       `json:"operator"`
	Email            string     `json:"email,omitempty"`
	GitHubLogin      string     `json:"github_login,omitempty"`
	GitHubVerifiedAt *time.Time `json:"github_verified_at,omitempty"`
	Subject          string     `json:"subject"`          // oidc.SubjectFor(handle)
	KeyFP            string     `json:"key_fp,omitempty"` // the key on THIS session; "" over HTTP
}

type KeyInfo struct {
	FP      string    `json:"fingerprint"`
	Label   string    `json:"label,omitempty"`
	Via     string    `json:"via"`
	AddedAt time.Time `json:"added_at"`
	Current bool      `json:"current,omitempty"` // matches Caller.KeyFP
}

type PasskeyInfo struct {
	ID         string     `json:"id"`
	Label      string     `json:"label,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// ImportResult reports skipped fingerprints rather than printing them: ctl
// writes one stderr note each, REST returns the list.
type ImportResult struct {
	Login    string   `json:"login"`
	Imported int      `json:"imported"` // genuinely new keys; AddKey is idempotent
	Listed   int      `json:"listed"`
	Skipped  []string `json:"skipped"` // already linked elsewhere; never nil
}

type TokenResult struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type InviteResult struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type VisibilityResult struct {
	Routes  []RouteInfo `json:"routes"` // never nil
	Changed int         `json:"changed"`
}

// Endpoint is where to dial a running sandbox's sshd. It is the one result type
// carrying internal topology, exists solely for the terminal bridge, and has no
// JSON tags precisely so it can never be serialized onto the edge by accident.
type Endpoint struct {
	Name    string
	SSHAddr string
	SSHUser string
}

// ---------------------------------------------------------------------------
// Argument types
// ---------------------------------------------------------------------------

// CreateArgs mirrors the new@ door with its ambiguity removed. The door has to
// read bare words as tags because ssh(1) eats leading-dash arguments, but a JSON
// body has named fields — so there is deliberately no Command field here, which
// is the bug execsCommand exists to prevent.
type CreateArgs struct {
	Name string   // "" generates an adjective-noun name
	Tags []string // normalized and stamped BEFORE Create, rolled back on failure
	// Node names the machine to build on. "" leaves the choice to the gateway,
	// which today means its own machine — so a single-box deployment, and every
	// caller written before there was a second machine, is unaffected.
	//
	// A node name is not a tenant's secret: an operator publishes them so people
	// can place work, and `node ls` renders them. So an unknown one is answered
	// plainly rather than masked, which is the one place this package's
	// not-found is not also an ownership answer.
	Node  string
	VCPUs int64 // 0 takes the manager default (2)
	MemMB int64 // 0 takes the manager default (8192)
}

type ForkArgs struct {
	Snapshot string
	Name     string
	Tags     []string // same ordering constraint as CreateArgs
}

type ScheduleArgs struct {
	Sandbox string
	Spec    string
	Command string
}
