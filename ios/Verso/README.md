# Verso

A one-person iOS app for plotting the next book of a life: what Book One (the job) still pays to stay,
the threads Book Two is already made of, and an honest place to work out *when* to turn the page.

**The name.** *Verso* is the left-hand page, the side you only see once you turn the leaf. Book One is the
recto you have been reading. The app is set like a book: New York, Didot and Iowan Old Style on paper and
ink, chapters numbered in Roman, a folio on every page, and a "Night Reading" dark mode.

## What's in it

- **Contents.** The chapter you are in (the next vest), the countdown, the thirteen chapters with what each
  one adds to everything banked by then (`+1⁄6`, `+1⁄7`, …), the Book Two bookshelf and the Turning Point line.
- **Ledger.** What staying pays per day (after tax or pre-tax, per day or per working day), what is left to
  vest, the next chapter (its equity, the pay on the way, and the per-day rate, which reconcile), the Last Page
  scrubber, the marginal-chapter chart, your Enough Line and a what-if price slider. The price is live CRWV
  (StockAnalysis, Yahoo as fallback) with a manual override.
- **Threads.** Countable goals (read 12 books), habits (move 4 days a week), practice (Serbian minutes) and
  projects with scenes (a short film). Logging is one tap on a spine, from the floating pen or from a widget.
- **Turning Point.** Your own clauses ("I'll know it's time when…"), a weekly five-line Marginalia check-in,
  the Pulse chart, the stay case, opening lines, a sealed letter, and the Reading.
- **Widgets.** Next Chapter, Day Rate, Turning Point and Thread, in `systemSmall`, `systemMedium`,
  `accessoryCircular`, `accessoryRectangular` and `accessoryInline` (Day Rate has no circular). There are also
  Control Center controls. Lock Screen families hide dollar figures unless you allow them in the Colophon.

## The clarity model

Verso never says "go", and it never shows a score. The Turning Point is a set of **clauses you write**. Each
one is tied to evidence Verso can check: a date, a chapter vesting, the amount left to vest, the price, your
Enough Line, a thread's progress, a film scene done, the Pulse, or simply your say-so. A clause is *true* when
its evidence holds and *held* once it has been true for 14 days running. The stage word (Exposition, Rising
Action, The Crisis, The Turning Point) comes from how many clauses are true and held, whether every required
clause is held, and whether a "regardless" clause has fired. The **Pulse** comes from the weekly Marginalia: how
much Book One gives and holds you, against how hard Book Two pulls and how restless you are. It is smoothed with
an EWMA and only changes its word after three weeks in a row. While that is still building it says which way it
is leaning rather than "even". The money side shows the diminishing margin of each extra quarter: every chapter
pays about the same, but each adds a smaller share of everything you'll have. The **Reading** sets all of this
into one deterministic paragraph and always ends with a question.

## Build and run

