#!/usr/bin/env python3
"""Preview a sparkbox HTML page locally with mock data.

Every page here talks to a live sparkbox edge (edge-session auth, a WebSocket,
a spec document), so opening the file on its own shows a sign-in screen or an
error banner. This server serves the *real* HTML with just enough stubbed out
that the whole UI renders with no backend.

    python3 hack/preview-console.py                   # user console -> :8799
    python3 hack/preview-console.py docs              # the REST API docs page
    python3 hack/preview-console.py terminal 9000     # the browser terminal

Any page can be asked for the terminal instead by adding ?xterm=true to its
URL — the console's environments tab grows a "Preview terminal" button that
does exactly this. /ws answers with a fake PTY (see _fake_pty) rather than a
real sandbox, so the terminal actually connects: enough to check the chrome,
type into it, and exercise the OSC 52 clipboard addon with its `clip` command.

The page is re-read on every request, so while you edit it you just refresh the
browser to see changes. No dependencies (stdlib only).

Add ?theme=dark or ?theme=light to force a theme (default follows the OS).
"""
import base64
import hashlib
import http.server
import json
import os
import struct
import sys
import time
from urllib.parse import parse_qs, urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
SHARED_CSS = os.path.join(HERE, "..", "internal", "webui", "shared.css")
SHARED_JS = os.path.join(HERE, "..", "internal", "webui", "shared.js")


def _pkg(*parts):
    return os.path.join(HERE, "..", "internal", *parts)


# The pages this can preview. `stub` says whether the user console's mock fetch
# is injected: the docs page needs only its spec (served below), and the
# terminal needs a WebSocket it does not otherwise have — /ws below fakes one
# rather than leaving it honestly disconnected, because a copy/paste or
# keybinding change is only checkable against a shell that actually answers.
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


