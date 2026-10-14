import SwiftUI
import VersoCore

/// Threads: one row per active thread. Swipe right logs the default; ✒︎ opens Quick Log.
struct ThreadsView: View {
    let zoom: Namespace.ID
    @Environment(AppModel.self) private var model
    /// Arranging, in the model (`PageState.arrangingThreads`): a fold rebuilds this list mid-arrangement.
    private var editMode: EditMode {
        get { model.pages.arrangingThreads ? .active : .inactive }
        nonmutating set { model.pages.arrangingThreads = newValue.isEditing }
    }
    @Environment(\.pageLayout) private var layout
    @Environment(\.bookPage) private var bookPage
    /// The list's side safe areas. A List's content margin is measured from inside the safe area, so where the
    /// iPhone Duo keeps its ~84pt bar column the page's margin is added on top of it, as a `Page` has it.
    @State private var sides: [CGFloat] = [0, 0]

    var body: some View {
        let lines = model.board.threads
        let archived = model.state.threads.filter { !$0.isActive }
        let open = model.isSpread ? model.threadsPath.last ?? Self.lastLogged(lines) : nil
        ScrollViewReader { proxy in
            List {
                Section {
                    HeadBar(model.isRecto ? "BOOK TWO · THE THREADS" : "BOOK TWO, IN DRAFT") {
                        Text("· \(rollup)").runningHeadStyle()
                    } end: {
                        // Off a phone the arrange button is set in the head's line, not in a bar row of its own above it.
                        if !layout.isPhone { arrange }
                    }
                    .environment(\.headFolio, bookPage == .single ? nil : model.board.folio)
                    .listRowInsets(layout.isPhone ? EdgeInsets(top: 6, leading: 24, bottom: 6, trailing: 18)
                                                  : EdgeInsets(top: 6, leading: 0, bottom: 6, trailing: 0))
                    .listRowSeparator(.hidden)
                    .listRowBackground(Palette.paper)
                    PartTitle(part: "THE THREADS", title: "What Book Two has going")
                        // Centred on the page's measure, as on every other tab, not on the list's default insets.
                        .listRowInsets(layout.isPhone ? nil : EdgeInsets())
                        .listRowSeparator(.hidden)
                        .listRowBackground(Palette.paper)
                }

                Section {
                    ForEach(lines) { line in
                        row(line)
                        .matchedTransitionSource(id: "thread-\(line.id)", in: zoom)
                        .listRowBackground(line.id == open ? Palette.deckle : Palette.paper)
                        // On a phone the cloth strip is the page's edge; elsewhere the rows keep the page's margins,
                        // strip on the column's leading edge, ✒︎ on its trailing one.
                        .listRowInsets(EdgeInsets(top: 0, leading: 0, bottom: 0, trailing: layout.isPhone ? 16 : 0))
                        .listRowSeparatorTint(Palette.rule)
                        .swipeActions(edge: .leading, allowsFullSwipe: true) {
                            if case .project = line.thread.kind {
                                Button { model.openThread(line.id) } label: { Label("Scenes", systemImage: "list.bullet") }
                                    .tint(Palette.binding(line.thread.binding))
                            } else {
                                Button { model.log(line.id) } label: { Label(line.progress.logLabel, systemImage: "pencil.tip") }
                                    .tint(Palette.binding(line.thread.binding))
                            }
                        }
                        .swipeActions(edge: .trailing) {
                            Button { model.sheet = .log(line.id) } label: { Label("Log…", systemImage: "pencil.tip") }
                                .tint(Palette.fadedInk)
                            Button { model.sheet = .editThread(line.id) } label: { Label("Edit", systemImage: "slider.horizontal.3") }
                                .tint(Palette.rule)
                        }
                    }
                    .onMove { from, to in
                        var ids = lines.map(\.id)
                        ids.move(fromOffsets: from, toOffset: to)
                        let order = ids
                        model.perform { try await $0.reorderThreads(order) }
                    }

                    Button { model.sheet = .newThread } label: {
                        Text("+ New thread").font(VersoFont.serif(.body)).italic().foregroundStyle(Palette.verdigris)
                            .padding(.vertical, 10)
                    }
                    .listRowBackground(Palette.paper)
                    .listRowSeparator(.hidden)
                }

                if !archived.isEmpty {
                    Section {
                        ForEach(archived) { t in
                            HStack(spacing: 12) {
                                Rectangle().fill(Palette.binding(t.binding).opacity(0.5)).frame(width: 4, height: 28)
                                Text(t.name).font(VersoFont.serif(.body)).foregroundStyle(Palette.fadedInk)
                                Spacer()
                                QuietButton("Pick up") { model.perform { try await $0.archiveThread(t.id, archived: false) } }
                            }
                            .listRowBackground(Palette.paper)
                            .swipeActions {
                                Button(role: .destructive) { model.perform { try await $0.deleteThread(t.id) } } label: {
                                    Label("Delete", systemImage: "trash")
                                }
                            }
                        }
                    } header: {
                        Text("Loose ends").font(VersoFont.serif(.title3)).italic().foregroundStyle(Palette.fadedInk)
                            .textCase(nil)
                    }
                }

                Section {
                    if bookPage == .single { Folio(model.board.folio).listRowBackground(Palette.paper).listRowSeparator(.hidden) }
                    Color.clear.frame(height: 40).listRowBackground(Palette.paper).listRowSeparator(.hidden)
                }
            }
            .listStyle(.plain)
            .scrollContentBackground(.hidden)
            // A phone's rows keep their own insets. Anywhere else the list takes the page's margins, as a `Page`
            // does, so the head, strip and ✒︎ line up with the text on every other tab.
            .contentMargins(.leading, layout.isPhone ? 0 : layout.gutter + sides[0], for: .scrollContent)
            .contentMargins(.trailing, layout.isPhone ? 0 : layout.gutter + sides[1], for: .scrollContent)
            .onGeometryChange(for: [CGFloat].self) { [$0.safeAreaInsets.leading, $0.safeAreaInsets.trailing] } action: {
                sides = $0
            }
            .debugScrollAnchor()
            .background(Palette.paper.ignoresSafeArea())
            // Unfolded with a thread open: the left page is new, and opens at that thread's row, not the top.
            .onAppear {
                guard let open, !model.threadsPath.isEmpty else { return }
                Task { @MainActor in proxy.scrollTo(open, anchor: .center) }
            }
        }
        .environment(\.editMode, Binding { editMode } set: { editMode = $0 })
        .toolbar {
            if layout.isPhone { ToolbarItem(placement: .topBarTrailing) { arrange } }
        }
        .toolbar(layout.isPhone ? .automatic : .hidden, for: .navigationBar)
        .toolbarBackgroundVisibility(.hidden, for: .navigationBar)
    }

