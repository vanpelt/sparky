package domainmeta

import (
	"context"
	"testing"
)

func TestDisplayName(t *testing.T) {
	cases := map[string]string{
		"github.com":        "GitHub",
		"api.github.com":    "GitHub",          // subdomain -> registrable -> curated
		"i.ytimg.com":       "YouTube",         // curated on registrable ytimg.com
		"some-startup.io":   "Some Startup",    // title-case fallback, hyphen split
		"files.example.org": "Example",         // fallback on registrable's label
		"93.184.216.34":     "93.184.216.34",   // not a domain -> unchanged
	}
	for in, want := range cases {
		if got := DisplayName(in); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegistrableCollapsesSubdomains(t *testing.T) {
	if got := Registrable("i.ytimg.com"); got != "ytimg.com" {
		t.Errorf("Registrable = %q, want ytimg.com", got)
	}
	if got := Registrable("API.GitHub.Com."); got != "github.com" {
		t.Errorf("Registrable normalise = %q", got)
	}
}

// a 1x1 png so http.DetectContentType classifies it as image/png.
var pngPixel = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
}

func TestFaviconCacheFetchesOnceThenServesFromDisk(t *testing.T) {
	var calls int
	fetch := func(ctx context.Context, reg string) ([]byte, string, bool) {
		calls++
		if reg != "github.com" {
			t.Errorf("fetch keyed on %q, want registrable github.com", reg)
		}
		return pngPixel, "image/png", true
	}
	c := NewFaviconCache(t.TempDir(), fetch)

	// Two different subdomains collapse to one registrable icon, one fetch.
	for _, d := range []string{"github.com", "api.github.com"} {
		data, ct, ok := c.Get(context.Background(), d)
		if !ok || ct != "image/png" || len(data) == 0 {
			t.Fatalf("Get(%q) = ok=%v ct=%q", d, ok, ct)
		}
	}
	if calls != 1 {
		t.Errorf("fetched %d times, want 1 (disk cache + registrable keying)", calls)
	}
}

func TestFaviconCacheNegativeFallsBackToGlobe(t *testing.T) {
	var calls int
	fetch := func(ctx context.Context, reg string) ([]byte, string, bool) {
		calls++
		return nil, "", false // always a miss
	}
	c := NewFaviconCache(t.TempDir(), fetch)
	data, ct, ok := c.Get(context.Background(), "nonexistent.example")
	if ok {
		t.Error("miss should report ok=false")
	}
	if ct != "image/svg+xml" || len(data) == 0 {
		t.Errorf("fallback = %q/%d bytes, want globe svg", ct, len(data))
	}
	// Second call is served from the negative cache, no refetch.
	c.Get(context.Background(), "nonexistent.example")
	if calls != 1 {
		t.Errorf("negative not cached: fetched %d times", calls)
	}
}
