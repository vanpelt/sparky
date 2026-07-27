package nodelink

// The dedicated SSH data-lane handshake and the gateway-side lane pool.
//
// A data lane is useful only while the control link which minted its
// generation is alive. The generation is deliberately opaque and short lived:
// it is an attachment token, not an identity. SSH public-key authentication
// still supplies identity at the node door.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

// DataHello registers one independently supervised SSH connection with the
// live control generation for Node.
type DataHello struct {
	Protocol   int    `json:"protocol"`
	Node       string `json:"node"`
	Generation string `json:"generation"`
	Lane       string `json:"lane"`
}

// DataWelcome is the bounded acknowledgement for a data-lane registration.
// Error is display-only; callers branch on Accepted.
type DataWelcome struct {
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

// DataServerOptions is the fleet joiner's half of an authenticated data-lane
// session. Node is the roster name, never a node-authored identity.
type DataServerOptions struct {
	Node    string
	Hello   DataHello
	Session io.Writer
	Conn    xssh.Conn
	Log     *slog.Logger
}

func hasCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}

func negotiatedCapabilities(node, gateway []string) []string {
	selected := make([]string, 0, min(len(node), len(gateway)))
	for _, capability := range gateway {
		if hasCapability(node, capability) && !hasCapability(selected, capability) {
			selected = append(selected, capability)
		}
	}
	return selected
}

// SupportsDataPool reports whether a welcome selected the separate SSH data
// pool. Requiring both the capability and a generation prevents a malformed
// welcome from silently disabling the combined-link fallback.
func (w Welcome) SupportsDataPool() bool {
	return hasCapability(w.Capabilities, CapabilitySSHDataPoolV1) &&
		w.ControlGeneration != ""
}

func newControlGeneration() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("nodelink: create control generation: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// ReadDataHello reads the one-line registration sent immediately after
// DataCommand starts.
func ReadDataHello(ctx context.Context, r io.Reader, within time.Duration) (DataHello, error) {
	type result struct {
		hello DataHello
		err   error
	}
	done := make(chan result, 1)
	go func() {
		var hello DataHello
		err := json.NewDecoder(io.LimitReader(r, MaxFrameBytes)).Decode(&hello)
		done <- result{hello: hello, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			return DataHello{}, fmt.Errorf("nodelink: read data hello: %w", got.err)
		}
		if got.hello.Protocol != Protocol {
			return DataHello{}, fmt.Errorf("nodelink: data protocol %d is unsupported", got.hello.Protocol)
		}
		if got.hello.Node == "" || got.hello.Generation == "" || got.hello.Lane == "" {
			return DataHello{}, errors.New("nodelink: incomplete data hello")
		}
		if len(got.hello.Node) > 128 || len(got.hello.Generation) > 128 || len(got.hello.Lane) > 64 {
			return DataHello{}, errors.New("nodelink: data hello fields exceed their limits")
		}
		return got.hello, nil
	case <-time.After(within):
		return DataHello{}, fmt.Errorf("nodelink: no data hello within %s", within)
	case <-ctx.Done():
		return DataHello{}, ctx.Err()
	}
}

func writeDataWelcome(w io.Writer, accepted bool, err error) error {
	reply := DataWelcome{Accepted: accepted}
	if err != nil {
		reply.Error = err.Error()
	}
	return json.NewEncoder(w).Encode(reply)
}

// RefuseDataLane sends the registration-shaped refusal used when the fleet
// cannot resolve a live control link for an otherwise authenticated session.
func RefuseDataLane(w io.Writer, err error) error {
	return writeDataWelcome(w, false, err)
}

// AttachDataLane registers a connection with this client's live control
// generation. It returns a detach function which only removes this exact lane;
// a replacement with the same lane ID is not removed by the old session
// unwinding.
func (c *Client) AttachDataLane(hello DataHello, conn xssh.Conn) (func(), error) {
	if conn == nil {
		return nil, errors.New("nodelink: data lane has no SSH connection")
	}
	c.smu.Lock()
	if c.dead != nil {
		reason := c.dead
		c.smu.Unlock()
		return nil, reason
	}
	if !c.split || hello.Generation != c.generation {
		c.smu.Unlock()
		return nil, errors.New("nodelink: stale or unknown control generation")
	}
	lane := &dataLane{id: hello.Lane, conn: conn}
	old := c.lanes[hello.Lane]
	c.lanes[hello.Lane] = lane
	c.smu.Unlock()
	if old != nil {
		_ = old.conn.Close()
	}
	c.log.Info("nodelink: data lane attached", "lane", hello.Lane)

	var once sync.Once
	return func() {
		once.Do(func() { c.detachDataLane(lane, errors.New("nodelink: data lane closed")) })
	}, nil
}

// ServeDataLane acknowledges an authenticated registration and owns it until
// the SSH session ends. Losing this context removes only the lane: control
// liveness and node online state are deliberately untouched.
func (c *Client) ServeDataLane(ctx context.Context, opts DataServerOptions) error {
	detach, err := c.AttachDataLane(opts.Hello, opts.Conn)
	if err != nil {
		_ = writeDataWelcome(opts.Session, false, err)
		return err
	}
	if err := writeDataWelcome(opts.Session, true, nil); err != nil {
		detach()
		return err
	}
	defer detach()
	<-ctx.Done()
	return nil
}

// ControlGeneration exposes the opaque token only to the fleet adapter which
// binds authenticated data sessions to the current remote node.
func (c *Client) ControlGeneration() string { return c.generation }

type dataLane struct {
	id     string
	conn   xssh.Conn
	active int
}

func (c *Client) chooseDataLane() *dataLane {
	var best *dataLane
	for _, lane := range c.lanes {
		if best == nil || lane.active < best.active ||
			(lane.active == best.active && lane.id < best.id) {
			best = lane
		}
	}
	if best != nil {
		best.active++
	}
	return best
}

func (c *Client) detachDataLane(lane *dataLane, reason error) {
	c.smu.Lock()
	if c.lanes[lane.id] != lane {
		c.smu.Unlock()
		return
	}
	delete(c.lanes, lane.id)
	var live []*streamConn
	for stream, owner := range c.streams {
		if owner == lane {
			delete(c.streams, stream)
			live = append(live, stream)
			c.metrics.AddLiveStreams(c.node, "ssh-data", metricStreamKind(stream.kind), -1)
		}
	}
	c.smu.Unlock()
	for _, stream := range live {
		stream.fail(reason)
	}
	_ = lane.conn.Close()
	c.log.Info("nodelink: data lane detached", "lane", lane.id, "streams", len(live))
}
