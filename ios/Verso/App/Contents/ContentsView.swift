import SwiftUI
import VersoCore

/// Contents — the home page, set like a novel's title page and table of contents.
struct ContentsView: View {
    let zoom: Namespace.ID
    @Environment(AppModel.self) private var model
    @Environment(\.bookPage) private var bookPage
    @Environment(\.pageLayout) private var layout

    /// The chapter open on the right page of a spread (the current one when none is chosen); nil in one column.
    private func openChapter(_ schedule: LedgerResult?) -> Int? {
        guard bookPage == .verso else { return nil }
        if case .chapter(let i) = model.contentsPath.first { return i }
        return model.contentsPath.isEmpty ? schedule?.next?.index : nil
    }

    var body: some View {
        let board = model.board
        let schedule = board.ledger ?? model.scheduleLedger
        // In the model, not here: a fold rebuilds this page (`PageState`).
        @Bindable var pages = model.pages
        // On a spread the right page is the chapter itself: its head reads "CHAPTER III", so this one reads
        // just the book.
        let head = bookPage == .verso ? (board.runningHead.components(separatedBy: " · ").first ?? "") : board.runningHead
        let open = openChapter(schedule)
        ScrollViewReader { proxy in
            Page(folio: board.folio) {
                HeadBar(head, onSettings: { model.openContents(.colophon) }) {
                    // The fleuron is ornament again; long-press still turns up a first line.
                    Text(Glyph.fleuron)
                        .font(VersoFont.serif(.body)).foregroundStyle(Palette.oxblood)
                        .onLongPressGesture(minimumDuration: 0.4) { showOpeningLine() }
                        .accessibilityAction(named: "A first line") { showOpeningLine() }
                }
            } content: {
                if let line = pages.openingLine {
                    Text(line).font(VersoFont.his(16)).foregroundStyle(Palette.fadedInk)
                        .padding(.vertical, 8)
                        .transition(.opacity.combined(with: .move(edge: .top)))
                        .onTapGesture { withAnimation { pages.openingLine = nil } }
                }

                if model.state.book.grant?.isSample == true {
                    Text("Sample figures. Set yours in the Colophon.")
                        .font(VersoFont.his(14)).foregroundStyle(Palette.fadedInk)
                        .padding(.vertical, 6).frame(maxWidth: .infinity)
                        .background(Palette.deckle)
                        .onTapGesture { model.openContents(.colophon) }
                }

                if model.isRecto {
                    RectoContents()
                } else {
                    // On a spread the right page opens on the chapter's own big numeral: this page keeps the count
                    // and the contents, and names the chapter in small caps.
                    ChapterHero(schedule: schedule, opener: bookPage != .verso, short: layout.isShort)
                    DinkusRule()
                    PartOne(schedule: schedule, showAll: $pages.showAllChapters, zoom: zoom, open: open)
                }

                DinkusRule()
                PartTwo(zoom: zoom)

                if !model.isRecto {
                    DinkusRule()
                    TurningLine()
                }

                if board.marginaliaDue {
                    Button { model.sheet = .marginalia } label: {
                        Text("Marginalia — five lines for the week ›")
                            .font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
                    }
                    .buttonStyle(.plain)
                    .padding(.top, 28)
                }

                FooterEpigraph()
            }
            .navigationBarHidden(true)
            .refreshable { await model.refreshPrice(force: true) }
            // A chapter turned to on the right page (a deep link, the recto's own links) brings its row into view
            // on the left, expanding the list if the row was folded away.
            // (The path, not `open`: the schedule loading also moves `open`, and that must not scroll the page.)
            .onChange(of: model.contentsPath) { _, _ in bringOpenRowIntoView(schedule, proxy) }
            // Unfolded with a chapter open: the left page is new, and opens at that chapter's row, not the top.
            .onAppear { if !model.contentsPath.isEmpty { bringOpenRowIntoView(schedule, proxy, animated: false) } }
        }
    }

    /// Scrolls the open chapter's row into view on a spread's left page, expanding the list if it was folded away.
    func bringOpenRowIntoView(_ schedule: LedgerResult?, _ proxy: ScrollViewProxy, animated: Bool = true) {
        guard let i = openChapter(schedule), let schedule else { return }
        let pages = model.pages
        if !PartOne.visibleRows(schedule, showAll: pages.showAllChapters).contains(.row(i)) { pages.showAllChapters = true }
        // A beat later, so a row the expansion just added (or a page just built) is laid out to scroll to.
        Task { @MainActor in
            withAnimation(animated ? .smooth : nil) { proxy.scrollTo(PartOne.rowID(i), anchor: .center) }
        }
    }

