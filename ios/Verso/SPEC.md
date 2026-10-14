# Verso — Build Spec (v1)

> A private iOS app for one reader. It helps him plot Book Two: what Book One (the job) still pays, what Book Two (the threads) already has going, and when the page should turn.
>
> **Privacy rule (non-negotiable):** this repo is PUBLIC. No real share count, salary, bonus, price snapshot or net-worth figure may appear in any committed file: source, tests, previews, fixtures, docs or screenshots. Real values reach the app only through in-app entry or the gitignored seed file (§10). Every number in this spec is fake.

---

## 1. Name

**Verso**. The verso is the left-hand page, the side of the leaf you only see once you turn it. The word comes from Latin *vertere*, "to turn". The whole app is about one turn: from Book One to Book Two.

- Two of the three concepts chose this name on their own, which suggests it is the right one. It is five letters, reads well as a Home Screen label and a widget title, and does not sound like a productivity app.
- Bundle id `$(VERSO_ID)` (set once in project.yml). App Group `group.$(VERSO_ID)`. URL scheme `verso://`. Folder `ios/Verso/`.

**Vocabulary** (used consistently in UI copy and code):

| Term | Meaning |
|---|---|
| Book One | The current job. |
| Chapter | One vest tranche. Chapters are numbered with Roman numerals I–XIII, and he can give each one a title. |
| Book Two | The life after. |
| Thread | One interest or goal (fitness, a short film, books, Serbian). |
| Marginalia | The weekly check-in. |
| The Turning Point | The clarity screen: his own conditions plus the evidence. |
| Clause | One of his "I go when…" conditions. |
| Colophon | Settings. |
| Recto | The mode the app enters after he closes Book One. |

### Creative direction decisions

The base is **Concept 1, Verso as a typeset novel**. It has the strongest and most ownable visual identity, and its book vocabulary maps exactly onto his own framing.

**Grafted in from the other concepts:**
- From MECO:
  - **Crossover price**: the CRWV price at which staying stops beating his Enough Line.
  - **Launch window = vest day + 1**: copy pointing out that the honest exit days are the days right after a vest.
  - **"Regardless" clauses**: "if X, I go regardless" (health, a bad reorg).
  - **The doubled-stock prompt**.
  - **Drift view**: every Marginalia save snapshots the board, so the drift over months can be seen.
- From Concept 3:
  - **EWMA plus hysteresis** smoothing of the pulse, so that one bad week cannot swing the reading.
  - **The Ebb fraction**: "this chapter adds 1/6 to all of Book One you'll have banked". This is the honest diminishing-returns signal.
  - **The sealed letter**.
  - **Veil-by-default for money**.
  - **Anchor reasons** as a single optional chip row.

**Cut:**
- The once-a-day cold open. It slows down every launch. A title card now plays only on first launch, on vest day and on Close Book One.
- The 3D page curl.
- The film-grain Metal shader.
- Seasonal tints.
- Sound.
- Shake gestures.
- The guarded flip switch.
- Live Activities.
- The Opening-Line widget. Opening lines still live in the app.
- Callsigns.
- The log-utility curve, because it needs a net-worth input.
- Rotary knobs. They are replaced by one-tap 1–5 rows, which are faster.

The stretch goal, a Control Center control, is listed in §9.5 and is optional.

---

## 2. Vibe and visual system

**Feeling:** a beautifully set novel crossed with a film title sequence. It should feel like Saul Bass restraint on a Penguin Modern Classic.

- Typography comes first and there is almost no chrome.
- Content sits directly on paper.
- **Liquid Glass is used only for floating chrome**: the tab bar, toolbars and the floating ✒︎ Log button. Think of a glass paperweight resting on a manuscript.
- Do not use: cards with drop shadows, rows of SF Symbols, stacks of progress bars, or emoji.

### 2.1 Color tokens (`Core/Design/Palette.swift`)

Each token is defined once and has a light value ("Day Edition") and a dark value ("Night Reading"). The app follows the system color scheme. Dark mode is a lamp-lit page, never pure black.

| Token | Light | Dark | Use |
|---|---|---|---|
| `paper` | `#F2ECDF` | `#141210` | page background |
| `deckle` | `#E6DDCB` | `#1F1C18` | inset panels, wells, the ribbon track |
| `rule` | `#CDBFA8` | `#3A322B` | 0.5pt hairlines and ledger rules |
| `ink` | `#1C1A17` | `#E8E0CF` | primary text |
| `fadedInk` | `#6E665A` | `#8C8375` | secondary text, running heads, vested rows |
| `oxblood` | `#7B2121` | `#D4673F` | Book One accent: money, chapters, the bookmark ribbon |
| `gilt` | `#B08D3C` | `#C9A55A` | rare: vest events, met clauses, the Turning Point; never body text |
| `verdigris` | `#3F6B5F` | `#6FA394` | Book Two accent: threads, momentum |
| `rust` | `#A3452F` | `#D0694E` | stale price, warnings (rare) |
| `dusk` | `#4A3A55` | `#8E7AA0` | the sealed letter |

Each thread picks one of six **binding** colors. The same hex is used in both modes, and in dark mode it is drawn at 90% brightness:

| Binding | Hex |
|---|---|
| `oxblood` | `#7B2121` |
| `navy` | `#24324A` |
| `forest` | `#2F4A3A` |
| `ochre` | `#A9782B` |
| `plum` | `#4E2E48` |
| `slate` | `#4A5056` |

When a binding color sits on dark paper, its ring stroke uses a lighter variant (+25% lightness) so it stays legible.

### 2.2 Typography (`Core/Design/Type.swift`)

All of these fonts ship with iOS, so nothing is bundled.

| Role | Font | Notes |
|---|---|---|
| Display / headings | New York, `Font.system(..., design: .serif)` | Running heads are small caps: `.smallCaps()` plus `.tracking(1.5)` at 11–12pt, e.g. "BOOK ONE · CHAPTER VI". |
| Hero numerals | **Didot**, `Font.custom("Didot", size:)` at 64–120pt | Used for the countdown and the day rate. Fallback is `"BodoniSvtyTwoITCTT-Book"`, then New York. Always add `.monospacedDigit()` and `.contentTransition(.numericText())`. |
| Body / marginalia | **Iowan Old Style**: `Font.custom("IowanOldStyle-Roman")` and `"IowanOldStyle-Italic"` | Italic is reserved for *his* words: epigraphs, Marginalia sentences, clause text, opening lines. |
| Ledger figures | New York + `.monospacedDigit()` | Tabular columns line up like an old accounts book. |
| Slug | SF Mono, 10pt | Only in the Colophon's typesetter slug. |

Rules:
- Use Roman numerals for chapters.
- Format numbers with `.formatted(.number.grouping(.automatic))`.
- Use typographic ornaments as glyphs: ❦ fleuron, ⁂ asterism, § and ¶. The section divider is `DinkusRule`: a 0.5pt hairline with a centered ⁂.
- Every custom font goes through a `relativeTo:` text style so it scales with Dynamic Type. Hero numerals are capped at 1.3× scale.

### 2.3 Layout

- The side gutter is at least 20pt, and up to 28pt on Pro Max.
- Every top-level screen has:
  - a **running head** at the top, e.g. "BOOK ONE · CHAPTER VI · ❦", in fadedInk small caps;
  - a **folio** at the bottom, e.g. "p. 412". The folio is the number of days in Book One since `bookOneStart`. If that date is not set, it counts days since first launch.
- Text is ragged-right. Hero numerals are centered, like a title page.
- There are no cards. Sections are separated by `DinkusRule` and whitespace. Inset panels use a `deckle` fill with no border and no shadow.

### 2.4 Motion

- **Rolling digits:** every number changes with `.contentTransition(.numericText())` inside `withAnimation(.smooth(duration: 0.5))`.
- **Ink-bleed reveal:** a hero number appears from `blur(6) + opacity 0` to sharp over 0.45s. The same effect plays when the veil lifts.
- **Pen-stroke underline:** when a thread logs progress, a 1.5pt binding-color line draws under its figure from left to right. It is a `Path` trim from 0 to 1 over 0.35s, easeOut.
- **Title cards:** a full-screen sequence of about 1.6s that the reader can tap to skip. It plays only in three cases:
  1. **First launch:** "VERSO", then "BOOK ONE".
  2. **The first open on or after a vest date:** "END OF CHAPTER V ❦", then the next chapter's title.
  3. **Close Book One:** "THE END", a blank beat, then "BOOK TWO · Page 1".
