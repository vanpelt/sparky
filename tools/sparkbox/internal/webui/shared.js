// sparkbox shared console helpers: DOM lookup, toast, relative time, GB
// formatting, HTML escaping, and the running/paused/archived state badge.
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

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function badge(state) {
  // running | paused | archived (rootfs parked in object storage, 0 host disk)
  const cls = state === "running" ? "running" : (state === "archived" ? "archived" : "paused");
  return '<span class="badge ' + cls + '"><span class="dot"></span>' + cls + "</span>";
}
