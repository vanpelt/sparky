import SwiftUI
import VersoCore

/// First run: three skippable cards over Contents — the epigraph, the grant, the threads.
struct OnboardingView: View {
    @Environment(AppModel.self) private var model
    #if DEBUG
    @State private var step = DebugLaunch.onboardingStep
    #else
    @State private var step = 0
    #endif
    @State private var epigraph = ""
    static let defaultPicks: Set<String> = ["Move", "Read 12 books", "Make a short film", "Learn Serbian"]
    @State private var picked: Set<String> = OnboardingView.defaultPicks
    @State private var clause = ""
    @State private var revest: Double = 0
    @State private var rsu: Double = 0
    @State private var salary: Double = 0
    @State private var bonus: Double = 0
    /// The whole window, keyboard or not (measured on the paper, which ignores every safe area).
    @State private var window = CGSize(width: 402, height: 874)
    /// Title-page cards never run wider than this, so a wide window gets a page, not a banner.
    static let cardWidth: CGFloat = 520

    var body: some View {
        ZStack {
            Palette.paper.ignoresSafeArea()
                .onGeometryChange(for: CGSize.self) { $0.size } action: { window = $0 }
            VStack(spacing: 0) {
                HStack {
                    Text("TITLE PAGE · \(Roman.numeral(step + 1)) OF III").runningHeadStyle()
                    Spacer()
                    QuietButton(step == 2 ? "Begin ›" : "Skip") { advance() }
                }
                .frame(maxWidth: Self.cardWidth + 8)
                .padding(.horizontal, 24).padding(.top, 8)
                .frame(maxWidth: .infinity)

                TabView(selection: $step) {
                    epigraphCard.tag(0)
                    grantCard.tag(1)
                    threadsCard.tag(2)
                }
                .tabViewStyle(.page(indexDisplayMode: .never))
                .animation(.smooth, value: step)

                HStack(spacing: 10) {
                    ForEach(0..<3) { i in
                        Circle().fill(i == step ? Palette.oxblood : Palette.rule).frame(width: 6, height: 6)
                    }
                }
                .padding(.bottom, 20)

                Button(action: advance) {
                    Text(step == 2 ? "Start plotting" : "Turn the page")
                        .font(VersoFont.serif(.headline)).italic()
                        .foregroundStyle(Palette.ink)
                        .padding(.horizontal, 28).padding(.vertical, 14)
                }
                .buttonStyle(.glass)
                .padding(.bottom, 24)
            }
            // Don't squeeze the title page when the keyboard rises: on a portrait window the fields sit above
            // it anyway, and shrinking slid VERSO up under the running head. A landscape window (the open Duo,
            // an iPad on its side) is too short for that: there the keyboard lifts the page and the epigraph
            // card scrolls, so the field stays above the keys.
            .ignoresSafeArea(.keyboard, edges: window.height > window.width ? .bottom : [])
        }
        .onAppear {
            epigraph = model.state.book.settings.epigraph ?? ""
            if let g = model.state.book.grant { revest = g.revestShares; rsu = g.rsuShares; salary = g.salaryAnnual; bonus = g.bonusAnnual }
        }
    }

    // MARK: Cards

    /// A portrait window keeps the plain, fixed title page it always had. A landscape one (where the keyboard
    /// lifts the page, above) scrolls when it has to, at least as tall as the card so the spacers still place VERSO.
    @ViewBuilder var epigraphCard: some View {
        if window.width > window.height {
            GeometryReader { card in
                ScrollView {
                    epigraphPage.frame(minHeight: card.size.height)
                }
                .scrollBounceBehavior(.basedOnSize)
                .scrollDismissesKeyboard(.interactively)
            }
        } else {
            epigraphPage
        }
    }

    /// The title page proper; its spacers set VERSO a third of the way down whatever height the card has.
    var epigraphPage: some View {
        VStack(alignment: .leading, spacing: 24) {
            Spacer()
            Text("VERSO").font(VersoFont.hero(54)).tracking(8).foregroundStyle(Palette.ink).frame(maxWidth: .infinity)
            Text("the left-hand page — the side you only see once you turn the leaf.")
                .font(VersoFont.his(16)).foregroundStyle(Palette.fadedInk).multilineTextAlignment(.center)
                .frame(maxWidth: .infinity)
            DinkusRule()
            Text("Write the first line of Book Two.").font(VersoFont.serif(.title3)).foregroundStyle(Palette.ink)
            PaperField(label: nil, text: $epigraph, prompt: "Begin where the margin runs out.", axis: .vertical,
                       id: "opening-line")
            Aside("Saved as your first Opening Line. Redraft it any time on the Turning Point.")
            Spacer(); Spacer()
        }
        .cardColumn()
    }

