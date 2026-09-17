// Tests for the note merge in template.html. The page is one self-contained
// file with no build step, so the merge is lifted straight out of it by name
// rather than imported — which also means these tests exercise the code that
// actually ships, not a copy that can drift from it.
//
//     node test-merge.js
//
// The cases that matter are the ones that were live bugs: a note reached rev 47
// in the real database holding four copies of itself, so most of what follows is
// about text that must NOT reappear.

const fs = require("fs");
const path = require("path");

const src = fs.readFileSync(path.join(__dirname, "template.html"), "utf8");
const from = src.indexOf("  function splitLines(");
const to = src.indexOf("\n  // Replace a textarea's text");
if (from < 0 || to < 0) throw new Error("merge section not found in template.html");
const merge3 = new Function(src.slice(from, to) + "\n return merge3;")();

let failed = 0;
function eq(name, got, want) {
  const ok = got === want;
  if (!ok) failed++;
  console.log(`${ok ? "ok  " : "FAIL"} ${name}`);
  if (!ok) console.log(`       got  ${JSON.stringify(got)}\n       want ${JSON.stringify(want)}`);
}
function m(base, mine, theirs) { return merge3(base, mine, theirs).text; }

// --- the plain cases ---------------------------------------------------
eq("nobody moved", m("a", "a", "a"), "a");
eq("only they moved", m("a", "a", "a\nb"), "a\nb");
eq("only I moved", m("a", "a\nb", "a"), "a\nb");
eq("same edit both sides", m("a", "a\nb", "a\nb"), "a\nb");

// --- what corrupted the live notes -------------------------------------
// The ancestor is lost or rewound, so my own text comes back as if it were
// someone else's addition. The old merge appended it a second time.
eq("stale ancestor, they have mine plus more", m("", "A", "A\nB"), "A\nB");
eq("stale ancestor, I have theirs plus more", m("", "A\nB", "A"), "A\nB");
eq("their copy is an old prefix of mine",
   m("", "one\ntwo\nthree", "one\ntwo"), "one\ntwo\nthree");
// A write still on the wire is cut wherever the typist had got to.
eq("their copy was cut mid-word",
   m("", "drugačija - feminine\ndrugačije - neutral", "drugačija - feminine\ndruga"),
   "drugačija - feminine\ndrugačije - neutral");
eq("a short line is not treated as a truncation of a longer one",
   m("", "da", "dakle - so"), "da\ndakle - so");

// --- two people, genuinely different text ------------------------------
eq("both appended, kept once each", m("", "x", "y"), "x\ny");
eq("both appended to a shared start",
   m("note", "note\nmine", "note\ntheirs"), "note\nmine\ntheirs");
eq("a real conflict is flagged", merge3("", "x", "y").conflict, true);
eq("absorbing what they already have is not", merge3("", "A\nB", "A").conflict, false);

// --- convergence: the store, two browsers, and a lot of racing ---------
//
// The unit cases above say what one merge does. What went wrong in the field was
// a loop: merge, write, poll, merge again. So this runs the real shape of it —
// one row behind a compare-and-swap, two clients that edit, save and poll in an
// interleaved order — and asserts the only two things that actually matter: they
// all end up agreeing, and nothing anybody typed appears twice.

function makeStore() {
  return {
    text: "", rev: 0,
    // Exactly what the SQL does: the update carries `where rev = ?`, and the
    // same round trip reads the row back either way.
    save: function (text, baseRev) {
      if (baseRev !== this.rev) return { ok: false, remote: { text: this.text, rev: this.rev } };
      this.text = text; this.rev++;
      return { ok: true, rev: this.rev };
    },
    read: function () { return { text: this.text, rev: this.rev }; }
  };
}

function makeClient(store) {
  return {
    text: "", base: "", baseRev: 0,
    type: function (line) { this.text = this.text ? this.text + "\n" + line : line; },
    absorb: function (remote) {
      if (remote.rev < this.baseRev) return;          // a read older than what we know
      const merged = merge3(this.base, this.text, remote.text);
      this.text = merged.text;
      this.base = remote.text; this.baseRev = remote.rev;
    },
    poll: function () { this.absorb(store.read()); },
    save: function () {
      for (let i = 0; i < 4; i++) {
        const res = store.save(this.text, this.baseRev);
        if (res.ok) { this.base = this.text; this.baseRev = res.rev; return; }
        this.absorb(res.remote);
      }
    }
  };
}