    func showOpeningLine() {
        let own = model.state.book.openingLines.map(\.text)
        let pool = own.count >= 3 ? own : FirstLines.bundled
        withAnimation(.smooth) { model.pages.openingLine = pool.randomElement() }
    }
}

/// A small bundled list of famous first lines (public domain), for the ❦ long-press.
enum FirstLines {
    static let bundled = [
        "Call me Ishmael.",
        "It was a bright cold day in April, and the clocks were striking thirteen.",
        "Happy families are all alike; every unhappy family is unhappy in its own way.",
        "Mrs. Dalloway said she would buy the flowers herself.",
        "All this happened, more or less.",
        "The past is a foreign country; they do things differently there.",
        "It was the best of times, it was the worst of times.",
        "Riverrun, past Eve and Adam's, from swerve of shore to bend of bay.",
    ]
}

// MARK: - Title-page hero

struct ChapterHero: View {
    let schedule: LedgerResult?
    /// The chapter's big numeral and its title. Off on a spread's left page, where the right page opens on them.
    var opener = true
    /// A short window (the closed Duo): numeral and count a step smaller and closer, so the contents start
    /// above the foot.
    var short = false
    @Environment(AppModel.self) private var model
    @FocusState private var editing: Bool

    var body: some View {
        let ledger = model.board.ledger
        if let schedule, let next = schedule.next {
            VStack(spacing: 0) {
                if opener {
                    Text(next.numeral)
                        .font(VersoFont.hero(short ? 64 : 96)).foregroundStyle(Palette.oxblood)
                        .heroCap().heroReveal()
                        .accessibilityLabel("Chapter \(next.index + 1)")
                    TextField("", text: title(next.index), prompt: Text(editing ? "Name this chapter" : "").foregroundStyle(Palette.rule))
                        .displayFont(22).italic()
                        .multilineTextAlignment(.center)
                        .foregroundStyle(Palette.ink)
                        .focused($editing)
                        .submitLabel(.done)
                        .onSubmit(save)
                        .onChange(of: editing) { _, e in if !e { save() } }
                        // The chapter's own page shares the key: unfolded, the right page's title field takes the
                        // keyboard. With that page pushed over this one, the keyboard is the pushed page's.
                        .keepsFocus(model.contentsPath.isEmpty ? "chapter-title-\(next.index)" : nil, $editing)
                        .frame(minHeight: short ? 22 : 30)
                        .accessibilityLabel("Chapter title")
                } else {
                    Text("CHAPTER \(next.numeral)").runningHeadStyle(Palette.oxblood, size: 12)
                        .padding(.top, 16).padding(.bottom, 14)
                        .accessibilityLabel("Chapter \(next.index + 1)")
                }

                Group {
                    if next.daysToNext <= 1 {
                        Text(next.date, style: .relative)
                            .font(VersoFont.hero(short ? 44 : 52))
                    } else {
                        Text("\(next.daysToNext)")
                            .font(VersoFont.hero(short ? 84 : 120))
                            .contentTransition(.numericText(value: Double(next.daysToNext)))
                    }
                }
                .monospacedDigit()
                .foregroundStyle(Palette.ink)
                .heroCap().heroReveal()
                // Short, the count's tall line box is pulled up into the gap under the title.
                .padding(.top, short ? -12 : 6)
                Text(next.daysToNext <= 1 ? "UNTIL THE VEST" : "DAYS").runningHeadStyle(size: 12).padding(.top, -4)

                // One line normally; at large Dynamic Type the money drops to its own line instead of truncating.
                ViewThatFits(in: .horizontal) {
                    HStack(spacing: 4) {
                        Text("until the \(DateText.dayMonth(next.date)) vest ·")
                        MoneyText(ledger?.next?.after, style: .compact, afterTax: true)
                        Text("after tax")
                    }
                    VStack(spacing: 2) {
                        Text("until the \(DateText.dayMonth(next.date)) vest")
                        HStack(spacing: 4) {
                            MoneyText(ledger?.next?.after, style: .compact, afterTax: true)
                            Text("after tax")
                        }
                    }
                }
                .font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
                .lineLimit(1)
                .padding(.top, short ? 10 : 18)

                if ledger == nil {
                    Text("Set a price in the Colophon.").font(VersoFont.his(14)).foregroundStyle(Palette.rust).padding(.top, 4)
                }

                RibbonBar(progress: next.chapterProgress).padding(.top, short ? 12 : 18).padding(.horizontal, 12)
            }
            .frame(maxWidth: .infinity)
            .padding(.top, short ? 0 : 20)
        } else if schedule?.isComplete == true {
            VStack(spacing: 10) {
                Text(Glyph.fleuron).font(VersoFont.hero(64)).foregroundStyle(Palette.gilt)
                Text("Book One is fully read.").font(VersoFont.his(24)).foregroundStyle(Palette.ink)
            }
            .frame(maxWidth: .infinity).padding(.vertical, 48)
        } else {
            VStack(spacing: 14) {
                Text("I").font(VersoFont.hero(96)).foregroundStyle(Palette.rule)
                Text("No grant yet.").font(VersoFont.his(22)).foregroundStyle(Palette.ink)
                QuietButton("Set yours in the Colophon ›", color: Palette.oxblood) { model.openContents(.colophon) }
            }
            .frame(maxWidth: .infinity).padding(.vertical, 40)
        }
    }

