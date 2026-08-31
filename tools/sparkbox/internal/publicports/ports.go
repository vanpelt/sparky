// Package publicports owns the HTTPS ports Sparkbox intentionally exposes at
// its public edge. Keep provider manifests synchronized with CommonHTTPS; their
// contract tests import this package so a change cannot silently reach only one
// deployment surface.
package publicports

import (
	"strconv"
	"strings"
)

// commonHTTPS is the source of truth for the non-default public ports intended
// for browser-facing development servers. Port 443 is not included: it is the
// portless default endpoint. Nor are database ports: Sparkbox terminates TLS
// and forwards HTTP; it is not a generic TCP proxy.
var commonHTTPS = [...]int{
	3000, 3001, 4000, 4200, 5000, 5173, 6006,
	7860, 8000, 8080, 8443, 8501, 8888, 9000,
}

// CommonHTTPS returns a copy so callers cannot mutate the process-wide source
// of truth while building a probe list or response.
func CommonHTTPS() []int {
	ports := make([]int, len(commonHTTPS))
	copy(ports, commonHTTPS[:])
	return ports
}

// HumanList formats CommonHTTPS for prose embedded in guest documentation and
// agent guidance.
func HumanList() string {
	parts := make([]string, len(commonHTTPS))
	for i, port := range commonHTTPS {
		parts[i] = strconv.Itoa(port)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
