import WidgetKit
import SwiftUI
import AppIntents
import VersoCore

/// Thread (§9.4): one Book Two thread, with a +1 button that logs from the widget itself.
struct ThreadWidgetView: View {
    let entry: ThreadEntry
    var family: WidgetFamily? = nil

    init(entry: ThreadEntry, family: WidgetFamily? = nil) {
        self.entry = entry; self.family = family
    }

    var body: some View {
        FamilyReader(explicit: family) { f in content(f) }
    }

    @ViewBuilder
    func content(_ f: WidgetFamily) -> some View {
        if entry.unreadable {
            WidgetRefreshNotice(family: f, head: "BOOK TWO")
        } else if let t = entry.threads.first {
            switch f {
            case .accessoryInline: Text("\(t.name) · \(t.label)")
            case .accessoryCircular: circular(t)
            case .accessoryRectangular: rectangular(t)
            case .systemSmall: small(t)
            case .systemMedium: medium
            case .systemLarge: shelf(capacity: 6, xl: false)
            case .systemExtraLargePortrait: register(capacity: 8)
            default: small(t)
            }
        } else {
            switch f {
            case .accessoryInline: Text("No threads yet")
            case .accessoryCircular: Text(Glyph.pen).font(VersoFont.display(20))
            case .accessoryRectangular:
                Text("Start a thread in Verso.").font(VersoFont.his(14)).frame(maxWidth: .infinity, alignment: .leading)
            case .systemSmall, .systemMedium: WidgetBlankPage(line: "Start a thread in Verso.", head: "BOOK TWO")
            case .systemLarge, .systemExtraLargePortrait:
                WidgetBlankPage(line: "Start a thread in Verso.", head: "BOOK TWO", large: true)
            default: WidgetBlankPage(line: "Start a thread in Verso.", head: "BOOK TWO")
            }
        }
    }

    // MARK: - Families

    func circular(_ t: ThreadSummary) -> some View {
        ThreadRing(progress: t.progress, binding: t.binding, lineWidth: 4) {
            Text(t.initial).font(VersoFont.display(20))
        }
        .padding(2)
        .accessibilityLabel("\(t.name), \(t.label)")
    }

