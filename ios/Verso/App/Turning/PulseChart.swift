import Charts
import SwiftUI
import VersoCore

/// § Pulse: twelve weeks of Toward (verdigris) against Away (oxblood), the gap shaded, missed weeks left blank.
struct PulseSection: View {
    @Environment(AppModel.self) private var model
    @State private var chartWidth: CGFloat = 0

    var body: some View {
        let pulse = model.board.pulse
        let today = model.board.today
        let cal = model.calendar
        let from = Days.add(-7 * 11, to: Days.weekStart(today, calendar: cal), calendar: cal)
        let points = pulse.points.filter { $0.weekOf >= from }
        let segs = segments(points, cal)

        SectionTitle("Pulse", glyph: "∿")
        if points.isEmpty {
            Aside("Your Marginalia draws this: each week, how much Book One gives and holds, and how hard Book Two pulls.")
            QuietButton("Write this week's Marginalia ›", color: Palette.oxblood) { model.sheet = .marginalia }
        } else {
            Chart {
                ForEach(points) { p in
                    BarMark(x: .value("Week", p.weekOf, unit: .weekOfYear),
                            yStart: .value("Away", p.away), yEnd: .value("Toward", p.toward), width: .ratio(0.9))
                        .foregroundStyle((p.toward >= p.away ? Palette.verdigris : Palette.oxblood).opacity(0.12))
                }
                ForEach(points) { p in
                    LineMark(x: .value("Week", p.weekOf, unit: .weekOfYear), y: .value("Toward", p.toward),
                             series: .value("Line", "toward-\(segs[p.weekOf] ?? 0)"))
                        .foregroundStyle(Palette.verdigris)
                        .lineStyle(StrokeStyle(lineWidth: 1.6, lineCap: .round))
                        .interpolationMethod(.monotone)
                    LineMark(x: .value("Week", p.weekOf, unit: .weekOfYear), y: .value("Away", p.away),
                             series: .value("Line", "away-\(segs[p.weekOf] ?? 0)"))
                        .foregroundStyle(Palette.oxblood)
                        .lineStyle(StrokeStyle(lineWidth: 1.6, lineCap: .round))
                        .interpolationMethod(.monotone)
                }
                if let x = pulse.theX, x >= from {
                    RuleMark(x: .value("The X", x, unit: .weekOfYear))
                        .foregroundStyle(Palette.gilt.opacity(0.7))
                        .lineStyle(StrokeStyle(lineWidth: 0.75, dash: [2, 3]))
                        .annotation(position: .top, alignment: .center) {
                            Text("the X").font(VersoFont.his(12)).foregroundStyle(Palette.gilt)
                        }
                }
            }
            .chartYScale(domain: 1...5)
            .chartXScale(domain: from...Days.add(7, to: Days.weekStart(today, calendar: cal), calendar: cal))
            .chartYAxis {
                AxisMarks(values: [1, 3, 5]) { v in
                    AxisGridLine().foregroundStyle(Palette.rule)
                    AxisValueLabel { Text(v.as(Int.self).map { $0 == 5 ? "hard" : $0 == 1 ? "faint" : "" } ?? "").font(VersoFont.his(10)) }
                }
            }
            .chartXAxis {
                AxisMarks(values: .stride(by: .weekOfYear, count: 4)) { _ in
                    AxisValueLabel(format: .dateTime.month(.abbreviated).day()).font(VersoFont.serif(.caption2))
                }
            }
            // 180 on a phone; on a wider page it keeps a 2.2:1 shape (to 240) rather than flattening to a strip.
            .frame(height: max(180, min(chartWidth / 2.2, 240)))
            .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { chartWidth = $0 }
            // Weeks are ISO (Monday-first) everywhere else; without this Charts buckets by the locale's
            // Sunday-first week and the axis labels land on the Sunday before each Marginalia week.
            .environment(\.calendar, Days.isoCalendar(from: cal))
            .environment(\.timeZone, cal.timeZone)
            .accessibilityLabel("Pulse over twelve weeks")

            HStack(spacing: 14) {
                legend(Palette.verdigris, "toward Book Two")
                legend(Palette.oxblood, "held by Book One")
            }
            .padding(.top, 6)

            Text(pulse.headline).displayFont(20, relativeTo: .title3).italic()
                .foregroundStyle(pulse.state == .pulls || pulse.leaning == .pulls ? Palette.verdigris : Palette.ink)
                .padding(.top, 12)
            if pulse.divergenceWeeks > 0 {
                Text("Pull outran energy \(pulse.divergenceWeeks) of the last 8 weeks.").font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
            }
            if pulse.burnout {
                Text("Three weeks spent and restless at once. That can be tiredness as much as the book.")
                    .font(VersoFont.his(14)).foregroundStyle(Palette.fadedInk).padding(.top, 4)
            }
            anchors(pulse)
        }
    }

    @ViewBuilder func anchors(_ pulse: PulseAnalysis) -> some View {
        let ranked = pulse.anchorRanking.filter { $0.reason != "nothing" }
        if !ranked.isEmpty {
            Text(ranked.prefix(4).map { "\($0.reason) ×\($0.count)" }.joined(separator: ", "))
                .font(VersoFont.his(16)).foregroundStyle(Palette.ink).padding(.top, 10)
            if ranked.first?.reason == "the money", let ebb = model.board.ledger?.chapters.first?.ebbWeight {
                Text("You named the money \(ranked[0].count) times; the next chapter adds \(UnitFraction.render(ebb)).")
                    .font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
            }
        }
    }

    /// Segment ids: a new line segment starts after any missed week, so gaps stay gaps.
    func segments(_ pts: [PulsePoint], _ cal: Calendar) -> [Date: Int] {
        var out: [Date: Int] = [:]
        var seg = 0
        var prev: Date?
        for p in pts {
            if let prev, Days.between(prev, p.weekOf, calendar: cal) > 7 { seg += 1 }
            out[p.weekOf] = seg
            prev = p.weekOf
        }
        return out
    }

    func legend(_ c: Color, _ s: String) -> some View {
        HStack(spacing: 6) {
            Rectangle().fill(c).frame(width: 12, height: 2)
            Text(s).font(VersoFont.his(13)).foregroundStyle(Palette.fadedInk)
        }
    }
}
