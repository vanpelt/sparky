// Package macosassets embeds the three files that make up the nested gateway
// image's build context, so `sparkbox setup` can build that image on a Mac with
// no repo checkout — exactly the way package deploy embeds the linux host's
// packet-filter script and units.
//
// The .go file has to live HERE, beside the files: go:embed cannot reach into a
// sibling or parent directory, which is the same constraint that put
// deploy/assets.go where it is.
//
// The set is deliberately these three and nothing else. macos/poc.sh stages the
// identical list into macos/out/image-context (IMAGE_CONTEXT_FILES) rather than
// building from tools/sparkbox, because the Containerfile needs no Go module and
// no 149 MB kernel tarball — and a test asserts the two lists agree, so the
// shell provisioner and the Go one cannot build different images.
package macosassets

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

//go:embed Containerfile.gateway
var containerfileGateway []byte

//go:embed gateway-verify.sh
var gatewayVerify []byte

//go:embed sparkbox-bootstrap.sh
var sparkboxBootstrap []byte

// UbuntuImage is the pinned base the gateway image is built FROM.
//
// Pinned by digest, not by tag: `ubuntu:24.04` moves, and a moving base would
// mean the content-addressed image tag (see ContextSHA) naming a build that is
// not reproducible from the same inputs. It duplicates the Containerfile's ARG
// default, and TestUbuntuImageMatchesContainerfile asserts they stay equal.
const UbuntuImage = "docker.io/library/ubuntu:24.04@sha256:4fbb8e6a8395de5a7550b33509421a2bafbc0aab6c06ba2cef9ebffbc7092d90"

// ContainerfileName is the build file's name inside the staged context.
const ContainerfileName = "Containerfile.gateway"

// File is one staged build-context file.
type File struct {
	Name string
	Body []byte
	Mode uint32
}

// Files returns the build context in a FIXED order. Fixed because ContextSHA
// hashes it, and a map-order shuffle would produce a different image tag for an
// identical context — which would rebuild the image on every other run.
func Files() []File {
	return []File{
		{Name: ContainerfileName, Body: containerfileGateway, Mode: 0o644},
		// Both scripts land at 0755 in the image via a RUN chmod, but they are
		// written executable here too so a human poking at the staged context
		// can run them.
		{Name: "gateway-verify.sh", Body: gatewayVerify, Mode: 0o755},
		{Name: "sparkbox-bootstrap.sh", Body: sparkboxBootstrap, Mode: 0o755},
	}
}

// ContextSHA is the content hash of the whole build context: every embedded
// file (name and body) plus the pinned base image.
//
// It exists so the image tag can be CONTENT-ADDRESSED
// (local/sparkbox-gateway:<first 12 hex>). That turns "is the image current?"
// from an existence check into a content comparison — the lesson every other
// step in hostsetup already encodes, and a genuine improvement over poc.sh,
// which tags a constant and would therefore keep a stale image forever after
// one of these files changed.
//
// It is a pure function of the binary, so two runs of the same sparkbox always
// want the same tag and a re-run never rebuilds.
func ContextSHA() string {
	h := sha256.New()
	for _, f := range Files() {
		h.Write([]byte(f.Name))
		h.Write([]byte{0})
		h.Write(f.Body)
		h.Write([]byte{0})
	}
	h.Write([]byte(UbuntuImage))
	return hex.EncodeToString(h.Sum(nil))
}
