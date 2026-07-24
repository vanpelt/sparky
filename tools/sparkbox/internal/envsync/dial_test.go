package envsync

// The dialer seam: a push must still land when the guest is reached through
// something other than a direct TCP dial to its address, because in a fleet
// the box may be on another machine and its address means nothing here.

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
)

func TestSetDialerCarriesTheDelivery(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")

	var mu sync.Mutex
	var dialed []string
	te.syncer.SetDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, network+" "+addr)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})

	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("PushEnv: %v", err)
	}
	if got := te.readEnv(t, "boxa"); !strings.Contains(got, `API_KEY="s3kr3t"`) {
		t.Fatalf("secret not delivered through the injected dialer:\n%s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || dialed[0] != "tcp "+box.SSHAddr {
		t.Fatalf("dialer saw %v, want one [tcp %s]", dialed, box.SSHAddr)
	}
}

// A syncer nobody handed a dialer keeps dialing the guest itself, which is
// every deployment that has one machine.
func TestNilDialerStillDelivers(t *testing.T) {
	te := newTestEnv(t)
	box := te.create(t, "boxa", "alice")
	te.seedEnv(t, "boxa", bakedEnv)
	te.tagAndPut(t, "alice", "boxa", "web", "API_KEY", "s3kr3t")

	if err := te.syncer.PushEnv(context.Background(), box); err != nil {
		t.Fatalf("PushEnv: %v", err)
	}
	if got := te.readEnv(t, "boxa"); !strings.Contains(got, `API_KEY="s3kr3t"`) {
		t.Fatalf("secret not delivered:\n%s", got)
	}
}
