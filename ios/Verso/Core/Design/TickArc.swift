import SwiftUI
import WidgetKit

/// The Turning Point gauge: a 220° arc, one tick per clause (unmet faded ink, met ink with a cap, holding gilt),
/// plus one separate ringed "!" for the regardless clauses. Never a continuous fill or a percentage.
public struct TickArc: View {
    let ticks: [TickState]
    let regardless: TickState?
    var lineWidth: CGFloat
    public static let sweep: Double = 220

    public init(ticks: [TickState], regardless: TickState? = nil, lineWidth: CGFloat = 2) {
        self.ticks = ticks; self.regardless = regardless; self.lineWidth = lineWidth
    }

    public var body: some View {
        Canvas { ctx, size in
            let r = min(size.width, size.height * 1.25) / 2 - lineWidth * 2
            let c = CGPoint(x: size.width / 2, y: r + lineWidth * 2)
            let start = 90 + (360 - Self.sweep) / 2   // degrees, measured clockwise from +x in screen space
            // The track: faded ink, not the rule colour, so it survives Night Reading paper.
            var track = Path()
            track.addArc(center: c, radius: r * 0.78, startAngle: .degrees(start), endAngle: .degrees(start + Self.sweep), clockwise: false)
            ctx.stroke(track, with: .color(Palette.fadedInk.opacity(0.55)), lineWidth: max(0.75, lineWidth * 0.42))

            let n = max(ticks.count, 1)
            let unmetWidth = max(1, lineWidth * 0.55)
            for (i, t) in ticks.enumerated() {
                let a = Angle.degrees(start + Self.sweep * (n == 1 ? 0.5 : Double(i) / Double(n - 1)))
                var p = Path()
                let inner = r * 0.66, outer = r * (t == .unmet ? 0.9 : 1.0)
                p.move(to: point(c, inner, a)); p.addLine(to: point(c, outer, a))
                switch t {
                case .unmet:
                    ctx.stroke(p, with: .color(Palette.fadedInk.opacity(0.85)), style: StrokeStyle(lineWidth: unmetWidth, lineCap: .round))
                case .met, .holding:
                    let color = t == .holding ? Palette.gilt : Palette.ink
                    ctx.stroke(p, with: .color(color), style: StrokeStyle(lineWidth: lineWidth, lineCap: .round))
                    // A small cap on the outer end, so met reads as "set down", not just "thicker".
                    let d = lineWidth * 1.8
                    let tip = point(c, outer, a)
                    ctx.fill(Path(ellipseIn: CGRect(x: tip.x - d / 2, y: tip.y - d / 2, width: d, height: d)), with: .color(color))
                }
            }
            if let reg = regardless {
                // The regardless marker: a ringed "!" beyond the arc's end ("Or, regardless —").
                let a = Angle.degrees(start + Self.sweep + 16)
                let d = max(7, lineWidth * 4.2)
                let pt = point(c, r * 0.83, a)
                let rect = CGRect(x: pt.x - d / 2, y: pt.y - d / 2, width: d, height: d)
                let met = reg != .unmet
                if met { ctx.fill(Path(ellipseIn: rect), with: .color(Palette.rust)) }
                else { ctx.stroke(Path(ellipseIn: rect), with: .color(Palette.rust.opacity(0.9)), lineWidth: max(0.75, lineWidth * 0.45)) }
                let bang = Text("!").font(.system(size: d * 0.72, weight: .bold, design: .serif))
                    .foregroundColor(met ? Palette.paper : Palette.rust)
                ctx.draw(bang, at: pt, anchor: .center)
            }
        }
        .aspectRatio(1.35, contentMode: .fit)
        .widgetAccentable()
        .accessibilityElement()
        .accessibilityLabel("Clauses")
        .accessibilityValue("\(ticks.filter { $0 != .unmet }.count) of \(ticks.count) met, \(ticks.filter { $0 == .holding }.count) held 14 days\(regardless.map { $0 == .unmet ? ", regardless clause not met" : ", a regardless clause is met" } ?? "")")
    }

    func point(_ c: CGPoint, _ r: CGFloat, _ a: Angle) -> CGPoint {
        CGPoint(x: c.x + r * cos(a.radians), y: c.y + r * sin(a.radians))
    }
}

/// A 28pt inline version for the Contents "Turning Point line".
public struct TickGlyph: View {
    let ticks: [TickState]
    public init(ticks: [TickState]) { self.ticks = ticks }
    public var body: some View { TickArc(ticks: ticks, lineWidth: 1.2).frame(width: 28, height: 21) }
}