def _wants_xterm(path):
    return parse_qs(urlparse(path).query).get("xterm", [""])[0] == "true"

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
         # AT RISK: an unpushed commit and a dirty tree exist in this VM and
         # nowhere else, so removing the machine loses them — the red banner,
         # and the one that also has to carry "behind" when both are true.
         "repos": [
             {"slug": "wandb/weave", "path": "/home/sparky/weave", "branch": "spike",
              "upstream": "origin/spike", "ahead": 2, "behind": 3, "dirty": True,
              "state": "ok"},
         ],
         # The port strip: the default port first, then this hostname's other
         # ports, then any extra hostname. "pinned" is a port the owner has an
         # opinion about (the only kind that can be forgotten); an unpinned one
         # only turned up in the host's scan.
         "routes": [
             {"subdomain": "dazzling-canyon", "port": 8000, "visibility": "public",
              "listening": False, "default": True},
             {"subdomain": "dazzling-canyon", "port": 5173, "visibility": "private",
              "listening": False, "pinned": True},
             {"subdomain": "app", "port": 4444, "visibility": "public", "listening": False},
         ]},
        {"name": "brave-meadow", "state": "running", "vcpus": 8,
         "mem_mb": 16384, "mem_used_mb": 6820, "disk_mb": 12040, "disk_total_mb": 25600,
         "cpu_seconds": 1200 + tick * 3.1, "image": "universal", "last_active": _iso(4),
         "net_rx_bytes": 4923847112, "net_tx_bytes": 812394002,
         "pinned": True, "env_undecryptable": False, "tags": ["ml", "prod"],
         "turbo": True, "base_vcpus": 4, "base_mem_mb": 8192,
         # BEHIND ONLY: somebody else pushed and this checkout has not pulled.
         # Nothing here is at risk, so the card's banner is amber. The two repo
         # banners are the reason both of these fixtures carry repo state: the
         # colour is the whole point and neither variant is visible without one.
         "repos": [
             {"slug": "wandb/core", "path": "/home/sparky/core", "branch": "main",
              "upstream": "origin/main", "ahead": 0, "behind": 7, "dirty": False,
              "state": "ok"},
             {"slug": "wandb/app", "path": "/home/sparky/app", "branch": "main",
              "upstream": "origin/main", "ahead": 0, "behind": 0, "dirty": False,
              "state": "ok"},
         ],
         # Enough ports to make the strip scroll, which is what it is for.
         "routes": [
             {"subdomain": "brave-meadow", "port": 8080, "visibility": "private",
              "listening": True, "default": True, "service": "uvicorn"},
             {"subdomain": "brave-meadow", "port": 3000, "visibility": "public",
              "listening": True, "pinned": True, "service": "Next.js"},
             {"subdomain": "brave-meadow", "port": 5173, "visibility": "private",
              "listening": True, "service": "Vite"},
             {"subdomain": "brave-meadow", "port": 6006, "visibility": "private",
              "listening": True, "service": "Storybook"},
             {"subdomain": "brave-meadow", "port": 8888, "visibility": "private",
              "listening": False, "pinned": True},
             {"subdomain": "brave-meadow", "port": 16686, "visibility": "private",
              "listening": True, "service": "Jaeger"},
             {"subdomain": "notebook", "port": 8888, "visibility": "private", "listening": False},
         ]},
        {"name": "cold-harbor", "state": "archived", "vcpus": 4,
         "mem_mb": 8192, "mem_used_mb": None, "disk_mb": 0, "disk_total_mb": 25600,
         "cpu_seconds": None, "image": "universal", "last_active": _iso(90000),
         "net_rx_bytes": 73400320, "net_tx_bytes": 15728640,
         "pinned": False, "env_undecryptable": False, "tags": ["staging"],
         "routes": [{"subdomain": "cold-harbor", "port": 8080, "visibility": "private",
                     "listening": False, "default": True}]},
        # The builder of an environment build IN FLIGHT, with the HiveMind
        # reading its vitals carry. It is here so the building card's "Watch the
        # agent" link is reachable in a preview: that link is drawn from the
        # BUILDER's row in this list, not from the environment, so an
        # environments fixture alone would silently render the card without it —
        # which is how the table shipped the last time this tab was drawn blind.
        {"name": "selfhost-build", "state": "running", "vcpus": 4,
         "mem_mb": 8192, "mem_used_mb": 3140, "disk_mb": 4820, "disk_total_mb": 25600,
         "cpu_seconds": 240 + tick * 1.7, "image": "universal", "last_active": _iso(2),
         "net_rx_bytes": 284832100, "net_tx_bytes": 12105530,
         "pinned": False, "env_undecryptable": False, "tags": ["selfhost", "default"],
         "hivemind_session_url": "https://hivemind.example/sessions/selfhost-build",
         "hivemind_session_title": "Write a setup script for the selfhost image",
         "hivemind_active": True,
         "routes": [{"subdomain": "selfhost-build", "port": 8080, "visibility": "private",
                     "listening": False, "default": True}]},
        # The builder a failed environment build leaves behind. It is here so
        # the failed card's "Open a terminal" is reachable in a preview: that
        # button is drawn only for a builder the machine list actually has.
        {"name": "weave-py-build", "state": "paused", "vcpus": 4,
         "mem_mb": 8192, "mem_used_mb": None, "disk_mb": 512, "disk_total_mb": 25600,
         "cpu_seconds": None, "image": "universal", "last_active": _iso(2700),
         "net_rx_bytes": 1204832, "net_tx_bytes": 210553,
         "pinned": False, "env_undecryptable": False, "tags": ["weave-py", "default"],
         "routes": [{"subdomain": "weave-py-build", "port": 8080, "visibility": "private",
                     "listening": False, "default": True}]},
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
     "state": "ready", "built_at": _iso(11400), "snapshot": "web-20260902-1412",
     # A finished agent build keeps its transcript, and this is the only thing
     # that survives the builder: the box was destroyed when it succeeded.
     "build_session": "https://hivemind.example/sessions/web-build"},
    # An agent build in flight: no script yet, because writing one is what the
    # agent is doing. This is the state a FIRST build of an environment is in.
    {"name": "selfhost", "description": "single-container selfhost image",
     "repos": ["wandb/server"], "secrets": [], "rules": ["selfhost"], "vars": [],
     "has_setup": False, "setup_bytes": 0, "setup_from": "agent",
     "state": "building", "build_box": "selfhost-build", "snapshot": ""},
    {"name": "weave-py", "description": "",
     "repos": ["wandb/weave"], "secrets": ["OPENAI_API_KEY"], "rules": ["weave-py"],
     "vars": [{"name": "PY", "value": "3.12"}],
     "has_setup": True, "setup_bytes": 902, "setup_from": "agent",
     "state": "failed", "build_box": "weave-py-build", "snapshot": "",
     "build_session": "https://hivemind.example/sessions/weave-py-build",
     # The whole stored sentence, the way markFailedBox writes it: the reason,
     # then the builder and the three commands that recover from it. It is here
     # in full because this card renders build_error verbatim, so the preview
     # has to show how long the real thing is next to the buttons that do the
     # same three things.
     "build_error": "sparkbox: this box is configured, but .sparkbox/setup.sh does not "
                    "run in it, so no later build could reproduce it. The builder "
                    "weave-py-build is paused \u2014 `ssh weave-py-build@sparkbox.example` to look, "
                    "then `ssh ctl@sparkbox.example env capture weave-py` to finish it, or "
                    "`ssh ctl@sparkbox.example rm weave-py-build` to start over"},
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
  // A refusal, in the console's own error shape: {error, code}. Needed because
  // the blanket "mutations: pretend success" at the bottom of this switch
  // swallows every non-200, and some of the states worth looking at ARE the
  // non-200s.
  function E(status, code, error) {
    return Promise.resolve(new Response(JSON.stringify({ error: error, code: code }), {
      status: status, headers: { "Content-Type": "application/json" } }));
  }
  // Saving an environment named `adopt-me` answers the adoption conflict, so
  // the confirm-and-retry can be walked without a server: type that name into
  // the New environment form and save. Saying yes sends the same body with
  // adopt:true, which falls through to the success below.
  var ADOPTED = {};
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
    var envPut = u.match(/\\/api\\/environments\\/([^/]+)$/);
    if (envPut && m === "PUT") {
      var envName = decodeURIComponent(envPut[1]);
      var wantsAdopt = envName === "adopt-me" && !JSON.parse(opts.body || "{}").adopt;
      if (wantsAdopt && !ADOPTED[envName]) {
        return E(409, "env_tag_in_use",
          "the tag \\"adopt-me\\" is already carrying 2 repositories, 1 secret and 3 sandboxes. " +
          "An environment's name IS its tag, so creating adopt-me adopts all of it: everything on " +
          "that tag becomes part of the environment, and every sandbox carrying it becomes one of " +
          "its machines. Because sandboxes already carry it, the environment is also created with " +
          "unrestricted egress rather than the default allowlist, so as not to cut them off.");
      }
      ADOPTED[envName] = true;
      return J({ ok: true });
    }
    if (u.indexOf("/api/machines") >= 0 && m === "GET" &&
        !/\\/(pause|resume|reboot|archive|pin|unpin|port|tags|rename|snapshot|bandwidth)/.test(u)) return J(machines());
    return J({ ok: true }); // mutations: pretend success
  };
  // Preview-only: a button on each environment card that opens the xterm
  // page here with a fake PTY behind it (see _fake_pty in this script's own
  // server), so a terminal-side change is checkable without a real sandbox.
  // A MutationObserver rather than a load-time query because envCard() runs
  // after the /api/environments fetch above resolves, well after this script
  // does.
  new MutationObserver(function () {
    document.querySelectorAll(".env-card .m-actions:not([data-xterm-preview])").forEach(function (el) {
      el.setAttribute("data-xterm-preview", "1");
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "btn-outline btn-sm";
      btn.textContent = "🖥 Preview terminal";
      btn.title = "Open the xterm terminal page against a fake PTY, to check terminal-side changes";
      btn.addEventListener("click", function () {
        window.open(location.pathname + "?xterm=true", "_blank");
      });
      el.appendChild(btn);
    });
  }).observe(document.body, { childList: true, subtree: true });
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