    func rectangular(_ t: ThreadSummary) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("\(t.name.uppercased()) · \(t.periodLabel)")
                .font(VersoFont.runningHead(11)).tracking(0.8).lineLimit(1).minimumScaleFactor(0.75)
                .widgetAccentable()
            if t.kindTag == "HABIT" && t.weekDots.count == 7 {
                ThreadWidgetWeekDots(dots: t.weekDots, binding: t.binding, today: weekdayIndex)
            } else {
                ProgressView(value: min(1, max(0, t.progress))).widgetAccentable()
            }
            Text(lastLine(t)).font(VersoFont.serif(.caption2)).lineLimit(1)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    func small(_ t: ThreadSummary) -> some View {
        HStack(alignment: .top, spacing: 10) {
            ThreadWidgetSpine(name: t.name, binding: t.binding)
            VStack(alignment: .leading, spacing: 4) {
                Link(destination: VersoWidgetURL.thread(t.id)) {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(t.kindTag).runningHeadStyle(size: 8)
                        ring(t, size: 62)
                        Text(t.kindTag == "HABIT" ? t.periodLabel : t.label)
                            .font(VersoFont.ledger(.caption)).foregroundStyle(Palette.ink)
                            .lineLimit(1).minimumScaleFactor(0.7)
                            .contentTransition(.numericText(value: t.progress))
                            .invalidatableContent()
                    }
                }
                Spacer(minLength: 0)
                logButton(t)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    var medium: some View {
        HStack(alignment: .top, spacing: 10) {
            ForEach(Array(entry.threads.prefix(3).enumerated()), id: \.element.id) { i, t in
                if i > 0 { Rectangle().fill(Palette.rule).frame(width: 0.5) }
                VStack(alignment: .leading, spacing: 4) {
                    Link(destination: VersoWidgetURL.thread(t.id)) {
                        VStack(alignment: .leading, spacing: 4) {
                            HStack(spacing: 4) {
                                Rectangle().fill(Palette.binding(t.binding)).frame(width: 3, height: 10).widgetAccentable()
                                Text(t.name.uppercased()).runningHeadStyle(size: 9).lineLimit(1).minimumScaleFactor(0.7)
                            }
                            ring(t, size: 52)
                            Text(t.kindTag == "HABIT" ? t.periodLabel : t.label)
                                .font(VersoFont.ledger(.caption2)).foregroundStyle(Palette.ink)
                                .lineLimit(1).minimumScaleFactor(0.6)
                                .contentTransition(.numericText(value: t.progress))
                                .invalidatableContent()
                        }
                    }
                    Spacer(minLength: 0)
                    logButton(t)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    // MARK: - Large and extra-large: the shelf

    /// Two even columns of threads (HIG: prefer even column counts), up to three rows on a large page and four on
    /// an extra-large one, the featured thread first. Each cell is the small widget's thread set flat: name,
    /// ring, figure, the week's dots (or when it was last logged) and its own log button.
    func shelf(capacity: Int, xl: Bool) -> some View {
        let shown = Array(entry.threads.prefix(capacity))
        let rows = stride(from: 0, to: shown.count, by: 2).map { Array(shown[$0..<min($0 + 2, shown.count)]) }
        return VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .firstTextBaseline) {
                Text("BOOK TWO · THE THREADS").runningHeadStyle(size: 10)
                Spacer(minLength: 8)
                if entry.threads.count > shown.count {
                    Text("\(entry.threads.count - shown.count) more in Verso").font(VersoFont.his(11))
                        .foregroundStyle(Palette.fadedInk)
                }
            }
            .padding(.bottom, xl ? 12 : 8)
            ForEach(Array(rows.enumerated()), id: \.offset) { r, row in
                if r > 0 { Hairline().padding(.vertical, xl ? 10 : 7) }
                HStack(alignment: .top, spacing: 12) {
                    ForEach(Array(row.enumerated()), id: \.element.id) { i, t in
                        if i > 0 { Rectangle().fill(Palette.rule).frame(width: 0.5) }
                        cell(t, xl: xl)
                    }
                    // An odd last thread keeps its column's width rather than spanning the page.
                    if row.count == 1 {
                        Rectangle().fill(Color.clear).frame(width: 0.5)
                        Color.clear.frame(maxWidth: .infinity)
                    }
                }
                .frame(maxHeight: .infinity)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    /// Extra-large: the threads as a register, one to a line across the whole page, the way the app's Threads
    /// list sets them: cloth strip, a big ring, name and kind, figure, the week (dots for a habit) and when it was
    /// last logged, and its log button at the line's end. The page's height goes to more threads, not air.
    func register(capacity: Int) -> some View {
        let shown = Array(entry.threads.prefix(capacity))
        return VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .firstTextBaseline) {
                Text("BOOK TWO · THE THREADS").runningHeadStyle(size: 11)
                Spacer(minLength: 8)
                if entry.threads.count > shown.count {
                    Text("\(entry.threads.count - shown.count) more in Verso").font(VersoFont.his(12))
                        .foregroundStyle(Palette.fadedInk)
                }
            }
            .padding(.bottom, 10)
            ForEach(Array(shown.enumerated()), id: \.element.id) { i, t in
                if i > 0 { Hairline() }
                registerLine(t)
                    .frame(maxHeight: 120)
            }
            Spacer(minLength: 0)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    func registerLine(_ t: ThreadSummary) -> some View {
        HStack(alignment: .center, spacing: 14) {
            Link(destination: VersoWidgetURL.thread(t.id)) {
                HStack(alignment: .center, spacing: 14) {
                    Rectangle().fill(Palette.binding(t.binding)).frame(width: 4).padding(.vertical, 10)
                        .widgetAccentable()
                    ring(t, size: 72)
                    VStack(alignment: .leading, spacing: 4) {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            Text(t.name).font(VersoFont.serif(.title3)).foregroundStyle(Palette.ink)
                                .lineLimit(1).minimumScaleFactor(0.7)
                            Text(t.kindTag).runningHeadStyle(size: 9)
                        }
                        Text(t.kindTag == "HABIT" ? t.periodLabel : t.label)
                            .font(VersoFont.ledger(.subheadline)).foregroundStyle(Palette.ink)
                            .lineLimit(1).minimumScaleFactor(0.7)
                            .contentTransition(.numericText(value: t.progress))
                            .invalidatableContent()
                        HStack(spacing: 10) {
                            if t.kindTag == "HABIT" && t.weekDots.count == 7 {
                                ThreadWidgetWeekDots(dots: t.weekDots, binding: t.binding, today: weekdayIndex)
                            }
                            Text(lastLine(t)).font(VersoFont.his(13)).foregroundStyle(Palette.fadedInk)
                                .lineLimit(1).minimumScaleFactor(0.7)
                        }
                    }
                    Spacer(minLength: 0)
                }
            }
            logButton(t).frame(width: 120)
        }
        .frame(maxHeight: .infinity)
    }

    /// Name at the head, ring and figures centred in what's left, the log button at the foot: the cell keeps its
    /// shape whether the shelf has two rows or four.
    func cell(_ t: ThreadSummary, xl: Bool) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            Link(destination: VersoWidgetURL.thread(t.id)) {
                VStack(alignment: .leading, spacing: 0) {
                    HStack(spacing: 4) {
                        Rectangle().fill(Palette.binding(t.binding)).frame(width: 3, height: 10).widgetAccentable()
                        Text(t.name.uppercased()).runningHeadStyle(size: 9).lineLimit(1).minimumScaleFactor(0.7)
                    }
                    Spacer(minLength: 4)
                    HStack(alignment: .center, spacing: 10) {
                        ring(t, size: xl ? 64 : 56)
                        VStack(alignment: .leading, spacing: 3) {
                            Text(t.kindTag == "HABIT" ? t.periodLabel : t.label)
                                .font(VersoFont.ledger(xl ? .subheadline : .caption)).foregroundStyle(Palette.ink)
                                .lineLimit(1).minimumScaleFactor(0.7)
                                .contentTransition(.numericText(value: t.progress))
                                .invalidatableContent()
                            if t.kindTag == "HABIT" && t.weekDots.count == 7 {
                                ThreadWidgetWeekDots(dots: t.weekDots, binding: t.binding, today: weekdayIndex)
                            } else {
                                // One line, shrinking rather than wrapping: a wrap put the "·" at the head of a line.
                                Text(lastLine(t)).font(VersoFont.serif(.caption2)).foregroundStyle(Palette.fadedInk)
                                    .lineLimit(1).minimumScaleFactor(0.7)
                            }
                        }
                    }
                    Spacer(minLength: 4)
                }
            }
            logButton(t)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    // MARK: - Pieces

    private var weekdayIndex: Int { (Calendar.current.component(.weekday, from: entry.date) + 5) % 7 }

    func lastLine(_ t: ThreadSummary) -> String {
        guard let last = t.lastEntryAt else { return "no lines yet" }
        let d = Days.between(last, entry.date)
        let ago = d <= 0 ? "today" : (d == 1 ? "yesterday" : "\(d)d ago")
        // Non-breaking either side of the "·", so it never wraps to the head of a line.
        return "last \(ago)\u{00A0}·\u{00A0}\(t.status.word)"
    }

    /// `size` is a cap: the ring is that big when the slot allows (it always does on a phone) and shrinks, square,
    /// in a shorter one, its letter with it.
    func ring(_ t: ThreadSummary, size: CGFloat) -> some View {
        ThreadRing(progress: t.progress, binding: t.binding, lineWidth: 3) {
            Text(t.progress >= 1 ? Glyph.fleuron : t.initial)
                .font(VersoFont.hero(size * 0.4, relativeTo: .title3))
                .minimumScaleFactor(0.5)
                .foregroundStyle(t.progress >= 1 ? Palette.gilt : Palette.ink)
        }
        .frame(idealWidth: size, maxWidth: size, idealHeight: size, maxHeight: size)
        .aspectRatio(1, contentMode: .fit)
    }

    /// The +1 / Done today / +20 min button. A habit already logged today reads "✓ today" and does nothing
    /// new (the store keeps "Done today" idempotent).
    @ViewBuilder
    func logButton(_ t: ThreadSummary) -> some View {
        let done = t.doneToday(entry.date)
        Button(intent: LogThreadIntent(thread: ThreadEntity(t), amount: nil)) {
            Text(done ? "✓ today" : "\(Glyph.pen) \(t.logLabel)")
                .font(VersoFont.serif(.caption, weight: .medium)).lineLimit(1).minimumScaleFactor(0.7)
                .padding(.horizontal, 8).padding(.vertical, 4)
                .frame(maxWidth: .infinity)
                .background(Capsule().fill(done ? Palette.deckle : Palette.binding(t.binding).opacity(0.14)))
                .foregroundStyle(done ? Palette.fadedInk : Palette.bindingStroke(t.binding))
        }
        .buttonStyle(.plain)
        .disabled(done)
        .widgetAccentable()
        .accessibilityLabel(done ? "\(t.name) done today" : "Log \(t.name)")
    }
}

/// A thin cloth spine with the thread's name in vertical small caps.
private struct ThreadWidgetSpine: View {
    let name: String
    let binding: VersoCore.Binding
    var body: some View {
        RoundedRectangle(cornerRadius: 2, style: .continuous)
            .fill(Palette.binding(binding))
            .frame(width: 18)
            .overlay {
                Text(name.uppercased())
                    .font(VersoFont.runningHead(9)).tracking(1.5)
                    .foregroundStyle(Color(light: 0xF2ECDF, dark: 0xE8E0CF))
                    .lineLimit(1).fixedSize()
                    .rotationEffect(.degrees(-90))
            }
            .clipped()
            .widgetAccentable()
    }
}

/// Seven Monday-first dots for a habit week; today is ringed.
private struct ThreadWidgetWeekDots: View {
    let dots: [Bool]
    let binding: VersoCore.Binding
    let today: Int
    var body: some View {
        HStack(spacing: 5) {
            ForEach(0..<7, id: \.self) { i in
                Circle()
                    .fill(dots[i] ? Palette.bindingStroke(binding) : Color.clear)
                    .overlay(Circle().stroke(i == today ? Palette.ink : Palette.rule, lineWidth: i == today ? 1 : 0.75))
                    .frame(width: 7, height: 7)
                    .widgetAccentable(dots[i])
            }
        }
        .accessibilityElement()
        .accessibilityLabel("\(dots.filter { $0 }.count) of 7 days this week")
    }
}
