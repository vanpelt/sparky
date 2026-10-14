import WidgetKit
import SwiftUI
import VersoCore

/// Next Chapter (§9.1): days until the next vest. The intended StandBy pick.
struct NextChapterWidgetView: View {
    let entry: VersoEntry
    var family: WidgetFamily? = nil
    /// False in StandBy and wherever the system strips the widget's container: no paper behind it.
    @Environment(\.showsWidgetContainerBackground) private var showsBackground

    init(entry: VersoEntry, family: WidgetFamily? = nil) {
        self.entry = entry; self.family = family
    }

    var body: some View {
        FamilyReader(explicit: family) { f in
            content(f).environment(\.versoVeiled, !entry.moneyVisible(f))
        }
    }

    @ViewBuilder
    func content(_ f: WidgetFamily) -> some View {
        if entry.unreadable {
            WidgetRefreshNotice(family: f, head: "NEXT CHAPTER")
        } else if let page = entry.bookTwoPage {
            recto(page, f)
        } else if !entry.hasGrant {
            blank("Set your grant in Verso.", f)
        } else if let next = entry.live.next {
            switch f {
            case .accessoryInline: inline(next)
            case .accessoryCircular: circular(next)
            case .accessoryRectangular: rectangular(next)
            case .systemSmall: small(next)
            case .systemMedium:
                HStack(alignment: .top, spacing: 16) {
                    small(next)
                    Rectangle().fill(Palette.rule).frame(width: 0.5)
                    contents(next)
                }
            case .systemLarge: page(next, rows: 8, heroSize: 64)
            case .systemExtraLargePortrait: page(next, rows: 16, heroSize: 88)
            default: small(next)
            }
        } else {
            blank("Book One is fully read.", f)
        }
    }

    // MARK: - Pieces

    private var vestDay: TrancheRow? { entry.vestedToday }
    /// The last day before a vest: count down in hours with a live relative date.
    private func isLastDay(_ next: NextVest) -> Bool { next.daysToNext <= 1 && vestDay == nil }

    private func dayWord(_ n: Int) -> String { n == 1 ? "day" : "days" }
    private func short(_ d: Date) -> String { d.formatted(.dateTime.month(.abbreviated).day()) }

    func inline(_ next: NextVest) -> some View {
        Group {
            if let v = vestDay { Text("Chapter \(v.numeral) ends today \(Glyph.fleuron)") }
            else if isLastDay(next) { Text("Ch. \(next.numeral) ends \(next.date, style: .relative)") }
            else { Text("Ch. \(next.numeral) ends in \(next.daysToNext) \(dayWord(next.daysToNext))") }
        }
    }

    func circular(_ next: NextVest) -> some View {
        Gauge(value: vestDay == nil ? next.chapterProgress : 1) {
            Text(next.numeral).font(VersoFont.runningHead(8))
        } currentValueLabel: {
            if vestDay != nil {
                Text(Glyph.fleuron).font(VersoFont.display(18))
            } else {
                VStack(spacing: -2) {
                    Text("\(next.daysToNext)").font(VersoFont.hero(22, relativeTo: .title3)).monospacedDigit()
                        .minimumScaleFactor(0.6)
                    Text(next.numeral).font(VersoFont.runningHead(7)).tracking(0.5)
                }
            }
        }
        .gaugeStyle(.accessoryCircularCapacity)
        .widgetAccentable()
    }

