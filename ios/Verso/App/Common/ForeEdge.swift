import SwiftUI
import VersoCore

/// The four tabs set down the fore-edge, where the system puts its bars on the iPhone Duo: the closed outer
/// display, and the open inner one held landscape (Apple's order, top to bottom: Dynamic Island, status bar,
/// toolbar, tab bar). The column is the window's safe-area inset on that edge, ~84pt.
///
/// It reads as a dictionary's thumb index cut into the page block: three leaf edges and a deckle wash run up
/// the column from the foot, and the selected tab is a paper tab that joins the page, square on the page side
/// and rounded outside, so the page and its tab are one sheet. Above the tabs, as the toolbar's prominent
/// action, sits the pen; a gilt fleuron stands for the gap between toolbar and tab bar. Everything is anchored
/// to the foot, so the top of the column stays clear for the status bar and the system's Back.
///
/// Every row has a fixed height (recto mode renames a tab, it never moves one), and the block sits at the same
/// place closed and open, so unfolding moves nothing but the window. Changing tab slides the paper tab and
/// riffles the leaves it passes. On a leading column (the left app of a Split View) it all mirrors.
struct ForeEdge: View {
    @Binding var selection: AppTab
    let isRecto: Bool
    /// The column's edge of the window: the page lies on the other side.
    let edge: HorizontalEdge
    /// The column's width: the window's measured safe-area inset on `edge`.
    let inset: CGFloat
    /// The whole window's height, not the space left beside the keyboard: the column never moves.
    let height: CGFloat
    let bottomInset: CGFloat
    let keyboardUp: Bool

    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.dynamicTypeSize) private var typeSize
    @Environment(\.colorScheme) private var scheme
    /// The paper tab comes in just after the rail as the display turns sideways: the book opens at its place.
    @State private var tabIn = false
    @State private var riffle: Riffle?

    nonisolated static let penSize: CGFloat = 52
    nonisolated static let fleuronRow: CGFloat = 20
    /// Where the system's toolbar zone ends. On the closed Duo (27.1 simulator) Back's glass circle runs from
    /// about y 167 to 218 under the status bar, and a page's own item under it (a thread's overflow menu) to
    /// about 278. Only the squeeze rule leans on it: a 678pt window leaves 40pt more than the four tabs need.
    nonisolated static let toolbarZone: CGFloat = 280
    /// The visible tab stops this short of the window edge, so it reads as cut, not as a bar.
    nonisolated static let tabInset: CGFloat = 8

    private var mirrored: Bool { edge == .leading }
    private var items: [ShelfBar.Item] { ShelfBar.items(isRecto: isRecto) }
    private var footMargin: CGFloat { max(16, bottomInset - 10) }
    private var selectedIndex: Int { items.firstIndex { $0.tab == selection } ?? 0 }

    private var rows: Rows { Rows(height: height, foot: footMargin, iconsOnly: typeSize >= .accessibility1) }

    var body: some View {
        let rows = self.rows
        let tabsTop = height - footMargin - rows.tab * 4
        let penTop = tabsTop - Self.fleuronRow - Self.penSize
        ZStack(alignment: .topLeading) {
            LeafEdge(fadeFrom: penTop - 40, tabsTop: tabsTop, row: rows.tab, isRecto: isRecto, riffle: riffle,
                     mirrored: mirrored, dark: scheme == .dark)
                .allowsHitTesting(false)

            // The keyboard takes the pen and the tabs away (you can't log or turn the book mid-sentence), out of
            // the accessibility tree too; the ground and its page edge stay where they are.
            if !keyboardUp {
                VStack(spacing: 0) {
                    PenButton(size: Self.penSize)
                        .accessibilityShowsLargeContentViewer { Label("Log", systemImage: "pencil.tip") }
                    Text(Glyph.fleuron).font(VersoFont.body(9)).foregroundStyle(Palette.gilt)
                        .frame(height: Self.fleuronRow)
                        .accessibilityHidden(true)
                    index(rows)
                }
                .frame(width: inset)
                .offset(y: penTop)
                .transition(.opacity)
            }
        }
        .frame(width: inset, height: height, alignment: .topLeading)
        .animation(.easeOut(duration: 0.15), value: keyboardUp)
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("shelf.edge")
        .onAppear { withAnimation(.easeInOut(duration: 0.25).delay(0.08)) { tabIn = true } }
        .onChange(of: selection) { old, new in
            guard !reduceMotion,
                  let from = items.firstIndex(where: { $0.tab == old }),
                  let to = items.firstIndex(where: { $0.tab == new }) else { return }
            let r = Riffle(from: from, to: to, start: .now)
            riffle = r
            Task { @MainActor in
                try? await Task.sleep(for: .seconds(r.duration(row: rows.tab)))
                if riffle?.start == r.start { riffle = nil }
            }
        }
    }

    /// The thumb index: four rows, the paper tab behind the selected one, hairlines between the others.
    private func index(_ rows: Rows) -> some View {
        ZStack(alignment: .top) {
            ForEach(1..<items.count, id: \.self) { i in
                // A divider only between two tabs that are both cut back: none touches the paper tab.
                Rectangle().fill(Palette.rule).frame(width: 56, height: 0.5)
                    .padding(mirrored ? .trailing : .leading, 14)
                    .frame(width: inset, alignment: mirrored ? .trailing : .leading)
                    .offset(y: CGFloat(i) * rows.tab - 0.25)
                    .opacity(i == selectedIndex || i == selectedIndex + 1 ? 0 : 1)
            }
            IndexTab(dark: scheme == .dark)
                .frame(width: inset - Self.tabInset + 0.5, height: rows.tab)
                .scaleEffect(x: mirrored ? -1 : 1)
                .frame(width: inset, alignment: mirrored ? .trailing : .leading)
                .offset(x: mirrored ? 0.5 : -0.5, y: CGFloat(selectedIndex) * rows.tab)
                .opacity(tabIn ? 1 : 0)
                .allowsHitTesting(false)
            VStack(spacing: 0) {
                ForEach(items) { item in
                    tab(item, rows)
                }
            }
        }
        .frame(width: inset, height: rows.tab * 4, alignment: .top)
        .animation(reduceMotion ? .easeInOut(duration: 0.15) : .spring(response: 0.38, dampingFraction: 0.82),
                   value: selection)
    }

    private func tab(_ item: ShelfBar.Item, _ rows: Rows) -> some View {
        let on = selection == item.tab
        return Button {
            if !on { selection = item.tab }
        } label: {
            IndexLabel(item: item, on: on, row: rows.tab, oneLine: rows.oneLine, iconsOnly: rows.iconsOnly)
                .frame(width: inset - Self.tabInset)
                .frame(width: inset, height: rows.tab, alignment: mirrored ? .trailing : .leading)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .sensoryFeedback(.selection, trigger: on)
        .accessibilityLabel(item.title)
        .accessibilityAddTraits(on ? [.isButton, .isSelected] : .isButton)
        .accessibilityShowsLargeContentViewer { Label(item.title, systemImage: item.symbol) }
    }

    /// Row heights for a window `height` tall. 62pt tabs (room for Turning Point's two lines) fit every Duo
    /// window; the squeeze is a safety net should the toolbar zone come lower than measured: one-line 52s, then
    /// 44pt icons.
    private struct Rows {
        var tab: CGFloat = 62
        var oneLine = false
        var iconsOnly = false

        init(height: CGFloat, foot: CGFloat, iconsOnly large: Bool) {
            let room = height - foot - ForeEdge.fleuronRow - ForeEdge.penSize - ForeEdge.toolbarZone - 16
            if room < 4 * 62 { tab = 52; oneLine = true }
            if room < 4 * 52 { tab = 44; iconsOnly = true }
            if large { iconsOnly = true }
        }
    }

    /// The leaves riffling between two tabs, as when you thumb from one to the other.
    struct Riffle: Equatable {
        let from: Int, to: Int, start: Date
        /// Each 8pt band lights 12ms after the one before it, in the direction of travel; each pulse is 0.25s.
        static let pulse = 0.25, stagger = 0.012, band: CGFloat = 8
        func duration(row: CGFloat) -> Double {
            Riffle.pulse + Riffle.stagger * Double(abs(to - from)) * Double(row / Riffle.band) + 0.05
        }
    }
}

