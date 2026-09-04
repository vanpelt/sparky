// Package federation describes the relying parties a sandbox proves its
// identity to, and nothing else about them.
//
// A sandbox already carries an OIDC assertion this fleet signs (see
// internal/oidc and docs/identity-federation-design.md). Every relying party
// that accepts that shape of proof — HiveMind, OpenAI's workload identity,
// whatever comes next — needs the same three things from the guest: an
// assertion minted for ITS audience, kept fresh at a path its client reads,
// and a handful of environment variables that tell the client where the
// assertion is and which rule to present it against. None of that is specific
// to the party, so none of it is hard-coded any more: a Federator is one such
// party, a Config is the list an operator hands the fleet, and the guest walks
// the list.
//
// The host's half is deliberately small. It does not know what OpenAI or
// HiveMind do with the assertion; it mints for the audiences the list names
// and serves the list to guests at the metadata service's GET /federation.
// Everything a relying party needs beyond that — which env var its SDK reads,
// what the value is — is an operator's statement in the config file, which is
// what lets a new party be added in a deploy rather than a release.
//
// What is NOT here: egress. A relying party's hosts (auth.openai.com, say)
// must be reachable from a governed sandbox, and that is a Sluice allowlist
// and netrules.TrustedDomains concern, not this package's.
package federation

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Federator is one relying party every sandbox on the fleet keeps an
// assertion for.
type Federator struct {
	// Name is the operator's handle for the party: a short lowercase label
	// (`hivemind`, `openai`) that names the default token directory, the
	// guest's log lines and the profile snippet. Unique within a Config.
	Name string `json:"name"`
	// Audience is the `aud` the guest asks the metadata service to mint for.
	// The issuer's allowlist is what decides whether it may be minted, and
	// WithAudiences keeps that allowlist in step with this list; here it only
	// says which one to ask for. It must match what the relying party has
	// configured exactly — it is an opaque string to both ends.
	Audience string `json:"audience"`
	// TokenFile is the absolute path the guest keeps the assertion at, mode
	// 0600 in a 0700 directory owned by the login user. Empty means
	// /var/run/secrets/<name>/token.
	TokenFile string `json:"token_file,omitempty"`
	// TokenFileEnv, when set, is exported to every process in the guest with
	// TokenFile as its value — OPENAI_IDENTITY_TOKEN_FILE, for one. Left empty
	// when the client already looks at TokenFile by default, as HiveMind's
	// does.
	TokenFileEnv string `json:"token_file_env,omitempty"`
	// Env is exported to every process in the guest as-is: the identifiers a
	// client needs to present the assertion (a federation rule, a provider
	// id). Values are plain identifiers, never secrets — possession of them
	// grants nothing without an assertion this fleet's key signed — and they
	// land in /etc/environment, so they are restricted to characters that file
	// carries safely (see Validate).
	Env map[string]string `json:"env,omitempty"`
	// ContextEnv, when set, names a shell variable the guest exports with a
	// JSON attribution context describing the sandbox:
	// {"instance_id":"<sandbox>","labels":{"owner":"<owner>","box":"<node>"}}.
	// It is the shape OpenAI reads as OPENAI_WORKLOAD_IDENTITY_CONTEXT, and it
	// reaches login shells only — a JSON value cannot ride /etc/environment
	// safely, which the guest script explains at length.
	ContextEnv string `json:"context_env,omitempty"`
}

// Config is the fleet's whole list. The order is the order the guest mints in.
type Config struct {
	Federators []Federator `json:"federators"`
}

// DefaultTokenFile is where a federator's assertion lives when the operator
// did not say: a dedicated directory per party under /var/run/secrets, which is
// both what HiveMind's client already looks at and what OpenAI's guidance asks
// for ("an absolute path in a dedicated directory").
func DefaultTokenFile(name string) string {
	return "/var/run/secrets/" + name + "/token"
}

// HiveMind is the federator every fleet has had since before this package
// existed: an assertion for the platform's own audience at the path
// `hivemind start` reads with no configuration at all.
func HiveMind(audience string) Federator {
	return Federator{Name: "hivemind", Audience: audience}
}

// OpenAI is the federator for OpenAI's workload identity. The three ids are
// what an OpenAI organization admin hands back after creating a Workload
// Identity Provider and a federation rule for this fleet's issuer; see
// docs/openai-workload-identity.md for exactly what to ask them for.
//
// A rule id is the one identifier `codex` needs. The provider and service
// account ids are what the OpenAI SDKs take when they perform the exchange
// themselves. Any of the three may be empty and is then simply not exported,
// but a token file with no rule or provider to present it against leaves
// `codex` worse than untouched — it federates and fails instead of offering the
// login it would otherwise have offered — so pass at least one.
func OpenAI(providerID, ruleID, serviceAccountID string) Federator {
	env := map[string]string{}
	if ruleID != "" {
		env["OPENAI_FEDERATION_RULE_ID"] = ruleID
	}
	if providerID != "" {
		env["OPENAI_IDENTITY_PROVIDER_ID"] = providerID
	}
	if serviceAccountID != "" {
		env["OPENAI_SERVICE_ACCOUNT_ID"] = serviceAccountID
	}
	return Federator{
		Name: "openai",
		// The audience OpenAI's own Kubernetes guide tells operators to project,
		// so an admin configuring the provider sees a value they have seen
		// before. Opaque to both ends; it is not fetched.
		Audience: "https://api.openai.com/v1",
		// OpenAI documents this exact directory, and Codex reads the path from
		// OPENAI_IDENTITY_TOKEN_FILE.
		TokenFile:    "/var/run/secrets/openai.com/identity-token",
		TokenFileEnv: "OPENAI_IDENTITY_TOKEN_FILE",
		Env:          env,
		ContextEnv:   "OPENAI_WORKLOAD_IDENTITY_CONTEXT",
	}
}

