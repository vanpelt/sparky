#!/usr/bin/env python3
"""Preview a sparkbox HTML page locally with mock data.

Every page here talks to a live sparkbox edge (edge-session auth, a WebSocket,
a spec document), so opening the file on its own shows a sign-in screen or an
error banner. This server serves the *real* HTML with just enough stubbed out
that the whole UI renders with no backend.

    python3 hack/preview-console.py                   # user console -> :8799
    python3 hack/preview-console.py docs              # the REST API docs page
    python3 hack/preview-console.py terminal 9000     # the browser terminal

The page is re-read on every request, so while you edit it you just refresh the
browser to see changes. No dependencies (stdlib only).

Add ?theme=dark or ?theme=light to force a theme (default follows the OS).
"""
import http.server
import json
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
SHARED_CSS = os.path.join(HERE, "..", "internal", "webui", "shared.css")
SHARED_JS = os.path.join(HERE, "..", "internal", "webui", "shared.js")


def _pkg(*parts):
    return os.path.join(HERE, "..", "internal", *parts)


# The pages this can preview. `stub` says whether the user console's mock fetch
# is injected: the docs page needs only its spec (served below) and the terminal
# needs a WebSocket it cannot have, so both render themselves honestly without
# it — the terminal shows its own "reconnecting" state, which is a real state
# worth being able to look at. The terminal's instrument strip does not need the
# stub either: its /vitals poll is answered by a real route below, so the
# sparklines animate against mock counters while the shell stays disconnected.
PAGES = {
    "console": {"index": _pkg("userconsole", "index.html"), "stub": True,
                "assets": None},
    "docs": {"index": _pkg("restapi", "docs.html"), "stub": False,
             "assets": None},
    "terminal": {"index": _pkg("xterm", "index.html"), "stub": False,
                 "assets": _pkg("xterm", "assets")},
}
PAGE = PAGES["console"]
INDEX = PAGE["index"]

# ---- mock fleet: one machine per state, plus routes/tags/secrets/snapshots ----
def _iso(sec_ago):
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - sec_ago))

def _machines(tick):
    return [
        {"name": "dazzling-canyon", "state": "paused", "vcpus": 4,
         "mem_mb": 8192, "mem_used_mb": None, "disk_mb": 614, "disk_total_mb": 25600,
         "cpu_seconds": None, "image": "universal", "last_active": _iso(600),
         "net_rx_bytes": 48213, "net_tx_bytes": 9004,
         "pinned": False, "env_undecryptable": False, "tags": [],
         "routes": [
             {"subdomain": "app", "port": 4444, "visibility": "public", "listening": False},
             {"subdomain": "dazzling-canyon", "port": 8000, "visibility": "public", "listening": False},
         ]},
        {"name": "brave-meadow", "state": "running", "vcpus": 8,
         "mem_mb": 16384, "mem_used_mb": 6820, "disk_mb": 12040, "disk_total_mb": 25600,
         "cpu_seconds": 1200 + tick * 3.1, "image": "universal", "last_active": _iso(4),
         "net_rx_bytes": 4923847112, "net_tx_bytes": 812394002,
         "pinned": True, "env_undecryptable": False, "tags": ["ml", "prod"],
         "turbo": True, "base_vcpus": 4, "base_mem_mb": 8192,
         "routes": [
             {"subdomain": "brave-meadow", "port": 8080, "visibility": "private", "listening": True},
             {"subdomain": "api", "port": 3000, "visibility": "public", "listening": True},
             {"subdomain": "notebook", "port": 8888, "visibility": "private", "listening": False},
         ]},
        {"name": "cold-harbor", "state": "archived", "vcpus": 4,
         "mem_mb": 8192, "mem_used_mb": None, "disk_mb": 0, "disk_total_mb": 25600,
         "cpu_seconds": None, "image": "universal", "last_active": _iso(90000),
         "net_rx_bytes": 73400320, "net_tx_bytes": 15728640,
         "pinned": False, "env_undecryptable": False, "tags": ["staging"],
         "routes": [{"subdomain": "cold-harbor", "port": 8080, "visibility": "private", "listening": False}]},
        # The builder a failed environment build leaves behind. It is here so
        # the failed card's "Open a terminal" is reachable in a preview: that
        # button is drawn only for a builder the machine list actually has.
        {"name": "weave-py-build", "state": "paused", "vcpus": 4,
         "mem_mb": 8192, "mem_used_mb": None, "disk_mb": 512, "disk_total_mb": 25600,
         "cpu_seconds": None, "image": "universal", "last_active": _iso(2700),
         "net_rx_bytes": 1204832, "net_tx_bytes": 210553,
         "pinned": False, "env_undecryptable": False, "tags": ["weave-py", "default"],
         "routes": [{"subdomain": "weave-py-build", "port": 8080, "visibility": "private", "listening": False}]},
    ]