# ---- a bare RFC 6455 server, just enough for the terminal's own client ------
# http.server has no WebSocket support, and xterm.js's index.html hardcodes
# ws(s)://<host>/ws with subprotocol "sparkbox.terminal.v1" — so a preview of
# it needs a real (if tiny) upgrade handshake and frame codec, not a mock.
_WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def _ws_accept(key):
    return base64.b64encode(hashlib.sha1((key + _WS_GUID).encode()).digest()).decode()


def _ws_send(wfile, data, opcode):
    b0 = 0x80 | opcode  # FIN=1, no extensions
    n = len(data)
    if n <= 125:
        header = struct.pack("!BB", b0, n)
    elif n <= 0xFFFF:
        header = struct.pack("!BBH", b0, 126, n)
    else:
        header = struct.pack("!BBQ", b0, 127, n)
    wfile.write(header + data)
    wfile.flush()


def _ws_recv(rfile):
    """Yield (opcode, payload) for each client frame until close or EOF.

    Client frames are always masked; server frames (above) never are — the
    one asymmetry RFC 6455 insists on. Fragmented frames (FIN=0) are not
    handled: xterm.js never sends one for a keystroke or a resize message,
    which is all this ever receives.
    """
    while True:
        hdr = rfile.read(2)
        if len(hdr) < 2:
            return
        b0, b1 = hdr[0], hdr[1]
        opcode = b0 & 0x0F
        masked = b1 & 0x80
        length = b1 & 0x7F
        if length == 126:
            ext = rfile.read(2)
            if len(ext) < 2:
                return
            length = struct.unpack("!H", ext)[0]
        elif length == 127:
            ext = rfile.read(8)
            if len(ext) < 8:
                return
            length = struct.unpack("!Q", ext)[0]
        mask_key = rfile.read(4) if masked else b""
        payload = rfile.read(length) if length else b""
        if masked and payload:
            payload = bytes(byte ^ mask_key[i % 4] for i, byte in enumerate(payload))
        yield opcode, payload
        if opcode == 0x8:  # close
            return


