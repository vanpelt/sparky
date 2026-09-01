package host

// One reading of a running sandbox's live resource counters, gathered in one
// call.
//
// The three readers below it — CPUSeconds, MemStats, NetCounters — stay exactly
// as they were and are still the honest shape for a caller that wants one
// number. This exists because every surface that draws a meter wants all three
// at once, and because the fleet made "all three at once" the unit that has to
// cross a machine boundary: a balloon and a VMM process can only be asked of the
// host running them, so a sandbox on another node is asked over the link, and
// three round trips a second per open terminal tab is not a wire protocol, it is
// a mistake with a schema.

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/publicports"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Vitals is what one machine can currently say about one sandbox.
//
// Every field is a pointer because a missing reading and a genuine zero are
// different facts and every surface draws them differently — a guest that has
// used no CPU since it booted shows an idle meter, a guest whose driver has no
// CPU stats shows no meter at all. A zero value of this struct is therefore the
// correct answer for a paused sandbox, a sandbox on an unreachable machine, and
// a build with no readers wired: nobody has said.
type Vitals struct {
	// CPUSeconds is cumulative host CPU time of the VM process, in seconds.
	CPUSeconds *float64
	// MemUsedMB is the guest's real memory use in MiB, from balloon statistics.
	MemUsedMB *int64
	// NetRxBytes and NetTxBytes are the raw tap counters in bytes, from the
	// guest's point of view. They reset to zero on every pause, resume and cold
	// boot — see NetCounters — so a caller deriving a rate must treat a reading
	// below the previous one as that reset rather than a negative rate.
	NetRxBytes *uint64
	NetTxBytes *uint64
	// ListeningPorts are the supported public HTTP ports that answered HEAD (or
	// a GET fallback) with success or redirect status. PortsChecked distinguishes
	// an authoritative empty result from a node that could not perform the probe.
	ListeningPorts []int
	PortsChecked   bool
}

// Empty reports whether this reading carries nothing at all. It is what tells
// "the machine answered and has no counters for that sandbox" apart from "the
// machine answered with numbers", which are rendered identically but logged
// differently.
func (v Vitals) Empty() bool {
	return v.CPUSeconds == nil && v.MemUsedMB == nil && v.NetRxBytes == nil && v.NetTxBytes == nil && !v.PortsChecked
}

const portScanTTL = 3 * time.Second
const portProbeTimeout = 250 * time.Millisecond

type portScanSample struct {
	hostIP string
	at     time.Time
	ports  []int
}

// Vitals reads all three counters for one sandbox on THIS machine.
//
// The reads run concurrently under the caller's context because they touch
// different things — /proc for CPU, sysfs for the tap, the VMM's API socket for
// the balloon — and only the last of those can be slow. Sequentially, one wedged
// VMM would cost a caller its CPU number too, and at a reading a second the
// latency would compound into a visibly stuttering chart.
//
// The error is always nil here and exists for the interface's sake: the same
// method on a sandbox that lives on another machine can fail at the link, and a
// caller that has to switch on where a sandbox is placed is a caller the fleet
// has failed to hide anything from. A sandbox this manager does not hold, or
// does not have running, is not an error — it is the zero reading.
func (m *Manager) Vitals(ctx context.Context, name string) (Vitals, error) {
	var (
		out Vitals
		wg  sync.WaitGroup
	)
	wg.Add(4)
	go func() {
		defer wg.Done()
		if secs, ok := m.CPUSeconds(ctx, name); ok {
			out.CPUSeconds = &secs
		}
	}()
	go func() {
		defer wg.Done()
		if used, ok := m.MemStats(ctx, name); ok {
			out.MemUsedMB = &used
		}
	}()
	go func() {
		defer wg.Done()
		if rx, tx, ok := m.NetCounters(ctx, name); ok {
			out.NetRxBytes, out.NetTxBytes = &rx, &tx
		}
	}()
	go func() {
		defer wg.Done()
		box, ok := m.Get(name)
		if !ok || box.State != vmm.StateRunning || box.HostIP == "" {
			return
		}
		ports, err := m.listeningPorts(ctx, name, box.HostIP)
		if err == nil {
			out.ListeningPorts = ports
			out.PortsChecked = true
		}
	}()
	wg.Wait()
	return out, nil
}

func (m *Manager) listeningPorts(ctx context.Context, name, hostIP string) ([]int, error) {
	m.portScanMu.Lock()
	if sample, ok := m.portScan[name]; ok && sample.hostIP == hostIP && time.Since(sample.at) < portScanTTL {
		ports := append([]int(nil), sample.ports...)
		m.portScanMu.Unlock()
		return ports, nil
	}
	m.portScanMu.Unlock()

	value, err, _ := m.portScans.Do(name, func() (any, error) {
		ports, err := probeHTTPPorts(ctx, hostIP, publicports.CommonHTTPS())
		if err != nil {
			return nil, err
		}
		m.portScanMu.Lock()
		if m.portScan == nil {
			m.portScan = make(map[string]portScanSample)
		}
		m.portScan[name] = portScanSample{hostIP: hostIP, at: time.Now(), ports: append([]int(nil), ports...)}
		m.portScanMu.Unlock()
		return ports, nil
	})
	if err != nil {
		return nil, err
	}
	return append([]int(nil), value.([]int)...), nil
}

func probeHTTPPorts(ctx context.Context, hostIP string, ports []int) ([]int, error) {
	ready := make([]bool, len(ports))
	var wg sync.WaitGroup
	wg.Add(len(ports))
	for i, port := range ports {
		i, port := i, port
		go func() {
			defer wg.Done()
			ready[i] = probeHTTPPort(ctx, hostIP, port)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	listening := make([]int, 0, len(ports))
	for i, port := range ports {
		if ready[i] {
			listening = append(listening, port)
		}
	}
	return listening, nil
}

func probeHTTPPort(ctx context.Context, hostIP string, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, portProbeTimeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: portProbeTimeout}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: portProbeTimeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	url := "http://" + net.JoinHostPort(hostIP, strconv.Itoa(port)) + "/"
	status, ok := probeHTTPMethod(ctx, client, http.MethodHead, url)
	if ok && (status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented) {
		status, ok = probeHTTPMethod(ctx, client, http.MethodGet, url)
	}
	return ok && status >= http.StatusOK && status < http.StatusBadRequest
}

func probeHTTPMethod(ctx context.Context, client *http.Client, method, url string) (int, bool) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	resp.Body.Close() //nolint:errcheck
	return resp.StatusCode, true
}
