#!/usr/bin/env python3
"""Preview the user console (internal/userconsole/index.html) locally with mock data.

The real console talks to a live sparkbox edge (/api/*, edge-session auth), so
opening index.html on its own just shows the sign-in screen. This server serves
the *real* index.html with a mock `fetch` injected, so you see the full UI —
machines in every state, routes, tags, secrets, snapshots — with no backend.

The file is re-read on every request, so while you edit index.html you just
refresh the browser to see changes. No dependencies (stdlib only).

    python3 hack/preview-console.py            # -> http://localhost:8799
    python3 hack/preview-console.py 9000        # pick a port

Add ?theme=dark or ?theme=light to force a theme (default follows the OS).
"""
import http.server
import json
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
INDEX = os.path.join(HERE, "..", "internal", "userconsole", "index.html")
SHARED_CSS = os.path.join(HERE, "..", "internal", "webui", "shared.css")
SHARED_JS = os.path.join(HERE, "..", "internal", "webui", "shared.js")

# ---- mock fleet: one machine per state, plus routes/tags/secrets/snapshots ----
def _iso(sec_ago):
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - sec_ago))

def _machines(tick):
    return [
        {"name": "dazzling-canyon", "state": "paused", "vcpus": 2,
         "mem_mb": 8192, "mem_used_mb": None, "disk_mb": 614, "disk_total_mb": 25600,
         "cpu_seconds": None, "image": "universal", "last_active": _iso(600),
         "net_rx_bytes": 48213, "net_tx_bytes": 9004,
         "pinned": False, "env_undecryptable": False, "tags": [],
         "routes": [
             {"subdomain": "app", "port": 4444, "visibility": "public", "listening": False},
             {"subdomain": "dazzling-canyon", "port": 8000, "visibility": "public", "listening": False},
         ]},
        {"name": "brave-meadow", "state": "running", "vcpus": 4,
         "mem_mb": 16384, "mem_used_mb": 6820, "disk_mb": 12040, "disk_total_mb": 25600,
         "cpu_seconds": 1200 + tick * 3.1, "image": "universal", "last_active": _iso(4),
         "net_rx_bytes": 4923847112, "net_tx_bytes": 812394002,
         "pinned": True, "env_undecryptable": False, "tags": ["ml", "prod"],
         "routes": [
             {"subdomain": "brave-meadow", "port": 8080, "visibility": "private", "listening": True},
             {"subdomain": "api", "port": 3000, "visibility": "public", "listening": True},
             {"subdomain": "notebook", "port": 8888, "visibility": "private", "listening": False},
         ]},
        {"name": "cold-harbor", "state": "archived", "vcpus": 2,
         "mem_mb": 8192, "mem_used_mb": None, "disk_mb": 0, "disk_total_mb": 25600,
         "cpu_seconds": None, "image": "universal", "last_active": _iso(90000),
         "net_rx_bytes": 73400320, "net_tx_bytes": 15728640,
         "pinned": False, "env_undecryptable": False, "tags": ["staging"],
         "routes": [{"subdomain": "cold-harbor", "port": 8080, "visibility": "private", "listening": False}]},
    ]

_ME = {"handle": "van", "operator": True}
_SECRETS = [
    {"name": "OPENAI_API_KEY", "tags": ["ml", "prod"], "version": 3, "updated_at": _iso(3600)},
    {"name": "DATABASE_URL", "tags": ["prod"], "version": 1, "updated_at": _iso(172800)},
    {"name": "HF_TOKEN", "tags": [], "version": 2, "updated_at": _iso(600)},
]
_SNAPSHOTS = [
    {"name": "cuda-base", "from_box": "brave-meadow", "created_at": _iso(86400)},
    {"name": "node-lts", "from_box": "dazzling-canyon", "created_at": _iso(432000)},
]
_NETRULES = [
    {"name": "CI egress", "tags": ["ci", "build"], "version": 2, "updated_at": _iso(1800),
     "spec": {"allow": ["github.com", "*.githubusercontent.com", "pypi.org",
                        "*.pythonhosted.org", "registry.npmjs.org", "ghcr.io"]}},
    {"name": "ML training", "tags": ["ml"], "version": 1, "updated_at": _iso(7200),
     "spec": {"allow": ["huggingface.co", "*.huggingface.co", "pytorch.org", "anthropic.com"]}},
]
def _dom(domain, display, resolved, tx, rx):
    return {"domain": domain, "display": display, "resolved": resolved,
            "tx_bytes": tx, "rx_bytes": rx, "total": tx + rx}
# Per-VM egress breakdown, keyed by machine name (only the running one has live data).
_BANDWIDTH = {
    "brave-meadow": {"name": "brave-meadow", "tx_bytes": 812394002, "rx_bytes": 4923847112,
        "domains": [
            _dom("github.com", "GitHub", True, 120_000_000, 3_200_000_000),
            _dom("huggingface.co", "Hugging Face", True, 41_200_000, 902_000_000),
            _dom("registry.npmjs.org", "npm", True, 8_400_000, 214_000_000),
            _dom("pypi.org", "PyPI", True, 6_100_000, 173_000_000),
            _dom("anthropic.com", "Anthropic", True, 22_400_000, 61_000_000),
            _dom("ghcr.io", "GitHub Container Registry", True, 3_200_000, 44_000_000),
            _dom("151.101.0.223", "151.101.0.223", False, 900_000, 2_100_000),
        ]},
}