def _ws_close(wfile, code, reason):
    # Mirrors internal/xterm/ws.go's closeWith: a 2-byte close code followed
    # by a UTF-8 reason, in the close frame's own payload — not a data frame.
    _ws_send(wfile, struct.pack("!H", code) + reason.encode("utf-8"), 0x8)


_PROMPT = "\x1b[32mpreview\x1b[0m:\x1b[34m~\x1b[0m$ "
_BANNER = (
    "\x1b[36mSparkbox preview shell\x1b[0m \u2014 a fake PTY, not a real sandbox.\r\n"
    "Type \x1b[1mclip\x1b[0m to test the OSC 52 clipboard addon, or drag-select any\r\n"
    "of this text and Ctrl/Cmd+C to test a plain copy. \x1b[1mexit\x1b[0m ends the\r\n"
    "session like a real shell would.\r\n\r\n"
)


def _osc52_set_clipboard(text):
    # OSC 52: set-clipboard, target "c" (the system clipboard). ClipboardAddon
    # (internal/xterm/assets/addon-clipboard.js) is what turns this into an
    # actual navigator.clipboard.writeText() call in the browser.
    b64 = base64.b64encode(text.encode("utf-8")).decode()
    return "\x1b]52;c;" + b64 + "\x07"


def _run_fake_command(cmd, wfile):
    cmd = cmd.strip()
    if cmd == "":
        return
    if cmd == "clip":
        text = "hello from the fake sparkbox terminal"
        _ws_send(wfile, _osc52_set_clipboard(text).encode("ascii"), 0x2)
        _ws_send(wfile, ("\x1b[2mwrote %r to the system clipboard over OSC 52 \u2014 "
                          "paste anywhere to check\x1b[0m\r\n" % text).encode("utf-8"), 0x2)
    elif cmd == "help":
        _ws_send(wfile, b"\x1b[2mcommands: clip, help\x1b[0m\r\n", 0x2)
    else:
        _ws_send(wfile, ("preview-shell: " + cmd + ": command not found\r\n").encode("utf-8"), 0x2)


