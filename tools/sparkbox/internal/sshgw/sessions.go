package sshgw

// `ssh ctl@<gateway> sessions <name>` — the agent sessions HiveMind holds for
// one sandbox. Parse, call ctlops, format; the ownership check and the query
// itself are ctlops'.

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	gssh "github.com/gliderlabs/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// sessionTitleWidth keeps the table readable in an 80-column terminal once the
// state and age columns have taken their share.
const sessionTitleWidth = 44

func (g *Gateway) controlSessions(
	s gssh.Session,
	c ctlops.Caller,
	args []string,
	log *slog.Logger,
) {
	if len(args) < 2 {
		fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> sessions <name>\r\n", ControlUser)
		s.Exit(2) //nolint:errcheck
		return
	}
	snapshot, err := g.ops.Sessions(s.Context(), c, args[1], 0)
	if err != nil {
		failCtl(s, log, "sessions", err)
		return
	}
	if len(snapshot.Sessions) == 0 {
		// The distinction matters to whoever is debugging: an empty answer from
		// a VM that has never run an agent is expected, and an empty answer from
		// one that has is a sync problem. Neither is visible from a bare "none",
		// so name the one thing we do know — that the question reached HiveMind
		// and it had nothing under this sandbox's identity.
		fmt.Fprintf(s, "no HiveMind sessions recorded from %s\r\n", args[1])
		fmt.Fprint(s, "  an agent run here syncs within a minute or so; "+
			"check the daemon with: systemctl --user status hivemind\r\n")
		s.Exit(0) //nolint:errcheck
		return
	}

	now := time.Now()
	for _, sess := range snapshot.Sessions {
		fmt.Fprintf(s, "%-*s  %-7s  %s\r\n",
			sessionTitleWidth, truncateTitle(sess, sessionTitleWidth),
			sess.State, sessionAge(sess, now))
		fmt.Fprintf(s, "  %s\r\n", sessionDetail(sess))
		if sess.URL != "" {
			fmt.Fprintf(s, "  %s\r\n", sess.URL)
		}
	}
	fmt.Fprintf(s, "\r\n%s\r\n", sessionFooter(args[1], snapshot))
	s.Exit(0) //nolint:errcheck
}

// truncateTitle renders a session's display name, falling back to its ID for
// one that never got a title.
func truncateTitle(sess host.HiveMindSession, width int) string {
	title := strings.TrimSpace(sess.Title)
	if title == "" {
		title = sess.ID
	}
	title = ctlops.SafeText(title, width)
	if len([]rune(title)) > width {
		return string([]rune(title)[:width-1]) + "…"
	}
	return title
}

func sessionDetail(sess host.HiveMindSession) string {
	parts := make([]string, 0, 2)
	if agent := ctlops.SafeText(sess.AgentType, 32); agent != "" {
		parts = append(parts, agent)
	}
	if model := ctlops.SafeText(sess.Model, 48); model != "" {
		parts = append(parts, model)
	}
	if len(parts) == 0 {
		return sess.ID
	}
	return strings.Join(parts, " · ")
}

// sessionAge is how long ago this session was last active, as a person would
// say it. A zero timestamp prints nothing rather than "55 years ago".
func sessionAge(sess host.HiveMindSession, now time.Time) string {
	at := sess.LastActivityAt
	if at.IsZero() {
		at = sess.StartedAt
	}
	if at.IsZero() {
		return ""
	}
	d := now.Sub(at)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// sessionFooter states the count, and says so honestly when the page is only
// part of it — a total that silently meant "the first 50" would be worse than
// no total at all.
func sessionFooter(name string, snapshot host.HiveMindSessionSnapshot) string {
	shown := len(snapshot.Sessions)
	total := snapshot.TotalCount
	if total < shown {
		total = shown
	}
	if snapshot.HasMore || shown < total {
		return fmt.Sprintf("%d of %d sessions from %s (most recent first)", shown, total, name)
	}
	if total == 1 {
		return fmt.Sprintf("1 session from %s", name)
	}
	return fmt.Sprintf("%d sessions from %s", total, name)
}
