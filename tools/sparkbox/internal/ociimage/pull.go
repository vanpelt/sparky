package ociimage

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Puller reads images from OCI registries. There is no daemon and no local
// image store: layers are streamed from the registry and flattened on the way
// past, so a host needs network and a credential, not a container runtime.
//
// The zero value is not usable; construct with NewPuller.
type Puller struct {
	keychain authn.Keychain
	platform v1.Platform
}

// PullerOption configures a Puller.
type PullerOption func(*Puller)

// WithKeychain overrides where registry credentials come from. The default is
// authn.DefaultKeychain, which reads ~/.docker/config.json and its credential
// helpers — so `docker login ghcr.io` on a host is enough, and a private
// rootfs repository needs no sparkbox-specific credential plumbing.
func WithKeychain(k authn.Keychain) PullerOption {
	return func(p *Puller) { p.keychain = k }
}

// WithPlatform pins the architecture to pull. It defaults to the host's, which
// is what a rootfs template always wants: a sandbox VM runs the host's
// architecture, and pulling the wrong one produces an image that boots to
// nothing rather than an error you can read.
func WithPlatform(os, arch string) PullerOption {
	return func(p *Puller) { p.platform = v1.Platform{OS: os, Architecture: arch} }
}

// NewPuller builds a Puller for the host's platform.
func NewPuller(opts ...PullerOption) *Puller {
	p := &Puller{
		keychain: authn.DefaultKeychain,
		platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Image is a resolved image: a specific digest, its config, and the means to
// read its filesystem.
type Image struct {
	// Ref is the reference as written, e.g. ghcr.io/vanpelt/sparkbox-rootfs:edge.
	Ref string
	// Digest is what Ref resolved to at pull time. This is the identity that
	// matters — a tag moves, a digest does not, and the template cache is keyed
	// on this so "which tools does this sandbox have" is a fact about an
	// immutable artifact rather than an inference from a stamp file.
	Digest string
	// Labels are the image's config labels. sparkbox.login-user lives here and
	// tells the overlay which account the gateway logs in as.
	Labels map[string]string

	img v1.Image
}

// Resolve looks up a reference and returns the image it currently points at,
// without downloading any layer data. Callers use it to decide whether they
// already have a template for this digest before paying for a pull.
func (p *Puller) Resolve(ctx context.Context, ref string) (*Image, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("ociimage: parse reference %q: %w", ref, err)
	}
	img, err := remote.Image(parsed,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(p.keychain),
		remote.WithPlatform(p.platform),
	)
	if err != nil {
		return nil, fmt.Errorf("ociimage: resolve %q: %w", ref, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("ociimage: digest of %q: %w", ref, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("ociimage: config of %q: %w", ref, err)
	}
	return &Image{
		Ref:    ref,
		Digest: digest.String(),
		Labels: cfg.Config.Labels,
		img:    img,
	}, nil
}

// Filesystem streams the image's flattened root filesystem as a tar.
//
// Flattened means the layers are already stacked and whiteouts applied: what
// comes out is one filesystem, which is exactly what Unpack expects and what a
// rootfs is. Layer blobs are fetched lazily as the stream is read, so the caller
// never holds the whole image in memory or on disk.
//
// The caller must Close the returned reader.
func (i *Image) Filesystem() io.ReadCloser {
	return mutate.Extract(i.img)
}

// LoginUser reports the account the gateway should log into a sandbox as, read
// from the image's own sparkbox.login-user label.
//
// Asking the image rather than configuring it host-side is what lets a
// bring-your-own image work: whoever builds it declares its login user, the same
// way hack/images/Dockerfile does today. Images without the label are assumed to
// be root-login, which is what every pre-label template was.
func (i *Image) LoginUser() string {
	if u := i.Labels[LoginUserLabel]; u != "" {
		return u
	}
	return "root"
}

// LoginUserLabel is the image label naming the sandbox's login account.
const LoginUserLabel = "sparkbox.login-user"
