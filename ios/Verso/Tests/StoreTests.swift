import Foundation
import Testing
@testable import VersoCore

final class RecordingReloader: WidgetReloading, @unchecked Sendable {
    private let lock = NSLock()
    private var _kinds: [String] = []
    var kinds: [String] { lock.withLock { _kinds } }
    func reload(kinds: [String]) { lock.withLock { _kinds += kinds } }
}

struct StoreTests {
    func makeStore(bundle: Bundle = Bundle(for: RecordingReloader.self)) -> (VersoStore, URL, RecordingReloader) {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("verso-tests-\(UUID().uuidString)", isDirectory: true)
        let r = RecordingReloader()
        let store = VersoStore(directory: dir, bundle: bundle, reloader: r, calendar: SampleData.calendar,
                               clock: { SampleData.today })
        return (store, dir, r)
    }

    @Test func bootstrapWithoutSeedCreatesFiles() async throws {
        let (store, dir, _) = makeStore()
        let r = try await store.bootstrap()
        #expect(r.isFirstLaunch)
        #expect(!r.seedImported)
        #expect(FileManager.default.fileExists(atPath: dir.appendingPathComponent("book.json").path))
        #expect(FileManager.default.fileExists(atPath: dir.appendingPathComponent("snapshot.json").path))
        let again = try await store.bootstrap()
        #expect(!again.isFirstLaunch)
    }

    @Test func logRebuildsSnapshotAndReloadsWidgets() async throws {
        let (store, dir, reloader) = makeStore()
        try await store.setGrant(SampleData.grant)
        try await store.setOverride(100, enabled: true)
        for t in SampleData.threads { try await store.upsertThread(t) }
        let e = try await store.log(threadID: SampleData.booksID, title: "Pnin")
        #expect(e?.amount == 1)
        let snap = try #require(VersoStore.readSnapshot(directory: dir))
        #expect(snap.threads.first { $0.id == SampleData.booksID }?.label == "1 / 12")
        #expect(snap.grant?.revestShares == 100_000)
        #expect(LiveBoard(snapshot: snap, at: SampleData.today, calendar: SampleData.calendar).next?.daysToNext == 42)
        #expect(reloader.kinds.contains(WidgetKinds.thread))
        #expect(reloader.kinds.contains(WidgetKinds.turningPoint))
    }

    @Test func habitIsIdempotentPerDayAndProjectCompletesMilestones() async throws {
        let (store, _, _) = makeStore()
        for t in SampleData.threads { try await store.upsertThread(t) }
        #expect(try await store.log(threadID: SampleData.moveID) != nil)
        #expect(try await store.log(threadID: SampleData.moveID) == nil)
        let e = try await store.log(threadID: SampleData.filmID)
        let s = try await store.load()
        let film = s.threads.first { $0.id == SampleData.filmID }!
        #expect(film.milestones[2].doneAt != nil)           // "Shot list" was next
        #expect(e?.milestoneID == film.milestones[2].id)
    }

    @Test func pulseUpsertsPerWeek() async throws {
        let (store, _, _) = makeStore()
        try await store.savePulse(energy: 3, anchor: 3, pull: 4, restlessness: 4, sentence: "first")
        try await store.savePulse(energy: 2, anchor: 3, pull: 5, restlessness: 4, sentence: "second")
        let s = try await store.load()
        #expect(s.pulses.count == 1)
        #expect(s.pulses[0].sentence == "second")
        #expect(s.pulses[0].board != nil)
        #expect(SampleData.calendar.component(.weekday, from: s.pulses[0].weekOf) == 2) // Monday
    }

    @Test func clauseEditsAreRevisions() async throws {
        let (store, _, _) = makeStore()
        var c = Clause(text: "pulled for 6 weeks", kind: .pulse(metric: .pulls, weeks: 6, ofLast: 8), createdAt: SampleData.today)
        try await store.upsertClause(c)
        c.text = "pulled for 10 weeks"; c.kind = .pulse(metric: .pulls, weeks: 10, ofLast: 12)
        try await store.upsertClause(c, reason: "wanted more certainty")
        let saved = try await store.load().book.clauses[0]
        #expect(saved.revisions.count == 1)
        #expect(saved.revisions[0].beforeText == "pulled for 6 weeks")
        #expect(saved.revisions[0].reason == "wanted more certainty")
        // Toggling a manual clause is not a revision.
        let m = Clause(text: "ready", kind: .manual(checked: false), createdAt: SampleData.today)
        try await store.upsertClause(m)
        try await store.setManual(m.id, checked: true)
        let mm = try await store.load().book.clauses[1]
        #expect(mm.revisions.isEmpty)
        #expect(mm.metSince != nil)
    }

    @Test func exportImportRoundTrip() async throws {
        let (a, _, _) = makeStore()
        try await a.setGrant(SampleData.grant)
        for t in SampleData.threads { try await a.upsertThread(t) }
        _ = try await a.log(threadID: SampleData.booksID)
        let data = try await a.exportJSON()
        let (b, _, _) = makeStore()
        try await b.importJSON(data)
        let s = try await b.load()
        #expect(s.threads.count == 4)
        #expect(s.entries.count == 1)
        #expect(s.book.grant?.rsuShares == 100_000)
    }

    @Test func closeBookOneEntersRecto() async throws {
        let (store, dir, _) = makeStore()
        try await store.closeBookOne(on: SampleData.today, sentence: "The End.")
        #expect(try await store.load().book.state.mode == .recto)
        #expect(VersoStore.readSnapshot(directory: dir)?.stage == .recto)
    }

    @Test func timelineDatesIncludeVestInstant() {
        var snap = SampleData.snapshot
        snap.clauses = []
        let now = SampleData.day(2026, 11, 10)
        let dates = LiveBoard.timelineDates(snapshot: snap, from: now, calendar: SampleData.calendar)
        #expect(dates.first == now)
        #expect(dates.contains(SampleData.day(2026, 11, 20, hour: 0)))
        #expect(dates.count >= 14)
    }
}