/// One index tab's face: its mark, then its name in small caps. Every mark stands at the same height in its row
/// whatever the name below it runs to (Turning Point takes two lines), so the four keep one rhythm down the
/// column. The paper tab marks the selected one; the running foot's oxblood rule has no room here and isn't needed.
private struct IndexLabel: View {
    let item: ShelfBar.Item
    let on: Bool
    let row: CGFloat
    let oneLine: Bool
    let iconsOnly: Bool
    @Environment(\.colorSchemeContrast) private var contrast
    @ScaledMetric(relativeTo: .caption2) private var label: CGFloat = 9

    /// The mark's top: centred alone, centred with one line of name in a squeezed row, else fixed.
    private var markTop: CGFloat { iconsOnly ? (row - 22) / 2 : oneLine ? (row - 36) / 2 : 10 }

    var body: some View {
        VStack(spacing: 3) {
            Image(systemName: item.symbol).font(.system(size: 18, weight: on ? .regular : .light))
                .frame(height: 22)
            if !iconsOnly {
                Text(item.title.uppercased()).font(VersoFont.body(min(label, 9 * 1.3))).tracking(0.8)
                    .lineLimit(oneLine ? 1 : 2)
                    .multilineTextAlignment(.center)
                    .minimumScaleFactor(0.8)
                    .frame(width: 68)
            }
        }
        .padding(.top, markTop)
        .frame(height: row, alignment: .top)
        // 9pt on the wash wants more than faded ink: ink at 72% (solid under Increase Contrast).
        .foregroundStyle(on ? Palette.oxblood : Palette.ink.opacity(contrast == .increased ? 1 : 0.72))
    }
}

