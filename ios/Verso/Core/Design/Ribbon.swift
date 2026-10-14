import SwiftUI
import WidgetKit

/// The bookmark ribbon: a vertical tab with a V-notched tail. Marks the current chapter.
public struct BookmarkRibbon: Shape {
    public var notch: CGFloat
    public init(notch: CGFloat = 0.28) { self.notch = notch }
    public func path(in r: CGRect) -> Path {
        var p = Path()
        let depth = r.width * notch * 2
        p.move(to: CGPoint(x: r.minX, y: r.minY))
        p.addLine(to: CGPoint(x: r.maxX, y: r.minY))
        p.addLine(to: CGPoint(x: r.maxX, y: r.maxY))
        p.addLine(to: CGPoint(x: r.midX, y: r.maxY - depth))
        p.addLine(to: CGPoint(x: r.minX, y: r.maxY))
        p.closeSubpath()
        return p
    }
}

/// The ribbon bar: a hairline track with an oxblood fill showing chapter progress.
public struct RibbonBar: View {
    let progress: Double
    var tint: Color
    public init(progress: Double, tint: Color = Palette.oxblood) { self.progress = progress; self.tint = tint }
    public var body: some View {
        GeometryReader { g in
            ZStack(alignment: .leading) {
                Rectangle().fill(Palette.rule).frame(height: 0.5)
                Rectangle().fill(tint).frame(width: g.size.width * min(1, max(0, progress)), height: 2)
                    .widgetAccentable()
            }
            .frame(maxHeight: .infinity, alignment: .center)
        }
        .frame(height: 4)
        .accessibilityElement()
        .accessibilityLabel("Chapter progress")
        .accessibilityValue("\(Int((progress * 100).rounded())) percent")
    }
}

/// Pen-stroke underline that draws left→right (0.35s easeOut) when `trigger` changes.
public struct PenStroke: View {
    let color: Color
    let trigger: Int
    @State private var trim: CGFloat = 1
    public init(color: Color, trigger: Int) { self.color = color; self.trigger = trigger }
    public var body: some View {
        GeometryReader { g in
            Path { p in p.move(to: .init(x: 0, y: 0.75)); p.addLine(to: .init(x: g.size.width, y: 0.75)) }
                .trim(from: 0, to: trim)
                .stroke(color, style: StrokeStyle(lineWidth: 1.5, lineCap: .round))
        }
        .frame(height: 1.5)
        .onChange(of: trigger) { _, _ in
            trim = 0
            withAnimation(.easeOut(duration: 0.35)) { trim = 1 }
        }
    }
}