    func rectangular(_ next: NextVest) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Group {
                if let v = vestDay { Text("CH. \(v.numeral) ENDS TODAY \(Glyph.fleuron)") }
                else { Text("CH. \(next.numeral) · \(next.daysToNext) \(dayWord(next.daysToNext).uppercased())") }
            }
            .font(VersoFont.runningHead(12)).tracking(1).widgetAccentable().lineLimit(1)
            HStack(spacing: 3) {
                if isLastDay(next) { Text(next.date, style: .relative) } else { Text(short(next.date)) }
                Text("·")
                MoneyText(entry.live.ledger?.next?.after)
            }
            .font(VersoFont.serif(.caption)).lineLimit(1)
            ProgressView(value: next.chapterProgress).widgetAccentable()
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// StandBy (no container background) drops the chapter title and the ribbon's paper hairline and sets the
    /// count larger, since the page it sits on is gone and it is read from across a room.
    func small(_ next: NextVest) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(alignment: .firstTextBaseline) {
                Text("CHAPTER \(vestDay?.numeral ?? next.numeral)").runningHeadStyle(size: 10)
                Spacer(minLength: 4)
                if let t = entry.snapshot.chapterTitles[next.index], vestDay == nil, showsBackground {
                    Text(t).font(VersoFont.his(10)).foregroundStyle(Palette.fadedInk).lineLimit(1)
                }
            }
            Spacer(minLength: 0)
            hero(next, size: showsBackground ? 64 : 80)
            Spacer(minLength: 0)
            if showsBackground {
                RibbonBar(progress: vestDay == nil ? next.chapterProgress : 0)
            } else {
                // Just the oxblood stroke: the hairline track is drawn for paper.
                GeometryReader { g in
                    Capsule().fill(Palette.oxblood)
                        .frame(width: g.size.width * min(1, max(0, vestDay == nil ? next.chapterProgress : 0)), height: 3)
                        .widgetAccentable()
                }
                .frame(height: 3)
                .accessibilityHidden(true)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
    }

    /// The count and its caption, three ways: a vest day, the last day (live hours), or days to go. `size` is
    /// the days numeral; the other two states scale from it (34 and 30 at the small widget's 64).
    @ViewBuilder
    func hero(_ next: NextVest, size: CGFloat) -> some View {
        let k = size / 64
        if let v = vestDay {
            Text("ends\ntoday").font(VersoFont.hero(34 * k)).foregroundStyle(Palette.ink)
                .lineSpacing(-6 * k).minimumScaleFactor(0.6)
            Text("\(Glyph.fleuron) CHAPTER \(next.numeral) BEGINS").runningHeadStyle(Palette.gilt, size: 9)
                .lineLimit(1).minimumScaleFactor(0.7)
                .accessibilityLabel("Chapter \(v.numeral) ends today. Chapter \(next.numeral) begins.")
        } else if isLastDay(next) {
            Text(next.date, style: .relative).font(VersoFont.hero(30 * k)).foregroundStyle(Palette.oxblood)
                .monospacedDigit().minimumScaleFactor(0.5).lineLimit(1)
            Text("TILL \(short(next.date).uppercased())").runningHeadStyle(size: 10)
        } else {
            Text("\(next.daysToNext)")
                .font(VersoFont.hero(size)).monospacedDigit()
                .contentTransition(.numericText(value: Double(next.daysToNext)))
                .foregroundStyle(Palette.ink).minimumScaleFactor(0.5).lineLimit(1)
            Text("\(dayWord(next.daysToNext).uppercased()) · \(short(next.date).uppercased())")
                .runningHeadStyle(size: 10)
        }
    }

