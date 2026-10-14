import Foundation
import os

/// The single owner of every read and write. Files live in `<App Group>/Verso/`:
/// book.json, threads.json, entries.json, pulses.json, price.json and the derived snapshot.json.
///
/// Every mutation re-reads from disk (the widget process may have written), applies the change,
/// refreshes clause bookkeeping, writes the changed files atomically under NSFileCoordinator,
/// rebuilds snapshot.json and reloads the affected widget kinds.
public actor VersoStore {
    public static let shared = VersoStore()

    public nonisolated let directory: URL
    nonisolated let bundle: Bundle
    nonisolated let reloader: any WidgetReloading
    nonisolated let calendar: Calendar
    nonisolated let clock: @Sendable () -> Date
    static let log = Logger(subsystem: "verso", category: "store")

    /// When the snapshot was last written / widgets last reloaded (for the Colophon slug).
    public private(set) var lastSnapshotAt: Date?
    public private(set) var lastWidgetReloadAt: Date?

    public init(directory: URL = AppGroup.storeDirectory, bundle: Bundle = .main,
                reloader: any WidgetReloading = WidgetCenterReloader(), calendar: Calendar = .current,
                clock: @escaping @Sendable () -> Date = { Date() }) {
        self.directory = directory; self.bundle = bundle; self.reloader = reloader
        self.calendar = calendar; self.clock = clock
    }

    // MARK: Files

    enum File: String { case book, threads, entries, pulses, price, snapshot
        var name: String { rawValue + ".json" }
    }
    nonisolated func url(_ f: File) -> URL { directory.appendingPathComponent(f.name) }

    /// Read the whole state from disk.
    public func load() throws -> VersoState {
        let now = clock()
        let book = try FileIO.read(BookFile.self, at: url(.book)) ?? BookFile(state: BookState(firstLaunchAt: now))
        let threads = try FileIO.read(ThreadsFile.self, at: url(.threads))?.threads ?? []
        let entries = try FileIO.read(EntriesFile.self, at: url(.entries))?.entries ?? []
        let pulses = try FileIO.read(PulsesFile.self, at: url(.pulses))?.pulses ?? []
        let price = try FileIO.read(PriceCache.self, at: url(.price)) ?? PriceCache()
        return VersoState(book: book, threads: threads, entries: entries, pulses: pulses, price: price)
    }

    /// Read only snapshot.json. Safe from any process. `.success(nil)` = no file yet; a decode or IO error is
    /// logged (publicly, so it shows in Console) and returned, never swallowed into an empty state.
    public nonisolated static func readSnapshotResult(directory: URL = AppGroup.storeDirectory) -> Result<VersoSnapshot?, Error> {
        do {
            return .success(try FileIO.read(VersoSnapshot.self, at: directory.appendingPathComponent(File.snapshot.name)))
        } catch {
            log.error("snapshot.json unreadable: \(String(describing: error), privacy: .public)")
            return .failure(error)
        }
    }

    /// snapshot.json, or nil if it is missing or unreadable (the error is logged).
    public nonisolated static func readSnapshot(directory: URL = AppGroup.storeDirectory) -> VersoSnapshot? {
        try? readSnapshotResult(directory: directory).get()
    }

    public nonisolated func readSnapshot() -> VersoSnapshot? { Self.readSnapshot(directory: directory) }

    // MARK: Bootstrap & seed

    public struct BootstrapResult: Sendable, Hashable {
        public let isFirstLaunch: Bool
        public let seedImported: Bool
        public let seedBundled: Bool
    }

    /// Run once on launch. Creates book.json on first launch and silently imports the bundled seed
    /// if there is no grant yet. Never re-imports automatically.
    @discardableResult
    public func bootstrap() throws -> BootstrapResult {
        let first = !FileManager.default.fileExists(atPath: url(.book).path)
        var imported = false
        let bundled = SeedFile.isBundled(in: bundle)
        try commit(.all) { s, now in
            if s.book.grant == nil, let seed = SeedFile.bundled(in: bundle) {
                Self.applySeed(seed, to: &s, now: now)
                imported = true
            }
        }
        if imported { Self.log.notice("Imported grant from bundled local seed") }
        return BootstrapResult(isFirstLaunch: first, seedImported: imported, seedBundled: bundled)
    }

    /// Colophon › Re-import seed: overwrites the grant only. Returns false if no seed is bundled.
    @discardableResult
    public func reimportSeed() throws -> Bool {
        guard let seed = SeedFile.bundled(in: bundle) else { return false }
        try commit(.grant) { s, now in Self.applySeed(seed, to: &s, now: now) }
        return true
    }

    static func applySeed(_ seed: SeedFile, to s: inout VersoState, now: Date) {
        var g = seed.grant
        g.seededFrom = "local seed"; g.isSample = false; g.updatedAt = now
        s.book.grant = g
        if s.book.settings.enoughLinePerDay == nil { s.book.settings.enoughLinePerDay = seed.enoughLinePerDay }
        s.book.state.seedImportedAt = now
    }

    // MARK: The one mutation path

    /// Apply `body` to fresh on-disk state and commit. `body` receives the store's notion of now.
    /// The escape hatch for UI edits that have no dedicated method.
    @discardableResult
    public func mutate(_ changes: ChangeSet, _ body: @Sendable (inout VersoState, Date) throws -> Void) throws -> VersoState {
        try commit(changes, body)
    }

    /// The whole read-modify-write runs under one cross-process lock (`StoreLock`): the actor only serialises
    /// this process, and the widget extension has its own store that also commits (price refresh, +1 buttons).
    /// `kinds` overrides which widget kinds reload, given the state before and after (default: `changes`).
    @discardableResult
    func commit(_ changes: ChangeSet, kinds: ((VersoState, VersoState) -> [String])? = nil,
                _ body: (inout VersoState, Date) throws -> Void) throws -> VersoState {
        try StoreLock.withLock(in: directory) {
            try commitLocked(changes, kinds: kinds, body)
        }
    }

    private func commitLocked(_ changes: ChangeSet, kinds: ((VersoState, VersoState) -> [String])?,
                              _ body: (inout VersoState, Date) throws -> Void) throws -> VersoState {
        let now = clock()
        let before = try load()
        var s = before
        try body(&s, now)
        commitBookkeeping(&s, now: now)
        if s.book != before.book || !FileManager.default.fileExists(atPath: url(.book).path) {
            try FileIO.write(s.book, to: url(.book))
        }
        if s.threads != before.threads { try FileIO.write(ThreadsFile(threads: s.threads), to: url(.threads)) }
        if s.entries != before.entries { try FileIO.write(EntriesFile(entries: s.entries), to: url(.entries)) }
        if s.pulses != before.pulses { try FileIO.write(PulsesFile(pulses: s.pulses), to: url(.pulses)) }
        if s.price != before.price { try FileIO.write(s.price, to: url(.price)) }
        // Always (re)write the snapshot: date-derived content changes even if nothing else did.
        try writeSnapshot(Board(state: s, today: now, calendar: calendar))
        let reload = kinds?(before, s) ?? changes.widgetKinds
        if !reload.isEmpty { reloader.reload(kinds: reload); lastWidgetReloadAt = now }
        return s
    }

    /// Clause metSince, priceAtVest snapshots and Turning-Point aftercare.
    func commitBookkeeping(_ s: inout VersoState, now: Date) {
        let board = Board(state: s, today: now, calendar: calendar)
        s.book.clauses = ClauseEvaluator.refreshMetSince(s.book.clauses, board.context)
        let after = Board(state: s, today: now, calendar: calendar)
        if after.clarity.stage == .turningPoint {
            if s.book.state.turningPointSince == nil { s.book.state.turningPointSince = calendar.startOfDay(for: now) }
        } else if s.book.state.mode == .bookOne { s.book.state.turningPointSince = nil }
        // Snapshot priceAtVest at the first open on or after each vest.
        if let l = after.ledger, s.price.price != nil {
            for t in l.tranches where t.isVested {
                var meta = s.book.chapter(t.index)
                if meta.priceAtVest == nil && Days.between(t.date, now, calendar: calendar) <= 7 {
                    meta.priceAtVest = s.price.effective
                    s.book.setChapter(meta)
                }
            }
        }
        // A sealed letter opens itself at the Turning Point or on its date.
        if var letter = s.book.letter, letter.openedAt == nil {
            if after.clarity.stage == .turningPoint || (letter.opensOn.map { $0 <= now } ?? false) {
                letter.openedAt = now; s.book.letter = letter
            }
        }
    }

    func writeSnapshot(_ board: Board) throws {
        try FileIO.write(SnapshotBuilder.build(from: board), to: url(.snapshot))
        lastSnapshotAt = board.today
    }

    /// Rebuild snapshot.json and reload every widget. The app calls this on every foreground, at midnight and
    /// whenever another process changed the store, because foreground reloads are not budgeted and this is
    /// what retries a reload WidgetKit throttled or dropped earlier.
    public func refreshSnapshot() throws {
        try commit(.all) { _, _ in }
    }

    // MARK: Book, grant & settings

    public func updateBook(_ changes: ChangeSet = .book, _ body: @Sendable (inout BookFile) -> Void) throws {
        try commit(changes) { s, _ in body(&s.book) }
    }

    public func setGrant(_ grant: Grant?) throws {
        try commit(.grant) { s, now in
            var g = grant; g?.updatedAt = now
            s.book.grant = g
        }
    }

    public func useSampleGrant() throws {
        try commit([.grant, .price]) { s, now in
            var g = SampleData.grant; g.updatedAt = now
            s.book.grant = g
            if s.price.effective == nil { s.price.overridePrice = SampleData.price; s.price.overrideEnabled = true }
        }
    }

    public func updateSettings(_ body: @Sendable (inout Settings) -> Void) throws {
        try commit(.settings) { s, _ in body(&s.book.settings) }
    }

    public func setChapter(_ meta: ChapterMeta) throws {
        try commit(.book) { s, _ in s.book.setChapter(meta) }
    }

    public func addOpeningLine(_ text: String) throws {
        try commit(.book) { s, now in s.book.openingLines.append(OpeningLine(date: now, text: text)) }
    }

    public func sealLetter(_ body: String, opensOn: Date?) throws {
        try commit(.book) { s, now in s.book.letter = SealedLetter(body: body, sealedAt: now, opensOn: opensOn) }
    }

    public func openLetter() throws {
        try commit(.book) { s, now in if s.book.letter?.openedAt == nil { s.book.letter?.openedAt = now } }
    }

    /// Close Book One. Only he presses this.
    public func closeBookOne(on date: Date, sentence: String?) throws {
        try commit(.all) { s, _ in
            s.book.state.mode = .recto
            s.book.state.closedAt = date
            s.book.state.closingSentence = sentence
        }
    }

    // MARK: Threads & entries

    public func upsertThread(_ thread: PlotThread) throws {
        try commit(.threads) { s, _ in
            if let i = s.threads.firstIndex(where: { $0.id == thread.id }) { s.threads[i] = thread }
            else {
                var t = thread
                t.sortIndex = (s.threads.map(\.sortIndex).max() ?? -1) + 1
                s.threads.append(t)
            }
        }
    }

    public func archiveThread(_ id: UUID, archived: Bool = true) throws {
        try commit(.threads) { s, now in
            if let i = s.threads.firstIndex(where: { $0.id == id }) { s.threads[i].archivedAt = archived ? now : nil }
        }
    }

    public func deleteThread(_ id: UUID) throws {
        try commit([.threads, .clauses]) { s, _ in
            s.threads.removeAll { $0.id == id }
            s.entries.removeAll { $0.threadID == id }
        }
    }

    public func reorderThreads(_ ids: [UUID]) throws {
        try commit(.threads) { s, _ in
            for (n, id) in ids.enumerated() { if let i = s.threads.firstIndex(where: { $0.id == id }) { s.threads[i].sortIndex = n } }
        }
    }

    /// The one logging path (UI, widget button, Siri). `amount` nil = the thread's default.
    /// For a project thread, logging completes the next undone milestone (or `milestoneID`).
    @discardableResult
    public func log(threadID: UUID, amount: Double? = nil, note: String? = nil, title: String? = nil,
                    milestoneID: UUID? = nil, date: Date? = nil) throws -> Entry? {
        var made: Entry?
        try commit([.threads, .clauses]) { s, now in
            guard let i = s.threads.firstIndex(where: { $0.id == threadID }) else { return }
            let t = s.threads[i]
            let when = date ?? now
            var mid = milestoneID
            if case .project(var ms) = t.kind {
                let target = mid.flatMap { m in ms.firstIndex { $0.id == m } } ?? ms.firstIndex { $0.doneAt == nil }
                if let target { ms[target].doneAt = when; mid = ms[target].id; s.threads[i].kind = .project(milestones: ms) }
            }
            if case .habit = t.kind,
               let j = s.entries.firstIndex(where: { $0.threadID == threadID && calendar.isDate($0.date, inSameDayAs: when) }) {
                // "Done today" is idempotent per day, but a note typed under it isn't thrown away:
                // it joins that day's entry. Without a note nothing changes and nil says so.
                guard let n = note?.trimmingCharacters(in: .whitespacesAndNewlines), !n.isEmpty else { return }
                let old = s.entries[j].note?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                s.entries[j].note = old.isEmpty ? n : old + " · " + n
                made = s.entries[j]
                return
            }
            let e = Entry(threadID: threadID, date: when, amount: amount ?? t.defaultAmount, note: note, title: title, milestoneID: mid)
            s.entries.append(e)
            made = e
        }
        return made
    }

    public func setMilestone(threadID: UUID, milestoneID: UUID, done: Bool) throws {
        try commit([.threads, .clauses]) { s, now in
            guard let i = s.threads.firstIndex(where: { $0.id == threadID }), case .project(var ms) = s.threads[i].kind,
                  let m = ms.firstIndex(where: { $0.id == milestoneID }) else { return }
            ms[m].doneAt = done ? (ms[m].doneAt ?? now) : nil
            s.threads[i].kind = .project(milestones: ms)
            if !done { s.entries.removeAll { $0.milestoneID == milestoneID } }
        }
    }

    public func updateEntry(_ entry: Entry) throws {
        try commit([.threads, .clauses]) { s, _ in
            if let i = s.entries.firstIndex(where: { $0.id == entry.id }) { s.entries[i] = entry }
        }
    }

    public func deleteEntry(_ id: UUID) throws {
        try commit([.threads, .clauses]) { s, _ in s.entries.removeAll { $0.id == id } }
    }

    // MARK: Marginalia

    /// Save this week's record; updates rather than duplicates. Stamps the board snapshot.
    @discardableResult
    public func savePulse(energy: Int, anchor: Int, pull: Int, restlessness: Int, anchorReasons: [String] = [],
                          sentence: String? = nil, manualChecks: [UUID: Bool] = [:], weekOf: Date? = nil) throws -> Pulse {
        var saved: Pulse!
        try commit([.pulses, .clauses]) { s, now in
            for (id, checked) in manualChecks {
                if let i = s.book.clauses.firstIndex(where: { $0.id == id }), case .manual = s.book.clauses[i].kind {
                    s.book.clauses[i].kind = .manual(checked: checked)
                }
            }
            let week = Days.weekStart(weekOf ?? now, calendar: calendar)
            let clampv = { (x: Int) in min(5, max(1, x)) }
            var p = Pulse(weekOf: week, energy: clampv(energy), anchor: clampv(anchor), pull: clampv(pull),
                          restlessness: clampv(restlessness), anchorReasons: anchorReasons,
                          sentence: sentence?.trimmingCharacters(in: .whitespacesAndNewlines))
            if let i = s.pulses.firstIndex(where: { calendar.isDate($0.weekOf, inSameDayAs: week) }) {
                p.id = s.pulses[i].id
                s.pulses[i] = p
            } else { s.pulses.append(p) }
            s.pulses.sort { $0.weekOf < $1.weekOf }
            // Board snapshot after the pulse is in.
            var probe = s
            commitBookkeeping(&probe, now: now)
            p.board = Board(state: probe, today: now, calendar: calendar).boardSnapshot
            if let i = s.pulses.firstIndex(where: { $0.id == p.id }) { s.pulses[i] = p }
            saved = p
        }
        return saved
    }

    public func deletePulse(_ id: UUID) throws {
        try commit(.pulses) { s, _ in s.pulses.removeAll { $0.id == id } }
    }

    // MARK: Clauses

    /// Insert or edit a clause. An edit to text, kind or role appends a ClauseRevision with `reason`.
    public func upsertClause(_ clause: Clause, reason: String? = nil) throws {
        try commit(.clauses) { s, now in
            if let i = s.book.clauses.firstIndex(where: { $0.id == clause.id }) {
                let old = s.book.clauses[i]
                var c = clause
                c.revisions = old.revisions
                let manualToggleOnly: Bool = {
                    if case .manual = old.kind, case .manual = c.kind { return old.text == c.text && old.role == c.role }
                    return false
                }()
                if (old.text != c.text || old.kind != c.kind || old.role != c.role) && !manualToggleOnly {
                    c.revisions.append(ClauseRevision(at: now, beforeText: old.text, beforeKind: old.kind,
                                                      beforeRole: old.role, reason: reason))
                }
                c.metSince = old.kind == c.kind ? old.metSince : nil
                s.book.clauses[i] = c
            } else {
                s.book.clauses.append(clause)
            }
        }
    }

    public func deleteClause(_ id: UUID) throws {
        try commit(.clauses) { s, _ in s.book.clauses.removeAll { $0.id == id } }
    }

    public func setManual(_ id: UUID, checked: Bool) throws {
        try commit(.clauses) { s, _ in
            if let i = s.book.clauses.firstIndex(where: { $0.id == id }), case .manual = s.book.clauses[i].kind {
                s.book.clauses[i].kind = .manual(checked: checked)
            }
        }
    }

    public func setStayCase(_ text: String?) throws {
        try commit(.book) { s, _ in s.book.stayCase = text }
    }

    // MARK: Price

    public func setPrice(_ body: @Sendable (inout PriceCache) -> Void) throws {
        try commit(.price) { s, _ in body(&s.price) }
    }

    public func setOverride(_ price: Double?, enabled: Bool) throws {
        try setPrice { p in p.overridePrice = price; p.overrideEnabled = enabled && price != nil }
    }

    /// The fetch in flight in this process, and every kind its callers are building right now.
    private var priceFetch: Task<PriceCache, Error>?
    private var priceFetchExcluding: Set<String> = []

    /// Fetch if the cache is older than `ttl` (or always when `force`). Keeps the cached value on failure.
    ///
    /// `reloadAllPriceKinds` (the app, whose foreground reloads WidgetKit does not budget) reloads every kind
    /// that shows the price after any fetch, so a widget can never sit on a quote the app has moved past, even
    /// if an earlier reload was throttled or dropped. Without it (the widget extension, which is budgeted) only
    /// what can look different reloads: every price kind when the effective price moved, otherwise just Day
    /// Rate (its age label). `excluding` drops kinds whose timeline the caller is building right now.
    /// Concurrent callers in one process share one fetch.
    @discardableResult
    public func refreshPrice(service: PriceService? = nil, ttl: TimeInterval = PriceService.appTTL,
                             timeout: TimeInterval = 8, force: Bool = false, excluding: Set<String> = [],
                             reloadAllPriceKinds: Bool = false) async throws -> PriceCache {
        if let running = priceFetch {
            priceFetchExcluding.formUnion(excluding)
            return try await running.value
        }
        let current = try load().price
        let now = clock()
        guard force || current.needsRefresh(ttl: ttl, now: now) else { return current }
        let svc = service ?? PriceService(ticker: (try? load().book.settings.ticker) ?? "CRWV")
        priceFetchExcluding = excluding
        let task = Task<PriceCache, Error> {
            let quote = await svc.fetch(timeout: timeout)
            let s = try commit(.price, kinds: { before, after in
                Self.priceReloadKinds(before: before.price, after: after.price, excluding: self.priceFetchExcluding,
                                      reloadAll: reloadAllPriceKinds)
            }) { s, now in s.price = PriceService.apply(quote, to: s.price, now: now) }
            return s.price
        }
        priceFetch = task
        defer { priceFetch = nil; priceFetchExcluding = [] }
        return try await task.value
    }

    static func priceReloadKinds(before: PriceCache, after: PriceCache, excluding: Set<String>,
                                 reloadAll: Bool = false) -> [String] {
        let kinds: [String]
        if before.effective != after.effective || (reloadAll && before != after) { kinds = ChangeSet.price.widgetKinds }
        else if before != after { kinds = [WidgetKinds.dayRate] }
        else { kinds = [] }
        return kinds.filter { !excluding.contains($0) }
    }

    // MARK: Export / import

    public func exportJSON() throws -> Data {
        let s = try load()
        return try VersoJSON.encoder().encode(VersoExport(exportedAt: clock(), book: s.book, threads: s.threads,
                                                          entries: s.entries, pulses: s.pulses))
    }

    public func importJSON(_ data: Data) throws {
        let x = try VersoJSON.decoder().decode(VersoExport.self, from: data)
        try commit(.all) { s, _ in
            s.book = x.book; s.threads = x.threads; s.entries = x.entries; s.pulses = x.pulses
        }
    }

    /// Erase everything (tests, or a fresh start from the Colophon).
    public func eraseAll() throws {
        try StoreLock.withLock(in: directory) {
            for f in [File.book, .threads, .entries, .pulses, .price, .snapshot] {
                try? FileManager.default.removeItem(at: url(f))
            }
        }
        reloader.reload(kinds: WidgetKinds.all)
    }
}
