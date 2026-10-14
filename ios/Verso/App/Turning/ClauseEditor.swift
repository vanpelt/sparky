import SwiftUI
import VersoCore

/// Write or revise one "I go when…" clause. Every edit is kept as a tracked change, and loosening a
/// threshold asks what changed.
struct ClauseEditor: View {
    let clauseID: UUID?
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    enum Kind: String, CaseIterable, Identifiable {
        case manual, date, chapter, remaining, enough, priceBelow, priceAbove, thread, scene, pulls, divergence
        var id: String { rawValue }
        var title: String {
            switch self {
            case .manual: "Your say-so"
            case .date: "A date"
            case .chapter: "A chapter vests"
            case .remaining: "Little left to vest"
            case .enough: "Below the Enough Line"
            case .priceBelow: "Price falls below"
            case .priceAbove: "Price rises above"
            case .thread: "A thread hits its target"
            case .scene: "A scene is done"
            case .pulls: "Book Two keeps pulling"
            case .divergence: "Pull outruns energy"
            }
        }
    }

    @State private var text = ""
    @State private var role: ClauseRole = .ordinary
    @State private var kind: Kind = .manual
    @State private var date = Date().addingTimeInterval(86_400 * 90)
    @State private var chapter = 0
    @State private var amount: Double = 1_000_000
    @State private var price: Double = 50
    @State private var threadID: UUID?
    @State private var milestoneID: UUID?
    @State private var weeks = 6
    @State private var ofLast = 8
    @State private var original: Clause?
    @State private var askWhy = false
    @State private var why = ""

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    Text(clauseID == nil ? "A new clause" : "Revise the clause").displayFont(30).italic().padding(.top, 8)
                    VStack(alignment: .leading, spacing: 6) {
                        Text("I\u{2019}ll know it\u{2019}s time when\u{2026}").font(VersoFont.serif(.title3))
                        PaperField(label: nil, text: $text, prompt: "the script is drafted", axis: .vertical)
                    }

                    VStack(alignment: .leading, spacing: 8) {
                        Text("HOW IT COUNTS").runningHeadStyle(size: 10)
                        HStack(spacing: 8) {
                            Chip(title: "§ required", on: role == .required) { role = .required }
                            Chip(title: "ordinary", on: role == .ordinary) { role = .ordinary }
                            Chip(title: "‼︎ regardless", on: role == .regardless, tint: Palette.rust) { role = .regardless }
                        }
                        Aside(roleHelp)
                    }

                    VStack(alignment: .leading, spacing: 8) {
                        Text("WHAT MAKES IT TRUE").runningHeadStyle(size: 10)
                        PickerRow("True when") {
                            Picker("What makes it true", selection: $kind.animation(.smooth)) {
                                ForEach(Kind.allCases) { k in Text(k.title).tag(k) }
                            }
                            .pickerStyle(.menu).tint(Palette.oxblood)
                        }
                        kindFields
                    }

