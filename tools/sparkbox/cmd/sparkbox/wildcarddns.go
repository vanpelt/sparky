package main

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/frontdoor"
)

// wildcardPublishTimeout bounds the one DNS round-trip. Generous, because it
// runs in the background where nothing waits on it, and stingy enough that a
// wedged API call cannot outlive a short-lived process.
const wildcardPublishTimeout = 60 * time.Second

// publishXtermWildcard makes <name>.<label>.<domain> resolve, by publishing one
// wildcard A/AAAA at the shared proxy edge. It is the DNS half of the browser
// terminal; the TLS half is the second wildcard in wildcardTLSConfig.
//
// Everything about it is best-effort and non-fatal: DNS the operator already
// wrote by hand is the common case, and a host that serves terminals only over
// the tailnet (where the built-in responder answers the whole subtree) needs no
// record at all. Whenever it declines, it logs the name an operator would have
// to publish themselves, so the answer to "why is my terminal NXDOMAIN" is in
// the startup log rather than in this source file.
//
// It returns immediately: a slow or unreachable Cloudflare API must not delay
// the edge coming up, and nothing later in startup depends on the record.
func publishXtermWildcard(ctx context.Context, domain, label string, edge []netip.Addr, log *slog.Logger) {
	if label == "" {
		return // browser terminals are off
	}
	name := "*." + label + "." + domain
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	switch {
	case token == "":
		// The tunnel deployment lands here on purpose — it withholds the token
		// so per-name records cannot shadow its proxied wildcard CNAME — and so
		// does any zone managed elsewhere. Both want the same manual record.
		log.Info("browser-terminal wildcard DNS not published", "name", name,
			"reason", "no CLOUDFLARE_API_TOKEN",
			"note", "publish it yourself: an A/AAAA at the edge, or `cloudflared tunnel route dns <tunnel> "+name+"`")
		return
	case len(edge) == 0:
		log.Warn("browser-terminal wildcard DNS not published", "name", name,
			"reason", "no edge address (pass --edge-v4)")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(ctx, wildcardPublishTimeout)
		defer cancel()
		if err := frontdoor.PublishWildcard(ctx, domain, label, token, edge, log); err != nil {
			log.Warn("browser-terminal wildcard DNS publish failed", "name", name, "err", err)
		}
	}()
}
