import WidgetKit
import SwiftUI
import VersoCore

/// Debug gallery: every widget kind in every family, rendered in-app. `live` = the real snapshot
/// (e.g. `SnapshotBuilder.build(from: model.board)`); nil shows sample data only.
struct WidgetGalleryView: View {
    let live: VersoSnapshot?
    @State private var useLive: Bool
    @State private var profile: WidgetSlotProfile = .proMax
    private let anchor: UnitPoint?
    /// The app's veil: when it is down and the gallery shows live data, privacy-sensitive figures redact.
    @Environment(\.versoVeiled) private var appVeiled

    /// `startLive` / `anchor` let debug launch arguments open the gallery on live data or scrolled.
    init(live: VersoSnapshot? = nil, startLive: Bool = false, anchor: UnitPoint? = nil) {
        self.live = live; self.anchor = anchor
        _useLive = State(initialValue: startLive && live != nil)
        // `-versoGalleryProfile "Duo outer"` opens on a slot profile, for screenshots.
        _profile = State(initialValue: UserDefaults.standard.string(forKey: "versoGalleryProfile")
            .flatMap(WidgetSlotProfile.init(rawValue:)) ?? .proMax)
    }

    enum Kind: String, CaseIterable, Identifiable {
        case nextChapter = "Next Chapter", dayRate = "Day Rate", turningPoint = "Turning Point", thread = "Thread"
        var id: String { rawValue }
        /// The same list the widget's configuration offers.
        var families: [WidgetFamily] {
            switch self {
            case .nextChapter: VersoFamilies.supported(WidgetKinds.nextChapter)
            case .dayRate: VersoFamilies.supported(WidgetKinds.dayRate)
            case .turningPoint: VersoFamilies.supported(WidgetKinds.turningPoint)
            case .thread: VersoFamilies.supported(WidgetKinds.thread)
            }
        }
    }

    private var snapshot: VersoSnapshot { (useLive ? live : nil) ?? SampleData.snapshot }
    private var date: Date { useLive ? Date() : SampleData.today }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 28) {
                if live != nil {
                    Picker("Data", selection: $useLive) {
                        Text("Sample").tag(false)
                        Text("Live").tag(true)
                    }
                    .pickerStyle(.segmented)
                }
                VStack(alignment: .leading, spacing: 6) {
                    Picker("Slots", selection: $profile) {
                        ForEach(WidgetSlotProfile.allCases) { Text($0.rawValue).tag($0) }
                    }
                    .pickerStyle(.segmented)
                    if profile.isEstimate {
                        Text("Estimated slot sizes (TBD): measure on an iPhone Duo.")
                            .font(VersoFont.his(13)).foregroundStyle(Palette.fadedInk)
                    }
                }
                ForEach(Kind.allCases) { kind in
                    VStack(alignment: .leading, spacing: 12) {
                        Text(kind.rawValue.uppercased()).runningHeadStyle()
                        let system = kind.families.filter { !$0.isAccessory }
                        let accessory = kind.families.filter(\.isAccessory)
                        // Side by side where the page is wide enough (an iPad, the open Duo), stacked on a phone.
                        TileFlow(spacing: 14) {
                            ForEach(system, id: \.self) { f in tile(kind, f) }
                        }
                        VStack(alignment: .leading, spacing: 10) {
                            HStack(spacing: 10) {
                                ForEach(accessory.filter { $0 != .accessoryInline }, id: \.self) { f in tile(kind, f) }
                            }
                            if accessory.contains(.accessoryInline) { tile(kind, .accessoryInline) }
                        }
                    }
                    DinkusRule()
                }
            }
            .padding(.vertical, 20)
            // 16, not 20: a 364pt large tile must fit a 402pt phone.
            .padding(.horizontal, 16)
        }
        .defaultScrollAnchor(anchor)
        // A hard edge under the bar so dark tiles never scroll beneath the back button and title.
        .scrollEdgeEffectStyle(.hard, for: .top)
        .background(Palette.deckle.ignoresSafeArea())
        .navigationTitle("Widget Gallery")
        .navigationBarTitleDisplayMode(.inline)
    }

    @ViewBuilder
    func tile(_ kind: Kind, _ f: WidgetFamily) -> some View {
        WidgetPreviewFrame(family: f, profile: profile) { view(kind, f) }
            .redacted(reason: useLive && appVeiled ? .privacy : [])
    }

    @ViewBuilder
    func view(_ kind: Kind, _ f: WidgetFamily) -> some View {
        let e = VersoEntry.make(snapshot, at: date)
        switch kind {
        case .nextChapter: NextChapterWidgetView(entry: e, family: f)
        case .dayRate: DayRateWidgetView(entry: e, family: f)
        case .turningPoint: TurningPointWidgetView(entry: e, family: f)
        case .thread: ThreadWidgetView(entry: .make(snapshot, at: date, featuring: useLive ? nil : SampleData.serbianID), family: f)
        }
    }
}

/// Lays tiles left to right and wraps when the next one doesn't fit, top-aligned within each line. A tile
/// wider than the page keeps its size (it is a fixed slot) and sits on a line of its own.
struct TileFlow: Layout {
    var spacing: CGFloat = 14

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let lines = lines(width: proposal.width ?? .infinity, subviews)
        let w = lines.map(\.width).max() ?? 0
        let h = lines.map(\.height).reduce(0, +) + spacing * CGFloat(max(0, lines.count - 1))
        return CGSize(width: w, height: h)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        var y = bounds.minY
        for line in lines(width: bounds.width, subviews) {
            var x = bounds.minX
            for i in line.items {
                let s = subviews[i].sizeThatFits(.unspecified)
                subviews[i].place(at: CGPoint(x: x, y: y), proposal: ProposedViewSize(s))
                x += s.width + spacing
            }
            y += line.height + spacing
        }
    }

    private struct Line { var items: [Int] = []; var width: CGFloat = 0; var height: CGFloat = 0 }

    private func lines(width: CGFloat, _ subviews: Subviews) -> [Line] {
        var out: [Line] = [], cur = Line()
        for i in subviews.indices {
            let s = subviews[i].sizeThatFits(.unspecified)
            let w = cur.items.isEmpty ? s.width : cur.width + spacing + s.width
            if !cur.items.isEmpty && w > width { out.append(cur); cur = Line() }
            cur.width = cur.items.isEmpty ? s.width : cur.width + spacing + s.width
            cur.height = max(cur.height, s.height)
            cur.items.append(i)
        }
        if !cur.items.isEmpty { out.append(cur) }
        return out
    }
}

#Preview("Widget Gallery") {
    NavigationStack { WidgetGalleryView() }
}