    /// The chapter's title as typed: the same draft its own page edits (`PageState.chapters`), so a title half
    /// typed here is still there on the right page when the book is opened, and back here when it is closed.
    func title(_ i: Int) -> Binding<String> {
        let pages = model.pages
        return Binding { pages.chapter(i, in: model).title ?? "" } set: { t in
            var m = pages.chapter(i, in: model)
            m.title = t.isEmpty ? nil : t
            pages.chapters[i] = m
        }
    }

    func save() {
        guard let i = schedule?.next?.index, var m = model.pages.chapters[i] else { return }
        let t = (m.title ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        m.title = t.isEmpty ? nil : t
        model.pages.chapters[i] = m
        model.pages.saveChapter(i, in: model)
    }
}

// MARK: - Part One: the table of contents

struct PartOne: View {
    let schedule: LedgerResult?
    @Binding var showAll: Bool
    let zoom: Namespace.ID
    /// The chapter open on the facing page, shaded like a page held open; nil in one column.
    var open: Int? = nil
    @Environment(AppModel.self) private var model

    static func rowID(_ i: Int) -> String { "contents-row-\(i)" }

    var body: some View {
        PartTitle(part: "PART ONE", title: "Book One")
        if let schedule {
            let rows = Self.visibleRows(schedule, showAll: showAll)
            VStack(alignment: .leading, spacing: 0) {
                ForEach(rows, id: \.self) { item in
                    switch item {
                    case .row(let i):
                        let t = schedule.tranches[i]
                        // A button, not a NavigationLink: on an open spread the chapter turns up on the right page
                        // instead of being pushed over the contents.
                        Button { model.openContents(.chapter(i)) } label: {
                            ChapterRow(tranche: t, title: model.state.book.chapter(i).title,
                                       ebb: model.board.ledger?.chapters.first { $0.index == i }.map { UnitFraction.render($0.ebbWeight) },
                                       isCurrent: schedule.next?.index == i)
                        }
                        .buttonStyle(.plain)
                        .background(i == open ? Palette.deckle : .clear)
                        .matchedTransitionSource(id: "chapter-\(i)", in: zoom)
                        .id(Self.rowID(i))
                    case .more(let n):
                        Button { withAnimation(.smooth) { showAll = true } } label: {
                            Text("· · · all \(Roman.spelled(n).lowercased())")
                                .font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
                                .frame(maxWidth: .infinity).padding(.vertical, 10)
                        }
                        .buttonStyle(.plain)
                    }
                }
            }
        } else {
            Aside("The thirteen chapters appear once the grant is set.")
        }
    }

    enum Item: Hashable { case row(Int), more(Int) }

