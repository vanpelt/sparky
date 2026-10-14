import SwiftUI
import VersoCore

/// The Turning Point — the heart. His conditions, the evidence, and a Reading that ends in a question.
/// Nothing here ever says "go".
struct TurningPointView: View {
    @Environment(AppModel.self) private var model
    /// The hero's column: the arc grows with it on a wide page, never below the phone's 220.
    @State private var heroWidth: CGFloat = 0
    @Environment(\.pageLayout) private var layout

    var body: some View {
        let board = model.board
        let c = board.clarity
        let clauses = model.state.book.clauses
        let main = clauses.filter { $0.role != .regardless }
        let regardless = clauses.filter { $0.role == .regardless }
        // Open, a book: his case on the left (the gauge, the clauses, the stay case set against them), the
        // evidence on the right (the Reading, the Pulse and Drift it is drawn from, and Book Two's first pages).
        // In one column it reads exactly as it always has.
        Spread(folio: board.folio, rectoHead: "THE EVIDENCE") {
            HeadBar(model.isRecto ? "THE TURN" : "THE TURNING POINT")
        } verso: {
            // Hero
            VStack(spacing: 6) {
                TickArc(ticks: c.tickStates, regardless: regardless.isEmpty ? nil : (c.regardlessMet ? .met : .unmet), lineWidth: 2.4)
                    // A short page (the closed Duo, an open spread) sets it a step smaller, so the clauses start
                    // above the foot.
                    .frame(width: layout.isShort ? min(max(170, heroWidth * 0.42), 220) : min(max(220, heroWidth * 0.55), 300))
                    .animation(.smooth(duration: 0.6), value: c.tickStates)
                Text(c.stage.title).displayFont(34).italic().foregroundStyle(Palette.stage(c.stage))
                    .contentTransition(.interpolate)
                    .heroReveal()
                Text("\(c.tallyLabel) · \(c.holdingLabel)").font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
                    .multilineTextAlignment(.center)
                if !model.isRecto {
                    // Day one has a clause or two and no Marginalia: say what to do next, right under the gauge.
                    if clauses.isEmpty {
                        QuietButton("Write your first clause ›", color: Palette.oxblood) { model.sheet = .newClause }
                            .padding(.top, 6)
                    } else if board.state.pulses.isEmpty {
                        QuietButton("Write your first Marginalia ›", color: Palette.oxblood) { model.sheet = .marginalia }
                            .padding(.top, 6)
                        Aside("Five lines a week. The Pulse and the Reading are drawn from them.")
                            .multilineTextAlignment(.center)
                    }
                }
            }
            .frame(maxWidth: .infinity)
            .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { heroWidth = $0 }
            .padding(.top, layout.isShort ? 0 : 12)

            DinkusRule()

            // Clauses
            SectionTitle("You\u{2019}ll know it when —", glyph: "")
            if main.isEmpty && regardless.isEmpty {
                Aside("Write the conditions you'd want true before you turn the page. Verso keeps score of what you said, not what it thinks.")
            }
            VStack(alignment: .leading, spacing: 0) {
                ForEach(Array(main.enumerated()), id: \.element.id) { i, clause in
                    ClauseRow(number: i + 1, clause: clause)
                    Hairline()
                }
            }
            if !regardless.isEmpty {
                Text("Or, regardless —").font(VersoFont.serif(.title3)).italic().foregroundStyle(Palette.rust).padding(.top, 20).padding(.bottom, 4)
                VStack(alignment: .leading, spacing: 0) {
                    ForEach(Array(regardless.enumerated()), id: \.element.id) { i, clause in
                        ClauseRow(number: i + 1, clause: clause)
                        Hairline()
                    }
                }
            }
            if !model.isRecto {
                QuietButton("+ Add a clause", color: Palette.oxblood) { model.sheet = .newClause }.padding(.top, 8)
            }
            revisions(clauses)

            DinkusRule()

            // Stay case — always visible, directly opposite the clauses.
            SectionTitle("The stay case", glyph: "↺")
            Text("What would make Book One worth another year?").font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
            // The words as typed live in the model (`PageState.drafts`): a fold rebuilds this page mid-sentence.
            PaperField(label: nil, text: stayCase, prompt: "A team I'd follow anywhere…", axis: .vertical, id: "stay-case") {
                let s = stayCase.wrappedValue.trimmingCharacters(in: .whitespacesAndNewlines)
                guard s != (model.state.book.stayCase ?? "") else { model.pages.drafts["stay-case"] = nil; return }
                model.pages.commit("stay-case", in: model) { try await $0.setStayCase(s.isEmpty ? nil : s) }
            }
            .disabled(model.isRecto)
        } recto: {
            Seam()

            // The Reading
            SectionTitle("The Reading", glyph: Glyph.pilcrow)
            Text(board.reading(veiled: model.veiled))
                .font(VersoFont.body(17)).foregroundStyle(Palette.ink)
                .lineSpacing(4)
                .fixedSize(horizontal: false, vertical: true)
                .contentTransition(.opacity)
            Text(Reading.footer).font(VersoFont.his(13)).foregroundStyle(Palette.fadedInk).padding(.top, 10)

            DinkusRule()
            PulseSection()

            if board.state.pulses.contains(where: { $0.board != nil }) {
                DinkusRule()
                DriftStrip()
            }

            DinkusRule()
            OpeningLines()

            DinkusRule()
            SealedLetterView()

            if !model.isRecto {
                DinkusRule()
                Button { model.sheet = .closeBook } label: {
                    Text("Close Book One").font(VersoFont.serif(.body)).italic().foregroundStyle(Palette.fadedInk)
                        .frame(maxWidth: .infinity).padding(.vertical, 12)
                }
                .buttonStyle(.plain)
            }
        }
        .navigationBarHidden(true)
    }

