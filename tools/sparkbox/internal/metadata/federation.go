package metadata

import (
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/federation"
)

// federation serves the fleet's list of relying parties to the calling
// sandbox, in the guest-facing encoding federation.Config.Guest documents: one
// `<name> TAB <key> TAB <value>` line per fact, which is what lets the token
// unit walk it with grep and cut rather than a JSON parser it does not have.
//
// The list is fleet configuration and it is SERVED rather than baked into a
// rootfs template for the same reason the identifiers in it are not secrets:
// correcting a provider id, or adding a party, must not mean rebuilding an
// image, and a running sandbox picks the change up on its next 45-minute
// refresh — including a fork template that crossed fleets, whose stale list is
// replaced by this fleet's on its first boot here.
//
// caller() runs first for the reason every other endpoint runs it: not because
// the list is secret — it is not — but because answering anything to a source
// this service cannot name as a sandbox is how the tap-position authentication
// gets eroded one convenience at a time.
//
// An empty list is served as an empty body, and the guest then mints nothing.
// That is an operator's explicit choice (a Config with no federators), never a
// default: main.go substitutes federation.Default when no file was given.
func (s *Server) serveFederation(w http.ResponseWriter, r *http.Request) {
	if _, err := s.caller(r); err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/tab-separated-values; charset=utf-8")
	// Not cacheable: the whole point of serving it is that the next refresh
	// sees the corrected list.
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(s.feds.Guest())) //nolint:errcheck
}

// Federation is the list this service serves, for tests and for the host's
// own log line.
func (s *Server) Federation() federation.Config { return s.feds }
