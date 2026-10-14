import SwiftUI
import VersoCore

/// Quick Log at a 260pt detent: a stepper prefilled with the default and a one-line note. Return saves.
/// With no thread given, it first shows the threads as spines to pick from.
struct QuickLogSheet: View {
    @State var threadID: UUID?
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var amount: Double = 1
    @State private var note = ""
    @State private var title = ""
    @State private var milestoneID: UUID?
    @FocusState private var noteFocused: Bool
    @Environment(\.dynamicTypeSize) private var typeSize

    var body: some View {
        Group {
            if let id = threadID, let line = model.board.threads.first(where: { $0.id == id }) {
                // Fixed heights fit the default sizes; accessibility sizes get room to grow.
                logger(line)
                    .presentationDetents(typeSize.isAccessibilitySize ? [.medium, .large] : [.height(isCount(line.thread) ? 320 : 260)])
            } else {
                picker.presentationDetents(typeSize.isAccessibilitySize ? [.medium, .large] : [.height(300), .medium]) // the shelf is one row; .medium left half the sheet blank
            }
        }
        .presentationBackground(Palette.paper)
        .presentationDragIndicator(.visible)
    }

    func isCount(_ t: PlotThread) -> Bool { if case .count = t.kind { return true }; return false }

    // MARK: Picker

