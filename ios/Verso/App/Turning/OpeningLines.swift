import SwiftUI
import VersoCore

/// § Opening lines: dated drafts of Book Two's first line. A new draft keeps the old ones.
struct OpeningLines: View {
    @Environment(AppModel.self) private var model
    /// A new draft as typed, kept in the model (`PageState.drafts`) so a fold doesn't lose it; never saved
    /// until "Keep this draft".
    private var draft: Binding<String> { model.pages.draft("opening-line") { "" } }

    var body: some View {
        let lines = model.state.book.openingLines.sorted { $0.date > $1.date }
        SectionTitle("Opening lines", glyph: "“")
        if let latest = lines.first {
            DropCapText(text: latest.text, size: 20)
            Text("draft of \(DateText.long(latest.date))").runningHeadStyle(size: 9).padding(.top, 6)
        } else {
            Aside("The first line of Book Two. Write it badly; write it again next month.")
        }
        if lines.count > 1 {
            DisclosureGroup(isExpanded: model.pages.expanded("earlier-drafts")) {
                VStack(alignment: .leading, spacing: 10) {
                    ForEach(lines.dropFirst()) { l in
                        VStack(alignment: .leading, spacing: 2) {
                            Text(l.text).font(VersoFont.his(16)).foregroundStyle(Palette.fadedInk)
                            Text(DateText.long(l.date)).runningHeadStyle(size: 9)
                        }
                    }
                }
                .padding(.top, 8)
            } label: {
                Text("Earlier drafts · \(lines.count - 1)").runningHeadStyle(size: 11)
            }
            .tint(Palette.fadedInk)
            .padding(.top, 10)
        }
        if !model.isRecto {
            PaperField(label: nil, text: draft, prompt: "a new draft…", axis: .vertical, id: "opening-line") { }
                .padding(.top, 12)
            if !draft.wrappedValue.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                QuietButton("Keep this draft", color: Palette.oxblood) {
                    let t = draft.wrappedValue.trimmingCharacters(in: .whitespacesAndNewlines)
                    draft.wrappedValue = ""
                    model.perform { try await $0.addOpeningLine(t) }
                }
            }
        }
    }
}