    /// The last read chapter, the current one, the next three and the last; "all thirteen" expands.
    static func visibleRows(_ l: LedgerResult, showAll: Bool) -> [Item] {
        let n = l.tranches.count
        if showAll || n <= 6 { return (0..<n).map(Item.row) }
        let cur = l.next?.index ?? n - 1
        var keep = Set([max(0, cur - 1), cur, n - 1])
        for k in 1...3 where cur + k < n { keep.insert(cur + k) }
        let rows = (0..<n).filter(keep.contains)
        guard rows.count < n else { return rows.map(Item.row) }
        var out = rows.dropLast().map(Item.row)
        out.append(.more(n))
        out.append(.row(n - 1))
        return out
    }
}

struct ChapterRow: View {
    let tranche: TrancheRow
    let title: String?
    /// What this chapter adds to everything banked by then ("1⁄7"). Every tranche pays the same, so a
    /// per-chapter amount would print one identical figure down the column; the fraction is what changes.
    let ebb: String?
    let isCurrent: Bool

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 0) {
            Text(tranche.numeral + ".")
                .font(VersoFont.serif(.body)).foregroundStyle(isCurrent ? Palette.oxblood : Palette.ink)
                .frame(width: 52, alignment: .leading)
            LeaderRow {
                Text(title ?? "").font(VersoFont.serif(.body)).italic().lineLimit(1)
            } trailing: {
                HStack(spacing: 10) {
                    Text(DateText.monthYear(tranche.date)).font(VersoFont.ledger(.subheadline))
                    if tranche.isVested {
                        Text("✓").font(VersoFont.serif(.subheadline)).frame(width: 52, alignment: .trailing)
                    } else {
                        Text(ebb.map { "+\($0)" } ?? "—")
                            .font(VersoFont.ledger(.subheadline))
                            .foregroundStyle(isCurrent ? Palette.oxblood : Palette.fadedInk)
                            .frame(width: 52, alignment: .trailing)
                            .accessibilityLabel(ebb.map { "adds \($0) of all banked by then" } ?? "")
                    }
                }
            }
            .readStrike(tranche.isVested)
        }
        .foregroundStyle(tranche.isVested ? Palette.fadedInk : Palette.ink)
        .padding(.vertical, 9)
        .padding(.trailing, 18)
        .overlay(alignment: .trailing) {
            if isCurrent {
                BookmarkRibbon().fill(Palette.oxblood).frame(width: 10, height: 30).offset(x: 6)
                    .accessibilityHidden(true)
            }
        }
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityValue(isCurrent ? "current chapter" : (tranche.isVested ? "read" : ""))
    }
}

// MARK: - Part Two: the bookshelf of threads

struct PartTwo: View {
    let zoom: Namespace.ID
    @Environment(AppModel.self) private var model
    /// The shelf's width. A phone column holds five spines; a wider one shelves as many as fit.
    @State private var shelf: CGFloat = 354

    var body: some View {
        PartTitle(part: "PART TWO", title: model.isRecto ? "Book Two" : "Book Two, in draft")
        // A spine is 56pt with 12pt between: n of them need 68n − 12.
        let lines = Array(model.board.threads.prefix(max(5, Int((shelf + 12) / 68))))
        if lines.isEmpty {
            VStack(spacing: 10) {
                Aside("No threads yet. What does Book Two have going?")
                QuietButton("+ Start a thread", color: Palette.verdigris) { model.sheet = .newThread }
            }
            .frame(maxWidth: .infinity)
        } else {
            HStack(alignment: .bottom, spacing: 12) {
                ForEach(lines) { line in
                    SpineRow(line: line)
                        .matchedTransitionSource(id: "spine-\(line.id)", in: zoom)
                        .onTapGesture { model.openContents(.thread(line.id)) }
                }
            }
            .frame(maxWidth: .infinity)
            .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { shelf = $0 }
            Text("Long-press a spine to log a line.")
                .font(VersoFont.his(13)).foregroundStyle(Palette.fadedInk.opacity(0.8))
                .frame(maxWidth: .infinity).padding(.top, 14)
        }
    }
}

/// One cloth spine on the shelf: name rotated in small caps, figure and 28-day ticks beneath.
struct SpineRow: View {
    let line: Board.ThreadLine
    @Environment(AppModel.self) private var model
    @State private var pressing = false

