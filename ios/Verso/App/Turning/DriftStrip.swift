import SwiftUI
import VersoCore

/// § Drift: every Marginalia save snapshots the board — a strip of stage glyphs over the months.
struct DriftStrip: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        let rows = model.state.pulses.filter { $0.board != nil }.sorted { $0.weekOf < $1.weekOf }
        SectionTitle("Drift", glyph: "≈")
        ScrollView(.horizontal) {
            HStack(alignment: .top, spacing: 14) {
                ForEach(rows) { p in
                    let b = p.board!
                    VStack(spacing: 6) {
                        Text(b.stage.initial)
                            .font(VersoFont.hero(30)).foregroundStyle(Palette.stage(b.stage))
                            .frame(width: 44, height: 44)
                            .overlay(Circle().strokeBorder(b.stage == .turningPoint ? Palette.gilt : Palette.rule, lineWidth: 0.75))
                        Text("\(b.met)/\(b.total)").font(VersoFont.ledger(.caption2)).foregroundStyle(Palette.fadedInk)
                        Text(p.weekOf.formatted(.dateTime.day().month(.abbreviated)).uppercased()).runningHeadStyle(size: 8)
                        Text(firstWords(p.sentence)).font(VersoFont.his(11)).foregroundStyle(Palette.fadedInk)
                            .multilineTextAlignment(.center).lineLimit(2)
                    }
                    .frame(width: 64)
                    .accessibilityElement(children: .combine)
                }
            }
            .padding(.vertical, 4)
        }
        .defaultScrollAnchor(.trailing)
        .scrollIndicators(.hidden)
        Aside("E exposition · R rising action · C crisis · T the turning point").padding(.top, 6)
    }

    func firstWords(_ s: String?) -> String {
        guard let s, !s.isEmpty else { return "" }
        return s.split(separator: " ").prefix(3).joined(separator: " ")
    }
}
