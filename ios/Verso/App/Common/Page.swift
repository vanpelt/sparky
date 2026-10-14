import SwiftUI
import VersoCore

/// Which sheet of the book a `Page` is: the whole window, or the left (verso) / right (recto) page of a spread.
enum PageSide: Sendable { case single, verso, recto }

extension EnvironmentValues {
    /// The window's layout. RootView measures the window and sets it; the default is today's phone.
    @Entry var pageLayout: PageLayout = Measure.layout(for: CGSize(width: 402, height: 874))
    /// Set by `Page` on its content, so a section can tell whether it shares a column with its neighbour (`Seam`).
    @Entry var pageSide: PageSide = .single
    /// Which page of an open book this view stands on: set by `BookSplit` on its two stacks and by `Spread` on
    /// its two pages. Unlike `pageSide` it reaches the running head too, which sets itself as a verso or a recto
    /// head (`HeadBar`).
    @Entry var bookPage: PageSide = .single
    /// The folio a running head carries at its outer corner, as a printed book's do; nil in one column, where
    /// the folio stays at the foot of the page.
    @Entry var headFolio: Int? = nil
}

/// A top-level page: paper, running head, content, folio. No chrome, no cards.
/// The text column never runs wider than `Measure.text`; past that the margins grow and the column stays centred.
struct Page<Head: View, Content: View>: View {
    let folio: Int?
    let head: Head
    let content: Content
    fileprivate var side: PageSide = .single
    /// A spread page's outer margin (half of it at the spine). A single page works its own out from its width.
    fileprivate var spreadGutter: CGFloat = 0
    fileprivate var onHeadHeight: ((CGFloat) -> Void)? = nil
    @Environment(\.tabShown) private var tabShown
    @Environment(\.bookPage) private var bookPage
    @Environment(\.pageLayout) private var layout

    init(folio: Int?, @ViewBuilder head: () -> Head, @ViewBuilder content: () -> Content) {
        self.folio = folio; self.head = head(); self.content = content()
    }

    private func margins(_ size: CGSize) -> (leading: CGFloat, trailing: CGFloat) {
        switch side {
        case .single: let g = Measure.layout(for: size).gutter; return (g, g)
        case .verso: return (spreadGutter, spreadGutter / 2)
        case .recto: return (spreadGutter / 2, spreadGutter)
        }
    }

    var body: some View {
        GeometryReader { geo in
            let m = margins(geo.size)
            // One page of an open book carries its folio in the running head, at the outer corner. A `BookSplit`'s
            // pages are each a whole `Page` handed the book's folio: the right one is the next page.
            let opened = side == .single ? bookPage : side
            let headFolio = opened == .single ? nil : (side == .single && opened == .recto ? folio.map { $0 + 1 } : folio)
            ScrollView {
                VStack(alignment: .leading, spacing: 0) {
                    head
                        .environment(\.bookPage, opened)
                        .environment(\.headFolio, headFolio)
                        .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { onHeadHeight?($0) }
                        .padding(.top, 6).padding(.bottom, 8)
                    content.environment(\.pageSide, side)
                    if let folio, opened == .single { Folio(folio).padding(.top, 12) }
                    // Clears the floating pen on a phone; elsewhere the pen is in the running foot.
                    Color.clear.frame(height: layout.isPhone ? 72 : 32)
                }
                .frame(maxWidth: Measure.text, alignment: .leading)
                .padding(.leading, m.leading).padding(.trailing, m.trailing)
                .frame(maxWidth: .infinity)
            }
            .scrollIndicators(.hidden)
            // Pages hold inline fields (the stay case, a new opening line) that are easy to tap by accident:
            // scrolling puts the keyboard away.
            .scrollDismissesKeyboard(.immediately)
            .debugScrollAnchor()
            // Paper under the status bar and, on pushed pages, the whole back-button band (the safe area then
            // includes the bar): solid down to the content edge, then a short fade, so scrolled text never runs
            // into the clock or slides beneath the chevron. A window with nothing above it (a letterboxed or
            // windowed scene) gets no fade: it would only veil the running head.
            .overlay(alignment: .top) {
                let top = geo.safeAreaInsets.top, fade: CGFloat = 34
                if top > 0 {
                    LinearGradient(stops: [.init(color: Palette.paper, location: 0),
                                           .init(color: Palette.paper, location: top / (top + fade)),
                                           .init(color: Palette.paper.opacity(0), location: 1)],
                                   startPoint: .top, endPoint: .bottom)
                        .frame(height: top + fade)
                        .offset(y: -top)
                        .allowsHitTesting(false)
                }
            }
        }
        .background(Palette.paper.ignoresSafeArea())
        .toolbarBackgroundVisibility(.hidden, for: .navigationBar)
        .pageIdentifier(side, shown: tabShown)
    }
}

