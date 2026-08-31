// Package ctlops is the transport-agnostic core of the sparkbox control plane:
// one method per `ssh ctl@<gateway>` command, taking the caller's handle and
// typed arguments and returning typed results and one typed error.
//
// It exists because the same operation is now reachable three ways — the SSH
// ctl channel, the REST API at api.<domain>, and the browser terminal's owner
// gate — and the ownership check, the timeout budget, and the
// tags-before-create ordering are each things a caller can silently forget.
// They live here so that no caller can. internal/sshgw keeps argument parsing
// and text formatting; internal/restapi keeps JSON and status codes; neither
// keeps policy.
//
// ctlops authenticates nothing. The transport has already proved who is asking
// (an SSH public key, or a verified edge session); ctlops only authorizes.
package ctlops

import (
	"context"
	"log/slog"
	"sync"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/templates"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// ---------------------------------------------------------------------------
// Narrow dependency interfaces
//
// Each is the slice of a real store that ctlops actually drives, stated as an
// interface so the package's own tests run against in-memory fakes with no
// sqlite, no temp dir, and no VM driver. *host.Manager, *users.Store,
// *secrets.Store, *schedule.Store, *routes.Store and *edgeauth.Signer satisfy
// them structurally — there are no adapters to write and nothing to keep in
// sync. Note that every method here is owner-agnostic: ctlops does the
// ownership check before it calls any of them, which is the whole point.
// ---------------------------------------------------------------------------

// Sandboxes is the VM-lifecycle slice of host.Manager.
type Sandboxes interface {
	Get(name string) (*host.Sandbox, bool)
	ListByOwner(owner string) []*host.Sandbox
	Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error)
	EnsureReady(ctx context.Context, name string) (*host.Sandbox, error)
	Pause(ctx context.Context, name string) error
	Archive(ctx context.Context, name string) error
	Resize(ctx context.Context, name string, sizeMB int64) error
	Reboot(ctx context.Context, name string) error
	Rename(ctx context.Context, oldName, newName, owner string) error
	Destroy(ctx context.Context, name string) error
	SetPinned(name string, pinned bool) error
	ResyncEnv(ctx context.Context, name string)
	AwaitEnv(ctx context.Context, name string) error
	MarkActive(name string)
	ArchivingEnabled() bool
}

// Checkpoints is the optional, currently node-local durable checkpoint slice.
// Enabled is target-specific: a gateway may have a checkpoint store while the
// requested sandbox lives on a remote node that does not expose this v1.
type Checkpoints interface {
	Enabled(name string) bool
	Checkpoint(ctx context.Context, name string) error
	RestoreCheckpoint(ctx context.Context, name string) error
}

// Templates is the snapshot/fork slice, separate because a host whose driver
// cannot archive has snapshots disabled while ordinary sandboxes still work.
type Templates interface {
	Snapshots(owner string) []*host.Snapshot
	Snapshot(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapName, owner string) error
	Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error)
	Snapshotter() bool
}

// Accounts is the identity slice of users.Store.
type Accounts interface {
	Get(handle string) (users.User, error)
	Keys(handle string) ([]users.Key, error)
	AddKey(handle string, key xssh.PublicKey, label, via string) error
	RemoveKey(handle, fp string) error
	LinkGitHub(handle, login, via string, id int64) error
	SetEmail(handle, email string) error
	Passkeys(handle string) ([]users.Passkey, error)
	RemovePasskey(handle, idPrefix string) error
	NewInvite(createdBy string) (string, error)
	InviteCount(handle string) (int, error)
	// Create and List are the operator-provisioning slice (user.go). They are
	// on the same interface rather than their own because they are the same
	// store and the same ownership rule — ctlops decides who may call them.
	Create(handle string, key xssh.PublicKey, label, via, invitedBy string) error
	List() ([]users.User, error)
	// CreateKeyless is federated.go's floor: an account for somebody who
	// publishes no ssh key and arrived through the browser. Kept beside Create
	// rather than on its own interface for the same reason — one store, and
	// ctlops decides who may reach it.
	CreateKeyless(handle, invitedBy string) error
}

