// Package domainmeta turns a bare domain into the two things a Ubiquiti-style
// per-domain bandwidth row needs: a friendly display name ("youtube.com" ->
// "YouTube") and a favicon.
//
// Favicons are fetched host-side and cached on disk, then served from the
// console's own origin — never hot-linked from the browser. That keeps the
// console's Content-Security-Policy tight (img-src 'self' data:) and, more
// importantly, works even when the viewer's browser is on a restricted network
// that can't reach an icon CDN: the box fetches once, everyone reads the cached
// bytes. Icons key on the registrable domain, so i.ytimg.com and youtube.com
// share one.
package domainmeta

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

// curated maps a registrable domain to a nicer display name than the title-case
// fallback would produce. Seeded with the dev-tooling destinations agents
// actually hit; extend freely — a miss just falls back to title-case.
var curated = map[string]string{
	"github.com":            "GitHub",
	"githubusercontent.com": "GitHub",
	"gitlab.com":            "GitLab",
	"bitbucket.org":         "Bitbucket",
	"npmjs.org":             "npm",
	"npmjs.com":             "npm",
	"nodejs.org":            "Node.js",
	"pypi.org":              "PyPI",
	"pythonhosted.org":      "PyPI",
	"docker.com":            "Docker",
	"docker.io":             "Docker Hub",
	"ghcr.io":               "GitHub Container Registry",
	"quay.io":               "Quay",
	"huggingface.co":        "Hugging Face",
	"anthropic.com":         "Anthropic",
	"openai.com":            "OpenAI",
	"googleapis.com":        "Google APIs",
	"google.com":            "Google",
	"gstatic.com":           "Google",
	"cloudflare.com":        "Cloudflare",
	"cloudfront.net":        "CloudFront",
	"amazonaws.com":         "Amazon AWS",
	"debian.org":            "Debian",
	"ubuntu.com":            "Ubuntu",
	"archlinux.org":         "Arch Linux",
	"alpinelinux.org":       "Alpine",
	"rubygems.org":          "RubyGems",
	"crates.io":             "crates.io",
	"golang.org":            "Go",
	"go.dev":                "Go",
	"microsoft.com":         "Microsoft",
	"vscode.dev":            "VS Code",
	"jsdelivr.net":          "jsDelivr",
	"unpkg.com":             "unpkg",
	"sentry.io":             "Sentry",
	"datadoghq.com":         "Datadog",
	"slack.com":             "Slack",
	"discord.com":           "Discord",
	"youtube.com":           "YouTube",
	"ytimg.com":             "YouTube",
	"wandb.ai":              "Weights & Biases",
	"wandb.com":             "Weights & Biases",
}

// Registrable returns the eTLD+1 for a domain (ytimg.com for i.ytimg.com). It
// lower-cases and strips a trailing dot first. A literal IP (a raw-IP bandwidth
// bucket) or any parse failure returns the input unchanged.
func Registrable(domain string) string {
	d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if _, err := netip.ParseAddr(d); err == nil {
		return d // raw IP, not a registrable domain
	}
	if reg, err := publicsuffix.EffectiveTLDPlusOne(d); err == nil {
		return reg
	}
	return d
}

// DisplayName returns a friendly label for a domain: a curated name for known
// registrable domains, else the title-cased leftmost label of the registrable
// domain (some-startup.io -> "Some Startup"). An input that isn't a domain
// (a raw IP bucket) is returned unchanged.
func DisplayName(domain string) string {
	reg := Registrable(domain)
	if _, err := netip.ParseAddr(reg); err == nil {
		return reg // raw IP bucket — show the address as-is
	}
	if name, ok := curated[reg]; ok {
		return name
	}
	label := reg
	if i := strings.IndexByte(reg, '.'); i > 0 {
		label = reg[:i]
	}
	if label == "" {
		return domain
	}
	return titleCase(label)
}

