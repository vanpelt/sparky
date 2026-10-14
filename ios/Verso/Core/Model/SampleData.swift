import Foundation

/// The ONLY fixture source for previews and tests. Every number here is fake and round.
public enum SampleData {
    public static let calendar: Calendar = {
        var c = Calendar(identifier: .gregorian)
        c.timeZone = TimeZone(identifier: "America/New_York")!
        c.locale = Locale(identifier: "en_US")
        return c
    }()

    public static func day(_ y: Int, _ m: Int, _ d: Int, hour: Int = 12) -> Date {
        calendar.date(from: DateComponents(year: y, month: m, day: d, hour: hour))!
    }

    /// Fixed "today" for previews and tests: 9 Oct 2026, noon in New York.
    public static let today = day(2026, 10, 9)
    public static let price: Double = 100

    public static let grant = Grant(
        revestShares: 100_000, rsuShares: 100_000, rolloverShares: 1_000, rolloverVestDate: nil,
        vestDayOfMonth: 20, bookOneStart: day(2025, 3, 1, hour: 0),
        salaryAnnual: 200_000, bonusAnnual: 50_000,
        taxRevest: 0.238, revestSheltered: false, taxOrdinary: 0.503,
        isSample: true, seededFrom: nil, updatedAt: day(2026, 10, 1))

    public static let priceCache = PriceCache(price: price, timestamp: today.addingTimeInterval(-240),
                                              source: "StockAnalysis", fetchedAt: today.addingTimeInterval(-240))

    // Stable ids so previews and tests can refer to them.
    public static let moveID = UUID(uuidString: "00000000-0000-0000-0000-00000000000A")!
    public static let booksID = UUID(uuidString: "00000000-0000-0000-0000-00000000000B")!
    public static let filmID = UUID(uuidString: "00000000-0000-0000-0000-00000000000C")!
    public static let serbianID = UUID(uuidString: "00000000-0000-0000-0000-00000000000D")!

    /// Fixed milestone ids so clauses can point at them across calls.
    public static let filmMilestones: [Milestone] = Milestone.filmDefaults().enumerated().map { i, m in
        Milestone(id: UUID(uuidString: String(format: "00000000-0000-0000-0001-%012d", i))!, title: m.title)
    }

    public static var threads: [PlotThread] {
        let created = day(2026, 7, 1)
        var film = filmMilestones
        film[0].doneAt = day(2026, 8, 20)
        film[1].doneAt = day(2026, 9, 28)
        return [
            PlotThread(id: moveID, name: "Move", logline: "A body that can carry Book Two.",
                       kind: .habit(perWeek: 4), binding: .forest, createdAt: created, sortIndex: 0),
            PlotThread(id: booksID, name: "Read 12 books", logline: "Read like it's the job.",
                       kind: .count(target: 12, deadline: day(2026, 12, 31)), binding: .navy, createdAt: created, sortIndex: 1),
            PlotThread(id: filmID, name: "Make a short film", logline: "Finish one thing that is wholly mine.",
                       kind: .project(milestones: film), binding: .oxblood, createdAt: created, sortIndex: 2),
            PlotThread(id: serbianID, name: "Learn Serbian", subtitle: "Српски", logline: "Talk to my grandmother's town.",
                       kind: .practice(unit: .minutes, weeklyTarget: 120), binding: .plum, defaultAmount: 20,
                       createdAt: created, sortIndex: 3),
        ]
    }