def _fake_pty(rfile, wfile):
    # Mirrors the real handshake's status messages (see xterm's index.html
    # ws.onmessage) closely enough that the connecting/starting/ready chrome
    # is exercised too, not just the shell once it is up.
    try:
        _ws_send(wfile, json.dumps({"type": "status", "state": "starting",
                                     "note": "Booting the fake preview shell\u2026"}).encode(), 0x1)
        time.sleep(0.4)
        _ws_send(wfile, json.dumps({"type": "status", "state": "ready"}).encode(), 0x1)
        _ws_send(wfile, _BANNER.encode("utf-8"), 0x2)
        _ws_send(wfile, _PROMPT.encode("utf-8"), 0x2)
        line = bytearray()
        for opcode, payload in _ws_recv(rfile):
            if opcode == 0x8:  # close
                return
            if opcode == 0x9:  # ping -> pong, same payload
                _ws_send(wfile, payload, 0xA)
                continue
            if opcode != 0x2:  # only binary frames carry keystrokes; text is resize JSON
                continue
            for b in payload:
                if b in (0x0D, 0x0A):  # Enter
                    _ws_send(wfile, b"\r\n", 0x2)
                    cmd = line.decode("utf-8", "replace").strip()
                    line.clear()
                    if cmd == "exit":
                        # Same two-part goodbye as a real shell: the exit
                        # status over the socket, then the 4001 close that
                        # tells the page to show "Shell exited" rather than
                        # treat this as a dropped connection to retry.
                        _ws_send(wfile, json.dumps({"type": "exit", "code": 0}).encode(), 0x1)
                        _ws_close(wfile, 4001, "shell exited with 0")
                        return
                    _run_fake_command(cmd, wfile)
                    _ws_send(wfile, _PROMPT.encode("utf-8"), 0x2)
                elif b in (0x7F, 0x08):  # Backspace
                    if line:
                        line.pop()
                        _ws_send(wfile, b"\b \b", 0x2)
                elif b == 0x03:  # Ctrl+C
                    line.clear()
                    _ws_send(wfile, b"^C\r\n", 0x2)
                    _ws_send(wfile, _PROMPT.encode("utf-8"), 0x2)
                elif b >= 0x20:
                    line.append(b)
                    _ws_send(wfile, bytes([b]), 0x2)
    except (BrokenPipeError, ConnectionResetError, OSError):
        pass  # the tab closed or reloaded; nothing to clean up


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        # The xterm page's own WebSocket, answered by a fake PTY (see
        # _fake_pty) instead of a real sandbox.
        if self.path.split("?")[0] == "/ws":
            self._handle_ws()
            return
        # Favicons: proxy DuckDuckGo host-side (as the real console does) so the
        # preview shows genuine brand marks; globe SVG on any failure.
        if self.path.startswith("/api/favicon"):
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
        # The console's own index.html points <img src="/sparkbox-logo.png">
        # at the console handler's route (console.go's h.logo), not the
        # /assets/ path the terminal uses — mirror it with the same file.
        if self.path.split("?")[0] == "/sparkbox-logo.png":
            self._sendfile(_pkg("userconsole", "sparkbox-logo.png"), "image/png")
            return
        # The terminal's vendored xterm.js and its addons, served from the same
        # /assets/ paths the real handler uses. Unconditional on the page this
        # process was started with — ?xterm=true can ask for the terminal from
        # any page, and its assets have to be reachable when it does.
        if self.path.startswith("/assets/"):
            name = os.path.basename(self.path.split("?")[0])
            if name == "sparkbox-logo.png":
                self._sendfile(_pkg("launch", "sparkbox-logo.png"), "image/png")
                return
            ctype = "text/css" if name.endswith(".css") else "text/javascript"
            self._sendfile(os.path.join(PAGES["terminal"]["assets"], name), ctype)
            return
        # ?xterm=true overrides whichever page this process was started with —
        # the console's injected "Preview terminal" button reaches it this way.
        page = PAGES["terminal"] if _wants_xterm(self.path) else PAGE
        index_path = page["index"]
        try:
            html = open(index_path, encoding="utf-8").read()
            # index.html carries /*SHARED_CSS*/ and /*SHARED_JS*/ markers that
            # the real server (internal/webui.Build) fills in from these same
            # files before minifying; mirror that here so the preview matches
            # what actually ships.
            html = html.replace("/*SHARED_CSS*/", open(SHARED_CSS, encoding="utf-8").read(), 1)
            html = html.replace("/*SHARED_JS*/", open(SHARED_JS, encoding="utf-8").read(), 1)
        except OSError as e:
            self.send_error(500, f"cannot read {os.path.basename(index_path)}: {e}")
            return
        stub = ""
        if page["stub"]:
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

    def _handle_ws(self):
        if (self.headers.get("Upgrade", "").lower() != "websocket"
                or not self.headers.get("Sec-WebSocket-Key")):
            # checkSession() in the terminal page does a plain GET here to
            # tell "the session expired" (401) apart from "the network
            # blipped" whenever the socket drops. This preview has no
            # sessions, so a harmless 200 keeps that probe honest.
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        self.send_response(101, "Switching Protocols")
        self.send_header("Upgrade", "websocket")
        self.send_header("Connection", "Upgrade")
        self.send_header("Sec-WebSocket-Accept", _ws_accept(self.headers["Sec-WebSocket-Key"]))
        self.send_header("Sec-WebSocket-Protocol", "sparkbox.terminal.v1")
        self.end_headers()
        self.close_connection = True  # this connection is a raw frame stream now
        _fake_pty(self.rfile, self.wfile)

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
    host = os.environ.get("HOST", "127.0.0.1")
    # Threaded: a /ws connection blocks its own thread for as long as the fake
    # shell stays open, and a plain HTTPServer would stall every other request
    # (favicons, /vitals) behind it.
    srv = http.server.ThreadingHTTPServer((host, port), Handler)
    print(f"sparkbox page preview → http://localhost:{port}")
    print(f"  serving {os.path.relpath(INDEX)} with mock data (edit + refresh to iterate)")
    print(f"  dark: http://localhost:{port}/?theme=dark   light: http://localhost:{port}/?theme=light")
    print(f"  xterm preview: http://localhost:{port}/?xterm=true  (fake PTY, try `clip`)")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        print("\nbye")


if __name__ == "__main__":
    main()