Requirements: Xcode 27, the iOS 26 SDK, and [XcodeGen](https://github.com/yonaskolb/XcodeGen)
(`brew install xcodegen`). `project.yml` is the source of truth. The generated `Verso.xcodeproj` is committed,
so the project opens directly in Xcode.

```sh
cd ios/Verso
xcodegen generate          # after editing project.yml or adding files
scripts/sim.sh build       # ad-hoc signed simulator build
scripts/sim.sh test        # VersoCoreTests (Swift Testing) and the UI tests
scripts/sim.sh run         # build, install and launch on the booted iPhone simulator
```

Simulator builds are **ad-hoc signed** (`CODE_SIGN_STYLE=Manual CODE_SIGN_IDENTITY=- DEVELOPMENT_TEAM=`).
That embeds the App Group entitlement, so the app and the widget extension share
`group.<VERSO_ID>` on the simulator and widgets render real data. Don't use `CODE_SIGNING_ALLOWED=NO`:
it strips the entitlement, the store falls back to a private directory, and widgets only show sample data.
`SIM=<udid>` picks a simulator; by default the script picks the first booted iPhone (an iPad with `IPAD=1`).

On a device, signing uses the personal team set as `DEVELOPMENT_TEAM` in `project.yml`, or pass
`DEVELOPMENT_TEAM=<team>` to override it. Every identifier comes from one line, `VERSO_ID` in
`project.yml`: the app is `<VERSO_ID>`, the widget extension `<VERSO_ID>.widgets`, the App Group
`group.<VERSO_ID>`. Bundle ids and App Groups are unique across all of Apple, and the first team to sign
one owns it (a free team can't release it). If Xcode says an identifier "is not available", change
`VERSO_ID` and run `xcodegen generate`.

Debug launch arguments for screenshots (Debug builds only, all on throwaway stores unless noted):
`-versoSample YES` (invented figures and history), `-versoFirstRun YES` (an empty store: a fresh install, onboarding and all), `-versoDayOne YES` (the state onboarding leaves: four empty
threads, one clause, no Marginalia, the bundled seed if present), `-versoRoute verso://ledger`,
`-versoScreen gallery|price|quicklog|marginalia|…`, `-versoScroll 0…1`, `-versoUnveil YES`,
`-versoOnboardingStep 0|1|2` (this one uses the real store). See `App/Common/DebugLaunch.swift`.

## Testing on iPhone Duo

**The real simulator** comes with Xcode 27.1: an "iPhone Duo" on the iOS 27.1 runtime, which takes no other
device. Fold it from Device Hub (Xcode › Open Developer Tool). What it showed that the iPad stand-in couldn't:

- Built with the 27.1 SDK, the open Duo puts the status bar and any system bars in a column down the trailing
  edge (~84pt), and *every* `NavigationStack` keeps such a column at its own trailing edge, bar hidden or not.
  That is why the open spread's left page has no stack of its own (`BookSplit`), and why every stack's ground is
  paper (`paperGround()`): otherwise the column showed white.
- `toolbarVerticalBehavior(.disabled)` anywhere turns the whole scene back to horizontal bars, which puts the
  status bar across the top and costs 82pt of height instead of 84 of width. Verso keeps the vertical layout.
- The inner display is screen 3: `xcrun simctl io <udid> screenshot --display=3` (the default is the outer one,
  black while open).
- Where the system sets its bars down the side (closed; open and held landscape; a Split View half, on that app's
  outer edge), the four tabs and the pen go down that column too, as a thumb index cut into the fore-edge
  (`ForeEdge`), anchored to the foot so the status bar and the system's Back keep the top. The running foot
  isn't built there, so pages run to the foot, with the paper thickening under the home indicator. Measured on
  the closed simulator: the column is 84pt, Back's glass circle sits at about y 167–218 and a page's own bar item
  under it (a thread's actions, in the system overflow menu on iOS 27) to about 278, and `toolbarVerticalEdge`
  read inside a stack's page comes back nil, so the edge is taken from the safe area (a column of 60pt or more
  on one side only) when the probe says nothing. The column is window-tall and hung from the top, so the
  keyboard removes the pen and tabs (from VoiceOver too) without moving anything. A `List` measures its content
  margin from inside the safe area, so Threads adds the page's gutter on top of the column.
- XCUITest can't drive the inner display (it takes the outer one for the screen, and no tap lands), so every UI
  test skips there (`skipOnDuoInnerDisplay`). Check the open Duo by screenshot, and its layout through the iPad
  harness below.

**The iPad stand-in.** An iPad simulator renders every Duo window size. Verso's layout follows the window's
width, not the device, so a window of the Duo's size is a faithful proxy for Verso's own pages (not for system
bars, sheets or the fold). With Xcode 27.1 the iPad has to run the iOS 27.0 runtime, and there UIKit crashes as the
keyboard comes up (`UITrackingElementWindowController changeToInputViewSet:`), so the keyboard tests only pass
on the iPhone simulators.

- `-versoFrame WxH` (Debug only) sets the whole app in a fixed window of that size, centred on deckle with a
  hairline round it. Size classes follow the frame: regular wide only when both sides are at least 600.
  `-versoFrameInsets top,leading,bottom,trailing` sets paper margins standing in for its safe area (default `24,0,20,0`).
- `-versoBarEdge trailing|leading` stands in for bars set down the side, which the iPad never reports: with
  `-versoFrameInsets 0,0,20,84` (or `0,84,20,0`) that margin becomes the fore-edge's column.
- `IPAD=1` makes `scripts/sim.sh` build for iPad as well (`TARGETED_DEVICE_FAMILY=1,2`, on the command line only;
  the shipping app stays iPhone-only and portrait-locked) and pick a booted iPad, the 13-inch first, since every
  Duo window fits it unrotated. On an iPad mini the open sizes need landscape.
- `scripts/sim.sh shot <name> [launch args…]` relaunches the last build and saves a screenshot to
  `VERSO_SHOT_DIR` (default `../.build/shots`).

| Window | `-versoFrame` |
|---|---|
| Closed (outer display) | `466x678` (`386x678` when built with the iOS 27.0 SDK) |
| Open (inner display) | `951x669` (`871x669` with the 27.0 SDK) |
| Open, rotated to portrait | `669x951` |
| Open, Split View half | `475x669` |

```sh
IPAD=1 scripts/sim.sh shot duo-open -versoSample YES -versoFrame 951x669
IPAD=1 VERSO_SHOT_DIR=../.build/duo scripts/sim.sh test -only-testing:VersoUITests/DuoLayoutUITests
```

`DuoLayoutUITests` launches each size on sample data, rotating the simulator when the frame only fits the other
way round, and visits the four tabs. It attaches a screenshot of the frame per tab to the `.xcresult` (and writes
PNGs to `VERSO_SHOT_DIR`), and checks the layout rule: at least 860 wide and wider than tall gives a two-page
spread (`page.verso` and `page.recto`, side by side), anything else one column no wider than a reading measure,
and the pen and tabs always inside the window. Sizes that can't fit the simulator's screen either way skip. The
two closed sizes run with a 50pt top inset, a guess at the outer display's camera and status bar.

What changes off a phone (`PageLayout.isPhone` is false: the closed Duo, the inner display, any spread):

- The pen sits in the running foot beside the four tabs instead of floating over the page's last line.
- Threads' arrange button and a thread's ⋯ menu join the running head instead of taking a bar row above it,
  and the Threads list keeps the page's margins (strip on the column's edge) instead of running edge to edge.
- On a short window (under 700pt tall: closed, open, Split View) the Contents and Turning Point heroes take a
  step down so the first chapter row and clauses show above the foot.
- On an open spread each page carries its own running head and folio (verso flush left, recto flush right,
  one veil eye per spread), and Contents' left page names the chapter in small caps rather than repeating the
  right page's numeral.

Folding and unfolding: Contents and Threads swap between one stack and a two-page split, and the Ledger and
Turning Point between one page and two, so those view trees are rebuilt. Everything a reader would miss lives in
the model instead (`PageState`, `AppModel.pages`; transient, never saved): the open chapter or thread, the expanded
"all thirteen", an arranging Threads list, the Last Page pick, the what-if slider, open disclosures, the first line
the ❦ turned up, the Colophon's form, and every unfinished field on those pages (a chapter's title, epigraph,
notes and price at vest; the stay case; a new opening line) as a draft that the rebuilt page reads before the
store. The focused field takes the keyboard back in the rebuilt tree (`keepsFocus`). An entry being edited is a
global sheet, so it stays up. A spread's left page opens at the open chapter's or thread's row.

