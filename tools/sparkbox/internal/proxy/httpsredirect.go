package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// sniffTimeout bounds how long we wait for a new connection's opening byte
// before abandoning it. A real client — TLS or HTTP — sends its first bytes
// immediately; a connection that stalls here is dropped so it can't tie up the
// classifier goroutine indefinitely.
const sniffTimeout = 10 * time.Second

// httpsRedirectListener fronts a TLS-terminating http.Server and peeks the first
// byte of every accepted connection. A TLS record starts with 0x16 (the
// handshake content type), so those connections are surfaced through Accept
// untouched for the server to handshake as usual. Anything else is a client
// that spoke cleartext HTTP to the HTTPS port: instead of letting the TLS stack
// answer with its bare "Client sent an HTTP request to an HTTPS server.", we
// read the request and reply with a 308 to the https:// URL for the same host,
// port, and path — so someone who types myvm.hivemind.tools:4444 (or whose
// browser hasn't learned to upgrade the scheme yet) lands on the working page.
type httpsRedirectListener struct {
	net.Listener
	log *slog.Logger

	conns     chan net.Conn // TLS connections, drained by Accept
	acceptErr chan error    // a terminal error from the underlying Accept
	closeCh   chan struct{}
	closeOnce sync.Once
}

// RedirectPlainHTTP wraps ln so cleartext-HTTP connections reaching the TLS edge
// are answered with a redirect to their https:// equivalent rather than a TLS
// handshake error. Pass the result to http.Server.ServeTLS. Only TLS
// connections are ever returned from Accept.
func RedirectPlainHTTP(ln net.Listener, log *slog.Logger) net.Listener {
	l := &httpsRedirectListener{
		Listener:  ln,
		log:       log,
		conns:     make(chan net.Conn),
		acceptErr: make(chan error, 1),
		closeCh:   make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

// acceptLoop pulls raw connections off the underlying listener and classifies
// each in its own goroutine, so a slow client can't hold up accepting the next.
func (l *httpsRedirectListener) acceptLoop() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			select {
			case l.acceptErr <- err:
			case <-l.closeCh:
			}
			return
		}
		go l.classify(c)
	}
}

// Accept returns the next TLS connection. Cleartext-HTTP connections never
// surface here — classify redirects and closes them.
func (l *httpsRedirectListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.acceptErr:
		return nil, err
	case <-l.closeCh:
		return nil, net.ErrClosed
	}
}

func (l *httpsRedirectListener) Close() error {
	l.closeOnce.Do(func() { close(l.closeCh) })
	return l.Listener.Close()
}

// classify peeks the first byte to decide whether a connection is TLS (handed
// on) or cleartext HTTP (redirected).
func (l *httpsRedirectListener) classify(c net.Conn) {
	if err := c.SetReadDeadline(time.Now().Add(sniffTimeout)); err != nil {
		c.Close()
		return
	}
	var first [1]byte
	if _, err := io.ReadFull(c, first[:]); err != nil {
		c.Close()
		return
	}
	// Clear the classify deadline; the handshake / redirect set their own.
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		c.Close()
		return
	}
	// Replay the byte we consumed so the downstream reader sees a whole stream.
	pc := &prefixConn{Conn: c, prefix: first[:]}
	if first[0] == 0x16 { // TLS handshake record
		select {
		case l.conns <- pc:
		case <-l.closeCh:
			pc.Close()
		}
		return
	}
	l.redirect(pc)
}

// redirect answers a single cleartext HTTP request with a 308 to its https://
// equivalent, then closes the connection. 308 (not 301) preserves the method
// and body, so a non-GET request survives the upgrade; it is permanent so a
// browser remembers the scheme for this host:port.
func (l *httpsRedirectListener) redirect(c net.Conn) {
	defer c.Close()
	if err := c.SetReadDeadline(time.Now().Add(sniffTimeout)); err != nil {
		return
	}
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		return // not a request we can turn into a redirect; just drop it
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if host == "" {
		return // no authority to build an absolute https URL from
	}
	// host and RequestURI come from a parsed request, so neither can carry a
	// CR/LF to smuggle into the Location header.
	loc := "https://" + host + req.URL.RequestURI()
	body := "Redirecting to " + loc + "\n"
	fmt.Fprintf(c, "HTTP/1.1 308 Permanent Redirect\r\n"+
		"Location: %s\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: close\r\n"+
		"\r\n%s", loc, len(body), body)
}

// prefixConn replays prefix before reading from the wrapped connection, handing
// back the byte(s) consumed while classifying so the TLS handshake (or the
// redirect's request parse) sees an intact stream.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}
