import SwiftUI
import VersoCore

/// The weekly sheet: four scales of five words, optional anchor chips, one line, manual clauses.
/// Should take under thirty seconds.
struct MarginaliaSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var energy: Int?
    @State private var anchor: Int?
    @State private var pull: Int?
    @State private var restlessness: Int?
    @State private var reasons: Set<String> = []
    @State private var customChips: [String] = []
    @State private var newChip = ""
    @State private var sentence = ""
    @State private var manual: [UUID: Bool] = [:]
    @State private var stamp = 0
    @State private var stamp2 = 0
    @FocusState private var sentenceFocused: Bool

    struct Scale: Identifiable {
        let id: Int
        let prompt: String
        let words: [String]
    }

    var scales: [Scale] {
        let recto = model.isRecto
        return [
            Scale(id: 0, prompt: recto ? "How much did Book Two give you?" : "How much did Book One give you this week?", words: Pulse.energyWords),
            Scale(id: 1, prompt: recto ? "How settled do you feel?" : "How strongly does it hold you?", words: Pulse.anchorWords),
            Scale(id: 2, prompt: recto ? "How much momentum?" : "How hard is Book Two pulling?", words: Pulse.pullWords),
            Scale(id: 3, prompt: recto ? "How much doubt?" : "How restless are you?", words: Pulse.restlessnessWords),
        ]
    }

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                VStack(alignment: .leading, spacing: 0) {
                    HStack {
                        Text("MARGINALIA · WEEK OF \(weekOf.formatted(.dateTime.day().month(.abbreviated)).uppercased())").runningHeadStyle()
                        Spacer()
                        QuietButton("Close") { dismiss() }
                    }
                    .padding(.top, 18)
                    Text("Five lines for the week.").displayFont(30).italic().padding(.top, 10).padding(.bottom, 18)

                    ForEach(scales) { s in
                        ScaleRow(prompt: s.prompt, words: s.words, value: binding(s.id)) {
                            withAnimation(.smooth) { proxy.scrollTo(s.id + 1 < scales.count ? "scale-\(s.id + 1)" : "anchors", anchor: .center) }
                        }
                        .id("scale-\(s.id)")
                        Hairline().padding(.vertical, 6)
                    }

                    if !model.isRecto {
                        VStack(alignment: .leading, spacing: 10) {
                            Text("What holds you? (optional)").font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
                            FlowLayout(spacing: 8) {
                                ForEach(Pulse.anchorChips + customChips, id: \.self) { chip in
                                    Chip(title: chip, on: reasons.contains(chip)) {
                                        if reasons.contains(chip) { reasons.remove(chip) } else { reasons.insert(chip) }
                                    }
                                }
                                TextField("", text: $newChip, prompt: Text("+ your own").foregroundStyle(Palette.fadedInk.opacity(0.7)))
                                    .font(VersoFont.body(15)).frame(width: 110)
                                    .onSubmit {
                                        let c = newChip.trimmingCharacters(in: .whitespaces).lowercased()
                                        guard !c.isEmpty else { return }
                                        if !customChips.contains(c) && !Pulse.anchorChips.contains(c) { customChips.append(c) }
                                        reasons.insert(c); newChip = ""
                                    }
                            }
                            .sensoryFeedback(.selection, trigger: reasons)
                        }
                        .id("anchors")
                        .padding(.vertical, 14)
                    }

                    VStack(alignment: .leading, spacing: 8) {
                        Text("This week in one line:").font(VersoFont.serif(.title3))
                        TextField("", text: $sentence, prompt: Text("Steady, but the pull is real.").foregroundStyle(Palette.fadedInk.opacity(0.6)), axis: .vertical)
                            .font(VersoFont.his(20)).lineLimit(1...4)
                            .focused($sentenceFocused)
                            .submitLabel(.done)
                            .returnEndsEditing(text: $sentence, focused: $sentenceFocused)
                        Hairline(sentenceFocused ? Palette.oxblood : Palette.rule)
                    }
                    .padding(.vertical, 16)

                    let manuals = model.state.book.clauses.filter(\.kind.isManual)
                    if !manuals.isEmpty {
                        VStack(alignment: .leading, spacing: 4) {
                            Text("Is this true yet?").font(VersoFont.serif(.title3)).padding(.bottom, 4)
                            ForEach(manuals) { c in
                                Toggle(isOn: Binding { manual[c.id] ?? false } set: { manual[c.id] = $0 }) {
                                    Text(c.text).font(VersoFont.his(17))
                                }
                                .tint(Palette.gilt)
                            }
                        }
                        .padding(.vertical, 12)
                    }

                    Button(action: save) {
                        Text("Stamp the week").font(VersoFont.serif(.headline)).italic()
                            .foregroundStyle(complete ? Palette.ink : Palette.fadedInk)
                            .padding(.horizontal, 28).padding(.vertical, 14)
                    }
                    .buttonStyle(.glass)
                    .disabled(!complete)
                    .frame(maxWidth: .infinity)
                    .padding(.top, 20).padding(.bottom, 40)
                }
                .sheetColumn()
            }
            .scrollDismissesKeyboard(.interactively)
        }
        .background(Palette.paper)
        .sensoryFeedback(Haptics.stampFirst, trigger: stamp)
        .sensoryFeedback(Haptics.stampSecond, trigger: stamp2)
        .onAppear(perform: load)
    }

    var weekOf: Date { Days.weekStart(model.now(), calendar: model.calendar) }
    var complete: Bool { energy != nil && anchor != nil && pull != nil && restlessness != nil }

    func binding(_ i: Int) -> Binding<Int?> {
        switch i {
        case 0: $energy
        case 1: $anchor
        case 2: $pull
        default: $restlessness
        }
    }

    func load() {
        for c in model.state.book.clauses { if case .manual(let on) = c.kind { manual[c.id] = on } }
        guard let p = model.state.pulses.first(where: { model.calendar.isDate($0.weekOf, inSameDayAs: weekOf) }) else { return }
        energy = p.energy; anchor = p.anchor; pull = p.pull; restlessness = p.restlessness
        reasons = Set(p.anchorReasons); sentence = p.sentence ?? ""
        customChips = p.anchorReasons.filter { !Pulse.anchorChips.contains($0) }
    }

    func save() {
        guard let e = energy, let a = anchor, let p = pull, let r = restlessness else { return }
        let rs = Array(reasons).sorted(), s = sentence, m = manual
        stamp += 1
        Task {
            try? await Task.sleep(for: .milliseconds(60))
            stamp2 += 1
        }
        model.perform { store in
            _ = try await store.savePulse(energy: e, anchor: a, pull: p, restlessness: r, anchorReasons: rs,
                                          sentence: s.isEmpty ? nil : s, manualChecks: m)
        }
        Task {
            try? await Task.sleep(for: .milliseconds(220))
            dismiss()
        }
    }
}

