import SwiftUI
import VersoCore

/// New / edit thread: name, kind and its target, logline, cloth colour, default amount.
struct ThreadEditor: View {
    let threadID: UUID?
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    enum Kind: String, CaseIterable { case count, habit, project, practice }

    @State private var name = ""
    @State private var subtitle = ""
    @State private var logline = ""
    @State private var kind: Kind = .habit
    @State private var cloth: Cloth = .forest
    @State private var defaultAmount: Double = 1
    @State private var target: Double = 12
    @State private var hasDeadline = false
    @State private var deadline = Date()
    @State private var perWeek: Double = 4
    @State private var unit: PracticeUnit = .minutes
    @State private var weekly: Double = 120
    @State private var milestones: [Milestone] = Milestone.filmDefaults()
    @State private var newMilestone = ""
    @State private var original: PlotThread?

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    Text(threadID == nil ? "A new thread" : "The thread").displayFont(30).italic().padding(.top, 8)
                    PaperField(label: "NAME", text: $name, prompt: "Make a short film", italic: false)
                    PaperField(label: "LOGLINE — WHY IT MATTERS FOR BOOK TWO", text: $logline, prompt: "Finish one thing that is wholly mine.", axis: .vertical)
                    PaperField(label: "SUBTITLE (OPTIONAL)", text: $subtitle, prompt: "Српски", italic: false)

                    VStack(alignment: .leading, spacing: 10) {
                        Text("KIND").runningHeadStyle(size: 10)
                        HStack(spacing: 8) {
                            ForEach(Kind.allCases, id: \.self) { k in
                                Chip(title: k.rawValue, on: kind == k, tint: Palette.binding(cloth)) {
                                    withAnimation(.smooth) { kind = k; defaultAmount = k == .practice && unit == .minutes ? 20 : 1 }
                                }
                            }
                        }
                        kindFields.padding(.top, 4)
                    }

                    VStack(alignment: .leading, spacing: 10) {
                        Text("CLOTH").runningHeadStyle(size: 10)
                        HStack(spacing: 14) {
                            ForEach(Cloth.allCases, id: \.self) { b in
                                Button { withAnimation(.smooth) { cloth = b } } label: {
                                    RoundedRectangle(cornerRadius: 2).fill(Palette.binding(b))
                                        .frame(width: 34, height: 48)
                                        .overlay(alignment: .bottom) {
                                            if cloth == b { Rectangle().fill(Palette.ink).frame(height: 2).offset(y: 6) }
                                        }
                                }
                                .buttonStyle(.plain)
                                .accessibilityLabel(b.displayName)
                                .accessibilityAddTraits(cloth == b ? .isSelected : [])
                            }
                        }
                        .sensoryFeedback(.selection, trigger: cloth)
                    }

                    if kind != .project && kind != .habit {
                        NumberRow(label: "Default line", value: $defaultAmount, format: .decimal,
                                  suffix: kind == .practice ? (unit == .minutes ? "min" : "sessions") : nil)
                    }
                    if threadID != nil {
                        QuietButton("Delete this thread and its lines", color: Palette.rust) {
                            if let id = threadID { model.perform { try await $0.deleteThread(id) } }
                            model.threadsPath.removeAll { $0 == threadID }
                            dismiss()
                        }
                        .padding(.top, 12)
                    }
                }
                .sheetColumn()
                .padding(.bottom, 40)
            }
            .scrollDismissesKeyboard(.interactively)
            .background(Palette.paper)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button { dismiss() } label: { Text("Cancel").font(VersoFont.serif(.body)).italic() }
                        .tint(Palette.fadedInk)
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button { save() } label: { Text("Bind it").font(VersoFont.serif(.body, weight: .semibold)).italic() }
                        .tint(Palette.oxblood)
                        .disabled(name.trimmingCharacters(in: .whitespaces).isEmpty)
                }
            }
        }
        .onAppear(perform: load)
    }

    @ViewBuilder var kindFields: some View {
        switch kind {
        case .count:
            NumberRow(label: "Target", value: $target)
            PaperToggle(label: "By a date", isOn: $hasDeadline)
            if hasDeadline { PaperDateRow(label: "Deadline", selection: $deadline) }
        case .habit:
            NumberRow(label: "Days a week", value: $perWeek)
        case .practice:
            HStack(spacing: 8) {
                Chip(title: "minutes", on: unit == .minutes) { unit = .minutes; defaultAmount = 20 }
                Chip(title: "sessions", on: unit == .sessions) { unit = .sessions; defaultAmount = 1 }
            }
            NumberRow(label: "Each week", value: $weekly, suffix: unit.short)
        case .project:
            VStack(alignment: .leading, spacing: 0) {
                ForEach(Array(milestones.enumerated()), id: \.element.id) { i, m in
                    HStack {
                        Text(Roman.numeral(i + 1) + ".").font(VersoFont.serif(.body)).frame(width: 40, alignment: .leading)
                        Text(m.title).font(VersoFont.serif(.body)).italic()
                        Spacer()
                        Button { milestones.remove(at: i) } label: { Image(systemName: "minus.circle").fontWeight(.light) }
                            .buttonStyle(.plain).foregroundStyle(Palette.fadedInk)
                    }
                    .padding(.vertical, 6)
                }
                HStack {
                    TextField("", text: $newMilestone, prompt: Text("add a scene").foregroundStyle(Palette.fadedInk.opacity(0.7)))
                        .font(VersoFont.serif(.body)).italic()
                        .onSubmit(addMilestone)
                    Button(action: addMilestone) { Image(systemName: "plus.circle").fontWeight(.light) }
                        .buttonStyle(.plain).foregroundStyle(Palette.verdigris)
                }
                Hairline()
            }
        }
    }

    func addMilestone() {
        let t = newMilestone.trimmingCharacters(in: .whitespaces)
        guard !t.isEmpty else { return }
        milestones.append(Milestone(title: t)); newMilestone = ""
    }

    func load() {
        guard original == nil, let id = threadID, let t = model.state.threads.first(where: { $0.id == id }) else { return }
        original = t
        name = t.name; subtitle = t.subtitle ?? ""; logline = t.logline ?? ""; cloth = t.binding; defaultAmount = t.defaultAmount
        switch t.kind {
        case let .count(tg, dl): kind = .count; target = Double(tg); hasDeadline = dl != nil; deadline = dl ?? Date()
        case let .habit(pw): kind = .habit; perWeek = Double(pw)
        case let .project(ms): kind = .project; milestones = ms
        case let .practice(u, w): kind = .practice; unit = u; weekly = w
        }
    }

    func save() {
        let k: ThreadKind = switch kind {
        case .count: .count(target: max(1, Int(target)), deadline: hasDeadline ? deadline : nil)
        case .habit: .habit(perWeek: min(7, max(1, Int(perWeek))))
        case .project: .project(milestones: milestones)
        case .practice: .practice(unit: unit, weeklyTarget: max(1, weekly))
        }
        var t = original ?? PlotThread(name: "", kind: k, binding: cloth)
        t.name = name.trimmingCharacters(in: .whitespaces)
        t.subtitle = subtitle.isEmpty ? nil : subtitle
        t.logline = logline.isEmpty ? nil : logline
        t.kind = k; t.binding = cloth
        t.defaultAmount = kind == .project || kind == .habit ? 1 : max(0.1, defaultAmount)
        let thread = t
        model.perform { try await $0.upsertThread(thread) }
        dismiss()
    }
}