    /// The right half of the medium widget: a mini table of contents of the next three chapters.
    func contents(_ next: NextVest) -> some View {
        let rows = Array((entry.live.schedule?.chapters ?? []).prefix(3))
        let money = entry.live.ledger
        return VStack(alignment: .leading, spacing: 7) {
            Text("CONTENTS").runningHeadStyle(size: 10)
            ForEach(rows) { c in
                HStack(spacing: 6) {
                    BookmarkRibbon()
                        .fill(c.index == next.index ? Palette.oxblood : Color.clear)
                        .frame(width: 5, height: 12)
                        .widgetAccentable()
                    LeaderRow {
                        Text(c.numeral).font(VersoFont.display(13))
                            .foregroundStyle(c.index == next.index ? Palette.ink : Palette.fadedInk)
                            .frame(minWidth: 26, alignment: .leading)
                        Text(short(c.date)).font(VersoFont.serif(.caption2)).foregroundStyle(Palette.fadedInk)
                    } trailing: {
                        MoneyText(money.flatMap { m in m.tranches.indices.contains(c.index) ? m.tranches[c.index].after : nil })
                            .font(VersoFont.ledger(.caption)).foregroundStyle(Palette.ink)
                    }
                }
            }
            Spacer(minLength: 0)
            if let c = rows.first {
                Text("adds \(UnitFraction.render(c.ebbWeight)) to all of Book One")
                    .font(VersoFont.his(11)).foregroundStyle(Palette.fadedInk).lineLimit(1).minimumScaleFactor(0.8)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    // MARK: - Large and extra-large: a contents page

    /// A whole page: the countdown as the chapter opening, then the book's table of contents with leader dots,
    /// set like the app's Contents (date and the fraction each chapter adds; read chapters struck through).
    /// `rows` is how many contents lines fit; a longer book keeps the app's window around the current chapter.
    func page(_ next: NextVest, rows: Int, heroSize: CGFloat) -> some View {
        let schedule = entry.live.schedule
        let tranches = schedule?.tranches ?? []
        let ebb = Dictionary(uniqueKeysWithValues: (schedule?.chapters ?? []).map { ($0.index, $0.ebbWeight) })
        let isXL = heroSize > 64
        return VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .firstTextBaseline) {
                Text("BOOK ONE · CHAPTER \(vestDay?.numeral ?? next.numeral)").runningHeadStyle(size: 10)
                Spacer(minLength: 8)
                if let t = entry.snapshot.chapterTitles[next.index], vestDay == nil {
                    Text(t).font(VersoFont.his(12)).foregroundStyle(Palette.fadedInk).lineLimit(1)
                }
            }
            HStack(alignment: .lastTextBaseline, spacing: 14) {
                VStack(alignment: .leading, spacing: 0) { hero(next, size: heroSize) }
                Spacer(minLength: 0)
                if vestDay == nil {
                    VStack(alignment: .trailing, spacing: 1) {
                        MoneyText(entry.live.ledger?.next?.after)
                            .font(VersoFont.ledger(isXL ? .title3 : .body)).foregroundStyle(Palette.ink)
                        Text("it brings").font(VersoFont.his(12)).foregroundStyle(Palette.fadedInk)
                    }
                    .padding(.bottom, 2)
                }
            }
            .padding(.top, isXL ? 10 : 4)
            RibbonBar(progress: vestDay == nil ? next.chapterProgress : 0).padding(.top, isXL ? 12 : 8)

            Text("CONTENTS").runningHeadStyle(size: 10).padding(.top, isXL ? 22 : 14).padding(.bottom, 4)
            VStack(alignment: .leading, spacing: isXL ? 7 : 4) {
                ForEach(Self.window(count: tranches.count, current: next.index, rows: rows), id: \.self) { i in
                    if let i, tranches.indices.contains(i) {
                        tocRow(tranches[i], ebb: ebb[i], isCurrent: i == next.index, isXL: isXL)
                    } else {
                        Text("· · ·").font(VersoFont.serif(.caption2)).foregroundStyle(Palette.fadedInk)
                            .frame(maxWidth: .infinity).accessibilityHidden(true)
                    }
                }
            }
            Spacer(minLength: 0)
            if let s = schedule {
                Hairline().padding(.bottom, 5)
                HStack(alignment: .firstTextBaseline) {
                    Text("\(s.chaptersLeft) \(s.chaptersLeft == 1 ? "chapter" : "chapters") left")
                        .font(VersoFont.his(12)).foregroundStyle(Palette.fadedInk)
                    Spacer(minLength: 6)
                    Text("THE LAST ENDS \(monthYear(s.finalDate).uppercased())").runningHeadStyle(size: 9).lineLimit(1)
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    /// One contents line: "V.  The Long Autumn ........ Nov ’26  +1⁄7", a ribbon on the current chapter.
    func tocRow(_ t: TrancheRow, ebb: Double?, isCurrent: Bool, isXL: Bool) -> some View {
        let text: Font = isXL ? VersoFont.serif(.subheadline) : VersoFont.serif(.caption)
        let figure: Font = isXL ? VersoFont.ledger(.subheadline) : VersoFont.ledger(.caption)
        return HStack(alignment: .firstTextBaseline, spacing: 0) {
            Text(t.numeral + ".").font(text)
                .foregroundStyle(isCurrent ? Palette.oxblood : (t.isVested ? Palette.fadedInk : Palette.ink))
                .frame(width: isXL ? 44 : 38, alignment: .leading)
            LeaderRow {
                Text(entry.snapshot.chapterTitles[t.index] ?? "").font(text).italic().lineLimit(1)
            } trailing: {
                HStack(spacing: 8) {
                    Text(monthYear(t.date)).font(figure)
                    Text(t.isVested ? "✓" : (ebb.map { "+\(UnitFraction.render($0))" } ?? "—"))
                        .font(figure)
                        .foregroundStyle(isCurrent ? Palette.oxblood : Palette.fadedInk)
                        .frame(width: isXL ? 40 : 34, alignment: .trailing)
                }
            }
            .readStrike(t.isVested)
        }
        .foregroundStyle(t.isVested ? Palette.fadedInk : Palette.ink)
        .padding(.trailing, 10)
        .overlay(alignment: .trailing) {
            if isCurrent {
                BookmarkRibbon().fill(Palette.oxblood).frame(width: 6, height: isXL ? 20 : 16).offset(x: 4)
                    .widgetAccentable().accessibilityHidden(true)
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityValue(isCurrent ? "current chapter" : (t.isVested ? "read" : ""))
    }

    /// Which tranche indices to list (nil = an elided run, drawn "· · ·"). Everything when it fits; otherwise
    /// the last chapter read, the current one and as many after it as fit, an ellipsis, and the last chapter.
    static func window(count n: Int, current: Int, rows: Int) -> [Int?] {
        guard n > rows, rows >= 4 else { return (0..<n).map { $0 } }
        let first = min(max(0, current - 1), n - rows + 1)
        let run = Array(first..<(first + rows - 2))
        // Near the end the run meets the last chapter: show the final `rows` chapters, no gap to mark.
        if run.last == n - 2 { return (n - rows..<n).map { $0 } }
        return run.map { $0 } + [nil, n - 1]
    }

    private func monthYear(_ d: Date) -> String {
        "\(d.formatted(.dateTime.month(.abbreviated))) \u{2019}\(d.formatted(.dateTime.year(.twoDigits)))"
    }

    // MARK: - States

    @ViewBuilder
    func recto(_ page: Int, _ f: WidgetFamily) -> some View {
        switch f {
        case .accessoryInline: Text("Book Two · p. \(page)")
        case .accessoryCircular:
            VStack(spacing: -2) {
                Text("p.").font(VersoFont.his(10))
                Text("\(page)").font(VersoFont.hero(20)).monospacedDigit().minimumScaleFactor(0.5)
            }
            .widgetAccentable()
        case .accessoryRectangular:
            VStack(alignment: .leading, spacing: 2) {
                Text("BOOK TWO").font(VersoFont.runningHead(12)).tracking(1).widgetAccentable()
                Text("Page \(page)").font(VersoFont.serif(.headline))
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        case .systemLarge, .systemExtraLargePortrait:
            VStack(alignment: .leading, spacing: 10) {
                Text("BOOK TWO").runningHeadStyle(Palette.verdigris, size: 11)
                Hairline()
                Spacer(minLength: 0)
                Text("p. \(page)").font(VersoFont.hero(f == .systemLarge ? 88 : 112)).monospacedDigit()
                    .minimumScaleFactor(0.5).lineLimit(1).foregroundStyle(Palette.ink)
                Text("the page is turned").font(VersoFont.his(20)).foregroundStyle(Palette.fadedInk)
                Spacer(minLength: 0)
                Text(Glyph.fleuron).font(VersoFont.display(18)).foregroundStyle(Palette.gilt).frame(maxWidth: .infinity)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
        case .systemSmall, .systemMedium: rectoTile(page)
        default: rectoTile(page)
        }
    }

    func rectoTile(_ page: Int) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("BOOK TWO").runningHeadStyle(Palette.verdigris, size: 10)
            Spacer(minLength: 0)
            Text("p. \(page)").font(VersoFont.hero(54)).monospacedDigit().minimumScaleFactor(0.5).lineLimit(1)
                .foregroundStyle(Palette.ink)
            Text("the page is turned").font(VersoFont.his(13)).foregroundStyle(Palette.fadedInk)
            Spacer(minLength: 0)
            Text(Glyph.fleuron).foregroundStyle(Palette.gilt)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
    }

    @ViewBuilder
    func blank(_ line: String, _ f: WidgetFamily) -> some View {
        switch f {
        case .accessoryInline: Text(line)
        case .accessoryCircular: Text(Glyph.fleuron).font(VersoFont.display(22))
        case .accessoryRectangular: Text(line).font(VersoFont.his(14)).frame(maxWidth: .infinity, alignment: .leading)
        case .systemSmall, .systemMedium: WidgetBlankPage(line: line, head: "NEXT CHAPTER")
        case .systemLarge, .systemExtraLargePortrait: WidgetBlankPage(line: line, head: "NEXT CHAPTER", large: true)
        default: WidgetBlankPage(line: line, head: "NEXT CHAPTER")
        }
    }
}