    var body: some View {
        let t = line.thread
        VStack(spacing: 8) {
            ThreadSpine(name: t.name, binding: t.binding)
                .frame(height: 150 + CGFloat(t.name.count % 4) * 8)
                .overlay(alignment: .bottom) {
                    // Fill rises with progress, like a bookmark of light in the cloth.
                    Rectangle().fill(Color.white.opacity(0.10))
                        .frame(height: (150 + CGFloat(t.name.count % 4) * 8) * line.progress.progress)
                        .allowsHitTesting(false)
                }
                .clipShape(UnevenRoundedRectangle(topLeadingRadius: 3, bottomLeadingRadius: 1, bottomTrailingRadius: 1, topTrailingRadius: 3))
                .scaleEffect(pressing ? 0.96 : 1, anchor: .bottom)
                .animation(.spring(duration: 0.25), value: pressing)
            VStack(spacing: 3) {
                Text(line.progress.label.replacingOccurrences(of: " ", with: ""))
                    .font(VersoFont.ledger(.caption)).foregroundStyle(Palette.ink)
                    .contentTransition(.numericText())
                    .lineLimit(1).minimumScaleFactor(0.6)
                LogStroke(threadID: t.id, binding: t.binding)
                    .frame(width: 34)
                MomentumTicks(line: line)
            }
            .frame(width: 56)
        }
        .onLongPressGesture(minimumDuration: 0.35, pressing: { p in pressing = p }) {
            model.log(t.id)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(t.name)
        .accessibilityValue("\(line.progress.label), \(line.momentum.status.word)")
        .accessibilityAddTraits(.isButton)
        .accessibilityAction(named: "Log \(line.progress.logLabel)") { model.log(t.id) }
    }
}

/// The 28-day momentum tick: four dashes, one per week, filled when the week met pace.
struct MomentumTicks: View {
    let line: Board.ThreadLine
    var body: some View {
        WeekTicks(Array(line.progress.weekTicks.suffix(4)), binding: line.thread.binding)
    }
}

// MARK: - Turning Point line and footer

struct TurningLine: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        let c = model.board.clarity
        Button { model.tab = .turning } label: {
            HStack(spacing: 12) {
                // TickGlyph (28pt, 1.2pt) vanished on paper and disappeared entirely in dark mode.
                TickArc(ticks: c.tickStates, lineWidth: 1.8).frame(width: 38, height: 28)
                Text(c.stage.title).font(VersoFont.serif(.title3)).italic().foregroundStyle(Palette.stage(c.stage))
                Text("· \(c.tallyLabel)").font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
                Spacer(minLength: 0)
                Text("›").font(VersoFont.serif(.title3)).foregroundStyle(Palette.fadedInk)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("The Turning Point: \(c.stage.title), \(c.tallyLabel)")
    }
}

struct FooterEpigraph: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        let latest = model.state.pulses.sorted { $0.weekOf > $1.weekOf }.first { !($0.sentence ?? "").isEmpty }
        let opening = model.state.book.openingLines.max { $0.date < $1.date }?.text
        if let text = latest?.sentence ?? opening ?? model.state.book.settings.epigraph {
            VStack(alignment: .trailing, spacing: 6) {
                Text(text).font(VersoFont.his(18)).foregroundStyle(Palette.ink)
                    .frame(maxWidth: .infinity, alignment: .leading)
                Text(latest != nil ? "— you, \(weekdayName)" : opening != nil ? "— Book Two, opening line" : "— the epigraph")
                    .font(VersoFont.body(13)).foregroundStyle(Palette.fadedInk)
            }
            .padding(.top, 32)
            .padding(.horizontal, 12)
        }
    }

    var weekdayName: String {
        let i = (model.state.book.settings.marginaliaWeekday - 1 + 7) % 7
        return model.calendar.weekdaySymbols[i]
    }
}

/// Recto: Part One collapses to one line.
struct RectoContents: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        let s = model.state.book.state
        VStack(alignment: .leading, spacing: 10) {
            Text("BOOK TWO").runningHeadStyle(Palette.oxblood, size: 13).frame(maxWidth: .infinity).padding(.top, 28)
            Text("Page \(model.board.folio)").font(VersoFont.hero(72)).frame(maxWidth: .infinity).heroCap().heroReveal()
            DinkusRule()
            if let closed = s.closedAt {
                let pages = Days.between(model.state.book.grant?.bookOneStart ?? s.firstLaunchAt, closed, calendar: model.calendar) + 1
                Text("Book One · \(pages.formatted()) pages · closed \(DateText.long(closed))")
                    .font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
            }
            if let sentence = s.closingSentence, !sentence.isEmpty {
                Text(sentence).font(VersoFont.his(18)).foregroundStyle(Palette.ink)
            }
        }
    }
}

#Preview {
    @Previewable @Namespace var ns
    NavigationStack { ContentsView(zoom: ns) }
        .environment(AppModel.preview)
        .environment(\.versoVeiled, false)
}
