import Charts
import SwiftUI
import VersoCore

/// § The marginal chapter: a pale, near-flat oxblood band (value) under a falling verdigris line (Ebb weight).
/// No dollar axis labels, so the chart stays visible under the veil.
struct MarginalChart: View {
    let l: LedgerResult
    /// Off a phone's column (up to 410, the closed Duo's included) height follows width (2.2 : 1), so a wide
    /// page doesn't flatten the curve into a strip; never shorter than the phone's fixed 170, never taller than 220.
    @State private var width: CGFloat = 0

    var body: some View {
        SectionTitle("The marginal chapter")
        let maxValue = l.chapters.map(\.value).max() ?? 1
        let maxEbb = l.chapters.map(\.ebbWeight).max() ?? 1
        Chart {
            // What each chapter pays: nearly flat, so a pale band with one hairline, not a row of identical bars.
            ForEach(l.chapters) { c in
                AreaMark(x: .value("Chapter", c.numeral), y: .value("Value", c.value / maxValue),
                         series: .value("Series", "pays"))
                    .foregroundStyle(Palette.oxblood.opacity(0.08))
                    .interpolationMethod(.monotone)
                LineMark(x: .value("Chapter", c.numeral), y: .value("Value", c.value / maxValue),
                         series: .value("Series", "pays"))
                    .foregroundStyle(Palette.oxblood.opacity(0.55))
                    .lineStyle(StrokeStyle(lineWidth: 0.75, dash: [3, 3]))
                    .interpolationMethod(.monotone)
            }
            // What it adds: the mark that actually moves.
            ForEach(l.chapters) { c in
                LineMark(x: .value("Chapter", c.numeral), y: .value("Ebb", c.ebbWeight / maxEbb),
                         series: .value("Series", "adds"))
                    .foregroundStyle(Palette.verdigris)
                    .lineStyle(StrokeStyle(lineWidth: 2.2, lineCap: .round))
                    .interpolationMethod(.monotone)
                PointMark(x: .value("Chapter", c.numeral), y: .value("Ebb", c.ebbWeight / maxEbb))
                    .foregroundStyle(Palette.verdigris)
                    .symbolSize(22)
                    .annotation(position: c.id == l.chapters.last?.id ? .topLeading : .topTrailing, spacing: 2) {
                        if c.id == l.chapters.first?.id || c.id == l.chapters.last?.id {
                            Text(UnitFraction.render(c.ebbWeight)).font(VersoFont.serif(.caption2)).foregroundStyle(Palette.verdigris)
                                .padding(.horizontal, 2)
                                .background(Palette.paper.opacity(0.9), in: .rect(cornerRadius: 2))
                        }
                    }
            }
        }
        .chartYAxis(.hidden)
        .chartYScale(domain: 0...1.15)
        .chartXAxis {
            AxisMarks { _ in
                AxisValueLabel().font(VersoFont.serif(.caption2)).foregroundStyle(Palette.fadedInk)
            }
        }
        .frame(height: width <= 410 ? 170 : min(220, max(170, width / 2.2)))
        .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { width = $0 }
        .accessibilityLabel("Each remaining chapter pays about the same; each adds a smaller share of the total.")

        HStack(spacing: 14) {
            legend(Palette.oxblood.opacity(0.55), "what it pays (about the same)")
            legend(Palette.verdigris, "what it adds")
        }
        .padding(.top, 8)
        Aside("Every chapter pays about the same. Each one adds a smaller share of everything you\u{2019}ll have.")
            .padding(.top, 6)
    }

    func legend(_ c: Color, _ s: String) -> some View {
        HStack(spacing: 6) {
            Rectangle().fill(c).frame(width: 12, height: 2)
            Text(s).font(VersoFont.his(13)).foregroundStyle(Palette.fadedInk)
        }
    }
}