    var picker: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("LOG A LINE").runningHeadStyle().padding(.top, 24)
            if model.board.threads.isEmpty {
                Aside("No threads yet.")
                QuietButton("+ Start a thread", color: Palette.verdigris) { model.sheet = .newThread }
            }
            ScrollView(.horizontal) {
                HStack(alignment: .bottom, spacing: 14) {
                    ForEach(model.board.threads) { line in
                        Button { select(line) } label: {
                            VStack(spacing: 8) {
                                ThreadSpine(name: line.thread.name, binding: line.thread.binding)
                                    .frame(height: 140)
                                    .clipShape(RoundedRectangle(cornerRadius: 2))
                                Text(line.progress.logLabel).font(VersoFont.serif(.caption)).foregroundStyle(Palette.fadedInk)
                            }
                        }
                        .buttonStyle(.plain)
                        .accessibilityLabel("Log \(line.thread.name)")
                    }
                }
                .padding(.horizontal, 2)
            }
            .scrollIndicators(.hidden)
            Text("Long-press the pen for Marginalia.").font(VersoFont.his(13)).foregroundStyle(Palette.fadedInk)
            Spacer(minLength: 0)
        }
        .sheetColumn()
    }

    func select(_ line: Board.ThreadLine) {
        withAnimation(.smooth) { threadID = line.id }
        amount = line.thread.defaultAmount
    }

    // MARK: Logger

    func logger(_ line: Board.ThreadLine) -> some View {
        let t = line.thread
        return VStack(alignment: .leading, spacing: 14) {
            HStack(alignment: .firstTextBaseline) {
                Rectangle().fill(Palette.binding(t.binding)).frame(width: 4, height: 18)
                Text(t.name.uppercased()).runningHeadStyle(Palette.ink, size: 12)
                Spacer()
                Text(line.progress.periodLabel).font(VersoFont.his(14)).foregroundStyle(Palette.fadedInk)
            }
            .padding(.top, 22)

            switch t.kind {
            case .project(let ms):
                let undone = ms.filter { $0.doneAt == nil }
                if undone.isEmpty {
                    Text("All scenes are done.").font(VersoFont.his(18))
                } else {
                    Picker("Scene", selection: Binding { milestoneID ?? undone.first?.id } set: { milestoneID = $0 }) {
                        ForEach(undone) { m in Text(m.title).tag(Optional(m.id)) }
                    }
                    .pickerStyle(.menu)
                    .tint(Palette.ink)
                    .font(VersoFont.serif(.title3))
                }
            case .habit:
                Text(doneToday(t) ? "Already done today." : "Done today.")
                    .accessibilityHint(doneToday(t) ? "A note you write joins today\u{2019}s entry." : "")
                    .displayFont(28).italic()
            default:
                HStack(alignment: .lastTextBaseline, spacing: 10) {
                    Text(amountLabel(amount))
                        .font(VersoFont.hero(52)).monospacedDigit()
                        .contentTransition(.numericText(value: amount))
                        .heroCap()
                    Text(unitLabel(t)).font(VersoFont.his(18)).foregroundStyle(Palette.fadedInk)
                    Spacer()
                    Stepper("Amount", value: $amount, in: step(t)...10_000, step: step(t))
                        .labelsHidden()
                        .sensoryFeedback(.selection, trigger: amount)
                }
            }

            if isCount(t) {
                TextField("", text: $title, prompt: Text("title (the book's name)").foregroundStyle(Palette.fadedInk.opacity(0.7)))
                    .font(VersoFont.serif(.body)).italic()
                    .submitLabel(.next)
                    .onSubmit { noteFocused = true }
                Hairline()
            }
            TextField("", text: $note, prompt: Text(notePrompt(t)).foregroundStyle(Palette.fadedInk.opacity(0.7)))
                .font(VersoFont.his(18))
                .focused($noteFocused)
                .submitLabel(.done)
                .onSubmit { save(t) }
            Hairline(noteFocused ? Palette.bindingStroke(t.binding) : Palette.rule)

            HStack {
                Spacer()
                let already = isHabit(t) && doneToday(t)
                let hasNote = !note.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
                if already && !hasNote {
                    Text("Already set down today").font(VersoFont.his(15)).foregroundStyle(Palette.fadedInk)
                        .padding(.vertical, 10)
                } else {
                    Button { save(t) } label: {
                        Label { Text(already ? "Add to today\u{2019}s line" : "Set it down") } icon: { PenMark() }
                            .font(VersoFont.serif(.headline)).foregroundStyle(Palette.ink)
                            .padding(.horizontal, 22).padding(.vertical, 10)
                    }
                    .buttonStyle(.glass)
                }
            }
        }
        .sheetColumn()
        .onAppear { amount = t.defaultAmount }
    }

    func isHabit(_ t: PlotThread) -> Bool { if case .habit = t.kind { return true }; return false }

    func doneToday(_ t: PlotThread) -> Bool {
        model.state.entries(for: t.id).contains { model.calendar.isDateInToday($0.date) }
    }

    func step(_ t: PlotThread) -> Double {
        if case .practice(let unit, _) = t.kind, unit == .minutes { return 5 }
        return 1
    }

    func amountLabel(_ a: Double) -> String {
        a.rounded() == a ? String(Int(a)) : a.formatted(.number.precision(.fractionLength(1)))
    }

    func unitLabel(_ t: PlotThread) -> String {
        switch t.kind {
        case .practice(let unit, _): unit == .minutes ? "minutes" : "sessions"
        case .count: "more"
        default: ""
        }
    }

    func notePrompt(_ t: PlotThread) -> String {
        if let sub = t.subtitle, Serbian.isCyrillic(sub) { return "reč nedelje…" }
        switch t.kind {
        case .count: return "a line about it"
        case .habit: return "how it felt"
        case .project: return "what happened in this scene"
        case .practice: return "what you practised"
        }
    }

    func save(_ t: PlotThread) {
        let n = note.trimmingCharacters(in: .whitespacesAndNewlines)
        let ti = title.trimmingCharacters(in: .whitespacesAndNewlines)
        var mid: UUID? = nil
        if case .project(let ms) = t.kind { mid = milestoneID ?? ms.first { $0.doneAt == nil }?.id }
        model.log(t.id, amount: amount, note: n.isEmpty ? nil : n, title: ti.isEmpty ? nil : ti, milestoneID: mid)
        dismiss()
    }
}

extension View {
    /// A sheet's body: 24pt in from the edges as on a phone, and never wider than the text measure, centred,
    /// when the sheet is wide (a form sheet on the open Duo, an iPad).
    func sheetColumn(_ margin: CGFloat = 24) -> some View {
        frame(maxWidth: Measure.text, alignment: .leading)
            .padding(.horizontal, margin)
            .frame(maxWidth: .infinity)
    }
}