/// One scale: a prompt and five tappable words. Tapping sets the value and moves on.
struct ScaleRow: View {
    let prompt: String
    let words: [String]
    @Binding var value: Int?
    let next: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(prompt).font(VersoFont.serif(.title3)).foregroundStyle(Palette.ink)
            HStack(spacing: 0) {
                ForEach(Array(words.enumerated()), id: \.offset) { i, w in
                    let on = value == i + 1
                    Button {
                        withAnimation(.smooth(duration: 0.25)) { value = i + 1 }
                        next()
                    } label: {
                        VStack(spacing: 4) {
                            Text(w)
                                .font(on ? VersoFont.his(17) : VersoFont.body(15))
                                .foregroundStyle(on ? Palette.oxblood : (value == nil ? Palette.ink : Palette.fadedInk))
                                .lineLimit(1).minimumScaleFactor(0.7)
                            Circle().fill(on ? Palette.oxblood : Palette.rule).frame(width: on ? 6 : 3, height: on ? 6 : 3)
                        }
                        .frame(maxWidth: .infinity, minHeight: 48)
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(w)
                    .accessibilityValue("\(i + 1) of 5")
                    .accessibilityAddTraits(on ? .isSelected : [])
                }
            }
            .sensoryFeedback(Haptics.scale, trigger: value)
        }
        .padding(.vertical, 8)
    }
}

#Preview {
    MarginaliaSheet().environment(AppModel.preview)
}
