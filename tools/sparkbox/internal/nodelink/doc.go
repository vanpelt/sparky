// Package nodelink is the wire a fleet node speaks to its gateway: control
// framing over a single SSH session channel, request/reply correlation,
// one-way events, and the sandbox-stream@sparkbox data channel adapted to
// net.Conn.
//
// It is pure protocol. It holds no stores and no policy: the gateway decides
// who may do what before it ever calls in here, and a node re-decides only
// what its own hardware is entitled to. Everything in this package is
// transport-agnostic where it can be — a Conn wants an io.Reader and an
// io.Writer and nothing else, so the whole control-channel suite runs over
// net.Pipe and the SSH session it rides in production is not a special case.
//
// The import DAG is host <- ctlops <- nodelink <- fleet <- sshgw/proxy/xterm.
// nodelink must never import internal/fleet: the gateway half takes plain
// callbacks rather than a fleet type, and that is the whole reason the DAG
// stays acyclic.
package nodelink