                    if clauseID != nil {
                        QuietButton("Strike this clause", color: Palette.rust) {
                            if let id = clauseID { model.perform { try await $0.deleteClause(id) } }
                            dismiss()
                        }
                        .padding(.top, 10)
                    }
                }
                // A reading column even when the sheet opens wide on the inner display.
                .frame(maxWidth: Measure.text, alignment: .leading)
                .padding(.horizontal, 24).padding(.bottom, 40)
                .frame(maxWidth: .infinity)
            }
            .scrollDismissesKeyboard(.interactively)
            .background(Palette.paper)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button { dismiss() } label: { Text("Cancel").font(VersoFont.serif(.body)).italic() }
                        .tint(Palette.fadedInk)
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button { attemptSave() } label: { Text("Set it").font(VersoFont.serif(.body, weight: .semibold)).italic() }
                        .tint(Palette.oxblood)
                        .disabled(text.trimmingCharacters(in: .whitespaces).isEmpty || !isComplete)
                }
            }
            .alert(whyTitle, isPresented: $askWhy) {
                TextField("What changed?", text: $why)
                Button("Save the change") { save(reason: why) }
                Button("Keep it as it was", role: .cancel) {}
            } message: {
                Text("You're making this easier to meet.")
            }
        }
        .onAppear(perform: load)
    }

    var roleHelp: String {
        switch role {
        case .required: "All required clauses must be holding before the Turning Point."
        case .ordinary: "Counts toward the tally."
        case .regardless: "If this one is true, the story turns whatever else is so."
        }
    }

    var whyTitle: String {
        "Past-you set this on \(original?.createdAt.formatted(.dateTime.day().month(.abbreviated)) ?? "an earlier day"). What changed?"
    }

    var isComplete: Bool {
        switch kind {
        case .thread: threadID != nil
        case .scene: threadID != nil && milestoneID != nil
        default: true
        }
    }

    @ViewBuilder var kindFields: some View {
        let threads = model.state.activeThreads
        switch kind {
        case .manual:
            Aside("You tick it yourself — on this page or in Marginalia.")
        case .date:
            PaperDateRow(label: "On or after", selection: $date)
        case .chapter:
            PickerRow("When") {
                Picker("Chapter", selection: $chapter) {
                    ForEach(0..<(model.state.book.grant?.schedule.count ?? 13), id: \.self) { i in
                        Text("Chapter \(Roman.chapter(i))").tag(i)
                    }
                }
                .pickerStyle(.menu).tint(Palette.oxblood)
            }
        case .remaining:
            NumberRow(label: "Left to vest below", value: $amount, format: .money)
        case .enough:
            Aside(model.state.book.settings.enoughLinePerDay == nil ? "Set an Enough Line in the Colophon first." : "True once staying pays less a day than your Enough Line.")
        case .priceBelow, .priceAbove:
            NumberRow(label: kind == .priceBelow ? "Below" : "Above", value: $price, format: .money)
        case .thread, .scene:
            PickerRow("Thread") {
                Picker("Thread", selection: $threadID) {
                    Text("Choose…").tag(UUID?.none)
                    ForEach(threads) { t in Text(t.name).tag(Optional(t.id)) }
                }
                .pickerStyle(.menu).tint(Palette.verdigris)
            }
            if kind == .scene, let t = threads.first(where: { $0.id == threadID }) {
                PickerRow("Scene") {
                    Picker("Scene", selection: $milestoneID) {
                        Text("Choose…").tag(UUID?.none)
                        ForEach(t.milestones) { m in Text(m.title).tag(Optional(m.id)) }
                    }
                    .pickerStyle(.menu).tint(Palette.verdigris)
                }
                if t.milestones.isEmpty { Aside("That thread has no scenes. Pick a project thread.") }
            }
        case .pulls, .divergence:
            Stepper("At least \(weeks) weeks", value: $weeks, in: 1...max(1, ofLast)).font(VersoFont.body(16))
            Stepper("of the last \(ofLast)", value: $ofLast, in: max(1, weeks)...26).font(VersoFont.body(16))
            Aside(kind == .pulls ? "Counts weeks where your smoothed pulse leaned toward Book Two." : "Counts weeks where pull beat energy by two or more.")
        }
    }

    var builtKind: ClauseKind {
        switch kind {
        case .manual: if case .manual(let c) = original?.kind { return .manual(checked: c) }; return .manual(checked: false)
        case .date: return .date(after: model.calendar.startOfDay(for: date))
        case .chapter: return .tranche(index: chapter)
        case .remaining: return .remainingBelow(amount)
        case .enough: return .dayRateBelowEnough
        case .priceBelow: return .priceBelow(price)
        case .priceAbove: return .priceAbove(price)
        case .thread: return .threadTarget(threadID: threadID ?? UUID())
        case .scene: return .milestone(threadID: threadID ?? UUID(), milestoneID: milestoneID ?? UUID())
        case .pulls: return .pulse(metric: .pulls, weeks: weeks, ofLast: ofLast)
        case .divergence: return .pulse(metric: .divergence, weeks: weeks, ofLast: ofLast)
        }
    }

    func load() {
        guard original == nil, let id = clauseID, let c = model.state.book.clauses.first(where: { $0.id == id }) else { return }
        original = c
        text = c.text; role = c.role
        switch c.kind {
        case .manual: kind = .manual
        case .date(let d): kind = .date; date = d
        case .tranche(let i): kind = .chapter; chapter = i
        case .remainingBelow(let a): kind = .remaining; amount = a
        case .dayRateBelowEnough: kind = .enough
        case .priceBelow(let x): kind = .priceBelow; price = x
        case .priceAbove(let x): kind = .priceAbove; price = x
        case .threadTarget(let t): kind = .thread; threadID = t
        case let .milestone(t, m): kind = .scene; threadID = t; milestoneID = m
        case let .pulse(metric, w, n): kind = metric == .pulls ? .pulls : .divergence; weeks = w; ofLast = n
        }
    }

    func attemptSave() {
        if let o = original, builtKind.loosens(o.kind) || (o.role == .required && role != .required) {
            askWhy = true
        } else {
            save(reason: nil)
        }
    }

    func save(reason: String?) {
        var c = original ?? Clause(text: "", kind: builtKind, role: role)
        c.text = text.trimmingCharacters(in: .whitespacesAndNewlines)
        c.kind = builtKind
        c.role = role
        let clause = c
        let r = reason?.trimmingCharacters(in: .whitespacesAndNewlines)
        model.perform { try await $0.upsertClause(clause, reason: (r?.isEmpty ?? true) ? nil : r) }
        dismiss()
    }
}