- **Navigation:** the standard push. Use `.navigationTransition(.zoom(sourceID:in:))` from a chapter row or thread spine into its detail page. Sheets use `.presentationDetents`.
- **Reduce Motion:** everything becomes a 0.2s cross-dissolve and the blur reveal is skipped.

### 2.5 Haptics (`Core/Design/Haptics.swift`)

| Moment | Haptic |
|---|---|
| Log +1 / tap a Marginalia scale | `.sensoryFeedback(.selection, trigger:)` |
| Hit a count target / finish a milestone | `.success` |
| Clause flips to met | `.success`, plus a gilt shimmer on the row (a 0.8s gradient sweep) |
| Vest-day title card | CoreHaptics "book closing": three transients at intensity 0.9 / 0.6 / 0.35, sharpness 0.3, spaced 0.18s apart |
| Marginalia saved | `.impact(weight: .medium)`, then 60ms later `.impact(weight: .light)` (the "stamp") |
| Close Book One commit | the vest pattern, followed by a 0.6s continuous ramp |

### 2.6 Iconography and app icon

**SF Symbols** appear only in the tab bar and toolbars, always at `.light` weight:
- `book.closed` (Contents)
- `text.book.closed` (Ledger)
- `scribble` (Threads)
- `scope` (Turning Point)
- `pencil.tip` (Log)

Everything else is typographic. The **bookmark ribbon** is a custom `Shape`: an oxblood vertical tab with a V-notched tail. It marks the current chapter.

**App icon** (an Icon Composer `.icon` file, or an asset catalog with light, dark and tinted variants):
- **Light:** a cream paper square. The bottom-right corner is folded over, showing an oxblood verso underneath, and a gilt Roman "II" is embossed on the fold.
- **Dark:** board black with the fold in ember.
- **Tinted:** the fold silhouette plus "II".
- **Liquid Glass layering:** paper is the back layer and the fold is the foreground glass layer.
- For v1, an SVG-derived PNG at 1024 is enough.

---

## 3. Information architecture

```
TabView (Liquid Glass, .tabBarMinimizeBehavior(.onScrollDown))
├─ Contents        (home)            verso://contents
│   ├─ Chapter Detail                verso://chapter/<index 0-12>
│   └─ Colophon (❦ in running head)  verso://colophon
├─ Ledger                            verso://ledger
├─ Threads                           verso://threads
│   └─ Thread Detail                 verso://thread/<uuid>
└─ Turning Point                     verso://turning
Global sheets: Marginalia (verso://marginalia), Quick Log (verso://log/<uuid>)
Floating glass ✒︎ button (bottom-trailing, above tab bar) → Quick Log thread picker; long-press → Marginalia
```

There are seven screens: Contents, Chapter Detail, Ledger, Threads (with Thread Detail), Turning Point, Marginalia and Colophon. First-run onboarding is three cards on top of Contents, not a separate screen.

A global **Veil** setting is on by default. While it is on, every dollar figure renders as `·····` (see `MoneyText` in §11). A two-finger tap on any figure, or the eye toggle in the running head, lifts the veil for the session. Countdowns, fractions, rings and gauges are never veiled.

---

## 4. Screens

### 4.0 First run ("Title page")

This is a three-card overlay. Each card can be skipped.

1. **The epigraph.** "Write one line to open Book Two." It is stored as `meta.epigraph`.
2. **The grant.**
   - If the store is empty and a bundled seed is present, it is imported silently and the card reads "Grant loaded from local seed ✓".
   - Otherwise it shows a form with empty fields, plus a "Use sample numbers" button. Sample grants set `isSample = true`, and Contents then shows a fadedInk ribbon: "Sample figures. Set yours in the Colophon."
3. **Threads.** Pick up to four starting threads from presets: Move (habit, 4/week), Read 12 books (count), Make a short film (project with the default milestones), Learn Serbian (practice, 120 min/week). Then write the first clause: "I'll know it's time when…" This becomes a manual clause, which he can retype later.

### 4.1 Contents (home)

**Running head:** "BOOK ONE · CHAPTER VI · ❦". Tapping ❦ opens the Colophon. Long-pressing ❦ shows a random line from his own Opening Lines (if he has written at least three) or from a small bundled list of first lines.

**Title-page hero:**
- The current chapter number in Didot Roman numerals at 96pt. The "current chapter" is the next unvested tranche.
- His chapter title, in New York italic, editable inline. The default is empty, in which case only the numeral shows.
- The countdown in Didot at 120pt, e.g. "37", with a small caps "DAYS" below it. On the last day it shows `Text(vestDate, style: .relative)` instead.
- One line: "until the Nov 20 vest · ≈ $786k after tax" (veiled by default).
- A **ribbon bar**: a hairline track with an oxblood fill showing `chapterProgress` (§5.4).

**PART ONE · Book One:** a leader-dot table of contents, one row per tranche:
```
I.    The Cliff ................ May '26        ✓
II.   .......................... Aug '26        ✓
VI.   The Long Autumn .......... Nov '26  $786k   ◀ ribbon
...
XIII. .......................... May '29  $786k
```
- Vested rows are drawn in fadedInk with a hairline strikethrough.
- The current row carries the ribbon.
- Values are right-aligned after-tax amounts in tabular figures, and they are veiled when the Veil is on.
- Tapping a row opens Chapter Detail.
- The list shows the current chapter, the next three and the last one. A "· · · all thirteen" row expands the rest.

**PART TWO · Book Two, in draft:**
- Up to five thread **spines**: a 44pt-wide cloth-colored vertical bar with the thread name in small caps rotated 90°, its progress figure and a 28-day momentum tick (§6.3).
- Tapping a spine opens Thread Detail.
- **Long-pressing a spine logs its default amount immediately**, with a selection haptic and the pen underline. Concretely, it uses `.onLongPressGesture(minimumDuration: 0.35)`.

**The Turning Point line:** one row showing the gauge glyph (§6.5, the tick arc at 28pt), the stage word in italic, and "3 of 5 clauses". Tapping it switches to the Turning Point tab.

**Footer epigraph:** the most recent Marginalia sentence in Iowan italic, attributed "— you, Sunday". If there is none, `meta.epigraph` is shown.

**Due banner:** if Marginalia is due (Sunday after 17:00, or 7 or more days since the last one), a single fadedInk line appears above the footer: "Marginalia — five lines for the week ›".

**Folio** at the bottom.

### 4.2 Chapter Detail

**Title page:**
- "CHAPTER XI" in Didot.
- The editable title (italic).
- An editable epigraph (Iowan italic, centered).

**§ What this chapter pays** (a ledger table):

| Row | Columns |
|---|---|
| Revest | shares, pre-tax, after-tax |
| RSU | shares, pre-tax, after-tax |
| Salary & bonus over this chapter's `gap` days | after-tax |
| Total | after a double rule |

**§ What staying for it costs:** `gap` days, and `chapterRate` per day.

**§ Its weight:** two fractions in Didot.
- "adds **1⁄6** to all of Book One you'll have banked". This is `ebbWeight`.
- "**1⁄8** of what's still at stake". This is `shareOfRemaining`.
- Render each as the nearest unit fraction 1/n: n = round(1/x).

**§ Notes:** a free-text field: "What do you want to have done by the end of this chapter?"

**Vested chapters:** show "Read." in place of the countdown, plus `priceAtVest`. When a vest passes, the app snapshots `priceAtVest` from the cached price at the first open on or after the vest date; he can also enter it by hand.

### 4.3 The Ledger

**Running head:** "THE LEDGER · CRWV $—.— ·".
- The trailing `·` pulses gilt when the price is fresh (under 5 minutes old). It turns rust with an age label, e.g. "· 3h", when the price is stale (more than 1 trading day) or only cached.
- Tapping the price opens a small sheet showing the source, its age, a manual override field (with on/off), and "Refresh". Pull-to-refresh on the screen also refetches.

**Hero:**
- "You are paid ≈ **$9,461** a day to keep writing Book One." The number is in Didot.
- Two segmented toggles below it: `after tax | pre-tax` and `per day | per working day`.
- Below that, the composition: "equity $9,121 · pay $340".

