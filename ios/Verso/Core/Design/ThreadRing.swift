import SwiftUI
import WidgetKit

/// A thread's progress ring in its binding colour.
public struct ThreadRing<Center: View>: View {
    let progress: Double
    let binding: Binding
    var lineWidth: CGFloat
    let center: Center

    public init(progress: Double, binding: Binding, lineWidth: CGFloat = 3, @ViewBuilder center: () -> Center) {
        self.progress = progress; self.binding = binding; self.lineWidth = lineWidth; self.center = center()
    }

    public var body: some View {
        ZStack {
            Circle().stroke(Palette.rule, lineWidth: 0.5)
            Circle()
                .trim(from: 0, to: min(1, max(0, progress)))
                .stroke(Palette.bindingStroke(binding), style: StrokeStyle(lineWidth: lineWidth, lineCap: .round))
                .rotationEffect(.degrees(-90))
                .widgetAccentable()
                .animation(.smooth(duration: 0.5), value: progress)
            center
        }
        .accessibilityElement(children: .combine)
        .accessibilityValue("\(Int((progress * 100).rounded())) percent")
    }
}

public extension ThreadRing where Center == EmptyView {
    init(progress: Double, binding: Binding, lineWidth: CGFloat = 3) {
        self.init(progress: progress, binding: binding, lineWidth: lineWidth) { EmptyView() }
    }
}

/// The 44pt cloth spine with the thread name in rotated small caps.
public struct ThreadSpine: View {
    let name: String
    let binding: Binding
    public init(name: String, binding: Binding) { self.name = name; self.binding = binding }
    public var body: some View {
        Rectangle()
            .fill(Palette.binding(binding))
            .overlay {
                Text(name.uppercased())
                    .font(VersoFont.runningHead(11)).tracking(1.5)
                    .foregroundStyle(Color(light: 0xF2ECDF, dark: 0xE8E0CF))
                    .lineLimit(1).fixedSize()
                    .rotationEffect(.degrees(-90))
            }
            .frame(width: 44)
            .clipped()
    }
}

/// Tiny 6-week tick strip: one dash per week, filled when the week met its pace.
public struct WeekTicks: View {
    let ticks: [Bool]
    let binding: Binding
    public init(_ ticks: [Bool], binding: Binding) { self.ticks = ticks; self.binding = binding }
    public var body: some View {
        HStack(spacing: 3) {
            ForEach(Array(ticks.enumerated()), id: \.offset) { _, on in
                Capsule().fill(on ? Palette.bindingStroke(binding) : Palette.rule).frame(width: 8, height: on ? 2 : 1)
            }
        }
        .accessibilityHidden(true)
    }
}
