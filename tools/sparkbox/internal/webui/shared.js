// sparkbox shared console helpers: DOM lookup, toast, relative time, GB and
// byte formatting, HTML escaping, and the running/paused/archived state badge.
// Shared between the operator console (internal/console) and the user
// console (internal/userconsole) — inlined into each page's IIFE.
const $ = (id) => document.getElementById(id);

function toast(msg, isErr) {
  const t = $("toast");
  t.textContent = msg;
  t.className = "show" + (isErr ? " err" : "");
  clearTimeout(t._h);
  t._h = setTimeout(() => (t.className = ""), 2600);
}

function rel(iso) {
  if (!iso) return "–";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return Math.floor(s) + "s ago";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
}

function gb(mb) {
  const g = (mb || 0) / 1024;
  return g.toFixed(g && g % 1 ? 1 : 0);
}

// bytes renders a lifetime byte counter. These span kilobytes on a box that
// just booted to hundreds of gigabytes on a long-lived one, so the unit has to
// float; one decimal below 10 keeps "1.4 GB" from collapsing to "1 GB".
function bytes(n) {
  if (typeof n !== "number" || n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i > 0 && n < 10 ? n.toFixed(1) : Math.round(n)) + " " + units[i];
}

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function badge(state) {
  // running | paused | archived (rootfs parked in object storage, 0 host disk)
  const cls = state === "running" ? "running" : (state === "archived" ? "archived" : "paused");
  return '<span class="badge ' + cls + '"><span class="dot"></span>' + cls + "</span>";
}

// createPoller runs fn on a schedule, one cycle at a time: the next cycle is
// armed only after the current one settles, so a slow response can never
// overlap with the next tick the way a bare setInterval(fn, 4000) can. A
// failure backs the delay off exponentially up to opts.max; a hidden tab stops
// scheduling until it is shown again. This generalizes the poller
// internal/xterm/index.html's vitals strip already used (scheduleVitals /
// pollVitals there) so the two consoles can have the same properties instead
// of their own bare setInterval with none of them.
//
// fn(signal) must return a promise, and should pass signal into every fetch it
// makes: that is what lets stop() actually cancel an in-flight cycle instead
// of leaving it to land after the poller has moved on. A rejection whose
// e.name is "AbortError" is treated as a cancellation, not a failure — it does
// not feed the backoff.
//
// opts.interval is the base delay in ms (default 4000), opts.max the backoff
// ceiling (default 60000).
function createPoller(fn, opts) {
  opts = opts || {};
  const base = opts.interval || 4000;
  const max = opts.max || 60000;
  let delay = base;
  let timer = null;
  let ctrl = null;
  let stopped = true;

  function schedule(ms) {
    clearTimeout(timer);
    timer = null;
    if (stopped || document.hidden) return;
    timer = setTimeout(cycle, ms === undefined ? delay : ms);
  }

  function cycle() {
    const mine = (ctrl = new AbortController());
    return fn(mine.signal)
      .then(() => { delay = base; })
      .catch((e) => {
        if (e && e.name === "AbortError") return;
        delay = Math.min(max, Math.max(delay, base) * 2);
      })
      .finally(() => {
        if (ctrl === mine) ctrl = null;
        schedule();
      });
  }

  // A hidden tab is not watching, so scheduling pauses — but a cycle already
  // in flight when the tab hides is left to finish rather than aborted: that
  // matches the vitals poller's own choice (see its comment on vPrev) and
  // means state a fresh cycle would otherwise have to rebuild from scratch,
  // like an open egress panel's own fetch, is not thrown away for nothing.
  // Coming back, a cycle already running from just before the hide is left to
  // its own reschedule rather than started again on top of itself.
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) { clearTimeout(timer); timer = null; return; }
    if (!stopped && !ctrl) { delay = base; schedule(0); }
  });

  return {
    // start begins polling and returns the first cycle's promise, so
    // `await poller.start()` reads the same as a bare `await refresh()` did.
    start() { stopped = false; delay = base; return cycle(); },
    // stop ends polling for good — leaving the app view, signing out — and
    // aborts whatever cycle is currently in flight rather than letting a
    // response for a page the visitor already left land and paint anyway.
    stop() {
      stopped = true;
      clearTimeout(timer);
      timer = null;
      if (ctrl) ctrl.abort();
    },
    // run triggers one cycle right now, through the same overlap guard a
    // scheduled tick uses: if a cycle is already in flight this is a no-op,
    // rather than starting a second one racing it. Call sites that used to
    // fire a bare refresh() after a mutation succeeds should call this
    // instead, so an action-triggered refresh can never race a poll tick.
    run() {
      if (ctrl) return Promise.resolve();
      clearTimeout(timer);
      timer = null;
      return cycle();
    },
  };
}
