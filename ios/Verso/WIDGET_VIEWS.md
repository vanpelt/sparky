# Widget views (shared with the app)

The widget SwiftUI views live in `Shared/WidgetViews/`, which is compiled into BOTH the `Verso` app and the
`VersoWidgets` extension, so the app's debug Widget Gallery can render them with no extra wiring.

The names below carry a `Widget` suffix on purpose: SPEC.md §11 reserves `TurningPointView` for the app's
Turning Point screen (`App/Turning/TurningPointView.swift`), so the widget views must not take those names.

## Entries (`Shared/WidgetViews/WidgetEntry.swift`)

```swift
struct VersoEntry: TimelineEntry {          // Next Chapter, Day Rate, Turning Point
    let date: Date
    let live: LiveBoard                     // VersoCore: everything recomputed for `date`
    var snapshot: VersoSnapshot { get }
    static func make(_ snap: VersoSnapshot, at date: Date) -> VersoEntry
    static var sample: VersoEntry { get }   // SampleData.snapshot at SampleData.today (fake numbers)
    static var empty: VersoEntry { get }    // no grant, no price: the "set up Verso" state
}

struct ThreadEntry: TimelineEntry {         // Thread
    let date: Date
    let threads: [ThreadSummary]            // first = the one the widget features
    static func make(_ snap: VersoSnapshot, at date: Date, featuring id: UUID? = nil) -> ThreadEntry
    static var sample: ThreadEntry { get }
}

enum VersoWidgetURL {                       // the widgetURL / Link targets, all parsed by App/DeepLink.swift
    static let contents, ledger, turning, marginalia: URL
    static func thread(_ id: UUID) -> URL
}
```

## Views

Every view takes an optional `family:`. Inside a real widget leave it `nil` and the view reads
`@Environment(\.widgetFamily)`. In the app, pass the family explicitly, because the app has no widget family.

```swift
NextChapterWidgetView(entry: VersoEntry, family: WidgetFamily? = nil)     // home + all three accessory
DayRateWidgetView(entry: VersoEntry, family: WidgetFamily? = nil)         // home + rectangular, inline
TurningPointWidgetView(entry: VersoEntry, family: WidgetFamily? = nil)    // home + all three accessory
ThreadWidgetView(entry: ThreadEntry, family: WidgetFamily? = nil)         // home + all three accessory
```

"Home" is `.systemSmall .systemMedium .systemLarge`, plus `.systemExtraLargePortrait` (4×6, a whole Home Screen
page) on iOS 27. The lists live in ONE place, `VersoFamilies` in `WidgetStyle.swift`:

```swift
VersoFamilies.home                       // the Home Screen families this OS offers
VersoFamilies.supported(WidgetKinds.x)   // what a kind offers; its .supportedFamilies and the gallery read it
```

Every `switch f` in a view names each home family explicitly. `default:` would draw the small layout stretched
across a large slot, so `WidgetParityTests.everyOfferedFamilyHasItsOwnBranch` reads the view sources and fails on
a switch that leaves one out. Adding a family: add it to `VersoFamilies.home`, then give every switch a branch.

The large and extra-large layouts:

| Widget | Large / extra-large |
|---|---|
| Next Chapter | The countdown as a chapter opening (what the chapter brings), then the contents with leader dots, set like the app's Contents: date and the fraction each adds, read chapters struck through. Large fits 8 lines and keeps the app's window around the current chapter; extra-large lists up to 16. |
| Day Rate | The rate as the heading, the Enough line drawn wide and labelled, then where the rate comes from beside each chapter ahead on its own (stacked on extra-large, which lists every chapter ahead). |
| Turning Point | His latest Marginalia sentence (or the book's epigraph) as an epigraph, the arc and stage as frontispiece, then every clause with its mark, "Or, regardless —" last; the overflow is counted. |
| Thread | Large: two even columns (HIG), 3 rows, each cell with ring, figure, week dots or last line, and its own log button. Extra-large: a register of up to 8 threads, one per line like the app's Threads list (strip, 72pt ring, name and kind, figure, week dots and last line, log button at the line's end). The status line never wraps ("last 2d ago · moving" shrinks instead). |

The blank, recto and refresh states have a page-sized variant (`WidgetBlankPage(..., large: true)`).

Next Chapter's small layout reads `showsWidgetContainerBackground`: in StandBy (no paper behind it) it drops the
chapter title and the ribbon's hairline track and sets the count at 80 instead of 64.

The views set no background and no `widgetURL`; the `Widget` configurations add those. To render one in the
app the way the Home Screen or Lock Screen shows it, wrap it in the frame:

```swift
WidgetPreviewFrame(family: .systemSmall) { NextChapterWidgetView(entry: .sample, family: .systemSmall) }
```

`WidgetPreviewFrame(family:profile:)` sizes the view to a `WidgetSlotProfile`'s slot for that family, draws the
paper (system families) or a dark Lock Screen tile (accessory families), and applies
`@Environment(\.widgetContentMargins)` (16pt where the app reports none). Profiles:

- **17 Pro Max** (default): 170, 364×170, 364×382 measured; 4×6 is 364×594, three small rows plus two gaps.
- **Duo outer** / **Duo inner**: ESTIMATES (TBD). The Pro Max's 38pt margins and 24pt gutter applied to the outer
  display's 466pt width and to one 475pt Home Screen page of the open spread. Replace with sizes logged on a real
  Duo (after Oct 23); the Duo simulator may not run extensions.

## A ready-made gallery

```swift
WidgetGalleryView(live: VersoSnapshot?)   // nil = sample data only
```

It shows every kind in every family, with a Sample / Live switch when `live` is non-nil and a slot-profile
switch (`-versoGalleryProfile "Duo outer"` opens on one). Tiles flow side by side where the page is wide enough
(iPad, the open Duo) and stack on a phone; a Duo tile wider than a phone overhangs it there. Push it from a
debug-only row in the Colophon, for example
`WidgetGalleryView(live: SnapshotBuilder.build(from: model.board))`.

A note on interactivity: the Thread widget's +1 is a real `Button(intent: LogThreadIntent)`. In the gallery
with **Live** data it logs for real. With **Sample** data the thread ids do not exist in the store, so a
tap does nothing.

## Interactive and configurable

- **Thread** uses `AppIntentConfiguration(intent: SelectThreadIntent)`. The person picks a `ThreadEntity`.
  Without a pick, the widget shows the most behind-pace active thread.
- The +1 / Done today / +20 min button runs `LogThreadIntent` in the widget process. The store appends the
  entry, rebuilds `snapshot.json` and reloads the Thread and Turning Point timelines.
- **Controls** (Control Center, Lock Screen, Action button): `MarginaliaControl`, which opens
  verso://marginalia, and `LogThreadControl`, which logs a thread the person chooses. The Log control's title
  is the thread's name only (side-mounted on iPhone Duo); the amount is its action hint, "Log +20 min".
