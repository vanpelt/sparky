package macosassets

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestFilesAreNonEmptyAndOrdered(t *testing.T) {
	files := Files()
	if len(files) != 3 {
		t.Fatalf("expected 3 context files, got %d", len(files))
	}
	for _, f := range files {
		if len(f.Body) == 0 {
			t.Errorf("%s is empty", f.Name)
		}
	}
	// Fixed order, because ContextSHA hashes it into the image tag.
	want := []string{"Containerfile.gateway", "gateway-verify.sh", "sparkbox-bootstrap.sh"}
	var got []string
	for _, f := range files {
		got = append(got, f.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("file order = %q, want %q", got, want)
	}
}

// TestFileSetMatchesPocSh is the guard against the shell provisioner and the Go
// one building different images: poc.sh stages IMAGE_CONTEXT_FILES, we embed
// Files(), and the two must name the same three files.
func TestFileSetMatchesPocSh(t *testing.T) {
	b, err := os.ReadFile("poc.sh")
	if err != nil {
		t.Skipf("poc.sh not readable: %v", err)
	}
	re := regexp.MustCompile(`(?m)^IMAGE_CONTEXT_FILES=\(([^)]*)\)`)
	m := re.FindSubmatch(b)
	if m == nil {
		t.Fatal("could not find IMAGE_CONTEXT_FILES=( … ) in poc.sh")
	}
	shell := strings.Fields(string(m[1]))
	var ours []string
	for _, f := range Files() {
		ours = append(ours, f.Name)
	}
	slices.Sort(shell)
	slices.Sort(ours)
	if !slices.Equal(shell, ours) {
		t.Errorf("poc.sh stages %q but the Go embed carries %q — the two provisioners would build different images", shell, ours)
	}
}

func TestUbuntuImageMatchesContainerfile(t *testing.T) {
	var body string
	for _, f := range Files() {
		if f.Name == ContainerfileName {
			body = string(f.Body)
		}
	}
	want := "ARG UBUNTU_IMAGE=" + UbuntuImage
	if !strings.Contains(body, want) {
		t.Errorf("the Containerfile's ARG default and macosassets.UbuntuImage disagree; expected a line %q", want)
	}
}

func TestContextSHAIsStableAndTagShaped(t *testing.T) {
	a, b := ContextSHA(), ContextSHA()
	if a != b {
		t.Fatalf("ContextSHA is not deterministic: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("ContextSHA = %q, want 64 hex characters", a)
	}
	// The first 12 characters become the image tag, which must be a legal OCI
	// tag: hex always is.
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(a[:12]) {
		t.Errorf("tag prefix %q is not hex", a[:12])
	}
}
