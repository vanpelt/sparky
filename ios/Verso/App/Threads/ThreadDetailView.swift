import SwiftUI
import VersoCore

/// A thread's own page: title, logline, a large ring, its scenes or week, and entries as marginalia.
struct ThreadDetailView: View {
    let threadID: UUID
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var width: CGFloat = 402
    @Environment(\.pageLayout) private var layout
    @Environment(\.bookPage) private var bookPage
    /// Pushed (a back button above it) rather than the root of its stack, as on a spread's right page.
    @Environment(\.isPresented) private var isPushed
    /// The right page of an open spread, where nothing sits above it: the menu joins a running head instead of
    /// a bar row of its own.
    private var inlineHead: Bool { bookPage == .recto && !isPushed }

    var body: some View {
        if let line = model.board.threads.first(where: { $0.id == threadID }) ?? archivedLine {
            content(line)
        } else {
            Text("This thread is gone.").font(VersoFont.his(20)).foregroundStyle(Palette.fadedInk)
                .frame(maxWidth: .infinity, maxHeight: .infinity).background(Palette.paper)
        }
    }

    /// Archived threads aren't on the board; read one by un-archiving it in a scratch copy of the state.
    var archivedLine: Board.ThreadLine? {
        var st = model.state
        guard let i = st.threads.firstIndex(where: { $0.id == threadID }) else { return nil }
        st.threads[i].archivedAt = nil
        return Board(state: st, today: model.board.today, calendar: model.calendar).threads.first { $0.id == threadID }
    }