`-versoFoldTo WxH` (with `-versoFrame`, Debug only) adds a `⇆` button in the deckle's leading margin (`debug.fold`) that
resizes the frame in place, as a fold does. `FoldContinuityUITests` uses it on an iPad mini in landscape: closed
466×678, it expands the list, opens a chapter and types half a note; unfolds to 951×669 and checks all of it
(keyboard included); picks a Last Page, starts arranging, types half a stay case; folds and checks again.

Still lost on a fold: scroll positions other than that row (each page starts at its top), and a number field's
digits typed but not yet committed (the field keeps its last committed value).

## Your real figures: the seed file

This repository is public, so no real share counts, salary or bonus are committed anywhere. Real figures reach
the app in one of two ways:

1. **Entered in the app**, during onboarding or later in Contents › Colophon.
2. **A local seed file** at `Seed/grant.local.json`. It is gitignored (`ios/**/Seed/*.local.json`). A build phase
   copies it into the app bundle only when it exists, and on first launch the app imports it as the grant if
   no grant is set yet. Colophon › Re-import seed overwrites the grant from it again.

To create one:

```sh
cp Seed/grant.example.json Seed/grant.local.json
$EDITOR Seed/grant.local.json      # your shares, salary, bonus, tax rates, dates (yyyy-MM-dd or null)
git check-ignore -v Seed/grant.local.json   # must print the ignore rule
scripts/check-privacy.sh           # fails if any seed figure appears in a committable file
```

The example file uses obviously fake round numbers. `scripts/check-privacy.sh` reads the forbidden values from
your local seed at runtime, so the list itself is never committed.

## Adding the widgets

Install the app and open it once, so the shared snapshot exists. Then:

- **Home Screen:** long-press an empty area › Edit › Add Widget › Verso. Pick Next Chapter, Day Rate,
  Turning Point or Thread, small or medium. To choose which thread a Thread widget shows, long-press it ›
  Edit Widget. Its `+1` button logs right from the Home Screen.
- **Lock Screen / StandBy:** long-press the Lock Screen › Customize › Lock Screen › tap the widget area
  (or the line above the clock for inline) › Verso. StandBy uses the small Home Screen widgets.
- **Control Center:** edit Control Center › Add a Control › Verso.
- **Preview in the app:** Contents › Colophon › The proofs › Widget Gallery renders every widget in every
  family, on sample data or your live data.

Dollar amounts on Lock Screen widgets stay hidden unless you turn on Colophon › Privacy › Show figures on the
Lock Screen. Home Screen widgets mark them privacy-sensitive, so they redact while the phone is locked.

## Layout

```
project.yml          XcodeGen spec (the .xcodeproj is generated from it and committed)
Core/                VersoCore framework: model, ledger/clarity/pulse math, store, price, design system
App/                 the SwiftUI app
Shared/              widget views + App Intents, compiled into both the app and the extension
Widgets/             the WidgetKit extension: providers, widget configurations, controls
Tests/               VersoCoreTests (fake numbers only)
Seed/                grant.example.json (committed) and grant.local.json (yours, gitignored)
scripts/             sim.sh, check-privacy.sh, make-icon.py
SPEC.md              the full design spec
```
