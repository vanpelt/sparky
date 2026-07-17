//go:build linux

package proxy

import (
	"net"

	"golang.org/x/sys/unix"
)

// soOriginalDst is the getsockopt option that returns a REDIRECTed
// connection's pre-DNAT destination (the port the client actually dialed). It
// shares its numeric value with IP6T_SO_ORIGINAL_DST.
const soOriginalDst = 80

// OriginalDstPort returns the port a connection was originally dialed on before
// iptables REDIRECT funnelled it to the edge listener, recovered via
// SO_ORIGINAL_DST. ok is false for a connection that wasn't redirected (a
// direct hit on the edge port) or on any error, in which case the caller falls
// back to the Host header.
func OriginalDstPort(conn net.Conn) (int, bool) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return 0, false
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return 0, false
	}
	var port int
	var found bool
	// The result is a sockaddr_in (IPv4) or sockaddr_in6 (IPv6); in both the
	// port sits in bytes [2:4], network byte order. IPPROTO_IP covers the v4
	// REDIRECT path; IPPROTO_IPV6 the v6 one.
	ctrlErr := raw.Control(func(fd uintptr) {
		for _, level := range []int{unix.IPPROTO_IP, unix.IPPROTO_IPV6} {
			mreq, err := unix.GetsockoptIPv6Mreq(int(fd), level, soOriginalDst)
			if err != nil {
				continue
			}
			port = int(mreq.Multiaddr[2])<<8 | int(mreq.Multiaddr[3])
			found = port > 0
			if found {
				return
			}
		}
	})
	if ctrlErr != nil {
		return 0, false
	}
	return port, found
}