    func content(_ line: Board.ThreadLine) -> some View {
        let t = line.thread
        let entries = model.state.entries(for: t.id).sorted { $0.date > $1.date }
        return List {
            Group {
                if inlineHead {
                    HeadBar(t.name.uppercased()) { EmptyView() } end: { menu(t) }
                        .environment(\.headFolio, model.board.folio + 1)
                        .padding(.top, 6)
                }
                titlePage(line)
                ringBlock(line)
                kindBlock(line)
            }
            .listRowBackground(Palette.paper)
            .listRowSeparator(.hidden)
            .listRowInsets(EdgeInsets(top: 0, leading: 24, bottom: 0, trailing: 24))

            Section {
                if entries.isEmpty {
                    Aside("No lines yet. The first one is the hardest.").listRowBackground(Palette.paper).listRowSeparator(.hidden)
                }
                ForEach(entries) { e in
                    EntryMarginalia(entry: e, thread: t)
                        .listRowBackground(Palette.paper)
                        .listRowSeparatorTint(Palette.rule)
                        .swipeActions {
                            Button(role: .destructive) { model.perform { try await $0.deleteEntry(e.id) } } label: {
                                Label("Delete", systemImage: "trash")
                            }
                            // A global sheet, not this page's: a fold rebuilds the page, and would take the sheet
                            // (and the edit in it) with it.
                            Button { model.sheet = .editEntry(e.id) } label: { Label("Edit", systemImage: "pencil") }.tint(Palette.fadedInk)
                        }
                }
            } header: {
                SectionTitle("In the margin", glyph: Glyph.pilcrow).textCase(nil)
            }

            Section {
                let used = model.state.book.clauses.filter { usesThread($0, t.id) }
                if !used.isEmpty {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("USED BY CLAUSES").runningHeadStyle(size: 10)
                        ForEach(used) { c in
                            Text("\(model.board.clauseState(c).mark)  \(c.text)").font(VersoFont.his(15)).foregroundStyle(Palette.ink)
                        }
                    }
                    .padding(.vertical, 8)
                }
                if bookPage == .single { Folio(model.board.folio) }
                Color.clear.frame(height: 40)
            }
            .listRowBackground(Palette.paper)
            .listRowSeparator(.hidden)
        }
        .listStyle(.plain)
        .scrollContentBackground(.hidden)
        .contentMargins(.horizontal, Measure.listMargin(width), for: .scrollContent)
        .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { width = $0 }
        .debugScrollAnchor()
        .background(Palette.paper.ignoresSafeArea())
        .toolbarBackgroundVisibility(.hidden, for: .navigationBar)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            // Under a bar, the thread's actions go in the system's own overflow menu (iOS 27), so on the iPhone Duo
            // they join whatever else overflows from the side column instead of a second ⋯ of Verso's own.
            if !inlineHead {
                if #available(iOS 27.0, *) {
                    ToolbarOverflowMenu { menuItems(t) }
                } else {
                    ToolbarItem(placement: .topBarTrailing) { menu(t) }
                }
            }
        }
        .toolbar(inlineHead ? .hidden : .automatic, for: .navigationBar)
    }

    func menu(_ t: PlotThread) -> some View {
        Menu { menuItems(t) } label: {
            if inlineHead {
                Image(systemName: "ellipsis").font(.system(size: 14, weight: .light)).foregroundStyle(Palette.fadedInk)
                    .frame(width: 32, height: 32).contentShape(Rectangle())
            } else {
                Image(systemName: "ellipsis").fontWeight(.light)
            }
        }
    }

    /// The thread's actions: in its own ⋯ in a spread's running head, else in the bar's overflow menu.
    @ViewBuilder func menuItems(_ t: PlotThread) -> some View {
        Button { model.sheet = .editThread(t.id) } label: { Label("Edit thread", systemImage: "slider.horizontal.3") }
        Button { model.sheet = .log(t.id) } label: { Label("Log with a note…", systemImage: "pencil.tip") }
        Divider()
        if t.isActive {
            Button { model.perform { try await $0.archiveThread(t.id) }; close(t.id) } label: {
                Label("Archive to Loose ends", systemImage: "archivebox")
            }
        } else {
            Button { model.perform { try await $0.archiveThread(t.id, archived: false) } } label: {
                Label("Pick it back up", systemImage: "arrow.uturn.left")
            }
        }
    }

    /// Leaves an archived thread. A push pops; a spread's right page (where this page is the stack's root, so
    /// `dismiss` does nothing) drops the route, and the page falls back to its default.
    func close(_ id: UUID) {
        if isPushed { dismiss(); return }
        model.threadsPath.removeAll { $0 == id }
        model.contentsPath.removeAll { $0 == .thread(id) }
    }

    func usesThread(_ c: Clause, _ id: UUID) -> Bool {
        switch c.kind {
        case .threadTarget(let t): t == id
        case .milestone(let t, _): t == id
        default: false
        }
    }

    // MARK: Blocks

    @ViewBuilder func titlePage(_ line: Board.ThreadLine) -> some View {
        let t = line.thread
        VStack(spacing: 8) {
            Text(t.kind.tag).runningHeadStyle(Palette.bindingStroke(t.binding), size: 11).padding(.top, 20)
            Text(t.name).displayFont(34).foregroundStyle(Palette.ink).multilineTextAlignment(.center)
            if let sub = t.subtitle, !sub.isEmpty {
                // Which script is showing is kept in the model, by thread: a fold rebuilds this page.
                let latin = model.pages.expanded.contains("latin-\(t.id)")
                Button { withAnimation(.smooth) { model.pages.expanded("latin-\(t.id)").wrappedValue.toggle() } } label: {
                    Text(latin && Serbian.isCyrillic(sub) ? Serbian.latin(sub) : sub)
                        .font(VersoFont.serif(.title3)).italic().foregroundStyle(Palette.fadedInk)
                        .contentTransition(.interpolate)
                }
                .buttonStyle(.plain)
                .accessibilityHint(Serbian.isCyrillic(sub) ? "Switch script" : "")
            }
            if let log = t.logline, !log.isEmpty {
                Text(log).font(VersoFont.his(18)).foregroundStyle(Palette.ink).multilineTextAlignment(.center)
                    .padding(.top, 6)
            }
        }
        .frame(maxWidth: .infinity)
    }

    @ViewBuilder func ringBlock(_ line: Board.ThreadLine) -> some View {
        let t = line.thread
        VStack(spacing: 14) {
            ThreadRing(progress: line.progress.progress, binding: t.binding, lineWidth: 5) {
                VStack(spacing: 2) {
                    Text(line.progress.label.components(separatedBy: " / ").first ?? line.progress.label)
                        .font(VersoFont.hero(54)).monospacedDigit().contentTransition(.numericText())
                    Text(line.progress.label.components(separatedBy: " / ").dropFirst().first.map { "of \($0)" } ?? "")
                        .font(VersoFont.his(15)).foregroundStyle(Palette.fadedInk)
                }
            }
            .frame(width: ring, height: ring)
            .heroCap()
            Text(line.progress.periodLabel).font(VersoFont.his(17)).foregroundStyle(Palette.fadedInk)
            HStack(spacing: 14) {
                Text(line.momentum.status.word.uppercased()).runningHeadStyle(statusColor(line.momentum.status), size: 11)
                if line.progress.streakWeeks > 0 {
                    Text("· \(line.progress.streakWeeks)-WEEK STREAK").runningHeadStyle(size: 11)
                }
            }
            if case .project = t.kind {} else {
                Button { model.log(t.id) } label: {
                    Label { Text(line.progress.logLabel) } icon: { PenMark() }
                        .font(VersoFont.serif(.headline)).foregroundStyle(Palette.ink)
                        .padding(.horizontal, 26).padding(.vertical, 12)
                }
                .buttonStyle(.glass)
                LogStroke(threadID: t.id, binding: t.binding).frame(width: 120)
            }
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 18)
    }

    /// 190pt on every phone (a Pro Max included); off a phone it grows with the column (half of it) up to 260.
    var ring: CGFloat {
        guard !layout.isPhone else { return 190 }
        let column = min(Measure.text, width - 2 * (24 + Measure.listMargin(width)))
        return min(max(190, column / 2), 260)
    }

    func statusColor(_ s: MomentumStatus) -> Color {
        switch s { case .moving: Palette.verdigris; case .slow: Palette.fadedInk; case .stalled: Palette.rust }
    }

    @ViewBuilder func kindBlock(_ line: Board.ThreadLine) -> some View {
        let t = line.thread
        switch t.kind {
        case .habit, .practice:
            WeekDots(dots: line.progress.weekDots, binding: t.binding, calendar: model.calendar).padding(.bottom, 12)
        case .project(let milestones):
            VStack(alignment: .leading, spacing: 0) {
                SectionTitle("Scenes")
                ForEach(Array(milestones.enumerated()), id: \.element.id) { i, m in
                    Button {
                        model.perform { try await $0.setMilestone(threadID: t.id, milestoneID: m.id, done: m.doneAt == nil) }
                        if m.doneAt == nil { model.successCount += 1 }
                    } label: {
                        HStack(alignment: .firstTextBaseline, spacing: 0) {
                            Text(Roman.numeral(i + 1) + ".").font(VersoFont.serif(.body)).frame(width: 40, alignment: .leading)
                            LeaderRow {
                                Text(m.title).font(VersoFont.serif(.body)).italic(m.id == line.progress.nextMilestone?.id)
                            } trailing: {
                                Text(m.doneAt.map(DateText.dayMonth) ?? (m.id == line.progress.nextMilestone?.id ? "next" : ""))
                                    .font(VersoFont.ledger(.caption))
                                    .foregroundStyle(m.id == line.progress.nextMilestone?.id ? Palette.bindingStroke(t.binding) : Palette.fadedInk)
                            }
                            .readStrike(m.doneAt != nil)
                        }
                        .foregroundStyle(m.doneAt != nil ? Palette.fadedInk : Palette.ink)
                        .padding(.vertical, 8)
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityValue(m.doneAt != nil ? "done" : "not done")
                }
            }
            .padding(.bottom, 12)
        case .count:
            let titles = model.state.entries(for: t.id).compactMap(\.title)
            if !titles.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    SectionTitle("The shelf")
                    ForEach(Array(titles.enumerated()), id: \.offset) { i, title in
                        HStack(alignment: .firstTextBaseline, spacing: 10) {
                            Text("\(i + 1).").font(VersoFont.ledger(.subheadline)).foregroundStyle(Palette.fadedInk).frame(width: 28, alignment: .trailing)
                            Text(title).font(VersoFont.serif(.body)).italic()
                        }
                    }
                }
                .padding(.bottom, 12)
            }
        }
    }
}

