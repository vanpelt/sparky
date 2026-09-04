// Per-port visibility.
//
// A route row owns one subdomain and the guest port its portless URL forwards
// to. Every OTHER port the same hostname can reach — https://<sub>.<domain>:5173,
// which the edge serves with no row of its own — gets its visibility from here.
//
// The split keeps exactly one source of truth per port. The route's own port is
// answered by routes.visibility, unchanged and where it has always been; any
// other port is answered by a route_ports row, and by private when it has none.
// Upsert deletes the row for a port it promotes to default, so the two can
// never both hold an opinion about the same port.

package routes

import (
	"database/sql"
	"fmt"
	"time"
)

// PortRoute is one explicitly configured non-default port under a subdomain.
//
// A row exists for two different reasons and the store deliberately cannot
// tell them apart: the owner made the port public, or the owner pinned it
// while still private so it keeps its place on the console's port strip when
// nothing is listening on it. Both mean "I have an opinion about this port",
// which is all the edge and the UI need.
type PortRoute struct {
	Subdomain  string    `json:"subdomain"`
	Port       int       `json:"port"`
	Visibility string    `json:"visibility"`
	CreatedAt  time.Time `json:"created_at"`
}

// VisibilityForPort answers who may reach one guest port of an already-loaded
// route. It takes the Route rather than a subdomain because the caller on the
// request path has just read it, and the route's own port — by far the common
// case — is then answered with no query at all.
//
// A port with no row is PRIVATE, not "whatever the default port is". That is
// the whole point of the split: making a preview public should expose the one
// port the owner meant, never the debugger listening beside it.
func (s *Store) VisibilityForPort(r Route, port int) (string, error) {
	if port == r.Port {
		return r.Visibility, nil
	}
	var vis string
	err := s.db.QueryRow(
		`SELECT visibility FROM route_ports WHERE subdomain = ? AND port = ?`,
		r.Subdomain, port).Scan(&vis)
	if err == sql.ErrNoRows {
		return VisibilityPrivate, nil
	}
	if err != nil {
		return "", err
	}
	return vis, nil
}

// ListPorts returns the explicit non-default port rows for a subdomain, in port
// order. The route's own port is never among them — it lives on the Route.
func (s *Store) ListPorts(subdomain string) ([]PortRoute, error) {
	return s.queryPorts(
		`SELECT subdomain, port, visibility, created_at FROM route_ports WHERE subdomain = ? ORDER BY port`,
		subdomain)
}

// ListPortsBySandbox returns every explicit port row across all of a sandbox's
// subdomains, in (subdomain, port) order.
func (s *Store) ListPortsBySandbox(sandbox string) ([]PortRoute, error) {
	return s.queryPorts(`
		SELECT p.subdomain, p.port, p.visibility, p.created_at
		FROM route_ports p JOIN routes r ON r.subdomain = p.subdomain
		WHERE r.sandbox = ? ORDER BY p.subdomain, p.port`, sandbox)
}

func (s *Store) queryPorts(q string, args ...any) ([]PortRoute, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortRoute
	for rows.Next() {
		var p PortRoute
		if err := rows.Scan(&p.Subdomain, &p.Port, &p.Visibility, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPortVisibility records an opinion about one port of one subdomain.
//
// The route's own port writes through to routes.visibility, so Route.Visibility
// stays the answer for the portless URL and nothing that reads it has to learn
// about this table. Any other port gets a row here.
//
// Setting a port private is not the same as having no opinion about it: the row
// is still written, and that is what pins the port to the console's strip so it
// can be pre-authorised before anything listens on it. ForgetPort is the way
// back to nothing.
func (s *Store) SetPortVisibility(subdomain string, port int, visibility string) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid port %d", port)
	}
	if !ValidVisibility(visibility) {
		return fmt.Errorf("invalid visibility %q", visibility)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var routePort int
	err = tx.QueryRow(`SELECT port FROM routes WHERE subdomain = ?`, subdomain).Scan(&routePort)
	if err == sql.ErrNoRows {
		return ErrNoSuchRoute
	}
	if err != nil {
		return err
	}
	if port == routePort {
		if _, err := tx.Exec(`UPDATE routes SET visibility = ? WHERE subdomain = ?`, visibility, subdomain); err != nil {
			return err
		}
		// Belt and braces on the one-opinion-per-port invariant: a row here
		// would be shadowed for as long as this port stays default and would
		// then resurface, stale, the moment it stopped being.
		if _, err := tx.Exec(`DELETE FROM route_ports WHERE subdomain = ? AND port = ?`, subdomain, port); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.Exec(`
		INSERT INTO route_ports (subdomain, port, visibility, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(subdomain, port) DO UPDATE SET visibility = excluded.visibility`,
		subdomain, port, visibility, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// ForgetPort drops the explicit row for a non-default port: it stops being
// pinned to the strip and goes back to private-because-nobody-said. Forgetting
// a port that has no row, or a subdomain's own default port (whose visibility
// is the route's and cannot be un-said), succeeds and changes nothing.
func (s *Store) ForgetPort(subdomain string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM route_ports WHERE subdomain = ? AND port = ?`, subdomain, port)
	return err
}

// PrivatizeAll makes every port of a subdomain private in one go: the route's
// own, and every port anyone has ever said anything about.
//
// The port rows are updated rather than deleted, so a panic-button `share …
// private` does not also silently unpin the strip the owner curated. It returns
// how many ports it changed the answer for, the route's own included.
func (s *Store) PrivatizeAll(subdomain string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec(`UPDATE routes SET visibility = ? WHERE subdomain = ?`, VisibilityPrivate, subdomain)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNoSuchRoute
	}
	changed := 1
	res, err = tx.Exec(
		`UPDATE route_ports SET visibility = ? WHERE subdomain = ? AND visibility != ?`,
		VisibilityPrivate, subdomain, VisibilityPrivate)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	changed += int(n)
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}