# The owner rollup behind the Machines tab's footprint card. It is mock data of
# its own rather than a sum of _machines() above, because the interesting half —
# the reflink baseline and the pool budgets — appears in no machine record; that
# is the whole reason the card has an endpoint of its own. Kept internally
# consistent so the ratios the page derives are the ones a real fleet shows:
# shared = raw - used, over four disks (two paused, one running, one parked).
_USAGE = {
    "owner": "van",
    "memory_pool_mb": 8192, "memory_burst_mb": 16384,
    "effective_memory_mb": 8192, "resident_memory_mb": 6820,
    "borrowed_memory_mb": 0, "allocated_memory_mb": 16384, "allocated_vcpus": 8,
    "disk_pool_mb": 102400, "used_disk_mb": 6752, "raw_disk_mb": 25166,
    "shared_disk_mb": 18414, "capacity_disk_mb": 76800,
    "running_sandboxes": 1, "total_sandboxes": 4, "archived_sandboxes": 1,
    "turbo_sandboxes": 1, "max_running": 4, "max_sandboxes": 8, "nodes": 1,
}

_ME = {"handle": "van", "operator": True, "terminal_subdomain": "xterm"}
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
# Repo attachments, one per install state the panel renders: reachable, not
# installed (the row that carries the one-click URL), and still being asked.
_REPOS = [
    {"host": "github.com", "slug": "wandb/hivemind", "ref": "", "path": "",
     "access": "write", "tags": ["hm"], "created_at": _iso(86400), "app": "ready",
     "user_auth": "bot", "user_auth_enabled": True, "github_login": "vanpelt"},
    {"host": "github.com", "slug": "wandb/sparky", "ref": "main", "path": "src/sparky",
     "access": "write", "tags": ["hm"], "created_at": _iso(172800), "app": "ready",
     "user_auth": "user", "user_auth_enabled": True, "github_login": "vanpelt"},
    {"host": "github.com", "slug": "wandb/dotfiles", "ref": "", "path": "",
     "access": "read", "tags": ["default"], "created_at": _iso(604800), "app": "missing",
     "install_url": "https://github.com/apps/sparkbox/installations/new", "user_auth": "bot"},
    {"host": "github.com", "slug": "torvalds/linux", "ref": "v6.1", "path": "src/linux",
     "access": "read", "tags": [], "created_at": _iso(300), "app": "blocked",
     "app_note": "the GitHub App may not do that: you are not an active member of torvalds",
     "user_auth": "bot"},
]


