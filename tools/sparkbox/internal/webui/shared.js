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