    public static var entries: [Entry] {
        var e: [Entry] = []
        // Move: 3 sessions this week (Mon 5 – Fri 9 Oct), 4 a week for the five weeks before.
        for d in [5, 6, 8] { e.append(Entry(threadID: moveID, date: day(2026, 10, d, hour: 7))) }
        for w in 1...5 {
            for d in [0, 1, 3, 5] {
                e.append(Entry(threadID: moveID, date: calendar.date(byAdding: .day, value: -7 * w + d, to: day(2026, 10, 5, hour: 7))!))
            }
        }
        // Books: 7 read.
        let titles = ["Middlemarch", "The Dispossessed", "Stoner", "Bluets", "Pnin", "The Rings of Saturn", "Housekeeping"]
        for (i, t) in titles.enumerated() {
            e.append(Entry(threadID: booksID, date: day(2026, 7, 10 + i * 12), title: t))
        }
        // Serbian: 80 min this week, ~120 most weeks before.
        e.append(Entry(threadID: serbianID, date: day(2026, 10, 6), amount: 40, note: "reč nedelje: polako"))
        e.append(Entry(threadID: serbianID, date: day(2026, 10, 8), amount: 40))
        for w in 1...4 { e.append(Entry(threadID: serbianID, date: day(2026, 10, 7 - 7 * w + 0, hour: 20).addingTimeInterval(0), amount: 120)) }
        // Film milestone entries.
        let film = threads[2].milestones
        e.append(Entry(threadID: filmID, date: film[0].doneAt!, milestoneID: film[0].id))
        e.append(Entry(threadID: filmID, date: film[1].doneAt!, milestoneID: film[1].id))
        return e
    }

    /// Eight weeks of Marginalia trending toward Book Two.
    public static var pulses: [Pulse] {
        let rows: [(Int, Int, Int, Int, String?)] = [
            (4, 4, 2, 2, "Good week; the team shipped."),
            (3, 4, 3, 2, nil),
            (3, 3, 3, 3, "Even."),
            (3, 3, 4, 3, "Wrote three pages at night."),
            (2, 3, 4, 4, nil),
            (3, 3, 4, 4, "Kept thinking about the film."),
            (2, 3, 5, 4, "The script is the only thing I want to do."),
            (3, 3, 4, 4, "Steady, but the pull is real."),
        ]
        let lastMonday = day(2026, 10, 5) // noon NY: still Monday in every US time zone
        return rows.enumerated().map { i, r in
            Pulse(weekOf: calendar.date(byAdding: .day, value: -7 * (rows.count - 1 - i), to: lastMonday)!,
                  energy: r.0, anchor: r.1, pull: r.2, restlessness: r.3,
                  anchorReasons: i % 2 == 0 ? ["the team", "the money"] : ["the money"], sentence: r.4)
        }
    }

    public static var clauses: [Clause] {
        let created = day(2026, 8, 1)
        let film = threads[2].milestones
        return [
            // Chapter III: the standard schedule's Nov 2026 vest, the next one after `today`.
            Clause(text: "the November chapter is in", kind: .tranche(index: 2), role: .ordinary, createdAt: created),
            Clause(text: "the script is drafted", kind: .milestone(threadID: filmID, milestoneID: film[1].id),
                   role: .required, createdAt: created, metSince: day(2026, 9, 28, hour: 0)),
            Clause(text: "Book Two has pulled for six weeks", kind: .pulse(metric: .pulls, weeks: 6, ofLast: 8),
                   role: .ordinary, createdAt: created),
            Clause(text: "I've read twelve books this year", kind: .threadTarget(threadID: booksID), role: .ordinary, createdAt: created),
            Clause(text: "I'd rather be broke than bored", kind: .manual(checked: false), role: .ordinary, createdAt: created),
            Clause(text: "my health says so", kind: .manual(checked: false), role: .regardless, createdAt: created),
        ]
    }

    public static var book: BookFile {
        BookFile(grant: grant, settings: Settings(enoughLinePerDay: 3_000, epigraph: "Begin where the margin runs out."),
                 chapters: [ChapterMeta(trancheIndex: 0, title: "The Cliff", priceAtVest: 90),
                            ChapterMeta(trancheIndex: 6, title: "The Long Autumn")],
                 clauses: clauses, stayCase: "A team I'd follow anywhere, building the thing I'd build anyway.",
                 openingLines: [OpeningLine(date: day(2026, 9, 1), text: "The first morning, nobody needed me before ten.")],
                 letter: SealedLetter(body: "Dear Book Two —", sealedAt: day(2026, 9, 1)),
                 state: BookState(firstLaunchAt: day(2026, 9, 1), onboardedAt: day(2026, 9, 1)))
    }

    public static var state: VersoState {
        VersoState(book: book, threads: threads, entries: entries, pulses: pulses, price: priceCache)
    }

    /// A ready-made board at the fixed `today`.
    public static var board: Board { Board(state: state, today: today, calendar: calendar) }

    public static var snapshot: VersoSnapshot { SnapshotBuilder.build(from: board) }
}