**§ Remaining in Book One:** a balance-sheet table with columns Shares · Pre-tax · After-tax and rows Revest, RSU, Salary & bonus (through May '29), then a double rule and **Total**.
- Under the table, set apart: "Rollover grant · already yours · vests 2026 regardless", with its value. It is excluded from every "walking away" figure.

**§ Next chapter:** the date, the countdown, its after-tax value, and `nextRate`, with one explanatory line: "This rises as the vest gets closer: the same lump spread over fewer days. The cheapest days to leave are the days just after a vest." The **next launch window** is the next vest date + 1 day.

**§ The Last Page (scrubber):**
- A horizontal strip of chapter numerals for the unvested chapters, plus "Now" at the start. It snaps with a selection haptic on each detent.
- A sentence reads the choice back: "Ending after Chapter IX keeps **$A**, leaves **$B** on the table, and gives you back **912** days, at **$C** a day."
- It uses the formulas in §5.6.

**§ The marginal chapter (chart):** built with Swift Charts.
- A thin oxblood `BarMark` for each remaining chapter showing `chapterValue`. The bars are about flat.
- A verdigris `LineMark` for `ebbWeight`, falling.
- Caption: "Every chapter pays about the same. Each one adds a smaller share of everything you'll have."
- The bars carry no axis labels, so the Veil can leave the chart visible.

**§ Against your Enough Line:** this section appears only once `enoughLinePerDay` is set.
- "Above the line by $X/day" or "Below the line".
- **Crossover price:** "If CRWV were below **$29.16**, staying would pay you less than your Enough Line." If pay alone already exceeds the line, the copy reads "Your salary alone clears your Enough Line; the stock can't push you below it."

**§ What-if price:** a slider from −50% to +50% of the live price.
- Every Ledger number re-renders live.
- A rust "HYPOTHETICAL $X" chip appears while it is moved. Tapping the chip resets it.
- The what-if is never persisted and never reaches widgets.

**Footer** in small fadedInk italic: "Not tax advice. Today's price, your rates. Price held flat — futr is the deep model."

### 4.4 Threads

**List:** one row per active thread, sorted by `sortIndex` and reorderable.
- A binding-color spine strip, 6pt wide, on the leading edge.
- The name in New York.
- A kind glyph in small caps: COUNT / HABIT / PROJECT / PRACTICE.
- A progress figure and a ring (§6.3).
- "last line 3 days ago".
- A tiny 6-week tick strip: one dash per week, filled when the week met its pace.

**Logging (the critical path; it must be one gesture):**
- **Swipe right** on a row: log the default amount. It uses a leading `swipeActions` with `allowsFullSwipe: true`.
  - count → +1
  - habit → done today
  - practice → `defaultAmount` minutes
  - project → opens the milestone list
- **Tap ✒︎**: the Quick Log sheet at `.height(260)` detent.
  - It contains a stepper prefilled with the default and a one-line note. Return saves and dismisses.
  - For a count thread there is an optional "title" field (book name).
  - The sheet is reachable from the floating button and from every row.
- **Widget +1 button and Siri / Shortcuts** use the same path: `LogThreadIntent` (§9.4).

**Thread kinds:**
- **count:** `target` plus an optional `deadline`. Ring = total/target. Optional list of item titles.
- **habit:** `perWeek`. Shows a 7-dot week strip (Monday start). The streak is counted in **weeks** that met the target, which is more forgiving than days. Ring = this week's count / perWeek.
- **project:** ordered milestones. They are shown as a mini table of contents with leader dots, and a checked milestone gets the strike treatment. Ring = done/total. "next scene: Script draft". Default film milestones: Logline → Script draft → Shot list → Shoot day → Edit → Screening.
- **practice:** `unit` (min or sessions) plus `weeklyTarget`. Ring = this week / target. Streak in weeks.
  - For a thread named like Serbian, the subtitle shows in Cyrillic, "Српски", and taps to switch to Latin. This is driven by an optional `subtitle` field, not by detecting the name.
  - The note field on the log sheet has the placeholder "reč nedelje…".
  - Clozemaster/klozar data is entered by hand only; there is no coupling.

**Thread Detail:**
- A title page with the name and the **logline** (italic): "why this matters for Book Two".
- A large ring and figure.
- Entries shown as marginalia: the date in small caps and the note in italic. Swipe to delete or edit an entry.
- "Used by clauses: …"
- Edit and archive actions. Archived threads go to a "Loose ends" section at the bottom of the list.

**+ New thread:** a short form with Name, Kind (and its target fields), Logline, Binding color (six swatches) and Default amount.

### 4.5 The Turning Point (the heart)

**Running head:** "THE TURNING POINT".

**Hero:**
- The tick-arc gauge (§6.5), 220pt wide.
- The stage word below it in New York italic at 34pt, e.g. "Rising Action".
- "3 of 5 clauses · holding 2".

**§ You'll know it when —**
- The clauses, one row each, with a drop-cap number.
- Each row shows his text in italic, a kind hint in small caps (e.g. "DATE", "THREAD · Read 12 books 7/12", "PULSE 4 of 6 wks"), and a state mark: ○ unmet, ● met, ❦ holding (in gilt).
- Required clauses carry a small "§". Regardless clauses carry "‼︎" and sit in their own sub-list titled "Or, regardless —".
- Manual clauses toggle on tap.
- Tapping any other clause shows its evidence and its threshold, editable inline.
- The **Revisions** disclosure lists every dated edit, like tracked changes: "Changed 'for 6 weeks' → 'for 10 weeks' · 12 Jan".

**§ The stay case:** shown directly opposite the clauses. Its prompt is "What would make Book One worth another year?", and it holds his free text. It is always visible, never hidden.

**§ The Reading:** the deterministic paragraph from §6.6, ending in one rotating question.

**§ Pulse:**
- A 12-week chart with two ink lines: Toward = mean(Pull, Restlessness), in verdigris, and Away = mean(Energy, Anchor), in oxblood. The gap between them is shaded.
- The first week Toward's smoothed line crossed above Away's carries a small italic "the X".
- Missed weeks are gaps, never zeros or interpolated values.
- Below the chart is the pulse state word (§6.2).
- Then the anchor reasons from the last 8 weeks, ranked, e.g. "the team ×4, the money ×3". If "the money" ranks first, the copy pairs it with the Ebb: "You named the money 3 times; the next chapter adds 1⁄6."

**§ Drift:** one row per Marginalia snapshot, as a horizontal strip of small stage glyphs over the months, with the week's one-word or first few words beneath each.

**§ Opening lines:**
- A drawer of dated drafts of "the first line of Book Two". Adding a new draft keeps the old ones.
- The latest draft is shown with a drop cap.

**§ Sealed letter:**
- "A letter to Book Two's first page". He writes it, then seals it.
- A sealed letter shows as a dusk-colored seal with "V" in the middle.
- It opens in one of three cases: the stage reaches **The Turning Point**; a chosen `opensOn` date arrives; or he holds the seal for 3 seconds.

**Bottom:** "Close Book One", in quiet fadedInk text.
- Step 1 asks for confirmation, a final date (default today) and a closing sentence.
- Step 2 is a 2-second press-and-hold.
- The commit sets `BookState.mode = .recto`.
- Nothing in the app ever says "go". Only he presses this.

### 4.6 Marginalia (the weekly sheet)

**Triggers:**
- A local notification on Sunday at 19:00 (configurable): "Marginalia — five lines for the week."
- The due banner.
- A long-press on the floating ✒︎ button.
- `verso://marginalia`.

**Layout:** four rows, each with a prompt and five tappable words. Tapping a word sets the value and moves focus to the next row. The whole sheet should take under 30 seconds.

| Field | Prompt | 1 → 5 |
|---|---|---|
| `energy` | How much did Book One give you this week? | spent · thin · even · full · alive |
| `anchor` | How strongly does it hold you? | loose · light · steady · firm · anchored |
| `pull` | How hard is Book Two pulling? | faint · murmur · tug · current · magnetic |
| `restlessness` | How restless are you? | settled · fidgety · itchy · pacing · caged |

**Anchor chips** (optional, multi-select): the team · the money · unfinished work · timing · fear · nothing. He can add a custom chip.

**Sentence:** "This week in one line:" The text is set in Iowan italic as he types. It becomes the Contents epigraph.

**Manual clauses:** if any manual clauses exist, a final compact list asks "Is this true yet?" with a toggle for each one.

**Save:** stamps the record with `weekOf` (the ISO week's Monday) and a snapshot of the board, then plays the stamp haptic and dismisses. If a record already exists for the week, it is updated, not duplicated.

### 4.7 Colophon (settings)

This page is typeset like a book's colophon. It opens with:

> *Set in New York, Didot & Iowan Old Style. Printed on CRWV at $—.— (StockAnalysis · 4 min) on 9 October 2026.*

Sections:

- **Grant:**
  - revest shares, RSU shares
  - rollover shares and their vest date (optional; if unknown, it is treated as "2026")
  - vest day of month (default 20)
  - the schedule: 13 editable rows of year / month / fraction, prefilled with the standard grant shape
  - Book One start date (drives the folio)
- **Taxes:**
  - revest rate (default 23.8%)
  - a "Revest sheltered by loss carryforward" switch: when it is on, the effective revest rate is 0, which matches futr's `trancheCash`
  - ordinary rate (37% + 13.3% = 50.3%), applied to RSUs, rollover, salary and bonus
- **Pay:** annual salary, annual bonus.
- **The Enough Line:** the $/day he considers worth staying for. The prompt reads: "What would you pay to have one Tuesday back?"
- **Price:** live / manual override. The override value persists until he clears it.
- **Privacy:** Veil by default (on), and "Show figures on Lock Screen" (off).
- **Marginalia:** weekday and time; notification on/off.
- **Data:**
  - seed status: "Imported from local seed · 9 Oct" or "No seed in bundle"
  - Re-import seed (confirmation required; it overwrites the grant only)
  - Export JSON (a `ShareLink` of a zip-free combined JSON)
  - Import JSON
- **Slug** in SF Mono: version and build, the App Group path, the time the snapshot was last written, and the time widgets were last reloaded.

### 4.8 Recto mode (after Close Book One)

- The running heads become "BOOK TWO · PAGE n", where n is the number of days since `closedAt`.
- On Contents, Part One collapses to one line: "Book One · 1,204 pages · closed 2 Mar 2027", followed by the closing sentence. Threads become the main text.
- The Ledger becomes "What Book One left you". It shows frozen vested totals, each using its `priceAtVest`, or the price at close if that is missing.
- The Turning Point becomes "The Turn": a read-only page with the clauses as they stood, the stay case, the sealed letter (opened), and the latest opening line.
- Marginalia keeps going with the same four scales and the labels re-worded for Book Two:
  - energy: "How much did Book Two give you?"
  - anchor: "How settled do you feel?"
  - pull: "How much momentum?"
  - restlessness: "How much doubt?"
- The tab bar keeps the same four tabs.

---

## 5. Money — exact formulas (`Core/Math/Ledger.swift`)

All functions are pure: `Ledger.compute(grant:price:today:calendar:) -> LedgerResult`. Time comes from `today = calendar.startOfDay(for: now)` in the local calendar, and every day count uses `calendar.dateComponents([.day], from:to:).day`. No price path is projected: every tranche, past or future, is valued at today's price `px`. futr does the projections; Verso does not.

### 5.1 Inputs

| Symbol | Source | Default |
|---|---|---|
| `REV` | `grant.revestShares` | — |
| `RSU` | `grant.rsuShares` | — |
| `ROLL` | `grant.rolloverShares`, vest date `dR` (optional) | — |
| `SCHED` | `grant.schedule`: 13 tranches `(y_i, m_i, f_i)`: (2026,5,.25), then .0625 at (2026,8), (2026,11), (2027,2), (2027,5), (2027,8), (2027,11), (2028,2), (2028,5), (2028,8), (2028,11), (2029,2), (2029,5). Σf = 1. | committed default |
| `vd` | `grant.vestDayOfMonth`; each tranche may override with `day` | 20 |
| `px` | the effective price: manual override if enabled, otherwise live/cached (§8) | — |
| `τR` | `revestSheltered ? 0 : taxRevest` | .238 |
| `τO` | `taxOrdinary` | .37 + .133 = .503 |
| `S`, `B` | annual salary and bonus (gross) | — |
| `E` | `enoughLinePerDay` (optional) | nil |

**Mirroring futr:**
- Revest shares are taxed at LTCG, and the "sheltered" switch reproduces `trancheCash`'s assumption that loss carryforward covers them fully.
- RSU and rollover shares are taxed as ordinary income.
- The rollover vests in 2026 whatever the departure date, so it never counts as "walking away".
- Salary and bonus stop on departure.

**Two deliberate differences from futr, both stated in the Colophon footnote:**
1. Vesting is **date-precise**: a tranche is vested on and after its vest day. futr is month-granular: a tranche counts as vested only once its month is behind.
2. Pay is **pro-rated per day**. futr counts whole calendar years worked.

### 5.2 Tranches

```
d_i      = date(y_i, m_i, day_i ?? vd)                 // clamp day to month length
vested_i = d_i <= today
sh_i     = (REV + RSU)·f_i            ; split revSh_i = REV·f_i, rsuSh_i = RSU·f_i
pre_i    = sh_i·px
after_i  = REV·f_i·px·(1−τR) + RSU·f_i·px·(1−τO)
k        = REV·(1−τR) + RSU·(1−τO)                     // after-tax $ per (fraction·$1 px)
```

### 5.3 Remaining and walking-away amounts

```
U            = { i : !vested_i }   (ordered);  N = |U|
F            = d_12 (last tranche date);  D = max(1, days(today → F))
remShares    = (REV+RSU)·Σ_{U} f_i        (per-pool split shown)
remPre       = remShares·px
remAfter     = Σ_{U} after_i  = px·k·Σ_{U} f_i
wageRateAfter= (S + B)·(1−τO)/365          // $/calendar day
wageRatePre  = (S + B)/365
wagesAfter   = wageRateAfter·D ;  wagesPre = wageRatePre·D
dayRateAfter = (remAfter + wagesAfter)/D   // HERO: the equivalent daily wage he walks away from
dayRatePre   = (remPre + wagesPre)/D
perWorkingDay(x) = x·365/261
equityPerDay = remAfter/D ; payPerDay = wageRateAfter
fuel         = Σ_{U} f_i                   // share of the grant still unread
rollover     = ROLL·px·(1−τO), shown as "already yours" while dR > today (or dR nil and year(today) == 2026); never in U-derived figures
```

If `U` is empty, the result is `.complete`: every walking-away figure is 0 and the UI shows "Book One is fully read."

### 5.4 Next vest

```
n            = U.first
nextDate     = d_n ; daysToNext = days(today → d_n)     // 0 on vest day is impossible (vested); min 1
prevDate     = n > 0 ? d_{n−1} : grant.bookOneStart ?? d_n − 91 days
chapterProgress = clamp(days(prevDate→today) / days(prevDate→d_n), 0, 1)
nextAfter    = after_n ; nextPre = pre_n
nextRate     = (after_n + wageRateAfter·daysToNext)/daysToNext
launchWindow = d_n + 1 day
```

### 5.5 Marginal chapter and Ebb

For each `j` in `U`, in order:
```
gap_j            = days(max(today, d_{j−1}) → d_j)          // first one: today → d_n
chapterValue_j   = after_j + wageRateAfter·gap_j
chapterRate_j    = chapterValue_j / gap_j
shareOfRemaining_j = chapterValue_j / Σ_{i∈U, i≥j} chapterValue_i     // rises 1/N → 1/1
bankedThrough_j  = Σ_{i ≤ j} after_i + rollover                       // all tranches through j, today's px
ebbWeight_j      = after_j / bankedThrough_j                          // FALLS: the honest diminishing signal
```

The honest finding goes into the copy in plain words: at a flat price there are **no diminishing dollars** per quarter after the cliff. Two things do diminish: the size of the remaining pot, and how much each quarter adds to what he will have (`ebbWeight`). The clarity model therefore uses `remAfter`, `ebbWeight` and the Enough Line, never an invented decay curve.

### 5.6 The Last Page scrubber

The choice `j` is either "now" (`j = −1`) or one of the chapters in `U`. "Ending after chapter j" means he leaves on `d_j + 1`.

```
kept_j     = Σ_{i∈U, i≤j} after_i + wageRateAfter·days(today → d_j)     (kept_now = 0)
forfeit_j  = (remAfter + wagesAfter) − kept_j
daysBack_j = days(d_j → F)          (now: D)
impliedRate_j = daysBack_j > 0 ? forfeit_j / daysBack_j : 0
```

### 5.7 Enough Line and crossover

These apply only when `E` is set.

```
margin   = dayRateAfter − E                   // "above/below the line by |margin|"
crossover px* = (E − wageRateAfter)·D / (k·Σ_{U} f_i)
            if E ≤ wageRateAfter → .payAloneClears  (no crossover)
            if Σ_{U} f_i == 0  → .none
priceSensitivity = k·Σ_{U} f_i     // "$1 of CRWV = $X of remaining after-tax"
```

### 5.8 Formatting (`Core/Math/MoneyFormat.swift`)

- **Compact form**, as in futr: values ≥ 1e9 → "$1.23B", ≥ 1e6 → "$1.2M", ≥ 1e3 → "$786k", otherwise "$950".
- **Ledger tables** use whole dollars with grouping.
- After-tax figures are prefixed with "≈". Pre-tax figures are always labelled "pre-tax".
- Footnote everywhere: "Not tax advice. Today's price, your rates."

### 5.9 Test vectors (fake numbers; these go in `VersoCoreTests/LedgerTests.swift`)

Inputs: REV = 100,000; RSU = 100,000; ROLL = 1,000; px = $100; S = $200,000; B = $50,000; τR = .238 (not sheltered); τO = .503; vd = 20; today = 2026-10-09; time zone America/New_York.

| Quantity | Expected |
|---|---|
| U | 11 tranches (Nov '26 … May '29); `fuel` = .6875 |
| `after_i` (one 6.25% tranche) | 786,875 |
| `remAfter` | 8,655,625 |
| `remPre` | 13,750,000 |
| `daysToNext` | 42 (Nov 20) |
| `D` | 954 |
| `wagesAfter` | 324,752.05 |
| `dayRateAfter` | ≈ 9,413.39 |
| `dayRatePre` | ≈ 15,097.93 |
| `nextRate` | ≈ 19,075.53 |
| `ebbWeight` (Nov '26) | ≈ 0.16493 (renders as 1⁄6) |
| `ebbWeight` (May '29) | ≈ 0.06225 |
| crossover with E = 3,000 | ≈ $29.31 |
| sheltered = true | `after_i` = 935,625 |
| today = 2026-11-20 | Nov '26 is vested, N = 10 |

---

## 6. Clarity — the Turning Point model (`Core/Math/Clarity.swift`)

**Principle:** Verso never says "go" or "stay" and never shows a 0–100 score.

It shows four independent strands of evidence and tallies the conditions **he** wrote. In story terms, a turning point is not calculated. It is the moment the protagonist realises the conditions he set have come true. The app's job is to make that realisation hard to miss, and hard to fool himself about.

### 6.1 Strand 1: the Ledger

This strand reports facts, not a verdict:
- `remAfter` and `fuel`
- `dayRateAfter` against `E` (above or below the line)
- `ebbWeight` of the next chapter
- `daysToNext` and `launchWindow`

### 6.2 Strand 2: the Pulse (Marginalia)

For each weekly record:
```
toward = (pull + restlessness)/2 ; away = (energy + anchor)/2 ; water = toward − away   (−4…+4)
smoothed s_t = EWMA(α = 0.35) over recorded weeks in order (gaps skipped, not zero-filled; a gap > 3 weeks resets s to the next raw value)
```

Pulse state, with hysteresis: the candidate state must hold for **3 consecutive recorded weeks** before the displayed state changes. The initial state is the candidate after 3 or more records. Below that, the state is `.unread` ("Not enough pages yet").

| State | Rule | Copy |
|---|---|---|
| `.holds` | s ≤ −0.75 | "Book One still holds you." |
| `.even` | −0.75 < s < +0.75 | "Even. Nothing pulling hard either way." |
| `.pulls` | s ≥ +0.75 | "Book Two is pulling, and has for N weeks." |

Derived values:
- **Divergence count:** the number of weeks out of the last 8 where `pull − energy ≥ 2`.
- **"The X":** the first week where s crossed from ≤ 0 to > 0.
- **Burnout telltale:** energy ≤ 2 and restlessness ≥ 4 for 3 consecutive weeks. It is shown as a fadedInk note and never folded into the stage.

### 6.3 Strand 3: Momentum (`Core/Math/Momentum.swift`)

`Momentum.status(thread:entries:today:)` returns `.moving`, `.slow` or `.stalled`, plus `pace` in the range 0…1+.
- **count:** `pace` = (entries in the last 28 days) / expected, where expected = target·28/days(createdAt → deadline). With no deadline, expected = target·28/365.
- **habit:** `pace` = (weeks of the last 4 meeting perWeek) / 4.
- **practice:** `pace` = (sum over the last 28 days) / (weeklyTarget·4).
- **project:** `pace` = 1 if a milestone was completed in the last 60 days. Otherwise it is 0.5 if any entry was logged in the last 28 days, and 0 if not.
- **Status:** `pace ≥ 0.75` → moving; `≥ 0.3` → slow; otherwise stalled. A thread with no entry for more than 45 days is "idle 45d" and gets named in the Reading.
- **Rollup:** "threads moving: 3 of 5".
- **The squeeze:** if most threads are stalled **and** the pulse state is `.pulls` or restlessness ≥ 4, add one line to the Reading: "Your threads are quiet while the restlessness is loud. It may be the job crowding them out, not the new book calling." This states the ambiguity honestly.

### 6.4 Strand 4: Clauses (`Core/Math/ClauseEvaluator.swift`)

`ClauseEvaluator.isMet(clause, context) -> Bool`, where the context bundles the LedgerResult, threads, entries, pulses, price and today.

| Kind | Met when |
|---|---|
| `.date(after)` | `today ≥ after` |
| `.tranche(index)` | `vested_index` |
| `.remainingBelow(amount)` | `remAfter < amount` |
| `.dayRateBelowEnough` | `E != nil && dayRateAfter < E` |
| `.priceBelow(x)` / `.priceAbove(x)` | effective `px` compares (no what-if) |
| `.threadTarget(threadID)` | count total ≥ target; habit/practice: streak weeks ≥ 6; project: all milestones done |
| `.milestone(threadID, milestoneID)` | `doneAt != nil` |
| `.pulse(.pulls, weeks: k, ofLast: n)` | at least k of the last n recorded weeks had smoothed s ≥ +0.75 |
| `.pulse(.divergence, weeks: k, ofLast: n)` | divergence weeks ≥ k of the last n |
| `.manual(checked)` | `checked` |

**State.** The app persists `metSince: Date?`. When a clause is evaluated as met and `metSince` is nil, it is set to today. When it is evaluated unmet, it is set back to nil.
- **Holding** = met && `metSince ≤ today − 14 days`. This guards against a single good or bad week.
- Widgets evaluate met-ness live, but they only *read* `metSince`.

**Roles:**
- `.required`: all of these must hold.
- `.ordinary`: these count toward the tally.
- `.regardless`: if any one of these is met, the stage jumps.

**Edits.** Any change to a clause's text, threshold or role appends a `ClauseRevision { at, before, after }`. When an edit loosens a threshold (a numeric move toward "easier to meet"), a gentle prompt asks: "Past-you set this on 3 Mar. What changed?" The answer is stored as `revision.reason`.

### 6.5 Output 1: the gauge stage

The stage is ordinal and shared with the widgets. Let:
```
C = clauses excluding .regardless ; m = met count in C ; h = holding count in C
req = C.filter(.required)
```

| Stage | Rule | Initial |
|---|---|---|
| `.recto` | `book.mode == .recto` | — |
| `.turningPoint` | any `.regardless` met, **or** (all `req` holding **and** m ≥ ⌈0.75·|C|⌉ **and** h ≥ ⌈0.75·|C|⌉) | T |
| `.crisis` | (m ≥ ⌈0.75·|C|⌉ but some `req` unmet) **or** (divergence ≥ 6 of 8 **and** m < ⌈0.5·|C|⌉) | C |
| `.risingAction` | m ≥ 1 | R |
| `.exposition` | otherwise (including no clauses) | E |

The **tick arc** is a 220° arc with one tick per clause in C:
- unmet → a `rule` hairline tick
- met → an ink tick
- holding → a gilt tick

Regardless clauses appear as a single separate dot at the end of the arc. There is never a continuous fill and never a percentage.

### 6.6 Output 2: the Reading

`Reading.compose(...)` builds a paragraph from templates. There is no LLM: the output is deterministic and private.

Rules:
1. **Always include the clauses:** "3 of 5 clauses are met; 2 are holding." If any required clause is unmet, name the first one: "Still waiting on *'the script is drafted'*."
2. **Always include the money**, as a veiled-aware string: "Staying to the end pays ≈ $X a day; the next chapter adds 1⁄6 to all you'll have." When E is set, add "…which is above your Enough Line by $Y."
3. **Pulse:** only with 4 or more records. "For N weeks Book Two has pulled harder than Book One holds." Alternatively, use the state copy. Add "the X" week if it happened in the last 12 weeks.
4. **Momentum:** "Two of your threads moved this month." Name the longest-idle thread if it has been idle more than 45 days: "the film hasn't moved since August."
5. **Squeeze** or **burnout** line, if it applies.
6. **Turning-point aftercare:** if the stage has been `.turningPoint` for 14 days or more, add "It's been N weeks since everything you said would be true was true." This is stated once, with no nagging.
7. **End with one question**, rotated by ISO week number from a list of 10 bundled questions, never a verdict. For example:
   - "If the next chapter were already paid, what would you do on Monday?"
   - "Which clause are you hoping stays unmet?"
   - "If CRWV doubled tomorrow, would you stay longer? If not, money isn't the binding constraint."
   - "Read your first line from three months ago. Still true?"
   - "At 80, which would you regret: leaving a quarter early, or starting Book Two a year late?"
   - "What would Book Two's first page say?"

**Footer**, always, in small italic: "Verso can't know when. It can only show you what you've been saying."

---

## 7. Data model (`Core/Model/*.swift`)

Storage is **Codable JSON** in the App Group container at `<group>/Verso/`. SwiftData is not used: the data is a few hundred records for one person, JSON is exportable and diffable, and the widget and intents processes share it without migrations.

**`VersoStore`** is an `actor` and the single owner of all reads and writes:
- It writes atomically (`Data.write(options: .atomic)`) inside an `NSFileCoordinator` coordinated write, so the app and the widget-process intents never tear a file.
- After every mutation it rebuilds `snapshot.json` and calls `WidgetCenter.shared.reloadTimelines(ofKind:)` for the affected kinds.

Every type below is `Codable, Sendable, Hashable`. Dates are encoded ISO-8601. Every file carries `version: Int` for forward migration.

```swift
// book.json — everything small and slow-changing
struct BookFile: Codable, Sendable { var version = 1; var grant: Grant?; var settings: Settings
  var chapters: [ChapterMeta]; var clauses: [Clause]; var stayCase: String?
  var openingLines: [OpeningLine]; var letter: SealedLetter?; var state: BookState }

struct Grant: Codable, Sendable, Hashable {
  var revestShares: Double; var rsuShares: Double
  var rolloverShares: Double; var rolloverVestDate: Date?
  var schedule: [Tranche] = Tranche.standard      // 13 rows, committed default (not secret)
  var vestDayOfMonth: Int = 20; var bookOneStart: Date?
  var salaryAnnual: Double; var bonusAnnual: Double
  var taxRevest: Double = 0.238; var revestSheltered: Bool = false; var taxOrdinary: Double = 0.503
  var isSample: Bool = false; var seededFrom: String?; var updatedAt: Date }
struct Tranche: Codable, Sendable, Hashable { var year: Int; var month: Int; var day: Int?; var fraction: Double }

struct Settings: Codable, Sendable { var enoughLinePerDay: Double?; var veilByDefault = true
  var showFiguresOnLockScreen = false; var marginaliaWeekday = 1 /*Sunday*/; var marginaliaHour = 19
  var notificationsOn = true; var epigraph: String? }

struct ChapterMeta: Codable, Sendable, Hashable { var trancheIndex: Int; var title: String?
  var epigraph: String?; var notes: String?; var priceAtVest: Double? }

enum ClauseRole: String, Codable, Sendable { case required, ordinary, regardless }
enum PulseMetric: String, Codable, Sendable { case pulls, divergence }
enum ClauseKind: Codable, Sendable, Hashable {
  case date(after: Date), tranche(index: Int), remainingBelow(Double), dayRateBelowEnough
  case priceBelow(Double), priceAbove(Double)
  case threadTarget(threadID: UUID), milestone(threadID: UUID, milestoneID: UUID)
  case pulse(metric: PulseMetric, weeks: Int, ofLast: Int)
  case manual(checked: Bool) }
struct Clause: Codable, Sendable, Identifiable, Hashable { var id: UUID; var text: String
  var kind: ClauseKind; var role: ClauseRole; var createdAt: Date; var metSince: Date?
  var revisions: [ClauseRevision] }
struct ClauseRevision: Codable, Sendable, Hashable { var at: Date; var beforeText: String
  var beforeKind: ClauseKind; var beforeRole: ClauseRole; var reason: String? }

struct OpeningLine: Codable, Sendable, Identifiable, Hashable { var id: UUID; var date: Date; var text: String }
struct SealedLetter: Codable, Sendable { var body: String; var sealedAt: Date; var opensOn: Date?; var openedAt: Date? }
struct BookState: Codable, Sendable { var mode: Mode = .bookOne; var closedAt: Date?; var closingSentence: String?
  var firstLaunchAt: Date; var lastTitleCardVestIndex: Int?; var seedImportedAt: Date?
  enum Mode: String, Codable, Sendable { case bookOne, recto } }

// threads.json
enum Binding: String, Codable, Sendable, CaseIterable { case oxblood, navy, forest, ochre, plum, slate }
enum ThreadKind: Codable, Sendable, Hashable {
  case count(target: Int, deadline: Date?)
  case habit(perWeek: Int)
  case project(milestones: [Milestone])
  case practice(unit: PracticeUnit, weeklyTarget: Double) }
enum PracticeUnit: String, Codable, Sendable { case minutes, sessions }
struct Milestone: Codable, Sendable, Identifiable, Hashable { var id: UUID; var title: String; var doneAt: Date? }
struct PlotThread: Codable, Sendable, Identifiable, Hashable { var id: UUID; var name: String
  var subtitle: String?; var logline: String?; var kind: ThreadKind; var binding: Binding
  var defaultAmount: Double = 1; var createdAt: Date; var archivedAt: Date?; var sortIndex: Int }

// entries.json (append-mostly)
struct Entry: Codable, Sendable, Identifiable, Hashable { var id: UUID; var threadID: UUID; var date: Date
  var amount: Double; var note: String?; var title: String?; var milestoneID: UUID? }

// pulses.json
struct Pulse: Codable, Sendable, Identifiable, Hashable { var id: UUID; var weekOf: Date  // ISO-week Monday
  var energy: Int; var anchor: Int; var pull: Int; var restlessness: Int   // 1...5
  var anchorReasons: [String]; var sentence: String?; var board: BoardSnapshot? }
struct BoardSnapshot: Codable, Sendable, Hashable { var stage: Stage; var met: Int; var holding: Int
  var total: Int; var dayRateAfter: Double?; var price: Double? }

// price.json
struct PriceCache: Codable, Sendable { var price: Double?; var timestamp: Date?; var source: String?
  var fetchedAt: Date?; var overridePrice: Double?; var overrideEnabled = false
  var effective: Double? { overrideEnabled ? overridePrice : price } }

// snapshot.json — derived, for widgets (never source of truth)
struct VersoSnapshot: Codable, Sendable { var generatedAt: Date; var grant: Grant?; var price: PriceCache
  var stage: Stage; var clausesMet: Int; var clausesHolding: Int; var clausesTotal: Int
  var tickStates: [TickState]; var nearestClauses: [ClauseLine]   // up to 2 unmet, closest first
  var pulseState: PulseState; var divergenceWeeks: Int; var latestSentence: String?
  var threads: [ThreadSummary]; var enoughLine: Double?; var veil: VeilPolicy; var mode: BookState.Mode }
struct ThreadSummary: Codable, Sendable, Identifiable, Hashable { var id: UUID; var name: String; var binding: Binding
  var kindTag: String; var progress: Double; var label: String /* "7 / 12" */; var periodLabel: String /* "3/4 this week" */
  var lastEntryAt: Date?; var status: MomentumStatus; var logLabel: String /* "+1" | "Done today" | "+20 min" */ }
enum Stage: String, Codable, Sendable { case exposition, risingAction, crisis, turningPoint, recto }
enum TickState: String, Codable, Sendable { case unmet, met, holding }
```

Because the snapshot carries `grant` and `price`, widgets **recompute** every date-derived value per timeline entry: countdown, chapter, rates. They stay correct even if the app has not run for weeks. Clause met-ness that depends on dates is recomputed the same way. Pulse and thread-dependent states come from the snapshot.

---

## 8. Price service (`Core/Price/PriceService.swift`)

1. **Primary:** `GET https://stockanalysis.com/api/quotes/s/CRWV/` with `URLSession` and an 8s timeout. Decode `{ "data": { "p": Double, "ts": Double(ms)? } }`. If `ts` is missing, use now.
2. **Fallback** (fired concurrently with `async let`): `GET https://query1.finance.yahoo.com/v8/finance/chart/CRWV?interval=5m&range=1d`. Decode `chart.result[0].meta.regularMarketPrice` and `regularMarketTime` (in seconds).
3. Accept only `0 < p < 100_000`. Prefer StockAnalysis if both succeed.
4. Write `price.json`, then reload the Day Rate and Next Chapter widgets.
5. **TTL:** 5 minutes in the app (fetch on foreground if the cache is older). In widgets, the provider fetches if the cache is older than 60 minutes, with a 5s timeout.
6. **On failure,** keep the cached value and mark it stale. If no price exists at all, money figures show "—" and the UI prompts "Set a price in the Colophon".
7. **A manual override,** when enabled, wins everywhere and is labelled "manual".
8. Network is used for the price GET and nothing else. Nothing is uploaded anywhere.

---

## 9. Widgets (`Widgets/`)

All widgets use `StaticConfiguration`, except Thread, which uses `AppIntentConfiguration`.

Shared rendering:
- Containers use `.containerBackground(Palette.paper, for: .widget)`.
- In `.accented` or `.vibrant` rendering mode (tinted or clear Home Screen, Lock Screen, StandBy night), drop the paper look and fills. The geometry then carries the meaning: ribbon and ticks use `.widgetAccentable()`.
- Typography carries over: Didot numerals and New York small-caps labels.
- **Money privacy:**
  - Every dollar `Text` uses `.privacySensitive()`.
  - In accessory families, dollars are replaced with `·····` unless `showFiguresOnLockScreen` is on.
  - On the Home Screen, dollars are hidden when `veilByDefault` is on **and** the widget is configured as veiled. For simplicity in v1, Home Screen widgets show money but rely on `privacySensitive` for locked redaction.

Each widget's `widgetURL` and `Link` targets the deep link listed below.

### 9.1 Next Chapter (`VersoNextChapter`) → `verso://contents`

| Family | Content |
|---|---|
| systemSmall | Running head "CHAPTER VI"; Didot "42" at about 64pt; "DAYS · NOV 20"; ribbon hairline at the bottom showing `chapterProgress`. This is the intended StandBy pick. |
| systemMedium | The small layout on the left. On the right, a mini table of contents of the next 3 chapters with leader dots and after-tax values, with the current one ribboned. |
| accessoryCircular | `Gauge(value: chapterProgress)` with `.gaugeStyle(.accessoryCircularCapacity)`; "37" in the center and a tiny "VI". |
| accessoryRectangular | "CH. VI · 42 DAYS" / "Nov 20 · ≈ $786k" (or ·····) / a `ProgressView(value:)` line. |
| accessoryInline | "Ch. VI ends in 42 days" |

On vest day the content reads "Chapter V ends today ❦". On the last day before a vest, use `Text(date, style: .relative)`.

**Timeline:** one entry per local midnight for 14 days, plus an entry at each vest instant inside that window. Policy `.atEnd`.

### 9.2 Day Rate (`VersoDayRate`) → `verso://ledger`

| Family | Content |
|---|---|
| systemSmall | "BOOK ONE PAYS"; Didot "$9,461"; "a day to stay"; footer "CRWV $100 · 1h". With E set, a hairline marker with ▲ or ▼ "Enough line". |
| accessoryRectangular | "$9.5k/day to stay" / "Enough $3k ▲" / "CRWV · 1h". When veiled, show "1⁄6 · 37 days · 11 left" instead. |
| accessoryInline | "$9.5k/day · Ch. VI" (veiled: "Ch. VI · 11 chapters left") |

**Timeline:** the provider loads the snapshot. If the price cache is older than 60 minutes, it fetches (5s timeout) and writes the cache back. It returns entries at now and at the next midnight, with policy `.after(now + 60 min)`. The app also reloads this kind whenever the price or the override changes.

### 9.3 Turning Point (`VersoTurningPoint`) → `verso://turning`

| Family | Content |
|---|---|
| systemSmall | The tick arc (one tick per clause; ink when met, gilt when holding) and the stage word below in New York italic. |
| systemMedium | The arc on the left. On the right, the two nearest unmet clauses (italic, with their state marks) and the latest Marginalia sentence as an epigraph. If Marginalia is due, a "Marginalia due" chip links to `verso://marginalia`. |
| accessoryCircular | `Gauge(value: met, in: 0...total)` with `.accessoryCircular` and tick marks; the stage initial E / R / C / T in the center. |
| accessoryRectangular | "THE TURNING POINT" / "3 of 5 · Rising Action" / "Book Two pulling 5 wks" (the pulse line) |
| accessoryInline | "3/5 · Rising Action" |

**Timeline:** an entry at now and at the next midnight (date clauses), plus entries at any date-clause flip dates within 14 days. Policy `.atEnd`. The app reloads this kind on every clause, Marginalia, thread or price change.

### 9.4 Thread (`VersoThread`, interactive) → `verso://thread/<id>`

The thread is chosen with `AppIntentConfiguration(intent: SelectThreadIntent)`, whose parameter is `ThreadEntity` (optional). The default is the most behind-pace active thread.

| Family | Content |
|---|---|
| systemSmall | A binding-color spine on the leading edge with the name in vertical small caps; a progress ring; "7 / 12" or "3/4 this week"; and `Button(intent: LogThreadIntent(thread:, amount: default))` with "✒︎ +1" / "Done today" / "+20 min". When pressed, the ring animates with `.contentTransition(.numericText())` and the timeline reloads. |
| systemMedium | Up to 3 threads side by side, each with a ring and a +1 button. Configured threads come first, then the remaining active threads by `sortIndex`. |
| accessoryCircular | The ring, with the thread's initial in the center. |
| accessoryRectangular | "SERBIAN · 3/4 this week" / a 7-dot week strip or bar / "last 3d ago" |

**Timeline:** an entry at now and at the next midnight. Habit weeks roll over on Monday. Policy `.atEnd`. `LogThreadIntent` reloads this kind and Turning Point.

### 9.5 App Intents (`Shared/Intents/`, compiled into BOTH the app and widget targets)

- `ThreadEntity: AppEntity` with a `ThreadQuery: EntityQuery`, which reads `threads.json` through `VersoStore`.
- `LogThreadIntent: AppIntent`:
  - Parameters: `thread: ThreadEntity` and `amount: Double?`. `openAppWhenRun = false`.
  - In the widget process, `perform()` calls `VersoStore.shared.log(...)`. The store appends the entry, rebuilds the snapshot and reloads timelines.
  - It returns a dialog: "Logged 20 min of Serbian."
- `OpenMarginaliaIntent` (`openAppWhenRun = true`): routes to `verso://marginalia`.
- `NextChapterIntent`: speaks "Chapter Six ends in 37 days."
- `VersoShortcuts: AppShortcutsProvider` with these phrases:
  - "Log \(\.$thread) in \(.applicationName)"
  - "Write marginalia in \(.applicationName)"
  - "When does the chapter end in \(.applicationName)"
- **Stretch (optional):** a `ControlWidgetButton` for "Marginalia" (`OpenMarginaliaIntent`) and one for "Log thread" (`LogThreadIntent`).

---

## 10. Seed file and privacy mechanism

- **Gitignored private seed:** `ios/Verso/Seed/grant.local.json` holds the real values. It already exists locally. `.gitignore` covers `ios/**/Seed/*.local.json`, `ios/**/xcuserdata/`, `ios/**/DerivedData/`, `ios/**/build/` and `*.xcuserstate`.
- **Committed shape document:** `ios/Verso/Seed/grant.example.json` contains fake round numbers.
- **Shape** (`SeedFile`, in `Core/Model/Seed.swift`):
  ```json
  { "version": 1,
    "grant": { "revestShares": 0, "rsuShares": 0, "rolloverShares": 0, "rolloverVestDate": null,
               "vestDayOfMonth": 20, "bookOneStart": null, "salaryAnnual": 0, "bonusAnnual": 0,
               "taxRevest": 0.238, "revestSheltered": false, "taxOrdinary": 0.503 },
    "enoughLinePerDay": null }
  ```
  Use a decoding helper that tolerates missing keys: every field except the share counts has a default.
- **Build phase:** a Run Script on the app target in `project.yml` (`postBuildScripts`) named "Copy local seed (if present)":
  ```sh
  SEED="$SRCROOT/Seed/grant.local.json"
  DEST="$TARGET_BUILD_DIR/$UNLOCALIZED_RESOURCES_FOLDER_PATH/grant.local.json"
  if [ -f "$SEED" ]; then cp "$SEED" "$DEST"; else rm -f "$DEST"; fi
  ```
  - Set `basedOnDependencyAnalysis: false`.
  - The app target needs `ENABLE_USER_SCRIPT_SANDBOXING: NO`. Otherwise the Xcode script sandbox blocks reading an undeclared input.
  - The script runs before code signing (post-build scripts in XcodeGen still run inside the build, before the sign step). Put it in `preBuildScripts` if it is ever found after signing.
  - `rm -f` matters: it keeps a stale copy from surviving in DerivedData after the seed is deleted.
- **Import logic:** on launch, `VersoStore.bootstrap()` runs. If `book.json` has no grant and `Bundle.main.url(forResource: "grant.local", withExtension: "json")` exists, it decodes the seed into `grant` with `seededFrom = "local seed"` and sets `state.seedImportedAt`. It never re-imports automatically. The Colophon's "Re-import seed" is explicit and overwrites only the grant.
- **Fixtures:** previews and tests use only `Core/Model/SampleData.swift`. That file contains 100,000 / 100,000 / 1,000 shares, a $100 price, $200k salary and $50k bonus, and a fixed `today` of 2026-10-09.
- **Verification:** a `scripts/check-privacy.sh` (committed) greps the tracked files under `ios/` against a list of forbidden numbers read from the local seed at runtime. The forbidden numbers come from the gitignored file itself, so the list is never committed. Run it together with `git status --ignored ios`.
- **Screenshots:** none are committed unless they were taken with the sample grant.

---

## 11. Project layout (XcodeGen)

```
ios/Verso/
├── SPEC.md
├── project.yml                     # XcodeGen source of truth (committed)
├── Verso.xcodeproj/                # generated, committed so it opens directly (xcuserdata ignored)
├── Seed/
│   ├── grant.example.json          # committed, fake
│   └── grant.local.json            # GITIGNORED, real
├── scripts/check-privacy.sh
├── Core/                           # framework target "VersoCore" (iOS 26, Swift 6, no UI deps except SwiftUI design tokens)
│   ├── Model/      Grant.swift Thread.swift Entry.swift Pulse.swift Clause.swift Book.swift Snapshot.swift Seed.swift SampleData.swift
│   ├── Math/       Ledger.swift Momentum.swift Pulse+Smoothing.swift ClauseEvaluator.swift Clarity.swift Reading.swift MoneyFormat.swift Roman.swift Fraction.swift
│   ├── Store/      VersoStore.swift (actor) AppGroup.swift FileCoordination.swift SnapshotBuilder.swift
│   ├── Price/      PriceService.swift
│   └── Design/     Palette.swift Type.swift Haptics.swift DinkusRule.swift Ribbon.swift TickArc.swift ThreadRing.swift MoneyText.swift (veil-aware)
├── Shared/                         # compiled into BOTH app and widget targets (App Intents must live in each binary that runs them)
│   └── Intents/    ThreadEntity.swift LogThreadIntent.swift SelectThreadIntent.swift OpenMarginaliaIntent.swift NextChapterIntent.swift
├── App/                            # application target "Verso"
│   ├── VersoApp.swift  RootView.swift  DeepLink.swift  Notifications.swift  TitleCard.swift
│   ├── Contents/   ContentsView.swift ChapterRow.swift SpineRow.swift ChapterDetailView.swift Onboarding.swift
│   ├── Ledger/     LedgerView.swift LastPageScrubber.swift MarginalChart.swift PriceSheet.swift
│   ├── Threads/    ThreadsView.swift ThreadDetailView.swift QuickLogSheet.swift ThreadEditor.swift
│   ├── Turning/    TurningPointView.swift ClauseRow.swift ClauseEditor.swift PulseChart.swift DriftStrip.swift OpeningLines.swift SealedLetterView.swift CloseBookFlow.swift
│   ├── Marginalia/ MarginaliaSheet.swift
│   ├── Colophon/   ColophonView.swift
│   ├── VersoShortcuts.swift
│   ├── Assets.xcassets (AppIcon, AccentColor = oxblood)
│   ├── Info.plist  Verso.entitlements (App Group)
├── Widgets/                        # app-extension target "VersoWidgets"
│   ├── VersoWidgetsBundle.swift  Provider+Snapshot.swift
│   ├── NextChapterWidget.swift DayRateWidget.swift TurningPointWidget.swift ThreadWidget.swift
│   ├── Info.plist  VersoWidgets.entitlements (same App Group)
└── Tests/                          # unit-test target "VersoCoreTests" (tests VersoCore only, no host app → fast)
    ├── LedgerTests.swift (§5.9 vectors)  ClarityTests.swift  MomentumTests.swift
    ├── PulseSmoothingTests.swift  ClauseEvaluatorTests.swift  StoreTests.swift (temp dir)  SeedDecodeTests.swift  PriceParseTests.swift (fixture JSON with fake prices)
```

### `project.yml` essentials

| Setting | Value |
|---|---|
| Project | `name: Verso` |
| Deployment | iOS 26.0 |
| Swift | `SWIFT_VERSION: 6.0`, `SWIFT_STRICT_CONCURRENCY: complete` |
| Signing | `DEVELOPMENT_TEAM: ""` (overridable via `XCODE_DEVELOPMENT_TEAM` / xcconfig), `CODE_SIGN_STYLE: Automatic` |
| Bundle ids | `$(VERSO_ID)`, `.widgets`, `.core`, `.coretests` |
| Entitlements | `com.apple.security.application-groups: [group.$(VERSO_ID)]` on the app and the widget |

Targets:
- **`VersoCore`** (`framework`, sources `Core`):
  - `APPLICATION_EXTENSION_API_ONLY: YES`.
  - Embedded in the app and linked, not embedded, by the widget.
- **`Verso`** (`application`, sources `App` + `Shared`):
  - Depends on `VersoCore` (embed) and `VersoWidgets` (embed app extension).
  - Info.plist keys: `CFBundleURLTypes` → `verso`, `NSSupportsLiveActivities: NO`, `UILaunchScreen: {}`.
  - The seed post-build script.
- **`VersoWidgets`** (`app-extension`, sources `Widgets` + `Shared`):
  - `NSExtension.NSExtensionPointIdentifier: com.apple.widgetkit-extension`.
  - Depends on `VersoCore`.
- **`VersoCoreTests`** (`bundle.unit-test`, sources `Tests`): depends on `VersoCore`.
- **Scheme** `Verso`: build app + widgets; test `VersoCoreTests`.

### Build and test commands

```
cd ios/Verso && xcodegen generate
scripts/sim.sh build     # ad-hoc signed simulator build (App Group entitlements applied)
scripts/sim.sh test
scripts/sim.sh run       # build, install and launch on the booted simulator
```

Simulator builds are **ad-hoc signed** (`CODE_SIGN_STYLE=Manual CODE_SIGN_IDENTITY=- DEVELOPMENT_TEAM=`), not
built with `CODE_SIGNING_ALLOWED=NO`. Ad-hoc signing embeds the simulated entitlements, so the app and the widget
extension share the real `group.$(VERSO_ID)` container on the simulator and widgets render live data there.
`CODE_SIGNING_ALLOWED=NO` strips the entitlements; `AppGroup.storeDirectory` then falls back to
`Application Support/Verso/` (and logs it) and widgets show only the sample or empty state. On a device, pass
`DEVELOPMENT_TEAM=<team>` (or set `XCODE_DEVELOPMENT_TEAM`) with automatic signing.

### Engineering notes

- **`MoneyText(_ value: Double, style: .compact | .full, afterTax: Bool)`** is the only way to render money. It handles the "≈" prefix, the Veil, `privacySensitive` and `numericText`. A shared `@Observable AppModel` holds the session-level `veilLifted`.
- **Previews** use `SampleData` exclusively, through `.environment(AppModel.preview)`.
- **Concurrency:** every Core value type is `Sendable`. The UI reads an `@Observable @MainActor AppModel` that mirrors store state and is refreshed after each store mutation. There are no `@unchecked Sendable` conformances in Core.
- **Notifications:** one repeating `UNCalendarNotificationTrigger` for Marginalia, and one "A chapter ends tomorrow." trigger 1 day before each upcoming vest, 6 months ahead at most. These are the only notifications.