// Tagger is the tag half of secrets.Store. Deliberately identical to the
// existing sshgw.SandboxTagger so *secrets.Store keeps satisfying both. Neither
// method checks ownership — that is exactly why nothing outside ctlops may hold
// a reference to one.
type Tagger interface {
	TagsFor(sandbox string) ([]string, error)
	SetTags(sandbox, owner string, tags []string) error
}

// Secrets is the value half of secrets.Store — the half the user console had
// to itself until the ssh channel grew the same verbs.
//
// There is deliberately no read-a-value method, because the store deliberately
// has none: values are write-only from every API's point of view and are only
// ever decrypted on the way into a guest. SandboxesForSecret is what makes a
// change take effect without waiting for a resume — it names the boxes whose
// environment just went stale.
type Secrets interface {
	PutSecret(owner, envName, value string, tags []string) error
	DeleteSecret(owner, envName string) error
	ListSecrets(owner string) ([]secrets.SecretMeta, error)
	SandboxesForSecret(owner, envName string) ([]string, error)
}

// Repos is the repo-attachment store — the third reader of the shared
// sandbox_tags table, after secrets and netrules. A nil one makes every repo
// operation answer KindDisabled, which is what a host with no repo store is.
//
// It is deliberately narrower than *repos.Store. ReposForSandbox is missing
// because nothing on this surface resolves a guest's manifest: that is
// internal/metadata's job, over the guest's own tap, and a control-plane verb
// that could ask "what does that sandbox get" would be a second answer to a
// question with one authority. Every method here is owner-agnostic for the same
// reason Secrets' are — ctlops authorizes the caller first, and the handle is a
// query term rather than a check a handler could forget.
type Repos interface {
	PutRepo(owner string, r repos.Repo, tags []string) error
	DeleteRepo(owner, host, slug string) error
	ListRepos(owner string) ([]repos.Repo, error)
	SandboxesForRepo(owner, host, slug string) ([]string, error)
	// SetSandboxRefs is the only method here keyed by a sandbox rather than by
	// an owner and a tag, because `--ref` is the only thing on this surface
	// that is about one instance. See reporef.go.
	SetSandboxRefs(owner, sandbox string, refs []repos.SandboxRef) error
}

// TemplateBindings is the tag-to-base-image store — the fourth reader of the
// shared sandbox_tags namespace, after secrets, netrules and repos. A nil one
// makes bind and unbind answer KindDisabled and leaves every create booting from
// Config.DefaultImage, which is exactly what shipped before bindings existed.
//
// The package is named `templates` and this package already has a `Templates`
// interface (the snapshot/fork slice above). The two coexist without ambiguity
// because Go resolves the identifier by scope: `templates.Binding` is the
// package, `Templates` is the type. They are not the same object — one is the
// driver's snapshot capability, the other is a sqlite table of pointers into it.
//
// templates.TemplatesForSandbox is deliberately absent, for the reason
// Repos.ReposForSandbox is: nothing on this surface should be able to ask "what
// would that box boot from", because the create path answers that question from
// the tags it just computed and a second answer with different inputs is a
// second authority.
type TemplateBindings interface {
	Bind(owner, tag, snapshot string) (templates.Binding, string, error)
	Unbind(owner, tag string) (templates.Binding, error)
	BindingsForOwner(owner string) ([]templates.Binding, error)
	BindingsForTags(owner string, tags []string) ([]templates.Binding, error)
}

// GitHubApp is the installation half of the GitHub App: which installation
// covers a repository, and whether the caller's linked GitHub identity may use
// it. *ghapp.App satisfies it.
//
// Minting a token is deliberately NOT on this interface. Installation tokens
// are handed out in internal/metadata, to a guest that identified itself on a
// channel it cannot forge, and every one of them is a live credential. Putting
// MintToken here would mean this package — whose whole job is to render results
// and write audit lines — held one, and the discipline that keeps a token out
// of a log is much easier to hold when there is no token in the room.
//
// A nil one is a host with no App configured: `repo check` and `github install`
// answer KindDisabled and `repo add` still records the attachment, because a
// public repository clones with no credential at all.
type GitHubApp interface {
	InstallationFor(ctx context.Context, owner, name string) (ghapp.Installation, error)
	Authorize(ctx context.Context, inst ghapp.Installation, githubID int64, githubLogin string) error
	InstallURL() string
}