function noRepeats(name, text) {
  const seen = {}, dupes = [];
  text.split("\n").forEach(function (l) {
    if (!l.trim()) return;
    if (seen[l]) dupes.push(l);
    seen[l] = true;
  });
  eq(name, dupes.join(","), "");
}

// Both type, both save; then each polls and saves again until it all settles.
{
  const store = makeStore();
  const me = makeClient(store), tutor = makeClient(store);
  me.type("pišem - ongoing");
  tutor.type("napišem - completed");
  me.save(); tutor.save();
  for (let i = 0; i < 4; i++) { me.poll(); me.save(); tutor.poll(); tutor.save(); }
  eq("two writers agree", me.text, tutor.text);
  eq("and the store agrees", store.text, me.text);
  noRepeats("neither line is repeated", store.text);
  eq("both survived", /pišem - ongoing/.test(store.text) && /napišem - completed/.test(store.text), true);
}

// The live failure: a poll carrying a copy from before my last write. The rev
// guard drops it; if it ever gets through, the merge must still absorb it.
{
  const store = makeStore();
  const me = makeClient(store);
  me.type("mrak - darkness"); me.save();
  const stale = store.read();                       // snapshot taken here
  me.type("svetlo - light"); me.save();
  me.absorb(stale);                                 // ...and delivered late
  eq("a late poll does not rewind", me.text, "mrak - darkness\nsvetlo - light");
  me.save();
  noRepeats("and leaves no second copy", store.text);
}

// Hammer it: random interleavings of typing, saving and late polls, the way a
// lesson actually goes. Every line typed is unique, so a line appearing twice in
// the store can only have been put there by a merge — and nothing that once
// reached the store is allowed to fall out of it again.
{
  let seed = 7;
  const rnd = function (n) { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed % n; };
  let bad = "";
  for (let run = 0; run < 50 && !bad; run++) {
    const store = makeStore();
    const clients = [makeClient(store), makeClient(store)];
    const snapshots = [];
    const landed = {};
    let n = 0;
    const record = function () {
      store.text.split("\n").forEach(function (l) { if (l.trim()) landed[l] = true; });
    };
    for (let step = 0; step < 30; step++) {
      const c = clients[rnd(2)];
      switch (rnd(4)) {
        case 0: c.type("line-" + (++n)); break;
        case 1: c.save(); record(); break;
        case 2: snapshots.push(store.read()); break;
        case 3: if (snapshots.length) c.absorb(snapshots[rnd(snapshots.length)]); break;
      }
    }
    for (let i = 0; i < 6; i++) clients.forEach(function (c) { c.poll(); c.save(); });
    record();
    const lines = store.text.split("\n").filter(function (l) { return l.trim(); });
    if (clients[0].text !== clients[1].text || clients[0].text !== store.text) {
      bad = "run " + run + " disagreed: " + JSON.stringify([clients[0].text, clients[1].text, store.text]);
    } else if (new Set(lines).size !== lines.length) {
      bad = "run " + run + " duplicated: " + JSON.stringify(store.text);
    } else {
      Object.keys(landed).forEach(function (l) {
        if (lines.indexOf(l) < 0) bad = "run " + run + " lost " + l + ": " + JSON.stringify(store.text);
      });
    }
  }
  eq("fifty random lessons settle, with nothing duplicated or lost", bad, "");
}

// --- shape of the text -------------------------------------------------
eq("blank lines survive", m("", "a\n\nb", "a\n\nb"), "a\n\nb");
eq("blank runs do not accumulate", m("", "a\n\nb", "c\n\nd"), "a\n\nb\nc\n\nd");
eq("indentation is kept", m("", "  a", "b"), "  a\nb");
eq("a line named like a property is still a line",
   m("", "constructor", "__proto__"), "constructor\n__proto__");
eq("empty against text", m("", "", "b"), "b");

console.log(failed ? `\n${failed} failed` : "\nall passed");
process.exit(failed ? 1 : 0);
