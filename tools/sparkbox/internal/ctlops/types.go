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
	Unreachable bool     `json:"unreachable,omitempty"`
	Pinned      bool     `json:"pinned"`
	Ballooned   bool     `json:"ballooned,omitempty"`
	Tags        []string `json:"tags"` // never nil
	VCPUs       int64    `json:"vcpus"`
	MemMB       int64    `json:"mem_mb"`
	// Turbo says vcpus and mem_mb above are a doubled allocation borrowed for
	// this run. Without it a listing would report a turbo sandbox's size as its
	// own, and then appear to shrink it the next time the reaper pauses it.
	Turbo        bool         `json:"turbo,omitempty"`
	DiskMB       int64        `json:"disk_mb,omitempty"`
	Repos        []RepoStatus `json:"repos,omitempty"`
	RepoStatusAt *time.Time   `json:"repo_status_at,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	LastActive   time.Time    `json:"last_active"`
	URL          string       `json:"url,omitempty"`          // https://<name>.<domain>
	TerminalURL  string       `json:"terminal_url,omitempty"` // https://<name>-xterm.<domain>
}

// RepoStatus is the latest advisory git state a sandbox reported from inside
// its own filesystem. It is restated rather than exposing host.RepoStatus so
// the public API remains independent of internal topology records.
type RepoStatus struct {
	Slug     string `json:"slug"`
	Path     string `json:"path"`
	Branch   string `json:"branch,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Ahead    int64  `json:"ahead,omitempty"`
	Behind   int64  `json:"behind,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
	State    string `json:"state"`
}

type SnapshotInfo struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	FromBox   string    `json:"from_sandbox"`
	CreatedAt time.Time `json:"created_at"`
	// Node names the machine whose image directory holds the template file. A
	// snapshot is a reflink source on ONE machine's disk, so it is also the only
	// machine a fork — or a create on a tag bound to it — can land on.
	//
	// It is filled in only on a host whose sandbox store can place on a named
	// machine (see placer). On a single-machine host every record carries
	// Node="local" anyway, because host.NewManager coerces an unset node name to
	// that word and load() re-stamps every snapshot with it (manager.go:748 and
	// :789) — so printing it there would invent a fleet nobody has, and this
	// payload stays byte-identical to the one that shipped.
	Node string `json:"node,omitempty"`
	// Port is the default port the source sandbox served on, carried forward so
	// a sandbox booted from this template lands on it too. Zero — omitted — for
	// a template captured from a box on the stock port, which is most of them.
	//
	// It is reported at all because the alternative is invisible magic: a fork
	// that quietly routes to 5173 with nothing listening there looks broken in
	// a way nothing on the box explains, and this is the line that explains it.
	Port int `json:"port,omitempty"`
	// BoundTags is the tags whose sandboxes boot from this snapshot. Empty for
	// the overwhelmingly common snapshot nobody has bound, which is why it is
	// omitempty rather than never-nil like SandboxInfo.Tags: a listing that
	// printed an empty column for every row would bury the one row it matters on.
	BoundTags []string `json:"bound_tags,omitempty"`
}

// TemplateBinding is one tag-to-snapshot binding as a caller sees it. The owner
// is deliberately absent: a binding is only ever read back under its own owner,
// so echoing the handle would be the one field on this surface that could
// differ from the caller's own.
type TemplateBinding struct {
	Tag       string    `json:"tag"`
	Snapshot  string    `json:"snapshot"`
	CreatedAt time.Time `json:"created_at"`
}

// TemplateBindResult is what a bind reports back.
//
// Previous is what makes a re-point not silent. Without it the person typing
// `bind` reads the same success line whether they created a binding or quietly
// changed what every future sandbox on that tag boots from, and there is no way
// for a transport to warn about something it was never told.
type TemplateBindResult struct {
	Binding  TemplateBinding `json:"binding"`
	Previous string          `json:"previous,omitempty"`
}

// SelfSnapshotPlan is everything a sandbox is told before it agrees to be
// paused and captured. It is produced by a PURE READ (PlanSelfSnapshot), which
// is what makes "the warnings are printed before anything moves" a structural
// property rather than a matter of ordering discipline: the request that
// produces this cannot pause anything, and the capture is a second request the
// user authorizes after reading it.
//
// Every hint that names a hostname is filled in HERE rather than by the guest.
// A sandbox does not know its own domain — nothing in the VM is told the
// gateway's name — so a shell that composed `ssh ctl@…` would have to guess.
type SelfSnapshotPlan struct {
	Sandbox string   `json:"sandbox"`
	Tags    []string `json:"tags"` // every tag this sandbox carries, `default` included
	Tag     string   `json:"tag"`  // the one being captured into
	// Snapshot is the name the capture will take. The commit sends it back
	// rather than re-deriving it, because a derived name that drifted into the
	// next minute would capture under a name nobody was shown.
	Snapshot string `json:"snapshot"`
	// Node is the machine this capture will land on; "" on a host with one
	// machine, for the reason SnapshotInfo.Node states.
	Node string `json:"node,omitempty"`
	// Bound is the snapshot this tag boots from TODAY, empty when the tag has
	// no binding. BoundFrom and BoundAt describe it well enough that somebody
	// can recognise it without going to look, and BoundNode is set only when it
	// differs from Node — the case where re-pointing also moves where the
	// owner's future sandboxes are placed.
	Bound     string    `json:"bound,omitempty"`
	BoundFrom string    `json:"bound_from,omitempty"`
	BoundAt   time.Time `json:"bound_at,omitzero"`
	BoundNode string    `json:"bound_node,omitempty"`
	// Carriers is every sandbox of this owner's that carries the tag, WITH its
	// state, because the warning it feeds is about running and paused boxes
	// alike: neither is re-based by a re-point.
	Carriers []TaggedSandbox `json:"carriers,omitempty"`
	// Busy names a rootfs operation already running on this sandbox. A warning
	// and never a gate — see host.Manager.DiskOperation.
	Busy   string `json:"busy,omitempty"`
	Turbo  bool   `json:"turbo,omitempty"`
	DiskMB int64  `json:"disk_mb,omitempty"`
	// CtlHint is `ssh ctl@<domain>` and SSHHint is `ssh <sandbox>.<domain>`,
	// both host-authored for the reason above.
	CtlHint string `json:"ctl_hint"`
	SSHHint string `json:"ssh_hint"`
	// Token digests the facts this plan reported. The commit re-plans and
	// compares, so a binding or a carrier set that moved while the user was
	// deciding is refused instead of acted on — the warnings they agreed to
	// were about a world that no longer exists.
	Token string `json:"token"`
}

// TaggedSandbox is one of the owner's sandboxes carrying the tag being
// captured into. Self marks the box the request came from, which is the one the
// reader is sitting in and the one whose session is about to end.
type TaggedSandbox struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Self  bool   `json:"self,omitempty"`
}

// SnapshotToTagArgs is one capture-and-re-point gesture. Tag is optional: empty
// makes this exactly CreateSnapshot, which is what keeps `snapshot create` with
// no --tag byte-identical to what it has always been.
type SnapshotToTagArgs struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name"`
	Tag     string `json:"tag,omitempty"`
}