// titleCase renders a domain label as a display word: split on - and _, then
// upper-case each part's first rune.
func titleCase(label string) string {
	parts := strings.FieldsFunc(label, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// globeSVG is the neutral fallback icon, inline so it needs no fetch and stays
// within an img-src 'self' data: CSP when the client renders it.
const globeSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.2"><circle cx="8" cy="8" r="6.5"/><path d="M1.5 8h13M8 1.5c2 2 2 11 0 13M8 1.5c-2 2-2 11 0 13"/></svg>`

// GlobeSVG returns the fallback icon bytes and content type.
func GlobeSVG() ([]byte, string) { return []byte(globeSVG), "image/svg+xml" }

// Fetcher fetches the bytes and content type of a favicon for a registrable
// domain. Split out so tests inject a fake and the default hits the network.
type Fetcher func(ctx context.Context, registrable string) (data []byte, contentType string, ok bool)

// FaviconCache serves favicons from an on-disk cache, fetching on a miss. Keyed
// by registrable domain. Negative results are cached with a TTL so a dead
// domain isn't refetched on every render.
type FaviconCache struct {
	dir     string
	fetch   Fetcher
	negTTL  time.Duration
	keyLock keyedMutex

	mu  sync.Mutex
	neg map[string]time.Time // registrable -> when the miss was recorded
}

// NewFaviconCache builds a cache backed by dir (created if missing). A nil
// fetcher uses the default DuckDuckGo→Google fetcher.
func NewFaviconCache(dir string, fetch Fetcher) *FaviconCache {
	if fetch == nil {
		fetch = defaultFetch
	}
	_ = os.MkdirAll(dir, 0o755)
	return &FaviconCache{dir: dir, fetch: fetch, negTTL: time.Hour, neg: map[string]time.Time{}}
}

// Get returns a favicon for domain (any subdomain resolves to its registrable
// icon). On any miss or failure it returns the neutral globe with ok=false, so a
// caller can still serve a 200 and the <img> never breaks.
func (c *FaviconCache) Get(ctx context.Context, domain string) (data []byte, contentType string, ok bool) {
	reg := Registrable(domain)
	if reg == "" {
		g, ct := GlobeSVG()
		return g, ct, false
	}

	// Serialise concurrent first-loads of the same domain (a page render fires
	// many favicon requests at once).
	unlock := c.keyLock.lock(reg)
	defer unlock()

	if data, ct, ok := c.readDisk(reg); ok {
		return data, ct, true
	}
	if c.negativeCached(reg) {
		g, ct := GlobeSVG()
		return g, ct, false
	}
	data, ct, ok := c.fetch(ctx, reg)
	if !ok || len(data) == 0 {
		c.markNegative(reg)
		g, gct := GlobeSVG()
		return g, gct, false
	}
	c.writeDisk(reg, data, ct)
	return data, ct, true
}

func (c *FaviconCache) path(reg string) string {
	// One file per registrable domain; the extension is irrelevant since the
	// content type is sniffed on read.
	return filepath.Join(c.dir, strings.ReplaceAll(reg, "/", "_")+".icon")
}

func (c *FaviconCache) readDisk(reg string) ([]byte, string, bool) {
	b, err := os.ReadFile(c.path(reg))
	if err != nil || len(b) == 0 {
		return nil, "", false
	}
	return b, sniffType(b), true
}

func (c *FaviconCache) writeDisk(reg string, data []byte, _ string) {
	_ = os.WriteFile(c.path(reg), data, 0o644)
}

func (c *FaviconCache) negativeCached(reg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.neg[reg]
	return ok && time.Since(at) < c.negTTL
}

func (c *FaviconCache) markNegative(reg string) {
	c.mu.Lock()
	c.neg[reg] = time.Now()
	c.mu.Unlock()
}

// sniffType classifies favicon bytes; falls back to a generic image type since
// browsers render by content regardless of the declared type for <img>.
func sniffType(b []byte) string {
	ct := http.DetectContentType(b)
	if strings.HasPrefix(ct, "image/") {
		return ct
	}
	// .ico files sniff as application/octet-stream on some inputs.
	if len(b) >= 4 && b[0] == 0x00 && b[1] == 0x00 && b[2] == 0x01 && b[3] == 0x00 {
		return "image/x-icon"
	}
	return "image/x-icon"
}

// defaultFetch pulls a favicon host-side: DuckDuckGo's keyless service first
// (privacy-friendlier), then Google's S2 as a fallback.
func defaultFetch(ctx context.Context, reg string) ([]byte, string, bool) {
	urls := []string{
		"https://icons.duckduckgo.com/ip3/" + reg + ".ico",
		"https://www.google.com/s2/favicons?sz=64&domain=" + reg,
	}
	client := &http.Client{Timeout: 4 * time.Second}
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, ct, ok := readImage(resp)
		if ok {
			return body, ct, true
		}
	}
	return nil, "", false
}

// readImage reads a favicon response: a 200 with a non-trivial image body.
// Bodies are capped at 256 KiB — a favicon, not a payload.
func readImage(resp *http.Response) ([]byte, string, bool) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil || len(body) < 16 {
		return nil, "", false
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = sniffType(body)
	}
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", false
	}
	return body, ct, true
}

// keyedMutex serialises work per string key without a lock per key living
// forever: it hands back an unlock that frees the entry when no one waits.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	mu, ok := k.m[key]
	if !ok {
		mu = &sync.Mutex{}
		k.m[key] = mu
	}
	k.mu.Unlock()
	mu.Lock()
	return func() { mu.Unlock() }
}