# Environments, one per build state, because the four states are four different
# card anatomies: ready names the disk it boots, building names the machine it
# is happening in, failed carries a guest log line and the two recovery
# actions, and draft has nothing yet. The tab had no fixture here at all until
# after it shipped, which is exactly how a table row whose contents wrapped one
# chip per line got as far as a deploy.
_ENVIRONMENTS = [
    {"name": "web", "description": "the wandb app dev stack — node 20 and postgres",
     "repos": ["wandb/core", "wandb/app"], "secrets": ["GITHUB_TOKEN", "DATABASE_URL"],
     "rules": ["web"], "vars": [{"name": "NODE_ENV", "value": "development"},
                                {"name": "LOG_LEVEL", "value": "debug"}],
     "has_setup": True, "setup_bytes": 3746, "setup_from": "agent",
     "state": "ready", "built_at": _iso(11400), "snapshot": "web-20260902-1412"},
    {"name": "selfhost", "description": "single-container selfhost image",
     "repos": ["wandb/server"], "secrets": [], "rules": ["selfhost"], "vars": [],
     "has_setup": True, "setup_bytes": 1180, "setup_from": "repo",
     "state": "building", "build_box": "selfhost-build", "snapshot": ""},
    {"name": "weave-py", "description": "",
     "repos": ["wandb/weave"], "secrets": ["OPENAI_API_KEY"], "rules": ["weave-py"],
     "vars": [{"name": "PY", "value": "3.12"}],
     "has_setup": True, "setup_bytes": 902, "setup_from": "agent",
     "state": "failed", "build_box": "weave-py-build", "snapshot": "",
     "build_error": "sparkbox: this box is configured, but .sparkbox/setup.sh does not "
                    "run in it, so no later build could reproduce it"},
    {"name": "scratch", "description": "empty starting point for one-off experiments",
     "repos": [], "secrets": [], "rules": [], "vars": [],
     "has_setup": False, "setup_bytes": 0, "setup_from": "", "state": "draft", "snapshot": ""},
]
_ENV_SCRIPT = {"from": "agent", "script": "#!/usr/bin/env bash\n"
               "set -euo pipefail\n\n"
               "sudo apt-get -y install postgresql-client\n"
               "pnpm install\n"}

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
  var NETRULES = %(netrules)s, BANDWIDTH = %(bandwidth)s, REPOS = %(repos)s;
  var USAGE = %(usage)s, ENVIRONMENTS = %(environments)s, ENVSCRIPT = %(envscript)s;
  function machines() {
    tick += 1;
    var list = %(machines_fn)s(tick);
    list.forEach(function (b) {
      if (!(b.name in TURBO)) return;
      var on = TURBO[b.name];
      if (on === !!b.turbo) return;
      var f = 2;
      b.vcpus = on ? b.vcpus * f : Math.round(b.vcpus / f);
      b.mem_mb = on ? b.mem_mb * f : Math.round(b.mem_mb / f);
      b.turbo = on;
    });
    return list;
  }
  function J(data) {
    return Promise.resolve(new Response(JSON.stringify(data), {
      status: 200, headers: { "Content-Type": "application/json" } }));
  }
  var TURBO = {};   // name -> on, applied over the canned list below
  var real = window.fetch;
  window.fetch = function (url, opts) {
    var u = String(url), m = (opts && opts.method) || "GET";
    var tb = u.match(/\\/api\\/machines\\/([^/]+)\\/turbo/);
    if (tb && m === "POST") {
      // Real turbo is a pause plus a cold boot; the delay is here so the LED
      // can be watched blinking rather than flicking instantly.
      var name = decodeURIComponent(tb[1]), on = JSON.parse(opts.body || "{}").on;
      return new Promise(function (ok) {
        setTimeout(function () { TURBO[name] = !!on; ok(J({ ok: true })); }, 2200);
      });
    }
    if (u.indexOf("/api/me") >= 0) return J(ME);
    if (u.indexOf("/api/usage") >= 0 && m === "GET") return J(USAGE);
    if (u.indexOf("/api/secrets") >= 0 && m === "GET") return J(SECRETS);
    if (u.indexOf("/api/snapshots") >= 0 && m === "GET") return J(SNAPSHOTS);
    if (u.indexOf("/api/network-rules") >= 0 && m === "GET") return J(NETRULES);
    if (u.indexOf("/api/repos") >= 0 && m === "GET") return J(REPOS);
    if (/\\/api\\/environments\\/[^/]+\\/script/.test(u) && m === "GET") return J(ENVSCRIPT);
    if (u.indexOf("/api/environments") >= 0 && m === "GET") return J(ENVIRONMENTS);
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


# ---- mock vitals for the terminal's instrument strip ------------------------
# The strip derives every rate from the delta between two readings, so the mock
# has to walk cumulative counters forward in real time — handing back a fresh
# random number each poll would produce no series at all. The intensity sweeps a
# slow cycle on purpose: it is the only way to watch the plot cross the 70% and
# 90% threshold colours without a genuinely busy VM to point at.
_VITALS = {"at": None, "cpu": 1200.0, "rx": 4_200_000, "tx": 910_000, "mem": 2600.0,
           "turbo": False}
_V_VCPUS, _V_MEM_MB = 2, 8192


def _vitals():
    import math
    import random
    now = time.time()
    dt = min(5.0, max(0.0, now - (_VITALS["at"] or now - 1.0)))
    _VITALS["at"] = now

    util = min(1.0, max(0.01, ((math.sin(now / 7.6) + 1) / 2) ** 2 * 1.15
                        + random.uniform(-0.06, 0.06)))
    _VITALS["cpu"] += util * _V_VCPUS * dt
    # Traffic is bursty rather than smooth, which is what makes a mirrored plot
    # worth drawing in the first place.
    burst = 1.0 if random.random() < 0.12 else 0.06
    _VITALS["rx"] += (18_000 + burst * random.uniform(0, 900_000)) * dt
    _VITALS["tx"] += (9_000 + burst * random.uniform(0, 300_000)) * dt
    _VITALS["mem"] = min(_V_MEM_MB * 0.96, max(700.0,
                         _VITALS["mem"] + random.uniform(-42, 40) + util * 34))
    rx, tx = int(_VITALS["rx"]), int(_VITALS["tx"])
    return {
        # The four constants the page cannot work out for itself: its own
        # name (the host is <name>-xterm.<zone>, so the first label is not it),
        # the ssh line, whose advertised port only the server knows, the default
        # proxy endpoint, and the console to link at. The menu hides the rows
        # these are missing from, so a preview without them would silently be a
        # different menu.
        "name": "brave-meadow", "ssh": "ssh brave-meadow@catnip.sh",
        "proxy": "https://brave-meadow.catnip.sh/",
        "console": "https://my.catnip.sh/",
        "hivemind_session_title": "Polish the Sparkbox terminal experience",
        "hivemind_session_url": "https://hivemind.example/sessions/demo",
        "state": "running", "at_ms": int(now * 1000),
        "vcpus": _V_VCPUS * (2 if _VITALS["turbo"] else 1),
        "mem_mb": _V_MEM_MB * (2 if _VITALS["turbo"] else 1),
        "turbo": _VITALS["turbo"], "turbo_available": True,
        "cpu_seconds": round(_VITALS["cpu"], 3),
        "mem_used_mb": int(_VITALS["mem"]),
        "net_rx_bytes": rx, "net_tx_bytes": tx,
        "life_rx_bytes": rx + 8_400_000_000, "life_tx_bytes": tx + 1_100_000_000,
    }


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
        # The terminal's instrument strip polls this once a second. It is not
        # under /api/, so it needs its own branch rather than the ok:true stub.
        if self.path.split("?")[0] == "/vitals":
            body = json.dumps(_vitals()).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path.startswith("/api/"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"ok":true}')
            return
        # The docs page is a renderer for the document it fetches, so serving
        # the real openapi.json is the whole backend it needs. It is the
        # canonical file, not a copy, so what the preview renders is what ships.
        if self.path.split("?")[0] in ("/openapi.json", "/openapi.yaml"):
            self._sendfile(_pkg("restapi", "openapi.json"), "application/json")
            return
        # The terminal's vendored xterm.js and its addons, served from the same
        # /assets/ paths the real handler uses.
        if PAGE["assets"] and self.path.startswith("/assets/"):
            name = os.path.basename(self.path.split("?")[0])
            if name == "sparkbox-logo.png":
                self._sendfile(_pkg("launch", "sparkbox-logo.png"), "image/png")
                return
            ctype = "text/css" if name.endswith(".css") else "text/javascript"
            self._sendfile(os.path.join(PAGE["assets"], name), ctype)
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
            self.send_error(500, f"cannot read {os.path.basename(INDEX)}: {e}")
            return
        stub = ""
        if PAGE["stub"]:
            stub = _STUB % {
                "me": json.dumps(_ME), "secrets": json.dumps(_SECRETS),
                "snapshots": json.dumps(_SNAPSHOTS), "machines_fn": _machines_js(),
                "netrules": json.dumps(_NETRULES), "bandwidth": json.dumps(_BANDWIDTH),
                "repos": json.dumps(_REPOS), "usage": json.dumps(_USAGE),
                "environments": json.dumps(_ENVIRONMENTS),
                "envscript": json.dumps(_ENV_SCRIPT),
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

    # The terminal page posts here when its turbo switch is thrown. Latched, and
    # slow on purpose: a real turbo is a pause plus a cold boot, and the lamp's
    # blink is only worth looking at if there is something to blink through.
    def do_POST(self):
        if self.path.split("?")[0] != "/turbo":
            self.send_error(404, "not found")
            return
        n = int(self.headers.get("Content-Length") or 0)
        req = json.loads(self.rfile.read(n) or b"{}")
        time.sleep(2.2)
        _VITALS["turbo"] = bool(req.get("on"))
        body = json.dumps({"turbo": _VITALS["turbo"]}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _sendfile(self, path, ctype):
        try:
            with open(path, "rb") as f:
                body = f.read()
        except OSError as e:
            self.send_error(404, str(e))
            return
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):  # quiet
        pass


def main():
    global PAGE, INDEX
    args = sys.argv[1:]
    if args and not args[0].isdigit():
        name = args.pop(0)
        if name not in PAGES:
            sys.exit(f"unknown page {name!r}: pick one of {', '.join(PAGES)}")
        PAGE = PAGES[name]
        INDEX = PAGE["index"]
    port = int(args[0]) if args else 8799
    srv = http.server.HTTPServer(("127.0.0.1", port), Handler)
    print(f"sparkbox page preview → http://localhost:{port}")
    print(f"  serving {os.path.relpath(INDEX)} with mock data (edit + refresh to iterate)")
    print(f"  dark: http://localhost:{port}/?theme=dark   light: http://localhost:{port}/?theme=light")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        print("\nbye")


if __name__ == "__main__":
    main()
