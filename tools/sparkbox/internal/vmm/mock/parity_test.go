package mock_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/vmmtest"
)

// TestMockParity runs the driver-parity suite against the mock driver.
//
// The mock cannot answer the questions the harness was built for — it boots no
// kernel, so "did the guest resume or reboot" has no meaning here. What this
// run is for is the suite itself: it keeps every case compiling and exercised
// on an ordinary laptop between hardware runs, which is the failure mode a
// KVM-only harness has. It also holds the mock to the same contracts as the
// real driver, which is what makes it a usable stand-in for the manager tests.
//
// The real answers come from internal/vmm/firecracker/parity_linux_test.go.
func TestMockParity(t *testing.T) {
	vmmtest.Run(t, func(t *testing.T) *vmmtest.Fixture {
		hostKey := newSigner(t)
		clientKey := newSigner(t)
		d := mock.New(t.TempDir(), hostKey)
		t.Cleanup(func() { d.Close() }) //nolint:errcheck
		return &vmmtest.Fixture{
			Driver:        d,
			BaseImage:     "ubuntu",
			VCPUs:         2,
			MemMB:         2048,
			AuthorizedKey: string(xssh.MarshalAuthorizedKey(clientKey.PublicKey())),
			Signer:        clientKey,
			BootTimeout:   15 * time.Second,
			Traits: vmmtest.Traits{
				// The mock's "disk" is a host directory, so its usage figure is
				// as live as the filesystem it sits on — the one trait it can
				// honestly claim, and worth claiming because it means the
				// positive form of that assertion runs somewhere. Everything
				// else is false: no kernel, no memory snapshot, every sandbox
				// on 127.0.0.1, and no template until Snapshot mints one.
				LiveDiskUsage: true,
			},
		}
	})
}

func newSigner(t *testing.T) xssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