    /// Arrange (reorder) the threads, or finish arranging: a bar button on a phone, a quiet glyph in the head.
    @ViewBuilder var arrange: some View {
        let label = editMode.isEditing ? "Done arranging" : "Arrange threads"
        let toggle = { withAnimation { editMode = editMode.isEditing ? .inactive : .active } }
        let glyph = Image(systemName: editMode.isEditing ? "checkmark" : "arrow.up.arrow.down").fontWeight(.light)
        if layout.isPhone {
            Button(action: toggle) { glyph }.accessibilityLabel(label)
        } else {
            Button(action: toggle) {
                glyph.font(.system(size: 14)).foregroundStyle(Palette.fadedInk)
                    .frame(width: 32, height: 32).contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(label)
        }
    }

    /// One column: a push, as it always was. On a spread the right page turns to the thread instead, and the
    /// row it shows is shaded like a page held open.
    @ViewBuilder func row(_ line: Board.ThreadLine) -> some View {
        if model.isSpread {
            // A tap, not a Button: the row's own ✒︎ button has to stay tappable inside it.
            ThreadListRow(line: line)
                .contentShape(Rectangle())
                .onTapGesture { model.openThread(line.id) }
                .accessibilityAddTraits(.isButton)
                .accessibilityAction { model.openThread(line.id) }
        } else {
            // Off a phone the row ends at its ✒︎, flush with the page's margin as on a spread: no chevron after it.
            NavigationLink(value: line.id) { ThreadListRow(line: line) }
                .navigationLinkIndicatorVisibility(layout.isPhone ? .automatic : .hidden)
        }
    }

    /// The thread an open spread shows when none is chosen: the one most recently written in.
    static func lastLogged(_ lines: [Board.ThreadLine]) -> UUID? {
        lines.compactMap { l in l.lastActivity.map { (l.id, $0) } }.max { $0.1 < $1.1 }?.0 ?? lines.first?.id
    }

    var rollup: String {
        let r = Momentum.rollup(model.board.threads.map(\.momentum))
        return "\(r.moving) OF \(r.total) MOVING"
    }
}

/// A list row: cloth strip, name, kind, ring and figure, last line, 6-week ticks, ✒︎.
struct ThreadListRow: View {
    let line: Board.ThreadLine
    @Environment(AppModel.self) private var model