    var stayCase: Binding<String> { model.pages.draft("stay-case") { model.state.book.stayCase ?? "" } }

    @ViewBuilder func revisions(_ clauses: [Clause]) -> some View {
        let revs = clauses.flatMap { c in c.revisions.map { (c, $0) } }.sorted { $0.1.at > $1.1.at }
        if !revs.isEmpty {
            DisclosureGroup(isExpanded: model.pages.expanded("revisions")) {
                VStack(alignment: .leading, spacing: 10) {
                    ForEach(Array(revs.enumerated()), id: \.offset) { _, pair in
                        let (c, r) = pair
                        VStack(alignment: .leading, spacing: 2) {
                            Text(revisionLine(c, r)).font(VersoFont.body(14)).foregroundStyle(Palette.ink)
                            if let why = r.reason, !why.isEmpty {
                                Text("\u{201C}\(why)\u{201D}").font(VersoFont.his(14)).foregroundStyle(Palette.fadedInk)
                            }
                        }
                    }
                }
                .padding(.top, 8)
            } label: {
                Text("Revisions · \(revs.count)").runningHeadStyle(size: 11)
            }
            .tint(Palette.fadedInk)
            .padding(.top, 14)
        }
    }

    func revisionLine(_ c: Clause, _ r: ClauseRevision) -> String {
        let date = r.at.formatted(.dateTime.day().month(.abbreviated))
        if r.beforeText != c.text { return "Changed \u{2018}\(r.beforeText)\u{2019} → \u{2018}\(c.text)\u{2019} · \(date)" }
        if r.beforeRole != c.role { return "\u{2018}\(c.text)\u{2019}: \(r.beforeRole.rawValue) → \(c.role.rawValue) · \(date)" }
        let before = ClauseText.describe(r.beforeKind, threads: model.state.threads)
        return "\u{2018}\(c.text)\u{2019}: \(before) → \(ClauseText.describe(c.kind, threads: model.state.threads)) · \(date)"
    }
}

#Preview {
    NavigationStack { TurningPointView() }
        .environment(AppModel.preview)
        .environment(\.versoVeiled, false)
}