// Default is what a fleet federates with when the operator has said nothing:
// HiveMind, at the audience the host itself exchanges for.
func Default(hivemindAudience string) Config {
	return Config{Federators: []Federator{HiveMind(hivemindAudience)}}
}

// Load reads a Config from a JSON file, fills its defaults and validates it.
// An empty path returns fallback, which is how "no --federation-config" means
// "what this fleet always did" rather than "federate with nothing".
func Load(path string, fallback Config) (Config, error) {
	if path == "" {
		cfg := fallback.WithDefaults()
		return cfg, cfg.Validate()
	}
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	cfg, err := Parse(f)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes a Config from JSON, fills its defaults and validates it.
// Unknown fields are an error rather than silently ignored: a misspelt
// `token_file_env` is a federation that half-works, discovered in a sandbox.
func Parse(r io.Reader) (Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, err
	}
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// WithDefaults returns a copy with every empty TokenFile filled in.
func (c Config) WithDefaults() Config {
	out := Config{Federators: make([]Federator, len(c.Federators))}
	for i, f := range c.Federators {
		if f.TokenFile == "" {
			f.TokenFile = DefaultTokenFile(f.Name)
		}
		out.Federators[i] = f
	}
	return out
}

var (
	// A name is a path segment and a log label; it is also what the guest
	// greps for, so it may not carry a tab or anything that looks like one.
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
	// The name /etc/environment and a POSIX shell both accept.
	envNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	// What a value may carry, in every field the guest exports. pam_env takes
	// /etc/environment values bare and a shell sourcing the file performs
	// quote removal and expansion on them, so the two readers agree on a value
	// only when it needs neither: no whitespace, no quotes, no backslash, no
	// `$`, no backtick, nothing a shell would expand or a JSON encoder would
	// escape. URLs, paths and every identifier a relying party issues fit.
	valueRe = regexp.MustCompile(`^[A-Za-z0-9._:/@+=,%~-]+$`)
)

// Validate is the check every Config passes before a guest can read it.
func (c Config) Validate() error {
	names := map[string]bool{}
	files := map[string]string{}
	for i, f := range c.Federators {
		if !nameRe.MatchString(f.Name) {
			return fmt.Errorf("federator %d: name %q must be lowercase letters, digits, '.' or '-'", i, f.Name)
		}
		if names[f.Name] {
			return fmt.Errorf("federator %q is listed twice", f.Name)
		}
		names[f.Name] = true
		if f.Audience == "" {
			return fmt.Errorf("federator %q: audience is required", f.Name)
		}
		if !valueRe.MatchString(f.Audience) {
			return fmt.Errorf("federator %q: audience %q carries a character /etc/environment cannot", f.Name, f.Audience)
		}
		if !path.IsAbs(f.TokenFile) || path.Clean(f.TokenFile) != f.TokenFile || strings.HasSuffix(f.TokenFile, "/") {
			return fmt.Errorf("federator %q: token_file %q must be a clean absolute path", f.Name, f.TokenFile)
		}
		if !valueRe.MatchString(f.TokenFile) {
			return fmt.Errorf("federator %q: token_file %q carries a character /etc/environment cannot", f.Name, f.TokenFile)
		}
		if prev, dup := files[f.TokenFile]; dup {
			return fmt.Errorf("federators %q and %q share token_file %s", prev, f.Name, f.TokenFile)
		}
		files[f.TokenFile] = f.Name
		if f.TokenFileEnv != "" && !envNameRe.MatchString(f.TokenFileEnv) {
			return fmt.Errorf("federator %q: token_file_env %q is not a variable name", f.Name, f.TokenFileEnv)
		}
		if f.ContextEnv != "" && !envNameRe.MatchString(f.ContextEnv) {
			return fmt.Errorf("federator %q: context_env %q is not a variable name", f.Name, f.ContextEnv)
		}
		for k, v := range f.Env {
			if !envNameRe.MatchString(k) {
				return fmt.Errorf("federator %q: env %q is not a variable name", f.Name, k)
			}
			if !valueRe.MatchString(v) {
				return fmt.Errorf("federator %q: env %s=%q carries a character /etc/environment cannot", f.Name, k, v)
			}
		}
	}
	return nil
}

// Names lists the federators in mint order.
func (c Config) Names() []string {
	out := make([]string, 0, len(c.Federators))
	for _, f := range c.Federators {
		out = append(out, f.Name)
	}
	return out
}

// Audiences lists every audience the guests will ask for, deduplicated, in
// order of first appearance.
func (c Config) Audiences() []string {
	var out []string
	for _, f := range c.Federators {
		if f.Audience != "" && !slices.Contains(out, f.Audience) {
			out = append(out, f.Audience)
		}
	}
	return out
}

// Get finds a federator by name.
func (c Config) Get(name string) (Federator, bool) {
	for _, f := range c.Federators {
		if f.Name == name {
			return f, true
		}
	}
	return Federator{}, false
}

// WithAudiences adds every federator's audience to the issuer's allowlist.
//
// The allowlist and the audiences guests are told to ask for are two
// statements of one fact, and an operator who has to repeat one in
// --oidc-audiences will one day not. The failure when they disagree is remote
// from its cause: everything configures cleanly, every log line is green, and
// guests take a 400 from a mint nobody is watching.
//
// It never widens an EMPTY list. Empty means "any audience", and appending to
// it would silently narrow the issuer to the listed values — turning a
// permissive development host into one that refuses the mint it was
// performing yesterday.
func WithAudiences(allowed []string, c Config) []string {
	if len(allowed) == 0 {
		return allowed
	}
	out := slices.Clone(allowed)
	for _, aud := range c.Audiences() {
		if !slices.Contains(out, aud) {
			out = append(out, aud)
		}
	}
	return out
}

// The guest-facing encoding.
//
// The guest reads this with sed, grep and cut, and nothing else: no JSON parser
// is a dependency the token unit may take on this early in boot, and a list of
// objects is exactly the shape sed cannot walk. So the metadata service serves
// the list flat: one field per line, `<name> TAB <key> TAB <value>`, in mint
// order, with `env` repeated once per variable as `KEY=VALUE`. Validate is what
// makes that unambiguous — no name, key or value can carry a tab, a newline or
// a quote — and it is why the guest's reader is a `cut -f3-` and not a parser.
//
// One encoding, one encoder, and the deploy tests feed the real one to the real
// guest script; the guest never types this format by hand.

// GuestKeys are the second column's vocabulary.
const (
	GuestKeyAudience     = "audience"
	GuestKeyTokenFile    = "token_file"
	GuestKeyTokenFileEnv = "token_file_env"
	GuestKeyContextEnv   = "context_env"
	GuestKeyEnv          = "env"
)

// Guest renders the list in the guest-facing encoding. The Config must have
// passed Validate; a value that cannot be encoded is written as-is and would
// be the guest's problem, which is why the service validates at construction.
func (c Config) Guest() string {
	var b strings.Builder
	for _, f := range c.WithDefaults().Federators {
		line := func(key, value string) {
			if value == "" {
				return
			}
			b.WriteString(f.Name)
			b.WriteByte('\t')
			b.WriteString(key)
			b.WriteByte('\t')
			b.WriteString(value)
			b.WriteByte('\n')
		}
		line(GuestKeyAudience, f.Audience)
		line(GuestKeyTokenFile, f.TokenFile)
		line(GuestKeyTokenFileEnv, f.TokenFileEnv)
		line(GuestKeyContextEnv, f.ContextEnv)
		// Sorted, so two hosts serving the same Config serve the same bytes
		// and a diff of two guests' copies means what it appears to.
		for _, k := range slices.Sorted(maps.Keys(f.Env)) {
			line(GuestKeyEnv, k+"="+f.Env[k])
		}
	}
	return b.String()
}

// ParseGuest is the inverse of Guest, for tests and tooling that want to see
// what a guest saw. It is not what the guest itself runs.
func ParseGuest(r io.Reader) (Config, error) {
	var cfg Config
	index := map[string]int{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		text := sc.Text()
		if text == "" {
			continue
		}
		parts := strings.SplitN(text, "\t", 3)
		if len(parts) != 3 {
			return Config{}, fmt.Errorf("line %d: want name, key and value separated by tabs", n)
		}
		name, key, value := parts[0], parts[1], parts[2]
		i, ok := index[name]
		if !ok {
			i = len(cfg.Federators)
			index[name] = i
			cfg.Federators = append(cfg.Federators, Federator{Name: name})
		}
		f := &cfg.Federators[i]
		switch key {
		case GuestKeyAudience:
			f.Audience = value
		case GuestKeyTokenFile:
			f.TokenFile = value
		case GuestKeyTokenFileEnv:
			f.TokenFileEnv = value
		case GuestKeyContextEnv:
			f.ContextEnv = value
		case GuestKeyEnv:
			k, v, found := strings.Cut(value, "=")
			if !found {
				return Config{}, fmt.Errorf("line %d: env %q is not KEY=VALUE", n, value)
			}
			if f.Env == nil {
				f.Env = map[string]string{}
			}
			f.Env[k] = v
		default:
			return Config{}, fmt.Errorf("line %d: unknown key %q", n, key)
		}
	}
	if err := sc.Err(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
