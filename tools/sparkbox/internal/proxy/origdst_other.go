//go:build !linux

package proxy

import "net"

// OriginalDstPort is a no-op off Linux: there is no SO_ORIGINAL_DST, and the
// mock/dev edge is dialed directly, so the target port comes from the Host
// header instead. Keeps the macOS build green.
func OriginalDstPort(conn net.Conn) (int, bool) { return 0, false }