// Schedules is the platform-cron store. A nil one makes every schedule
// operation answer KindDisabled.
type Schedules interface {
	Add(e schedule.Entry) (schedule.Entry, error)
	Get(id string) (schedule.Entry, error)
	ListByOwner(owner string) ([]schedule.Entry, error)
	Delete(id string) error
}

// Routes is the web-route store, driven only by the `share` commands.
type Routes interface {
	ListBySandbox(sandbox string) ([]routes.Route, error)
	SetVisibility(subdomain, visibility string) error
}

// NodeRoster is the fleet's node registry. Nil makes every node operation
// answer KindDisabled, which is what a single-box deployment is.
//
// It returns NodeInfo rather than a roster row because the roster alone cannot
// answer the two questions an operator actually asks — is that machine
// answering, and how many sandboxes would I strand by removing it — and
// because the row carries the key the node proves possession of, which has no
// rendering anywhere. Whoever wires this joins the roster to the live fleet;
// ctlops applies the policy.
// ApproveNode takes the fingerprint of the node's key, not its name: a name is
// node-authored and so cannot carry an approval. See Ops.ApproveNode.
type NodeRoster interface {
	ListNodes() ([]NodeInfo, error)
	ApproveNode(fp, by string) (NodeInfo, error)
	RemoveNode(name string) error
}

// Minter mints edge session tokens; *edgeauth.Signer satisfies it.
type Minter interface {
	Mint(id edgeauth.Identity, ttl time.Duration) (string, time.Time, error)
}

// GitHubKeys is the github.com dependency, behind an interface so no test in
// this package ever makes a network call. A nil one defaults to
// users.FetchGitHubKeys / users.VerifyGitHubKey / users.FetchGitHubPublicProfile.
type GitHubKeys interface {
	Fetch(ctx context.Context, login string) ([]xssh.PublicKey, error)
	Verify(ctx context.Context, login string, key xssh.PublicKey) (bool, error)
	// Profile is what github.com publishes about a login without any
	// credential. It exists on this interface so the key-proof path can record
	// GitHub's immutable account number too; a failure is not fatal to a link
	// that has already been proved.
	Profile(ctx context.Context, login string) (users.GitHubProfile, error)
}