/// Monday-start week strip: seven dots, filled on days with a line.
struct WeekDots: View {
    let dots: [Bool]
    let binding: Cloth
    let calendar: Calendar
    var body: some View {
        let letters = ["M", "T", "W", "T", "F", "S", "S"]
        HStack(spacing: 16) {
            ForEach(0..<7, id: \.self) { i in
                VStack(spacing: 6) {
                    Circle()
                        .fill(dots.indices.contains(i) && dots[i] ? Palette.bindingStroke(binding) : .clear)
                        .overlay(Circle().strokeBorder(Palette.rule, lineWidth: 0.75))
                        .frame(width: 14, height: 14)
                    Text(letters[i]).runningHeadStyle(size: 9)
                }
            }
        }
        .frame(maxWidth: .infinity)
        .accessibilityElement()
        .accessibilityLabel("This week: \(dots.filter { $0 }.count) of 7 days")
    }
}

/// One entry as marginalia: the date in small caps, the note in italic.
struct EntryMarginalia: View {
    let entry: Entry
    let thread: PlotThread
    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 14) {
            Text(DateText.smallCaps(entry.date)).runningHeadStyle(size: 10).frame(width: 92, alignment: .leading)
            VStack(alignment: .leading, spacing: 2) {
                Text(amountText).font(VersoFont.ledger(.subheadline)).foregroundStyle(Palette.bindingStroke(thread.binding))
                if let title = entry.title { Text(title).font(VersoFont.serif(.body)).italic() }
                if let m = entry.milestoneID.flatMap({ id in thread.milestones.first { $0.id == id } }) {
                    Text(m.title).font(VersoFont.serif(.body)).italic()
                }
                if let note = entry.note, !note.isEmpty { Text(note).font(VersoFont.his(16)).foregroundStyle(Palette.ink) }
            }
        }
        .padding(.vertical, 6)
    }

    var amountText: String {
        let a = entry.amount.rounded() == entry.amount ? String(Int(entry.amount)) : entry.amount.formatted(.number.precision(.fractionLength(1)))
        switch thread.kind {
        case .practice(let unit, _): return "+\(a) \(unit.short)"
        case .habit: return "done"
        case .project: return "scene"
        case .count: return "+\(a)"
        }
    }
}

/// Edit an entry's amount, note and date.
struct EntryEditor: View {
    @State var entry: Entry
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("THE ENTRY").runningHeadStyle().padding(.top, 20)
            NumberRow(label: "Amount", value: $entry.amount, format: .decimal)
            PaperField(label: "NOTE", text: Binding { entry.note ?? "" } set: { entry.note = $0.isEmpty ? nil : $0 }, prompt: "a line in the margin")
            PaperDateRow(label: "When", selection: $entry.date, components: [.date, .hourAndMinute])
            HStack {
                Spacer()
                QuietButton("Save", color: Palette.oxblood) {
                    let e = entry
                    model.perform { try await $0.updateEntry(e) }
                    dismiss()
                }
            }
        }
        .sheetColumn()
    }
}
