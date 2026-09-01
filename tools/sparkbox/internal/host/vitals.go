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
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/publicports"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"golang.org/x/net/html"
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
	// MemUsedMB is the guest's non-available memory in MiB, from balloon stats;
	// reclaimable cache is available rather than alarming "used" memory.
	MemUsedMB *int64
	// NetRxBytes and NetTxBytes are the raw tap counters in bytes, from the
	// guest's point of view. They reset to zero on every pause, resume and cold
	// boot — see NetCounters — so a caller deriving a rate must treat a reading
	// below the previous one as that reset rather than a negative rate.
	NetRxBytes *uint64
	NetTxBytes *uint64
	// ListeningPorts are the supported public ports that spoke HTTP. Any valid
	// HTTP response counts: a 404 at / still proves that an API is browser-facing.
	// PortServices carries optional names discovered from the same bounded GET.
	// PortsChecked distinguishes an authoritative empty result from a node that
	// could not perform the probe.
	ListeningPorts []int
	PortServices   []PortService
	PortsChecked   bool
	// HiveMind is the agent-activity reading, nil when this machine has never
	// heard from HiveMind about this sandbox.
	//
	// It rides the vitals reply rather than SandboxRow deliberately. The row is
	// broadcast on every lifecycle event to every gateway that cares; this is
	// pulled by one open terminal tab that is actually looking at it. Those are
	// different costs, and only one of them scales with the number of people
	// watching — which is the number that should pay.
	HiveMind *HiveMindLive
}

// HiveMindLive is one machine's current answer to "is an agent working in this
// VM, and on what". It pairs the cheap live reading with the most recent
// session from the catalog, because a person reading a terminal header wants
// both halves and neither is much use alone: a title with no liveness is
// history, and liveness with no title is a light with no label.
type HiveMindLive struct {
	Presence     *HiveMindPresence
	SessionTitle string
	SessionURL   string
}

// PortService is one supported browser-facing listener and the best small,
// untrusted display name its HTTP response supplied. Name is empty when the
// response had no useful title, product header, or media type.
type PortService struct {
	Port int    `json:"port"`
	Name string `json:"name,omitempty"`
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
const portHTTPBackoff = 60 * time.Second
const portProbeBodyLimit = 64 << 10

type portProbeState struct {
	tcpOpen bool
	http    bool
	name    string
	httpAt  time.Time
}

type portScanSample struct {
	hostIP   string
	at       time.Time
	ports    []int
	services []PortService
	states   map[int]portProbeState
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
			out.ListeningPorts = append([]int(nil), ports.ports...)
			out.PortServices = append([]PortService(nil), ports.services...)
			out.PortsChecked = true
		}
	}()
	wg.Wait()
	// Not in the group above: this reads memory rather than a VMM or a socket,
	// so it needs no goroutine of its own. Callers still only see it for a
	// running sandbox — webui.Probe returns the empty reading for any other
	// state before it ever gets here — which is the same rule the counters obey.
	out.HiveMind = m.hivemindLive(name)
	return out, nil
}

// hivemindLive composes the two independently-refreshed halves of the HiveMind
// view into the one reading that crosses a machine boundary.
func (m *Manager) hivemindLive(name string) *HiveMindLive {
	box, ok := m.Get(name)
	if !ok || (box.HiveMindPresence == nil && box.HiveMind == nil) {
		return nil
	}
	live := &HiveMindLive{Presence: box.HiveMindPresence}
	if recent := box.HiveMind.Recent(); recent != nil {
		live.SessionTitle, live.SessionURL = recent.Title, recent.URL
	}
	return live
}

type portScanResult struct {
	ports    []int
	services []PortService
}

