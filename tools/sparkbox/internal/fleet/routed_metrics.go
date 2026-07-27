package fleet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routedguest"
)

type routedMetricsDialer struct {
	node    string
	next    GuestDialer
	metrics *fleetmetrics.Registry
}

func (d *routedMetricsDialer) DialGuest(
	ctx context.Context,
	sandbox, kind string,
	port int,
) (connection net.Conn, err error) {
	started := time.Now()
	metricKind := routedMetricKind(kind)
	defer func() {
		d.metrics.ObserveStreamOpen(d.node, "routed", metricKind, routedMetricOutcome(err), time.Since(started))
	}()
	connection, err = d.next.DialGuest(ctx, sandbox, kind, port)
	if err != nil {
		return nil, err
	}
	d.metrics.AddLiveStreams(d.node, "routed", metricKind, 1)
	return &routedMetricsConn{
		Conn: connection, node: d.node, kind: metricKind, metrics: d.metrics,
	}, nil
}

type routedMetricsConn struct {
	net.Conn
	node    string
	kind    string
	metrics *fleetmetrics.Registry
	once    sync.Once
}

func (c *routedMetricsConn) Read(buffer []byte) (int, error) {
	n, err := c.Conn.Read(buffer)
	c.metrics.AddStreamBytes(c.node, "routed", c.kind, "from_guest", n)
	return n, err
}

func (c *routedMetricsConn) Write(buffer []byte) (int, error) {
	n, err := c.Conn.Write(buffer)
	c.metrics.AddStreamBytes(c.node, "routed", c.kind, "to_guest", n)
	return n, err
}

// CloseWrite preserves TCP half-close through both the routed metrics wrapper
// and Fleet's outer tracked wrapper. Environment sync and SSH command streams
// use EOF as a protocol signal; turning this into a full close would truncate
// the response half of the connection.
func (c *routedMetricsConn) CloseWrite() error {
	closeWriter, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return fmt.Errorf("fleet: %T cannot half-close", c.Conn)
	}
	return closeWriter.CloseWrite()
}

func (c *routedMetricsConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.metrics.AddLiveStreams(c.node, "routed", c.kind, -1)
	})
	return err
}

func routedMetricKind(kind string) string {
	switch kind {
	case nodelink.StreamSSH, nodelink.StreamTCP:
		return kind
	default:
		return "unknown"
	}
}

func routedMetricOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, routedguest.ErrRouteUnavailable):
		return "route_failure"
	case errors.Is(err, routedguest.ErrInvalidPrefix),
		errors.Is(err, routedguest.ErrMalformedHostIP),
		errors.Is(err, routedguest.ErrOutOfPrefix),
		errors.Is(err, routedguest.ErrInvalidPort),
		errors.Is(err, routedguest.ErrUnsupportedKind):
		return "rejected"
	default:
		return "error"
	}
}