private extension View {
    /// "page.verso" / "page.recto", for UI tests, on the showing tab only. A single page keeps its old (empty)
    /// accessibility tree.
    @ViewBuilder func pageIdentifier(_ side: PageSide, shown: Bool) -> some View {
        switch side {
        case .single: self
        case .verso: accessibilityElement(children: .contain).accessibilityIdentifier(shown ? "page.verso" : "")
        case .recto: accessibilityElement(children: .contain).accessibilityIdentifier(shown ? "page.recto" : "")
        }
    }
}

/// A page that opens into two on a wide window. Compact, it is one `Page` holding head, verso then recto, in
/// today's order. Open (`pageLayout.isSpread`), it is a book: verso on the left with the running head, recto on
/// the right, split at the WINDOW midline rather than the safe area's (which a 27.0-SDK build shifts off the
/// status bar), folios n and n+1. Each page scrolls on its own; the recto's first line sits level with the verso's.
///
///     Spread(folio: board.folio) {
///         HeadBar("THE LEDGER")
///     } verso: {
///         hero; RemainingTable(l: l)
///     } recto: {
///         Seam()              // the ⁂ that joins them in one column; nothing on an open spread
///         LastPageScrubber(l: l)
///     }
struct Spread<Head: View, Verso: View, Recto: View>: View {
    let folio: Int?
    let head: Head
    /// The recto's own running head, set flush right on the verso head's line ("THE EVIDENCE"). One column
    /// never shows it.
    let rectoHead: String
    let verso: Verso
    let recto: Recto
    @Environment(\.pageLayout) private var layout
    @State private var headHeight: CGFloat = 0

    init(folio: Int?, rectoHead: String = "", @ViewBuilder head: () -> Head, @ViewBuilder verso: () -> Verso,
         @ViewBuilder recto: () -> Recto) {
        self.folio = folio; self.rectoHead = rectoHead; self.head = head(); self.verso = verso(); self.recto = recto()
    }

    var body: some View {
        if layout.isSpread {
            GeometryReader { geo in
                // The safe area added back: the window this spread fills, and its midline in local coordinates.
                let inset = geo.safeAreaInsets
                let window = CGSize(width: inset.leading + geo.size.width + inset.trailing,
                                    height: inset.top + geo.size.height + inset.bottom)
                let open = Measure.layout(for: window)
                if open.isSpread {
                    let half = window.width / 2
                    HStack(spacing: 0) {
                        page(.verso, folio: folio, gutter: open.gutter) { head } content: { verso }
                            .frame(width: max(0, half - inset.leading))
                        page(.recto, folio: folio.map { $0 + 1 }, gutter: open.gutter) {
                            // As tall as the verso's head, so the two heads share a line and the two first lines
                            // sit level.
                            HeadBar(rectoHead).frame(height: headHeight > 0 ? headHeight : nil)
                        } content: { recto }
                            .frame(width: max(0, half - inset.trailing))
                    }
                    .overlay(alignment: .topLeading) {
                        Spine().offset(x: half - inset.leading - Spine.width / 2)
                    }
                } else {
                    // Told "spread" but standing in a narrower column (a split view's detail): one page.
                    single
                }
            }
        } else {
            single
        }
    }

