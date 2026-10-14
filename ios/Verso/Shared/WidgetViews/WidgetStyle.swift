import WidgetKit
import SwiftUI
import VersoCore

/// The paper look on the Home Screen; nothing at all in accented / vibrant modes (tinted or clear Home Screen,
/// Lock Screen, StandBy at night), where the geometry and `.widgetAccentable()` carry the meaning.
struct VersoWidgetBackground: ViewModifier {
    @Environment(\.widgetRenderingMode) private var mode
    func body(content: Content) -> some View {
        content.containerBackground(for: .widget) {
            if mode == .fullColor { WidgetPaper() } else { Color.clear }
        }
    }
}

/// Paper with a faint deckle shading toward the bottom edge, like light falling across a page.
struct WidgetPaper: View {
    var body: some View {
        ZStack {
            Palette.paper
            LinearGradient(colors: [Palette.deckle.opacity(0), Palette.deckle.opacity(0.55)],
                           startPoint: .top, endPoint: .bottom)
        }
    }
}

extension View {
    func versoWidgetBackground() -> some View { modifier(VersoWidgetBackground()) }
}

/// The one list of families each widget offers. Every `Widget` configuration and the in-app gallery read it, and
/// `WidgetParityTests` checks that every view's `switch` names each of them, so a new family can't silently fall
/// through to a stretched small layout.
enum VersoFamilies {
    /// Home Screen sizes. 4×6 extra-large portrait (a whole Home Screen page) arrives with iOS 27.
    static var home: [WidgetFamily] {
        if #available(iOS 27.0, *) { return [.systemSmall, .systemMedium, .systemLarge, .systemExtraLargePortrait] }
        return [.systemSmall, .systemMedium, .systemLarge]
    }
    /// Lock Screen / StandBy sizes.
    static let accessory: [WidgetFamily] = [.accessoryCircular, .accessoryRectangular, .accessoryInline]

    /// Everything a widget kind offers. Day Rate has no circular face: a rate doesn't fit in a 76pt ring.
    static func supported(_ kind: String) -> [WidgetFamily] {
        home + (kind == WidgetKinds.dayRate ? accessory.filter { $0 != .accessoryCircular } : accessory)
    }
}

/// Resolves a view's family: the explicit one (app gallery) or the widget environment's.
struct FamilyReader<Content: View>: View {
    let explicit: WidgetFamily?
    @ViewBuilder let content: (WidgetFamily) -> Content
    @Environment(\.widgetFamily) private var envFamily
    var body: some View { content(explicit ?? envFamily) }
}

/// Small-caps footer line that turns rust when the thing it reports is stale.
struct WidgetFooter: View {
    let text: String
    var warn: Bool = false
    var body: some View {
        Text(text)
            .font(VersoFont.runningHead(9)).tracking(1)
            .foregroundStyle(warn ? Palette.rust : Palette.fadedInk)
            .lineLimit(1).minimumScaleFactor(0.7)
    }
}

/// The "nothing to show yet" page, in his-voice italic. `large` sets it as a whole page (large and extra-large
/// slots): bigger italic, and the fleuron centred at the foot like the end of a chapter.
struct WidgetBlankPage: View {
    let line: String
    var head: String = "VERSO"
    var large = false
    var body: some View {
        if large { page } else { tile }
    }

    private var page: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(head).runningHeadStyle(size: 11)
            Hairline()
            Spacer(minLength: 0)
            Text(line).font(VersoFont.his(22)).foregroundStyle(Palette.fadedInk)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: 280, alignment: .leading)
            Spacer(minLength: 0)
            Text(Glyph.fleuron).font(VersoFont.display(18)).foregroundStyle(Palette.gilt)
                .frame(maxWidth: .infinity)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
    }

    private var tile: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(head).runningHeadStyle(size: 10)
            Spacer(minLength: 0)
            Text(line).font(VersoFont.his(15)).foregroundStyle(Palette.fadedInk)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
            Text(Glyph.fleuron).font(VersoFont.display(14)).foregroundStyle(Palette.gilt)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
    }
}