    var grantCard: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                Text("The grant").displayFont(32).italic().padding(.top, 40)
                if model.bootstrap?.seedImported == true || model.state.book.grant?.seededFrom != nil {
                    Text("Grant loaded from local seed ✓").font(VersoFont.his(19)).foregroundStyle(Palette.verdigris)
                    seedSummary
                    Aside("Figures stay on this phone. Adjust them any time in the Colophon.")
                } else if model.state.book.grant?.isSample == true {
                    Text("Sample figures loaded.").font(VersoFont.his(19)).foregroundStyle(Palette.fadedInk)
                    Aside("Contents will remind you to set yours in the Colophon.")
                } else {
                    Aside("What Book One still owes you. Everything stays on this phone; the schedule and taxes can be tuned in the Colophon.")
                    VStack(spacing: 2) {
                        NumberRow(label: "Revest shares", value: $revest)
                        NumberRow(label: "RSU shares", value: $rsu)
                        NumberRow(label: "Salary", value: $salary, format: .money, suffix: "/yr")
                        NumberRow(label: "Bonus", value: $bonus, format: .money, suffix: "/yr")
                    }
                    QuietButton("Use sample numbers", color: Palette.oxblood) {
                        model.perform { try await $0.useSampleGrant() }
                    }
                }
            }
            .cardColumn()
        }
        .scrollDismissesKeyboard(.interactively)
    }

    /// What the seed parsed to, without a dollar: enough to spot a bad seed on day one.
    @ViewBuilder var seedSummary: some View {
        if let g = model.state.book.grant, let l = model.scheduleLedger {
            VStack(alignment: .leading, spacing: 6) {
                LeaderRow { Text("Chapters").font(VersoFont.body(16)) } trailing: {
                    Text("\(l.tranches.count) · \(l.chaptersLeft) left").font(VersoFont.ledger(.body))
                }
                if let n = l.next {
                    LeaderRow { Text("Next vest").font(VersoFont.body(16)) } trailing: {
                        Text("Ch. \(n.numeral) · \(DateText.long(n.date))").font(VersoFont.ledger(.body))
                    }
                }
                LeaderRow { Text("Last vest").font(VersoFont.body(16)) } trailing: {
                    Text(DateText.long(l.finalDate)).font(VersoFont.ledger(.body))
                }
                LeaderRow { Text("Salary & bonus").font(VersoFont.body(16)) } trailing: {
                    Text(g.salaryAnnual + g.bonusAnnual > 0 ? "set" : "not set").font(VersoFont.ledger(.body))
                }
                if g.rolloverShares > 0 {
                    LeaderRow { Text("Rollover grant").font(VersoFont.body(16)) } trailing: {
                        Text(g.rolloverVestDate.map(DateText.long) ?? "date unknown").font(VersoFont.ledger(.body))
                            .foregroundStyle(g.rolloverVestDate == nil ? Palette.rust : Palette.ink)
                    }
                    if g.rolloverVestDate == nil {
                        Aside("The seed has no rollover vest date; it's treated as \u{201C}some time in 2026\u{201D}. Set it in the Colophon.")
                    }
                }
            }
            .foregroundStyle(Palette.ink)
            .padding(.top, 4)
        }
    }

    var threadsCard: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                Text("Threads").displayFont(32).italic().padding(.top, 40)
                Aside("Pick up to four to start Book Two with. You can add, rename and archive them later.")
                let presets = PlotThread.presets()
                VStack(spacing: 0) {
                    ForEach(presets) { t in
                        let on = picked.contains(t.name)
                        Button {
                            if on { picked.remove(t.name) } else if picked.count < 4 { picked.insert(t.name) }
                        } label: {
                            HStack(spacing: 14) {
                                Rectangle().fill(Palette.binding(t.binding)).frame(width: 6, height: 38)
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(t.name).font(VersoFont.serif(.body)).foregroundStyle(Palette.ink)
                                    Text(t.kind.tag).runningHeadStyle(size: 10)
                                }
                                Spacer()
                                Text(on ? "●" : "○").font(VersoFont.serif(.title3)).foregroundStyle(on ? Palette.verdigris : Palette.rule)
                            }
                            .padding(.vertical, 8)
                            .contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                        .sensoryFeedback(.selection, trigger: on)
                        Hairline()
                    }
                }
                DinkusRule()
                Text("I\u{2019}ll know it\u{2019}s time when\u{2026}").font(VersoFont.serif(.title3)).foregroundStyle(Palette.ink)
                PaperField(label: nil, text: $clause, prompt: "the script is drafted and the restlessness hasn't faded", axis: .vertical)
                Aside("This becomes your first clause. You can retype it on the Turning Point.")
            }
            .cardColumn()
        }
        .scrollDismissesKeyboard(.interactively)
    }

    // MARK: Flow

    func advance() {
        if step == 1 { saveGrantIfEntered() }
        if step < 2 { withAnimation(.smooth) { step += 1 }; return }
        finish()
    }

    func saveGrantIfEntered() {
        guard model.state.book.grant == nil, revest + rsu + salary + bonus > 0 else { return }
        let g = Grant(revestShares: revest, rsuShares: rsu, salaryAnnual: salary, bonusAnnual: bonus)
        model.perform { try await $0.setGrant(g) }
    }

    func finish() {
        let line = epigraph.trimmingCharacters(in: .whitespacesAndNewlines)
        let clauseText = clause.trimmingCharacters(in: .whitespacesAndNewlines)
        let threads = model.state.threads.isEmpty ? PlotThread.presets().filter { picked.contains($0.name) } : []
        Task {
            await model.run { store in
                for t in threads { try await store.upsertThread(t) }
                if !clauseText.isEmpty {
                    try await store.upsertClause(Clause(text: clauseText, kind: .manual(checked: false), role: .ordinary))
                }
                if !line.isEmpty { try await store.addOpeningLine(line) }
                try await store.updateBook(.all) { b in b.state.onboardedAt = Date() }
            }
            if model.state.book.settings.notificationsOn {
                _ = await Notifications.requestAuthorization()
                Notifications.reschedule(for: model.state, calendar: model.calendar)
            }
        }
    }
}

private extension View {
    /// A title-page card's text: 28pt margins on a phone, at most `cardWidth` wide and centred beyond that.
    func cardColumn() -> some View {
        frame(maxWidth: OnboardingView.cardWidth, alignment: .leading)
            .padding(.horizontal, 28)
            .frame(maxWidth: .infinity)
    }
}

#Preview {
    OnboardingView().environment(AppModel.preview)
}