    var body: some View {
        let t = line.thread
        HStack(spacing: 14) {
            Rectangle().fill(Palette.binding(t.binding)).frame(width: 6)
            ThreadRing(progress: line.progress.progress, binding: t.binding, lineWidth: 2.5)
                .frame(width: 40, height: 40)
            VStack(alignment: .leading, spacing: 4) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(t.name).font(VersoFont.serif(.body)).foregroundStyle(Palette.ink).lineLimit(1)
                    Text(t.kind.tag).runningHeadStyle(size: 9)
                }
                VStack(alignment: .leading, spacing: 2) {
                    Text(line.progress.label).font(VersoFont.ledger(.subheadline)).foregroundStyle(Palette.ink)
                        .contentTransition(.numericText())
                        .fixedSize()
                    LogStroke(threadID: t.id, binding: t.binding).frame(width: 60)
                }
                HStack(spacing: 10) {
                    Text(lastLine).font(VersoFont.his(13)).foregroundStyle(line.momentum.isIdle ? Palette.rust : Palette.fadedInk)
                    WeekTicks(line.progress.weekTicks, binding: t.binding)
                }
            }
            .padding(.vertical, 12)
            // Taking up the slack itself rather than through a Spacer saves the name one 14pt gap, which a long
            // name beside its kind needs on the closed Duo's page.
            .frame(maxWidth: .infinity, alignment: .leading)
            Button { model.sheet = .log(t.id) } label: {
                PenMark(size: 17).foregroundStyle(Palette.bindingStroke(t.binding))
                    .frame(width: 40, height: 40)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Log \(t.name)")
        }
        .accessibilityElement(children: .combine)
        .accessibilityAction(named: "Log \(line.progress.logLabel)") { model.log(t.id) }
    }

    var lastLine: String {
        guard line.lastActivity != nil else { return "no lines yet" }
        let d = line.momentum.idleDays
        return d == 0 ? "last line today" : "last line \(DateText.relativeDays(d))"
    }
}

extension Measure {
    /// Extra side margin for a full-width List whose rows already carry the phone's 24pt insets: nothing on any
    /// phone (a Pro Max included), then whatever holds the text to one measure, centred. A List is one column
    /// even in a wide window.
    static func listMargin(_ width: CGFloat) -> CGFloat { max(0, (width - text) / 2 - 24) }
}

#Preview {
    @Previewable @Namespace var ns
    NavigationStack { ThreadsView(zoom: ns) }
        .environment(AppModel.preview)
}