/// The selected tab: paper, square on the page side (it is the page), rounded outside, with reverse-curve
/// fillets where it leaves the page edge so it grows out of the sheet rather than being stuck to it. Drawn for
/// a trailing column; mirrored by the caller.
private struct IndexTab: View {
    /// In the dark a shadow can't lift paper off the wash: the outline carries the edge instead.
    let dark: Bool
    @Environment(\.colorSchemeContrast) private var contrast
    var body: some View {
        let increased = contrast == .increased
        TabOutline(closed: true)
            .fill(Palette.paper)
            .shadow(color: Palette.ink.opacity(dark ? 0 : 0.08), radius: 3, x: 1.5)
            .overlay {
                TabOutline(closed: false)
                    .stroke(increased ? Palette.ink : dark ? Palette.ink.opacity(0.28) : Palette.rule,
                            lineWidth: increased ? 1 : dark ? 0.75 : 0.5)
            }
    }
}

/// The tab's outline in its rect, with the fillets standing above and below it on the page side. Open, it is
/// the edge that gets a hairline: top, outer side and bottom, never the side that joins the page.
private struct TabOutline: Shape {
    let closed: Bool
    var corner: CGFloat = 12
    var fillet: CGFloat = 6

    func path(in r: CGRect) -> Path {
        var p = Path()
        let x0 = r.minX, x1 = r.maxX, y0 = r.minY, y1 = r.maxY, f = fillet, c = corner
        p.move(to: CGPoint(x: x0, y: y0 - f))
        p.addQuadCurve(to: CGPoint(x: x0 + f, y: y0), control: CGPoint(x: x0, y: y0))
        p.addLine(to: CGPoint(x: x1 - c, y: y0))
        p.addQuadCurve(to: CGPoint(x: x1, y: y0 + c), control: CGPoint(x: x1, y: y0))
        p.addLine(to: CGPoint(x: x1, y: y1 - c))
        p.addQuadCurve(to: CGPoint(x: x1 - c, y: y1), control: CGPoint(x: x1, y: y1))
        p.addLine(to: CGPoint(x: x0 + f, y: y1))
        p.addQuadCurve(to: CGPoint(x: x0, y: y1 + f), control: CGPoint(x: x0, y: y1))
        if closed { p.closeSubpath() }
        return p
    }
}