func (m *Manager) listeningPorts(ctx context.Context, name, hostIP string) (portScanResult, error) {
	m.portScanMu.Lock()
	if sample, ok := m.portScan[name]; ok && sample.hostIP == hostIP && time.Since(sample.at) < portScanTTL {
		result := portScanResult{
			ports:    append([]int(nil), sample.ports...),
			services: append([]PortService(nil), sample.services...),
		}
		m.portScanMu.Unlock()
		return result, nil
	}
	m.portScanMu.Unlock()

	value, err, _ := m.portScans.Do(name, func() (any, error) {
		m.portScanMu.Lock()
		previous := make(map[int]portProbeState)
		if sample, ok := m.portScan[name]; ok && sample.hostIP == hostIP {
			for port, state := range sample.states {
				previous[port] = state
			}
		}
		m.portScanMu.Unlock()

		services, states, err := probeSupportedPorts(ctx, hostIP, publicports.CommonHTTPS(), previous, time.Now())
		if err != nil {
			return nil, err
		}
		ports := make([]int, len(services))
		for i, service := range services {
			ports[i] = service.Port
		}
		m.portScanMu.Lock()
		if m.portScan == nil {
			m.portScan = make(map[string]portScanSample)
		}
		m.portScan[name] = portScanSample{
			hostIP: hostIP, at: time.Now(),
			ports: append([]int(nil), ports...), services: append([]PortService(nil), services...), states: states,
		}
		m.portScanMu.Unlock()
		return portScanResult{ports: ports, services: services}, nil
	})
	if err != nil {
		return portScanResult{}, err
	}
	result := value.(portScanResult)
	result.ports = append([]int(nil), result.ports...)
	result.services = append([]PortService(nil), result.services...)
	return result, nil
}

func probeSupportedPorts(ctx context.Context, hostIP string, ports []int, previous map[int]portProbeState, now time.Time) ([]PortService, map[int]portProbeState, error) {
	states := make([]portProbeState, len(ports))
	var wg sync.WaitGroup
	wg.Add(len(ports))
	for i, port := range ports {
		i, port := i, port
		go func() {
			defer wg.Done()
			states[i] = probeSupportedPort(ctx, hostIP, port, previous[port], now)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	services := make([]PortService, 0, len(ports))
	next := make(map[int]portProbeState, len(ports))
	for i, port := range ports {
		next[port] = states[i]
		if states[i].http {
			services = append(services, PortService{Port: port, Name: states[i].name})
		}
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Port < services[j].Port })
	return services, next, nil
}

func probeSupportedPort(ctx context.Context, hostIP string, port int, previous portProbeState, now time.Time) portProbeState {
	ctx, cancel := context.WithTimeout(ctx, portProbeTimeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: portProbeTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(hostIP, strconv.Itoa(port)))
	if err != nil {
		return portProbeState{}
	}
	conn.Close() //nolint:errcheck

	// Cheap TCP checks notice a stop immediately without writing application
	// traffic. Refresh HTTP metadata at most once a minute while the same port
	// remains open; observing it closed resets the state, so the next listener
	// receives a fresh HTTP probe immediately.
	if previous.tcpOpen && !previous.httpAt.IsZero() && now.Sub(previous.httpAt) < portHTTPBackoff {
		previous.tcpOpen = true
		return previous
	}

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
	service, ok := probeHTTP(ctx, client, url)
	return portProbeState{tcpOpen: true, http: ok, name: service.Name, httpAt: now}
}

func probeHTTP(ctx context.Context, client *http.Client, url string) (PortService, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PortService{}, false
	}
	req.Header.Set("Accept", "text/html, application/json;q=0.9, */*;q=0.1")
	req.Header.Set("Range", "bytes=0-65535")
	req.Header.Set("User-Agent", "Sparkbox-Port-Probe/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return PortService{}, false
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, portProbeBodyLimit))
	return PortService{Name: serviceName(resp.Header, body)}, true
}

func serviceName(header http.Header, body []byte) string {
	if title := htmlTitle(body); title != "" {
		return title
	}
	for _, key := range []string{"X-Powered-By", "Server"} {
		if value := cleanServiceName(header.Get(key)); value != "" {
			return value
		}
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(header.Get("Content-Type"), ";")[0]))
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		return "JSON API"
	}
	return ""
}

func htmlTitle(body []byte) string {
	tokens := html.NewTokenizer(strings.NewReader(string(body)))
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			tag, _ := tokens.TagName()
			if string(tag) != "title" {
				continue
			}
			if tokens.Next() == html.TextToken {
				return cleanServiceName(string(tokens.Text()))
			}
		}
	}
}

func cleanServiceName(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 80 {
		value = string(runes[:80])
	}
	return value
}
