//go:build !linux

// The eBPF data plane is Linux-only. On other platforms Load fails cleanly so
// the rest of the tool (allowlist, DNS proxy) still builds and the CLI can give
// a useful error instead of a link failure.
package meter

import (
	"errors"
	"net/netip"

	"github.com/vanpelt/sparky/tools/sluice/internal/report"
)

// ErrUnsupported is returned by Load off Linux.
var ErrUnsupported = errors.New("meter: eBPF data plane requires Linux")

// Meter is a non-functional placeholder on non-Linux platforms.
type Meter struct{}

// Load always fails off Linux.
func Load() (*Meter, error) { return nil, ErrUnsupported }

func (m *Meter) Attach(string) error                        { return ErrUnsupported }
func (m *Meter) Detach(string)                              {}
func (m *Meter) Attached(string) bool                       { return false }
func (m *Meter) Flows() (map[netip.Addr]report.Flow, error) { return nil, ErrUnsupported }
func (m *Meter) SetEnforce(bool) error                      { return ErrUnsupported }
func (m *Meter) SyncAllowed([]netip.Addr) error             { return ErrUnsupported }
func (m *Meter) Close() error                               { return nil }
