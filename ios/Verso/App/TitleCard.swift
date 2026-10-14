import SwiftUI
import VersoCore

/// A full-screen title sequence (~1.6s, tap to skip). Plays only on first launch, the first open
/// on or after a vest, and Close Book One.
struct TitleCardScript: Equatable, Identifiable {
    struct Line: Equatable { var text: String; var style: Style }
    enum Style: Equatable { case title, running, italic, blank }
    var lines: [Line]
    var haptic: Haptic = .none
    enum Haptic: Equatable { case none, bookClosing, bookClosingRamp }
    var id: String { lines.map(\.text).joined(separator: "|") }

    static let firstLaunch = TitleCardScript(lines: [.init(text: "VERSO", style: .title), .init(text: "BOOK ONE", style: .running)])

    static func vest(ended numeral: String, next: String?) -> TitleCardScript {
        TitleCardScript(lines: [.init(text: "END OF CHAPTER \(numeral) \(Glyph.fleuron)", style: .running),
                                .init(text: next ?? "", style: .italic)].filter { !$0.text.isEmpty },
                        haptic: .bookClosing)
    }

    static let theEnd = TitleCardScript(lines: [.init(text: "THE END", style: .title), .init(text: "", style: .blank),
                                                .init(text: "BOOK TWO · Page 1", style: .running)],
                                        haptic: .bookClosingRamp)
}

struct TitleCardView: View {
    let script: TitleCardScript
    let done: () -> Void
    @State private var index = 0
    @State private var shown = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        ZStack {
            Palette.paper.ignoresSafeArea()
            if index < script.lines.count {
                line(script.lines[index])
                    .id(index)
                    .transition(reduceMotion ? .opacity : .asymmetric(
                        insertion: .opacity.combined(with: .scale(scale: 1.02)),
                        removal: .opacity))
            }
        }
        .contentShape(Rectangle())
        .onTapGesture { finish() }
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(.isButton)
        .accessibilityHint("Tap to skip")
        .task {
            switch script.haptic {
            case .none: break
            case .bookClosing: Haptics.playBookClosing()
            case .bookClosingRamp: Haptics.playBookClosing(withRamp: true)
            }
            let per = max(0.55, 1.6 / Double(max(1, script.lines.count)))
            for i in script.lines.indices {
                withAnimation(.easeInOut(duration: reduceMotion ? 0.2 : 0.4)) { index = i }
                try? await Task.sleep(for: .seconds(per + (script.lines[i].style == .blank ? -0.2 : 0.15)))
            }
            finish()
        }
    }

    @ViewBuilder func line(_ l: TitleCardScript.Line) -> some View {
        switch l.style {
        case .title:
            Text(l.text).font(VersoFont.hero(64)).tracking(10).foregroundStyle(Palette.ink).heroCap()
        case .running:
            Text(l.text).runningHeadStyle(Palette.oxblood, size: 15)
        case .italic:
            // Held to the text measure: on a wide window a chapter's name stays a line or two, not a banner.
            Text(l.text).font(VersoFont.his(26)).foregroundStyle(Palette.ink).multilineTextAlignment(.center)
                .frame(maxWidth: Measure.text).padding(32)
        case .blank:
            Color.clear
        }
    }

    func finish() {
        withAnimation(.easeOut(duration: 0.35)) { done() }
    }
}

#Preview { TitleCardView(script: .theEnd) {} }