/// The column's ground: the page-edge hairline the full height of the window, and from just above the pen to
/// the foot a deckle wash with three leaf edges in it (gilt once the book is in recto: a finished book is
/// gilt-edged). Both fade in over their first 40pt, so the status bar and Back stand on clean paper.
private struct LeafEdge: View {
    let fadeFrom: CGFloat
    /// Where the index's rows start and how tall each is: a riffle lights the leaves between two of them.
    let tabsTop: CGFloat
    let row: CGFloat
    let isRecto: Bool
    let riffle: ForeEdge.Riffle?
    let mirrored: Bool
    /// Deckle at 40% on dark paper is all but black on black: the wash is laid on solid there.
    let dark: Bool
    static let fade: CGFloat = 40

    var body: some View {
        TimelineView(.animation(paused: riffle == nil)) { context in
            Canvas { g, size in
                let leaf = isRecto ? Palette.gilt : Palette.rule
                let base: Double = isRecto ? 0.6 : dark ? 0.7 : 0.45
                let washOpacity: Double = dark ? 1 : 0.4
                func x(_ v: CGFloat) -> CGFloat { mirrored ? size.width - v : v }
                let top = max(0, fadeFrom)

                // The wash, fading in.
                let wash = CGRect(x: 0, y: top, width: size.width, height: size.height - top)
                g.fill(Path(wash), with: .linearGradient(
                    Gradient(stops: [.init(color: Palette.deckle.opacity(0), location: 0),
                                     .init(color: Palette.deckle.opacity(washOpacity), location: min(1, Self.fade / max(1, wash.height)))]),
                    startPoint: CGPoint(x: 0, y: top), endPoint: CGPoint(x: 0, y: size.height)))

                // The page edge, top to bottom.
                g.fill(Path(CGRect(x: x(0) - (mirrored ? 0.5 : 0), y: 0, width: 0.5, height: size.height)),
                       with: .color(Palette.rule))

                // The leaves, in 8pt bands so a riffle can light them one after another.
                let band = ForeEdge.Riffle.band
                let t = context.date
                var y = top
                while y < size.height {
                    let fadeIn = min(1, (y - top + band / 2) / Self.fade)
                    var a = base + (1 - base) * pulse(at: y + band / 2, now: t)
                    a *= fadeIn
                    for lx: CGFloat in [2, 4, 6] {
                        g.fill(Path(CGRect(x: x(lx) - 0.25, y: y, width: 0.5, height: min(band, size.height - y))),
                               with: .color(leaf.opacity(a)))
                    }
                    y += band
                }
            }
        }
    }

    /// 0…1…0 over a pulse, for a band at `y` lying between the two tabs of the riffle.
    private func pulse(at y: CGFloat, now: Date) -> Double {
        guard let r = riffle else { return 0 }
        func mid(_ i: Int) -> CGFloat { tabsTop + (CGFloat(i) + 0.5) * row }
        let a = mid(r.from), b = mid(r.to)
        guard y >= min(a, b) - row / 2, y <= max(a, b) + row / 2 else { return 0 }
        let delay = Double(abs(y - a) / ForeEdge.Riffle.band) * ForeEdge.Riffle.stagger
        let p = (now.timeIntervalSince(r.start) - delay) / ForeEdge.Riffle.pulse
        guard p > 0, p < 1 else { return 0 }
        return sin(p * .pi)
    }
}

/// Which edge, if any, the system sets its bars down: `toolbarVerticalEdge` (iOS 27.1), read inside a stack's
/// page, where it is meaningful, and passed up to RootView. Nil (horizontal bars) before 27.1.
struct BarEdgeKey: PreferenceKey {
    static let defaultValue: HorizontalEdge? = nil
    static func reduce(value: inout HorizontalEdge?, nextValue: () -> HorizontalEdge?) {
        if value == nil { value = nextValue() }
    }
}

struct BarEdgeProbe: View {
    var body: some View {
        if #available(iOS 27.1, *) { VerticalEdgeReader() } else { Color.clear }
    }
}

@available(iOS 27.1, *)
private struct VerticalEdgeReader: View {
    @Environment(\.toolbarVerticalEdge) private var edge
    var body: some View { Color.clear.preference(key: BarEdgeKey.self, value: edge) }
}
