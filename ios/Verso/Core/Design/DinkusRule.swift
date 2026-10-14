import SwiftUI

/// The section divider: a 0.5pt hairline with a centred ⁂.
public struct DinkusRule: View {
    var glyph: String
    public init(_ glyph: String = Glyph.asterism) { self.glyph = glyph }
    public var body: some View {
        HStack(spacing: 12) {
            Hairline()
            Text(glyph).font(VersoFont.serif(.footnote)).foregroundStyle(Palette.fadedInk).accessibilityHidden(true)
            Hairline()
        }
        .padding(.vertical, 18)
    }
}

/// A 0.5pt `rule` line.
public struct Hairline: View {
    var color: Color
    public init(_ color: Color = Palette.rule) { self.color = color }
    public var body: some View { Rectangle().fill(color).frame(height: 0.5) }
}

/// The running head at the top of every page, e.g. "BOOK ONE · CHAPTER VI · ❦".
public struct RunningHead<Trailing: View>: View {
    let text: String
    let trailing: Trailing
    public init(_ text: String, @ViewBuilder trailing: () -> Trailing) { self.text = text; self.trailing = trailing() }
    public var body: some View {
        HStack(spacing: 6) {
            Text(text).runningHeadStyle()
            trailing
            Spacer(minLength: 0)
        }
        .accessibilityElement(children: .combine)
    }
}

public extension RunningHead where Trailing == EmptyView {
    init(_ text: String) { self.init(text) { EmptyView() } }
}

/// The folio at the foot of every page: "p. 412".
public struct Folio: View {
    let page: Int
    public init(_ page: Int) { self.page = page }
    public var body: some View {
        Text("p. \(page.formatted(.number.grouping(.automatic)))")
            .font(VersoFont.serif(.caption)).foregroundStyle(Palette.fadedInk)
            .frame(maxWidth: .infinity).padding(.vertical, 24)
    }
}

/// A leader-dot row: "VI.   The Long Autumn .......... Nov '26   $787k"
public struct LeaderRow<Leading: View, Trailing: View>: View {
    let leading: Leading
    let trailing: Trailing
    public init(@ViewBuilder leading: () -> Leading, @ViewBuilder trailing: () -> Trailing) {
        self.leading = leading(); self.trailing = trailing()
    }
    public var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 4) {
            leading.layoutPriority(1)
            LeaderDots().frame(height: 1).alignmentGuide(.firstTextBaseline) { $0[.bottom] + 3 }
            trailing.layoutPriority(1)
        }
    }
}

public struct LeaderDots: View {
    public init() {}
    public var body: some View {
        GeometryReader { g in
            Path { p in
                var x: CGFloat = 2
                while x < g.size.width - 2 { p.addEllipse(in: CGRect(x: x, y: 0, width: 1.2, height: 1.2)); x += 5 }
            }
            .fill(Palette.fadedInk.opacity(0.7))
        }
        .frame(minWidth: 12)
        .accessibilityHidden(true)
    }
}

/// Strikethrough hairline for vested chapters and done milestones.
public extension View {
    func readStrike(_ on: Bool) -> some View {
        overlay(alignment: .center) { if on { Hairline(Palette.fadedInk) } }
    }
}
