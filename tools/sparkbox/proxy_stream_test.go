package sparkbox_test

// Transparency tests for the HTTP edge. The proxy exists to assert private-vs-
// public access, and beyond that it should be invisible: whatever a user runs in
// their sandbox — a websocket server, an SSE endpoint, a chunked upload handler,
// a big file — must behave the same through the edge as it does on localhost.
//
// Every one of those assertions now lives in placement_e2e_test.go, where it
// runs twice: once against a sandbox on this machine and once against one on
// another. They are the tests with the most to say about placement, because a
// remote guest is reached over an SSH channel with its own 64 KiB window, its
// own half-close and its own teardown ordering — so "the edge never touches the
// bytes" is a much stronger claim over there than it is over loopback, and the
// two passes are what prove it holds either way.
//
// What is left here is the one piece of machinery those tests share: a raw
// connection to the edge, for driving upgrade handshakes and deliberately
// malformed responses by hand.

import (
	"net"
	"testing"
	"time"
)

// dialEdge opens a raw connection to the edge, so a test can drive the wire
// protocol directly (upgrade handshakes, deliberately malformed responses).
func dialEdge(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}