    private var single: some View {
        Page(folio: folio) { head } content: { verso; recto }
    }

    private func page<H: View, C: View>(_ side: PageSide, folio: Int?, gutter: CGFloat,
                                        @ViewBuilder head: () -> H, @ViewBuilder content: () -> C) -> Page<H, C> {
        var p = Page(folio: folio, head: head, content: content)
        p.side = side; p.spreadGutter = gutter
        if side == .verso { p.onHeadHeight = { h in if abs(h - headHeight) > 0.5 { headHeight = h } } }
        return p
    }
}

/// The fall of the paper into the binding: a faint shade down the middle of an open spread (and of a `BookSplit`).
struct Spine: View {
    static let width: CGFloat = 24
    var body: some View {
        LinearGradient(colors: [Palette.ink.opacity(0), Palette.ink.opacity(0.045), Palette.ink.opacity(0)],
                       startPoint: .leading, endPoint: .trailing)
            .frame(width: Self.width)
            .frame(maxHeight: .infinity)
            .ignoresSafeArea()
            .allowsHitTesting(false)
            .accessibilityHidden(true)
    }
}

/// What joins verso to recto when they share one column (by default a ⁂ rule). Drawn only on a single page:
/// on an open spread the spine does that job.
struct Seam<Content: View>: View {
    let content: Content
    @Environment(\.pageSide) private var side
    init(@ViewBuilder content: () -> Content) { self.content = content() }
    var body: some View { if side == .single { content } }
}

extension Seam where Content == DinkusRule {
    init() { self.init { DinkusRule() } }
}

/// "§ What this chapter pays" — a section title set like a book's subhead.
struct SectionTitle: View {
    let text: String
    var glyph: String = Glyph.section
    init(_ text: String, glyph: String = Glyph.section) { self.text = text; self.glyph = glyph }
    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            if !glyph.isEmpty { Text(glyph).font(VersoFont.serif(.title3)).foregroundStyle(Palette.oxblood) }
            Text(text).font(VersoFont.serif(.title3)).italic().foregroundStyle(Palette.ink)
        }
        .padding(.bottom, 10)
        .accessibilityAddTraits(.isHeader)
    }
}

/// "PART ONE" over "Book One".
struct PartTitle: View {
    let part: String
    let title: String
    var body: some View {
        VStack(spacing: 4) {
            Text(part).runningHeadStyle(size: 11)
            Text(title).displayFont(26).italic().foregroundStyle(Palette.ink)
        }
        .frame(maxWidth: .infinity)
        .padding(.bottom, 16)
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(.isHeader)
    }
}

/// Small faded explanatory copy (the app's own voice, not his).
struct Aside: View {
    let text: String
    init(_ text: String) { self.text = text }
    var body: some View {
        Text(text).font(VersoFont.body(14, relativeTo: .footnote)).foregroundStyle(Palette.fadedInk)
            .fixedSize(horizontal: false, vertical: true)
    }
}

/// A two-rule (accounting) double line.
struct DoubleRule: View {
    var body: some View {
        VStack(spacing: 2) { Hairline(Palette.ink.opacity(0.6)); Hairline(Palette.ink.opacity(0.6)) }
    }
}

/// A deckle-filled inset well: no border, no shadow.
struct Well<Content: View>: View {
    let content: Content
    init(@ViewBuilder content: () -> Content) { self.content = content() }
    var body: some View {
        content.padding(14).frame(maxWidth: .infinity, alignment: .leading)
            .background(Palette.deckle, in: RoundedRectangle(cornerRadius: 3, style: .continuous))
    }
}

/// A quiet small-caps text button.
struct QuietButton: View {
    let title: String
    var color: Color = Palette.fadedInk
    let action: () -> Void
    init(_ title: String, color: Color = Palette.fadedInk, action: @escaping () -> Void) {
        self.title = title; self.color = color; self.action = action
    }
    var body: some View {
        Button(action: action) { Text(title).runningHeadStyle(color, size: 12).padding(.vertical, 6) }
            .buttonStyle(.plain)
    }
}

