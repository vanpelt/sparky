import SwiftUI
import VersoCore

/// § Sealed letter: "A letter to Book Two's first page". Opens at the Turning Point, on its date,
/// or when he holds the seal for three seconds.
struct SealedLetterView: View {
    @Environment(AppModel.self) private var model
    @FocusState private var letterFocused: Bool
    @State private var body_ = ""
    @State private var withDate = false
    @State private var opensOn = Date().addingTimeInterval(86_400 * 180)
    @State private var holding = false

    var body: some View {
        SectionTitle("Sealed letter", glyph: "✉︎")
        if let letter = model.state.book.letter {
            if letter.isOpen { opened(letter) } else { sealed(letter) }
        } else if !model.isRecto {
            writing
        }
    }

    var writing: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("A letter to Book Two\u{2019}s first page.").font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
            TextField("", text: $body_, prompt: Text("Dear Book Two —").foregroundStyle(Palette.fadedInk.opacity(0.7)), axis: .vertical)
                .font(VersoFont.his(18)).lineLimit(4...12)
                .focused($letterFocused)
                // A letter keeps its paragraphs, so return stays a newline and this is the way out instead.
                // Keyboard toolbar items belong to the whole page, so it shows only while the letter has focus;
                // unconditional, it floated over every other field on the Turning Point and did nothing there.
                .toolbar {
                    if letterFocused {
                        ToolbarItemGroup(placement: .keyboard) {
                            Spacer()
                            Button("Done") { letterFocused = false }.font(VersoFont.serif(.body)).italic()
                        }
                    }
                }
                .padding(14)
                .background(Palette.deckle, in: RoundedRectangle(cornerRadius: 3))
            PaperToggle(label: "Also open it on a date", isOn: $withDate)
            if withDate {
                PaperDateRow(label: "Opens on", selection: $opensOn, range: Date()...)
            }
            if !body_.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                Button {
                    let b = body_, d = withDate ? opensOn : nil
                    model.perform { try await $0.sealLetter(b, opensOn: d) }
                    Haptics.playBookClosing()
                } label: {
                    Text("Seal it").font(VersoFont.serif(.headline)).italic().foregroundStyle(Palette.paper)
                        .padding(.horizontal, 24).padding(.vertical, 10)
                        .background(Palette.dusk, in: Capsule())
                }
                .buttonStyle(.plain)
                Aside("You won't be able to read it until it opens.")
            }
        }
    }

    func sealed(_ letter: SealedLetter) -> some View {
        VStack(spacing: 12) {
            ZStack {
                Circle().fill(Palette.dusk).frame(width: 96, height: 96)
                Circle().strokeBorder(Palette.paper.opacity(0.25), lineWidth: 1).frame(width: 82, height: 82)
                Text("V").font(VersoFont.hero(46)).foregroundStyle(Palette.paper)
                Circle().trim(from: 0, to: holding ? 1 : 0)
                    .stroke(Palette.gilt, style: StrokeStyle(lineWidth: 2, lineCap: .round))
                    .rotationEffect(.degrees(-90))
                    .frame(width: 108, height: 108)
                    .animation(holding ? .linear(duration: 3) : .easeOut(duration: 0.2), value: holding)
            }
            .scaleEffect(holding ? 0.96 : 1)
            .onLongPressGesture(minimumDuration: 3, pressing: { p in holding = p }) {
                Haptics.playBookClosing()
                model.perform { try await $0.openLetter() }
            }
            .accessibilityLabel("Sealed letter")
            .accessibilityHint("Hold for three seconds to open")
            .accessibilityAction(named: "Open the letter") { model.perform { try await $0.openLetter() } }
            Text("sealed \(DateText.long(letter.sealedAt))").runningHeadStyle(size: 10)
            Text(letter.opensOn.map { "Opens at the Turning Point, or on \(DateText.long($0)). Or hold the seal." }
                 ?? "Opens at the Turning Point. Or hold the seal.")
                .font(VersoFont.his(14)).foregroundStyle(Palette.fadedInk).multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 8)
    }

    func opened(_ letter: SealedLetter) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(letter.body).font(VersoFont.his(18)).foregroundStyle(Palette.ink).fixedSize(horizontal: false, vertical: true)
            Text("sealed \(DateText.long(letter.sealedAt)) · opened \(letter.openedAt.map(DateText.long) ?? "")")
                .runningHeadStyle(Palette.dusk, size: 9)
            if !model.isRecto {
                QuietButton("Write a new letter") {
                    model.perform { try await $0.updateBook { $0.letter = nil } }
                }
            }
        }
        .padding(16)
        .background(Palette.deckle, in: RoundedRectangle(cornerRadius: 3))
    }
}