// SnapshotToTagResult reports both halves. Bound is false — with no error —
// only when no tag was asked for; a tag that was asked for and did not bind is
// an error carrying this same populated Snapshot.
type SnapshotToTagResult struct {
	Snapshot SnapshotInfo `json:"snapshot"`
	Tag      string       `json:"tag,omitempty"`
	Bound    bool         `json:"bound"`
	// Previous is what the tag used to boot from, on the same terms as
	// TemplateBindResult.Previous: a re-point looks exactly like a first bind
	// and is not.
	Previous string `json:"previous,omitempty"`
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

// RouteInfo is one addressable PORT of a sandbox — not one route row. A route
// contributes its own port (Default, reached at the portless URL) plus an entry
// for every extra port configured under the same hostname, which the edge
// serves as https://<subdomain>.<domain>:<port> with no row of its own.
type RouteInfo struct {
	Subdomain  string `json:"subdomain"`
	Sandbox    string `json:"sandbox"`
	Port       int    `json:"port"`
	Visibility string `json:"visibility"`
	URL        string `json:"url,omitempty"`
	// Default marks the route's own port: the one the portless hostname
	// forwards to, and the one a bare `share <name> public` opens.
	Default bool `json:"default,omitempty"`
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
	// GuestSubnet and GRPCAddr are operator-approved topology, not values a
	// node is allowed to rewrite when it reports status. They are shown here
	// because this operator-only listing is the place to audit what the
	// gateway will route and dial.
	GuestSubnet string `json:"guest_subnet,omitempty"`
	GRPCAddr    string `json:"grpc_addr,omitempty"`
	// Certificate metadata describes the one current node-control credential.
	// A rotated certificate replaces these fields; a revoked one remains
	// visible so an operator can tell "never issued" from "withdrawn".
	CertSerial    string     `json:"cert_serial,omitempty"`
	CertExpiresAt *time.Time `json:"cert_expires_at,omitempty"`
	CertRevokedAt *time.Time `json:"cert_revoked_at,omitempty"`
}

type Whoami struct {
	Handle           string     `json:"handle"`
	Status           string     `json:"status"`
	Operator         bool       `json:"operator"`
	Email            string     `json:"email,omitempty"`
	GitHubLogin      string     `json:"github_login,omitempty"`
	GitHubVerifiedAt *time.Time `json:"github_verified_at,omitempty"`
	// GitHubVia is HOW the link was proved — "github-keys", "device-flow" or
	// "assertion". It is shown rather than kept internal because the three are
	// not interchangeable: only the first two may adopt keys, and somebody
	// wondering why `keys import-github` refused should be able to read the
	// answer off `whoami`.
	GitHubVia string `json:"github_via,omitempty"`
	Subject   string `json:"subject"`          // oidc.SubjectFor(handle)
	KeyFP     string `json:"key_fp,omitempty"` // the key on THIS session; "" over HTTP
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
	VCPUs int64 // 0 takes the manager default (4)
	MemMB int64 // 0 takes the manager default (12288)
	// Refs are the per-instance branch overrides: which branch THIS sandbox's
	// checkouts start on, whatever the attachment says. Resolved and refused
	// before any row is written, and written beside the tags for the same
	// reason they are — the boot that reads the manifest happens once the
	// sandbox exists, and both have to be true by then. See reporef.go.
	Refs []RepoRef
}

type ForkArgs struct {
	Snapshot string
	Name     string
	Tags     []string  // same ordering constraint as CreateArgs
	Refs     []RepoRef // same as CreateArgs, and the case it exists for
}

type ScheduleArgs struct {
	Sandbox string
	Spec    string
	Command string
}

// NodeApprovalConfig is the trusted topology an operator supplies while
// approving a node. GuestSubnet is required by configured fleet rosters;
// GRPCAddr is optional while a mixed SSH/gRPC fleet is migrating.
//
// GatewayGuestSubnet is deliberately absent: it is process configuration,
// never an operator-controlled command argument. Ops adds it immediately
// before the atomic roster write.
type NodeApprovalConfig struct {
	GuestSubnet string
	GRPCAddr    string
}
