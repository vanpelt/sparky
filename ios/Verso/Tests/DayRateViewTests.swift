import Foundation
import Testing
@testable import VersoCore

/// The Ledger's "after tax / pre-tax" and "per day / per working day" toggles are a saved setting the Day Rate
/// widget follows. They used to be screen state, so the widget always said after-tax-per-calendar-day while the
/// Ledger said whatever he'd picked. Fake numbers only.
struct DayRateViewTests {
    func makeStore() -> (VersoStore, RecordingReloader) {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("verso-dayrate-\(UUID().uuidString)", isDirectory: true)
        let r = RecordingReloader()
        return (VersoStore(directory: dir, bundle: Bundle(for: RecordingReloader.self), reloader: r,
                           calendar: SampleData.calendar, clock: { SampleData.today }), r)
    }

    @Test func scalingIsTheLedgersArithmetic() throws {
        let l = try #require(SampleData.board.ledger)
        #expect(l.dayRate(DayRateView()) == l.dayRateAfter)
        #expect(l.dayRate(DayRateView(afterTax: false)) == l.dayRatePre)
        #expect(abs(l.dayRate(DayRateView(perWorkingDay: true)) - l.dayRateAfter * 365 / 261) < 1e-9)
        #expect(abs(l.dayRate(DayRateView(afterTax: false, perWorkingDay: true)) - l.dayRatePre * 365 / 261) < 1e-9)
        #expect(l.walkAway(DayRateView(afterTax: false)) == l.walkAwayPre)
        #expect(DayRateView(perWorkingDay: true).shortUnit == "/workday")
    }

    @Test func choosingWorkingDaysReachesTheWidgetAndReloadsIt() async throws {
        let (store, reloader) = makeStore()
        try await store.importJSON(VersoJSON.encoder().encode(VersoExport(
            book: SampleData.state.book, threads: SampleData.state.threads,
            entries: SampleData.state.entries, pulses: SampleData.state.pulses)))
        try await store.setPrice { $0 = SampleData.priceCache }
        let before = reloader.kinds.count

        // Exactly what LedgerView's toggle does.
        try await store.updateBook(.settings) { $0.settings.dayRate.perWorkingDay = true }

        #expect(reloader.kinds.dropFirst(before).contains(WidgetKinds.dayRate), "flipping the toggle must reload Day Rate")
        let snap = try #require(store.readSnapshot())
        #expect(snap.dayRate == DayRateView(perWorkingDay: true))

        // The widget's figure (from its own read of the store) equals the Ledger hero's.
        let src = await WidgetData.source(store: store)
        let widget = try #require(WidgetData.boards(src, at: [SampleData.today], calendar: SampleData.calendar).first??.ledger)
        let app = try #require(Board(state: try await store.load(), today: SampleData.today, calendar: SampleData.calendar).ledger)
        let view = try await store.load().book.settings.dayRate
        #expect(widget.dayRate(snap.dayRate) == app.dayRate(view))
        #expect(abs(widget.dayRate(snap.dayRate) - app.dayRateAfter * 365 / 261) < 1e-6)
    }

    @Test func olderFilesDecodeToTheOldBehaviour() throws {
        let settings = try VersoJSON.decoder().decode(Settings.self, from: Data(#"{"ticker":"CRWV"}"#.utf8))
        #expect(settings.dayRate == DayRateView())
        let snap = try VersoJSON.decoder().decode(VersoSnapshot.self, from: Data("{}".utf8))
        #expect(snap.dayRate == DayRateView())
    }
}
