import Foundation
import Testing
@testable import VersoCore

/// Regression tests for the review round: honest pulse copy, quiet price reloads, market hours,
/// the cross-process store lock and habit notes.
struct ReviewFixTests {
    func store(_ dir: URL, _ r: RecordingReloader = RecordingReloader()) -> VersoStore {
        VersoStore(directory: dir, bundle: Bundle(for: RecordingReloader.self), reloader: r,
                   calendar: SampleData.calendar, clock: { SampleData.today })
    }
    func tempDir() -> URL {
        FileManager.default.temporaryDirectory.appendingPathComponent("verso-fix-\(UUID().uuidString)", isDirectory: true)
    }

    // MARK: Pulse

    @Test func sampleLeanIsNamedNotCalledEven() {
        let p = PulseAnalysis.analyze(SampleData.pulses, today: SampleData.today, calendar: SampleData.calendar)
        #expect(p.state == .even)                 // hysteresis hasn't called it…
        #expect(p.leaning == .pulls)              // …but the last two weeks pull
        #expect(p.leanWeeks == 2)
        #expect(p.headline.hasPrefix("Leaning toward Book Two, 2 weeks running"))
        #expect(!p.headline.contains("Nothing pulling"))
    }

    @Test func steadyStateHasNoLean() {
        let base = Days.weekStart(SampleData.today, calendar: SampleData.calendar)
        let rows = (0..<5).map { i in
            Pulse(weekOf: Days.add(-7 * (4 - i), to: base, calendar: SampleData.calendar), energy: 3, anchor: 3, pull: 3, restlessness: 3)
        }
        let p = PulseAnalysis.analyze(rows, today: SampleData.today, calendar: SampleData.calendar)
        #expect(p.state == .even && p.leaning == nil)
        #expect(p.headline == PulseState.even.copy(weeks: p.stateWeeks))
    }

    @Test func readingUsesTheHonestHeadline() {
        let text = SampleData.board.reading(veiled: true)
        #expect(text.contains("Leaning toward Book Two"))
        #expect(!text.contains("The X fell"))
    }

    // MARK: Price reloads & market hours

    @Test func priceReloadsOnlyWhatCanChange() {
        let a = PriceCache(price: 100, fetchedAt: SampleData.today)
        var same = a; same.fetchedAt = SampleData.today.addingTimeInterval(3_600)
        var moved = same; moved.price = 101
        #expect(VersoStore.priceReloadKinds(before: a, after: a, excluding: []) == [])
        #expect(VersoStore.priceReloadKinds(before: a, after: same, excluding: []) == [WidgetKinds.dayRate])
        #expect(VersoStore.priceReloadKinds(before: a, after: same, excluding: [WidgetKinds.dayRate]) == [])
        #expect(Set(VersoStore.priceReloadKinds(before: a, after: moved, excluding: [WidgetKinds.dayRate]))
                == [WidgetKinds.nextChapter, WidgetKinds.turningPoint])
    }

    @Test func marketHours() {
        let ny = PriceService.marketCalendar
        func at(_ y: Int, _ m: Int, _ d: Int, _ h: Int) -> Date { ny.date(from: DateComponents(year: y, month: m, day: d, hour: h))! }
        // Fri 9 Oct 2026, 12:00 → open. Sat 10 Oct → closed.
        #expect(PriceService.isSessionOpen(at: at(2026, 10, 9, 12)))
        #expect(!PriceService.isSessionOpen(at: at(2026, 10, 10, 12)))
        #expect(!PriceService.isSessionOpen(at: at(2026, 10, 9, 22)))
        // Fetched Friday 21:00, now Sunday: nothing could have moved.
        #expect(!PriceService.marketMayHaveMoved(since: at(2026, 10, 9, 21), now: at(2026, 10, 11, 15)))
        // Fetched Friday 15:00, now Saturday: Friday's close came after it.
        #expect(PriceService.marketMayHaveMoved(since: at(2026, 10, 9, 15), now: at(2026, 10, 10, 9)))
        #expect(PriceService.marketMayHaveMoved(since: nil, now: at(2026, 10, 10, 9)))
        #expect(PriceService.nextSessionOpen(after: at(2026, 10, 10, 9)) == at(2026, 10, 12, 4))
    }

    // MARK: Store

    @Test func twoStoresOnOneDirectoryDoNotLoseWrites() async throws {
        let dir = tempDir()
        let a = store(dir), b = store(dir)
        for t in SampleData.threads { try await a.upsertThread(t) }
        let n = 25
        await withTaskGroup(of: Void.self) { g in
            for s in [a, b] {
                g.addTask { for _ in 0..<n { _ = try? await s.log(threadID: SampleData.booksID) } }
            }
        }
        let entries = try await a.load().entries.filter { $0.threadID == SampleData.booksID }
        #expect(entries.count == 2 * n)
    }

    @Test func habitNoteJoinsTodaysEntry() async throws {
        let s = store(tempDir())
        for t in SampleData.threads { try await s.upsertThread(t) }
        let first = try await s.log(threadID: SampleData.moveID, note: "ran")
        #expect(first != nil)
        #expect(try await s.log(threadID: SampleData.moveID) == nil)           // nothing new, says so
        let again = try await s.log(threadID: SampleData.moveID, note: "and stretched")
        #expect(again?.id == first?.id)
        let today = try await s.load().entries.filter { $0.threadID == SampleData.moveID
            && SampleData.calendar.isDate($0.date, inSameDayAs: SampleData.today) }
        #expect(today.count == 1)
        #expect(today.first?.note == "ran · and stretched")
    }
}