/// Shown instead of any figure when the store can't be read (never sample numbers, never zeros).
struct WidgetRefreshNotice: View {
    let family: WidgetFamily
    var head: String = "VERSO"
    static let line = "Open Verso to refresh."
    var body: some View {
        switch family {
        case .accessoryInline: Text(Self.line)
        case .accessoryCircular: Text(Glyph.fleuron).font(VersoFont.display(22)).accessibilityLabel(Self.line)
        case .accessoryRectangular:
            Text(Self.line).font(VersoFont.his(14)).frame(maxWidth: .infinity, alignment: .leading)
        case .systemSmall, .systemMedium: WidgetBlankPage(line: Self.line, head: head)
        case .systemLarge, .systemExtraLargePortrait: WidgetBlankPage(line: Self.line, head: head, large: true)
        default: WidgetBlankPage(line: Self.line, head: head)
        }
    }
}

/// Home Screen slot sizes to preview widgets at. The 17 Pro Max values are the measured slots (its 4×6 is two
/// rows of small plus their gap); the Duo profiles are ESTIMATES until slots are logged on hardware (TBD after
/// Oct 23): the 17 Pro Max's 38pt side margins and 24pt gutter applied to the outer display's 466pt and to one
/// 475pt Home Screen page of the open spread.
enum WidgetSlotProfile: String, CaseIterable, Identifiable {
    case proMax = "17 Pro Max", duoOuter = "Duo outer", duoInner = "Duo inner"
    var id: String { rawValue }
    var isEstimate: Bool { self != .proMax }

    /// One small slot's side: the page minus two 38pt margins and one 24pt gutter, halved.
    private var small: CGFloat {
        switch self {
        case .proMax: 170
        case .duoOuter: ((466 - 76 - 24) / 2).rounded(.down)
        case .duoInner: ((475 - 76 - 24) / 2).rounded(.down)
        }
    }

    func size(_ family: WidgetFamily) -> CGSize {
        let s = small, wide = s * 2 + 24
        switch family {
        case .systemSmall: return CGSize(width: s, height: s)
        case .systemMedium: return CGSize(width: wide, height: s)
        case .systemLarge: return CGSize(width: wide, height: s * 2 + 42)
        case .systemExtraLargePortrait: return CGSize(width: wide, height: s * 3 + 84)
        case .accessoryCircular: return CGSize(width: 76, height: 76)
        case .accessoryRectangular: return CGSize(width: 172, height: 76)
        case .accessoryInline: return CGSize(width: 257, height: 26)
        default: return CGSize(width: s, height: s)
        }
    }
}

/// Renders a widget view in the app the way the system would place it (a `WidgetSlotProfile`'s slot sizes):
/// paper and the system's content margins for system families, a dark tile for accessory families.
struct WidgetPreviewFrame<Content: View>: View {
    let family: WidgetFamily
    var profile: WidgetSlotProfile = .proMax
    @ViewBuilder let content: () -> Content
    @Environment(\.widgetContentMargins) private var systemMargins

    init(family: WidgetFamily, profile: WidgetSlotProfile = .proMax, @ViewBuilder content: @escaping () -> Content) {
        self.family = family; self.profile = profile; self.content = content
    }

    static func size(_ family: WidgetFamily) -> CGSize { WidgetSlotProfile.proMax.size(family) }

    /// WidgetKit's default margins where it supplies them; outside a widget (the app) it may report none, so
    /// fall back to the 16pt the Home Screen uses.
    private var margins: EdgeInsets {
        let m = systemMargins
        return m.top + m.leading + m.bottom + m.trailing > 0 ? m : EdgeInsets(top: 16, leading: 16, bottom: 16, trailing: 16)
    }

    var body: some View {
        let s = profile.size(family)
        if family.isAccessory {
            content()
                .foregroundStyle(.white)
                .frame(width: s.width, height: s.height)
                // A little Lock Screen around the widget's real bounds, so edge-to-edge text doesn't look clipped.
                .padding(family == .accessoryCircular ? 0 : 8)
                .background(
                    RoundedRectangle(cornerRadius: family == .accessoryCircular ? s.width / 2 : 14, style: .continuous)
                        .fill(Color(white: 0.12))
                )
                .environment(\.colorScheme, .dark)
        } else {
            content()
                .padding(margins)
                .frame(width: s.width, height: s.height)
                .background(WidgetPaper())
                .clipShape(RoundedRectangle(cornerRadius: 24, style: .continuous))
        }
    }
}