# Injected into the page: overrides fetch() so every /api/* call returns mock data.
_STUB = """
<script>
(function () {
  var tick = 0;
  var ME = %(me)s, SECRETS = %(secrets)s, SNAPSHOTS = %(snapshots)s;
  var NETRULES = %(netrules)s, BANDWIDTH = %(bandwidth)s;
  function machines() {
    tick += 1;
    return %(machines_fn)s(tick);
  }
  function J(data) {
    return Promise.resolve(new Response(JSON.stringify(data), {
      status: 200, headers: { "Content-Type": "application/json" } }));
  }
  var real = window.fetch;
  window.fetch = function (url, opts) {
    var u = String(url), m = (opts && opts.method) || "GET";
    if (u.indexOf("/api/me") >= 0) return J(ME);
    if (u.indexOf("/api/secrets") >= 0 && m === "GET") return J(SECRETS);
    if (u.indexOf("/api/snapshots") >= 0 && m === "GET") return J(SNAPSHOTS);
    if (u.indexOf("/api/network-rules") >= 0 && m === "GET") return J(NETRULES);
    var bw = u.match(/\\/api\\/machines\\/([^/]+)\\/bandwidth/);
    if (bw && m === "GET") return J(BANDWIDTH[decodeURIComponent(bw[1])] || { name: "", domains: [] });
    if (u.indexOf("/api/machines") >= 0 && m === "GET" &&
        !/\\/(pause|resume|reboot|archive|pin|unpin|port|tags|rename|snapshot|bandwidth)/.test(u)) return J(machines());
    return J({ ok: true }); // mutations: pretend success
  };
})();
</script>
"""

def _machines_js():
    # emit the machine list as a JS function of `tick` (only cpu_seconds varies)
    return "function (tick) { return " + json.dumps(_machines(0)).replace(
        '"cpu_seconds": 1200.0', '"cpu_seconds": 1200 + tick * 3.1') + "; }"


_GLOBE_SVG = (
    b'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="none" '
    b'stroke="#888" stroke-width="1.2"><circle cx="8" cy="8" r="6.5"/>'
    b'<path d="M1.5 8h13M8 1.5c2 2 2 11 0 13M8 1.5c-2 2-2 11 0 13"/></svg>')


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        # Favicons: proxy DuckDuckGo host-side (as the real console does) so the
        # preview shows genuine brand marks; globe SVG on any failure.
        if self.path.startswith("/api/favicon"):
            from urllib.parse import urlparse, parse_qs
            import urllib.request
            domain = (parse_qs(urlparse(self.path).query).get("domain") or [""])[0]
            body, ctype = _GLOBE_SVG, "image/svg+xml"
            if domain:
                try:
                    with urllib.request.urlopen(
                            "https://icons.duckduckgo.com/ip3/%s.ico" % domain, timeout=4) as r:
                        data = r.read()
                    if data:
                        body, ctype = data, r.headers.get("Content-Type", "image/x-icon")
                except Exception:
                    pass
            self.send_response(200)
            self.send_header("Content-Type", ctype)
            self.send_header("Cache-Control", "public, max-age=86400")
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path.startswith("/api/"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"ok":true}')
            return
        try:
            html = open(INDEX, encoding="utf-8").read()
            # index.html carries /*SHARED_CSS*/ and /*SHARED_JS*/ markers that
            # the real server (internal/webui.Build) fills in from these same
            # files before minifying; mirror that here so the preview matches
            # what actually ships.
            html = html.replace("/*SHARED_CSS*/", open(SHARED_CSS, encoding="utf-8").read(), 1)
            html = html.replace("/*SHARED_JS*/", open(SHARED_JS, encoding="utf-8").read(), 1)
        except OSError as e:
            self.send_error(500, f"cannot read index.html: {e}")
            return
        stub = _STUB % {
            "me": json.dumps(_ME), "secrets": json.dumps(_SECRETS),
            "snapshots": json.dumps(_SNAPSHOTS), "machines_fn": _machines_js(),
            "netrules": json.dumps(_NETRULES), "bandwidth": json.dumps(_BANDWIDTH),
        }
        theme = ""
        if "theme=dark" in self.path:
            theme = '<script>document.documentElement.setAttribute("data-theme","dark")</script>'
        elif "theme=light" in self.path:
            theme = '<script>document.documentElement.setAttribute("data-theme","light")</script>'
        # Simulate the production zone so generated route/login links read as the
        # real public host (e.g. brave-meadow.catnip.sh) instead of localhost:PORT.
        html = html.replace(
            "</head>", '<meta name="sparkbox-domain" content="catnip.sh" />\n</head>', 1)
        html = html.replace("<body>", "<body>\n" + stub + theme, 1)
        body = html.encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):  # quiet
        pass


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8799
    srv = http.server.HTTPServer(("127.0.0.1", port), Handler)
    print(f"user-console preview → http://localhost:{port}")
    print(f"  serving {os.path.relpath(INDEX)} with mock data (edit + refresh to iterate)")
    print(f"  dark: http://localhost:{port}/?theme=dark   light: http://localhost:{port}/?theme=light")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        print("\nbye")


if __name__ == "__main__":
    main()