/// A big title-page numeral that ink-bleeds in once (skipped with Reduce Motion).
struct HeroReveal: ViewModifier {
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var shown = false
    func body(content: Content) -> some View {
        content
            .blur(radius: shown || reduceMotion ? 0 : 6)
            .opacity(shown ? 1 : 0)
            .onAppear {
                withAnimation(reduceMotion ? .easeInOut(duration: 0.2) : .easeOut(duration: 0.45)) { shown = true }
            }
    }
}

extension View {
    func heroReveal() -> some View { modifier(HeroReveal()) }
    /// Hero numerals cap at 1.3× (accessibility1).
    func heroCap() -> some View { dynamicTypeSize(...DynamicTypeSize.accessibility1) }
}

/// A drop-capped paragraph: the first letter set large in Didot.
struct DropCapText: View {
    let text: String
    var size: CGFloat = 19
    var color: Color = Palette.ink
    var body: some View {
        let first = text.first.map(String.init) ?? ""
        let rest = String(text.dropFirst())
        HStack(alignment: .top, spacing: 2) {
            Text(first).font(VersoFont.hero(size * 2.6)).foregroundStyle(Palette.oxblood)
                .padding(.top, -size * 0.35)
            Text(rest).font(VersoFont.his(size)).foregroundStyle(color)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.top, size * 0.4)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(text)
    }
}

/// The gilt shimmer: a 0.8s gradient sweep across a row when a clause flips to met.
struct GiltShimmer: ViewModifier {
    let trigger: Int
    @State private var phase: CGFloat = -1
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    func body(content: Content) -> some View {
        content.overlay {
            GeometryReader { g in
                LinearGradient(colors: [.clear, Palette.gilt.opacity(0.45), .clear], startPoint: .leading, endPoint: .trailing)
                    .frame(width: g.size.width * 0.6)
                    .offset(x: phase * g.size.width * 1.4)
                    .opacity(phase > -1 && phase < 1 ? 1 : 0)
                    .allowsHitTesting(false)
            }
            .clipped()
        }
        .onChange(of: trigger) { _, _ in
            guard !reduceMotion else { return }
            phase = -1
            withAnimation(.easeInOut(duration: 0.8)) { phase = 1 }
        }
    }
}

extension View {
    func giltShimmer(_ trigger: Int) -> some View { modifier(GiltShimmer(trigger: trigger)) }
}

/// "Oct 9" style short dates, "Nov '26" months.
enum DateText {
    static func monthYear(_ d: Date) -> String {
        let m = d.formatted(.dateTime.month(.abbreviated))
        let y = d.formatted(.dateTime.year(.twoDigits))
        return "\(m) \u{2019}\(y)"
    }
    static func dayMonth(_ d: Date) -> String { d.formatted(.dateTime.month(.abbreviated).day()) }
    static func long(_ d: Date) -> String { d.formatted(.dateTime.day().month(.wide).year()) }
    static func smallCaps(_ d: Date) -> String { d.formatted(.dateTime.weekday(.abbreviated).day().month(.abbreviated)).uppercased() }

    static func relativeDays(_ days: Int) -> String {
        switch days {
        case ..<1: "today"
        case 1: "yesterday"
        default: "\(days) days ago"
        }
    }
}

/// The pen-stroke underline, drawn only once a line has been logged this session (Core's PenStroke rests fully drawn).
struct LogStroke: View {
    let threadID: UUID
    let binding: Cloth
    @Environment(AppModel.self) private var model
    var body: some View {
        let ticks = model.logTicks[threadID]
        PenStroke(color: Palette.bindingStroke(binding), trigger: ticks ?? 0)
            .opacity(ticks == nil ? 0 : 1)
    }
}

/// The pen mark used on log buttons.
struct PenMark: View {
    var size: CGFloat = 15
    var body: some View { Image(systemName: "pencil.tip").font(.system(size: size, weight: .light)) }
}
