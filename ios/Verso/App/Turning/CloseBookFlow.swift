import SwiftUI
import VersoCore

/// Close Book One. Step one: a date and a closing sentence. Step two: a two-second press-and-hold.
/// Only he presses this; nothing in Verso says "go".
struct CloseBookFlow: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var step = 0
    @State private var date = Date()
    @State private var sentence = ""
    @State private var holding = false

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("CLOSE BOOK ONE").runningHeadStyle(Palette.oxblood).padding(.top, 28)
            if step == 0 {
                Text("Are you closing the book?").displayFont(30).italic()
                Aside("This puts Verso into Book Two. The Ledger freezes, the Turning Point becomes the Turn, and the threads become the main text.")
                PaperDateRow(label: "The last page", selection: $date, size: 17)
                PaperField(label: "A CLOSING SENTENCE", text: $sentence, prompt: "It was a good book. It was time.", axis: .vertical)
                Spacer()
                HStack {
                    QuietButton("Not yet") { dismiss() }
                    Spacer()
                    Button { withAnimation(.smooth) { step = 1 } } label: {
                        Text("Turn the page").font(VersoFont.serif(.headline)).italic().foregroundStyle(Palette.ink)
                            .padding(.horizontal, 22).padding(.vertical, 10)
                    }
                    .buttonStyle(.glass)
                }
            } else {
                Spacer()
                VStack(spacing: 18) {
                    Text("Hold to close").displayFont(28).italic()
                    ZStack {
                        Circle().strokeBorder(Palette.rule, lineWidth: 0.75).frame(width: 150, height: 150)
                        Circle().trim(from: 0, to: holding ? 1 : 0)
                            .stroke(Palette.oxblood, style: StrokeStyle(lineWidth: 3, lineCap: .round))
                            .rotationEffect(.degrees(-90))
                            .frame(width: 150, height: 150)
                            .animation(holding ? .linear(duration: 2) : .easeOut(duration: 0.2), value: holding)
                        Text("THE END").runningHeadStyle(Palette.ink, size: 13)
                    }
                    .contentShape(Circle())
                    .onLongPressGesture(minimumDuration: 2, pressing: { p in holding = p }, perform: commit)
                    .accessibilityLabel("Close Book One")
                    .accessibilityHint("Hold for two seconds")
                    .accessibilityAction(named: "Close Book One", commit)
                    Aside("Two seconds. Let go to stop.")
                }
                .frame(maxWidth: .infinity)
                Spacer()
                QuietButton("Back") { withAnimation(.smooth) { step = 0 } }
            }
        }
        .frame(maxWidth: Measure.text, alignment: .leading)
        .padding(.horizontal, 24)
        .padding(.bottom, 24)
        .frame(maxWidth: .infinity)
    }

    func commit() {
        let d = date, s = sentence.trimmingCharacters(in: .whitespacesAndNewlines)
        dismiss()
        model.titleCard = .theEnd
        model.perform { try await $0.closeBookOne(on: d, sentence: s.isEmpty ? nil : s) }
    }
}