// GitHubDeviceFlow is the OAuth device flow, behind an interface for the same
// reason. A nil one disables the flow entirely — which is the honest state of a
// host with no client id configured, and the reason every caller checks
// Capabilities().GitHubDevice before offering it.
type GitHubDeviceFlow interface {
	Start(ctx context.Context) (users.DeviceCode, error)
	Wait(ctx context.Context, dc users.DeviceCode) (users.GitHubProfile, error)
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

// Config wires the stores. The optional ones are optional in the same way and
// for the same reasons they are on the Gateway today: a nil store makes its
// commands answer KindDisabled rather than panic, which is what a unit test and
// a minimally-configured host both want.
type Config struct {
	Sandboxes Sandboxes // required
	Templates Templates // required
	Accounts  Accounts  // required

	Tags         Tagger           // nil: tag operations are KindDisabled
	Secrets      Secrets          // nil: secret operations are KindDisabled
	Repos        Repos            // nil: repo operations are KindDisabled
	TemplateTags TemplateBindings // nil: bind is KindDisabled and creates use DefaultImage
	Checkpoints  Checkpoints      // nil: manual durable checkpoints are KindDisabled
	Schedules    Schedules        // nil: schedule operations are KindDisabled
	Routes       Routes           // nil: share operations are KindDisabled
	Sessions     Minter           // nil: MintSessionToken is KindDisabled
	Nodes        NodeRoster       // nil: node operations are KindDisabled
	GitHub       GitHubKeys       // nil: the real github.com client
	// GitHubDevice runs the OAuth device flow. nil — the default, and the state
	// of any host with no --github-client-id — leaves the key check as the only
	// way to link, which is what shipped before this existed.
	GitHubDevice GitHubDeviceFlow
	// GitHubApp resolves installations and authorizes their use. nil — the
	// state of any host with no App private key — leaves repo attachment
	// working as pure configuration, which is all a public repository needs,
	// and makes `repo check` and `github install` KindDisabled.
	GitHubApp GitHubApp
	// HiveMind reads a sandbox's session catalog from the HiveMind SaaS. nil —
	// the state of any host with no --hivemind-api — makes `sessions`
	// KindDisabled.
	HiveMind HiveMind

	DefaultImage   string // rootfs template new sandboxes get
	Domain         string // base zone, for the URL fields on results; "" omits them
	XtermSubdomain string // "xterm" when browser terminals are served; "" omits TerminalURL
	InvitesPerUser int    // non-operator invite quota; 0 means operators only
	// GatewayGuestSubnet is this machine's local guest prefix. Configured node
	// approval requires it so the roster can reject a remote prefix that would
	// make routed guest traffic ambiguous.
	GatewayGuestSubnet string

	NewName func() string    // nil: the built-in adjective-noun generator
	Now     func() time.Time // nil: time.Now — injectable so schedule next-run is testable
	Log     *slog.Logger     // required; one audit line per mutation
}

// Ops is the control-plane core. One per process; safe for concurrent use
// because every store it holds already is.
type Ops struct {
	boxes        Sandboxes
	templates    Templates
	accounts     Accounts
	tags         Tagger
	secrets      Secrets
	repos        Repos
	templateTags TemplateBindings
	checkpoints  Checkpoints
	schedules    Schedules
	routes       Routes
	sessions     Minter
	nodes        NodeRoster
	github       GitHubKeys
	ghDevice     GitHubDeviceFlow
	ghApp        GitHubApp
	hivemind     HiveMind
	// orgMembers reads a GitHub org's roster. A function rather than another
	// narrow interface because it is one call with no state, and a field rather
	// than a direct users.ListOrgMembers so provisioning is testable without
	// reaching github.com.
	orgMembers func(ctx context.Context, org, team, token string) ([]string, error)

	defaultImage       string
	domain             string
	xtermSubdomain     string
	invitesPerUser     int
	gatewayGuestSubnet string

	newName func() string
	now     func() time.Time
	log     *slog.Logger

	// jobs is the in-memory async registry (jobs.go). It is guarded by its own
	// mutex rather than the stores' because a job's lifecycle is entirely local
	// to this process.
	jobsMu    sync.Mutex
	jobs      map[string]*Job
	stop      chan struct{}
	closeOnce sync.Once
}

func New(cfg Config) *Ops {
	o := &Ops{
		boxes:              cfg.Sandboxes,
		templates:          cfg.Templates,
		accounts:           cfg.Accounts,
		tags:               cfg.Tags,
		secrets:            cfg.Secrets,
		repos:              cfg.Repos,
		templateTags:       cfg.TemplateTags,
		checkpoints:        cfg.Checkpoints,
		schedules:          cfg.Schedules,
		routes:             cfg.Routes,
		sessions:           cfg.Sessions,
		nodes:              cfg.Nodes,
		github:             cfg.GitHub,
		ghDevice:           cfg.GitHubDevice,
		ghApp:              cfg.GitHubApp,
		hivemind:           cfg.HiveMind,
		defaultImage:       cfg.DefaultImage,
		domain:             normalizeDomain(cfg.Domain),
		xtermSubdomain:     cfg.XtermSubdomain,
		invitesPerUser:     cfg.InvitesPerUser,
		gatewayGuestSubnet: cfg.GatewayGuestSubnet,
		newName:            cfg.NewName,
		now:                cfg.Now,
		log:                cfg.Log,
		jobs:               map[string]*Job{},
		stop:               make(chan struct{}),
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.log == nil {
		// A nil logger would panic on the first audit line, which is a silly way
		// to lose a control plane; discard instead and let the operator notice
		// the missing audit trail.
		o.log = slog.New(slog.DiscardHandler)
	}
	if o.github == nil {
		o.github = realGitHub{}
	}
	if o.orgMembers == nil {
		o.orgMembers = users.ListMembers
	}
	go o.reapJobs()
	return o
}

// Close stops the job reaper. Idempotent.
func (o *Ops) Close() {
	o.closeOnce.Do(func() { close(o.stop) })
}

// normalizeDomain drops a leading dot, because --proxy-domain is written both
// ways in the wild and ".catnip.sh" would produce "https://box..catnip.sh".
func normalizeDomain(d string) string {
	for len(d) > 0 && d[0] == '.' {
		d = d[1:]
	}
	return d
}

// Caller is who is asking. Handle is already authenticated by the transport.
// KeyFP is the fingerprint of the SSH key on this session — audit only, echoed
// by Whoami, and used as the default GitHub proof on the SSH path; it is empty
// for HTTP callers. Operator status is deliberately NOT a field: ctlops resolves
// it from the account store when (and only when) a command needs it, so a
// transport that forgets to populate it cannot widen anyone's authority.
type Caller struct {
	Handle string
	KeyFP  string
}

// Capabilities reports what this host actually has configured, so a client can
// avoid provoking a KindDisabled instead of discovering it by trial.
type Capabilities struct {
	Archiving     bool `json:"archiving"`
	Snapshots     bool `json:"snapshots"`
	Scheduling    bool `json:"scheduling"`
	Tags          bool `json:"tags"`
	Routes        bool `json:"routes"`
	SessionTokens bool `json:"session_tokens"`
	Terminal      bool `json:"terminal"`
	// GitHubDevice reports that this host can link a GitHub account without the
	// user having published an SSH key there. It is what decides which dialog
	// the signup and `github link` paths offer, so a client asks rather than
	// starting a flow that would answer KindDisabled.
	GitHubDevice bool `json:"github_device"`
	// Repos reports that repositories can be attached to tags at all. False is
	// a host with no repo store, where every repo verb answers 501.
	Repos bool `json:"repos"`
	// GitHubApp reports that this host holds a GitHub App key, which is what
	// makes a PRIVATE repository clonable: without one an attachment is still
	// configuration a public repo clones from, so this is a second bit rather
	// than a narrowing of Repos.
	GitHubApp bool `json:"github_app"`
	// Fleet reports that this host is a gateway other machines can join, which
	// is what makes the node commands answerable. It says nothing about whether
	// any machine actually has joined — that is what `nodes.list` is for.
	Fleet bool `json:"fleet"`
	// TemplateTags reports that a tag can name the base image a sandbox boots
	// from. False is a host with no binding store, where bind and unbind answer
	// 501 and every create takes the operator's default image — which is a
	// different statement from Snapshots, since a host can hold snapshots to
	// fork by name while having nowhere to record a binding.
	TemplateTags bool `json:"template_tags"`
}

func (o *Ops) Capabilities() Capabilities {
	return Capabilities{
		Archiving:     o.boxes != nil && o.boxes.ArchivingEnabled(),
		Snapshots:     o.templates != nil && o.templates.Snapshotter(),
		Scheduling:    o.schedules != nil,
		Tags:          o.tags != nil,
		Routes:        o.routes != nil,
		SessionTokens: o.sessions != nil,
		Terminal:      o.domain != "" && o.xtermSubdomain != "",
		GitHubDevice:  o.ghDevice != nil,
		Repos:         o.repos != nil,
		GitHubApp:     o.ghApp != nil,
		Fleet:         o.nodes != nil,
		TemplateTags:  o.templateTags != nil,
	}
}

// ---------------------------------------------------------------------------
// Budgets
//
// Exported so both transports and the OpenAPI document quote the same numbers
// rather than copying them. Every method applies its own budget through
// withBudget, which is a no-op when ctx already carries an earlier deadline —
// so a disconnecting client still cancels, and a caller that wants a tighter
// ceiling can impose one without ctlops fighting it.
// ---------------------------------------------------------------------------

const (
	PauseTimeout   = 3 * time.Minute  // a full guest memory snapshot
	ArchiveTimeout = 15 * time.Minute // fsck + zerofree + zstd of 25 GB, then transfer
	ResizeTimeout  = 10 * time.Minute // e2fsck + resize2fs + cold boot
	DialTimeout    = 15 * time.Second // create/attach: reaching a freshly booted guest
)

// withBudget bounds ctx by d unless the caller already asked for less. Returning
// the caller's own context in that case is deliberate: a 30-second HTTP handler
// deadline must win over a 15-minute archive budget, or the handler leaks a
// goroutine it can no longer answer for.
func withBudget(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= d {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// ---------------------------------------------------------------------------
// The owner gate
// ---------------------------------------------------------------------------

// owned resolves a sandbox the caller may act on. "Does not exist" and "exists
// but belongs to someone else" return the byte-identical error, from this one
// line, so a cross-owner probe can never confirm a name — and because every
// mutating method calls this before touching the manager, it can never wake a
// stranger's VM either.
func (o *Ops) owned(op, name string, c Caller) (*host.Sandbox, error) {
	if name == "" {
		return nil, Invalid(op, "missing_name", "a sandbox name is required")
	}
	box, ok := o.boxes.Get(name)
	if !ok || box.Owner != c.Handle {
		return nil, NotFound(op, "sandbox", name)
	}
	return box, nil
}

// ownedSnapshot is the same gate for templates. The manager already keys
// snapshots by owner, but resolving through the owner's own list keeps the
// masked message identical to the sandbox one instead of leaving it to whatever
// the driver happens to say.
func (o *Ops) ownedSnapshot(op, name string, c Caller) (*host.Snapshot, error) {
	if name == "" {
		return nil, Invalid(op, "missing_name", "a snapshot name is required")
	}
	for _, s := range o.templates.Snapshots(c.Handle) {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, NotFound(op, "snapshot", name)
}

// ---------------------------------------------------------------------------
// Result projection
// ---------------------------------------------------------------------------

// info projects a manager record onto the public shape. SSHAddr, HostIP and
// GuestV6 are dropped here rather than at the transport, so no future edge can
// serialize the host's internal topology by forgetting to. Node and Unreachable
// are the deliberate exceptions — see SandboxInfo.
func (o *Ops) info(b *host.Sandbox) SandboxInfo {
	si := SandboxInfo{
		Name:        b.Name,
		Owner:       b.Owner,
		State:       string(b.State),
		Node:        b.Node,
		Unreachable: b.Unreachable,
		Pinned:      b.Pinned,
		Ballooned:   b.Ballooned,
		Tags:        []string{},
		VCPUs:       b.VCPUs,
		MemMB:       b.MemMB,
		Turbo:       b.Turbo,
		DiskMB:      b.DiskMB,
		CreatedAt:   b.CreatedAt,
		LastActive:  b.LastActive,
	}
	if o.tags != nil {
		// A tag-store hiccup must not turn `list` into an error: the tags are
		// decoration on this record, not its subject.
		if t, err := o.tags.TagsFor(b.Name); err == nil && len(t) > 0 {
			si.Tags = t
		}
	}
	if o.domain != "" {
		si.URL = "https://" + b.Name + "." + o.domain
		if o.xtermSubdomain != "" {
			si.TerminalURL = "https://" + b.Name + "-" + o.xtermSubdomain + "." + o.domain
		}
	}
	return si
}

// realGitHub is the default GitHubKeys: the package-level users helpers that
// actually talk to github.com. Tests always inject a fake, so nothing in this
// package's own suite reaches the network.
type realGitHub struct{}

func (realGitHub) Fetch(ctx context.Context, login string) ([]xssh.PublicKey, error) {
	return users.FetchGitHubKeys(ctx, login)
}

func (realGitHub) Verify(ctx context.Context, login string, key xssh.PublicKey) (bool, error) {
	return users.VerifyGitHubKey(ctx, login, key)
}

func (realGitHub) Profile(ctx context.Context, login string) (users.GitHubProfile, error) {
	return users.FetchGitHubPublicProfile(ctx, login)
}
