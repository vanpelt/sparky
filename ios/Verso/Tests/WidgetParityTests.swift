import Foundation
import Testing
@testable import VersoCore

/// Widgets must show exactly what the app shows. The app renders `Board(state:today:)` from the store; the
/// widget extension reads the same store through `WidgetData.source` and renders `WidgetData.boards`. These
/// tests drive both paths from one on-disk store and compare every figure a widget family displays, across the
/// instants where they could drift (midnights, a vest, weeks later with no app launch). Fake numbers only.
struct WidgetParityTests {
    static let cal = SampleData.calendar
    static let today = SampleData.today

    // MARK: Fixtures

    func makeStore(_ reloader: any WidgetReloading = NoWidgetReloader(),
                   clock: @escaping @Sendable () -> Date = { WidgetParityTests.today }) -> (VersoStore, URL) {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("verso-parity-\(UUID().uuidString)", isDirectory: true)
        return (VersoStore(directory: dir, bundle: Bundle(for: RecordingReloader.self), reloader: reloader,
                           calendar: Self.cal, clock: clock), dir)
    }

    /// SampleData plus clauses of every live kind and a thread clause, so date, money and streak
    /// re-evaluation are all exercised.
    static func fakeState(price: PriceCache = SampleData.priceCache) -> VersoState {
        var s = SampleData.state
        s.price = price
        let created = SampleData.day(2026, 8, 1)
        s.book.clauses += [
            Clause(text: "after the vest", kind: .date(after: SampleData.day(2026, 11, 21, hour: 0)), role: .ordinary, createdAt: created),
            Clause(text: "below enough", kind: .dayRateBelowEnough, role: .ordinary, createdAt: created),
            Clause(text: "price under 150", kind: .priceBelow(150), role: .ordinary, createdAt: created),
            Clause(text: "price over 90", kind: .priceAbove(90), role: .regardless, createdAt: created),
            Clause(text: "remaining below 30M", kind: .remainingBelow(30_000_000), role: .ordinary, createdAt: created),
            Clause(text: "moving six weeks", kind: .threadTarget(threadID: SampleData.moveID), role: .ordinary, createdAt: created),
        ]
        return s
    }

    /// Fixed instants around the fake grant's next vest (Nov 20) and well past today with no app launch.
    static var instants: [(String, Date)] {
        let vest = cal.date(from: DateComponents(year: 2026, month: 11, day: 20, hour: 10))!
        let midnight = cal.startOfDay(for: vest)
        func days(_ n: Int) -> Date { cal.date(byAdding: .day, value: n, to: today)! }
        return [("now", today), ("tomorrow", days(1)), ("+3d Monday", days(3)), ("+7d", days(7)), ("+14d", days(14)),
                ("+21d", days(21)), ("23:59 before vest", midnight.addingTimeInterval(-60)), ("vest midnight", midnight),
                ("vest day", vest), ("day after vest", cal.date(byAdding: .day, value: 1, to: vest)!)]
    }

    // MARK: Every widget figure

    static func f(_ x: Double?) -> String { x.map { String(format: "%.6f", $0) } ?? "nil" }

    /// What the app shows, keyed by the widget figure it corresponds to. Built from the app's own types only.
    static func appFigures(_ b: Board) -> [String: String] {
        let schedule = b.ledger ?? b.state.book.grant.map { Ledger.compute(grant: $0, price: 0, today: b.today, calendar: b.calendar) }
        var m = common(ledger: b.ledger, schedule: schedule, clarity: b.clarity, price: b.state.price, at: b.today)
        let titles = Dictionary(uniqueKeysWithValues: b.state.book.chapters.compactMap { c in c.title.flatMap { $0.isEmpty ? nil : (c.trancheIndex, $0) } })
        m["nc.title"] = schedule?.next.flatMap { titles[$0.index] } ?? "nil"
        m["nc.titles"] = titles.sorted { $0.key < $1.key }.map { "\($0.key)=\($0.value)" }.joined(separator: ",")
        m["nc.recto"] = b.state.book.state.mode == .recto ? "\(b.folio)" : "nil"
        m["dr.enoughLine"] = f(b.state.book.settings.enoughLinePerDay)
        m["dr.ticker"] = b.state.book.settings.ticker
        m["tp.pulseLine"] = b.pulse.headline
        m["tp.epigraph"] = b.footerEpigraph ?? "nil"
        m["tp.marginaliaDue"] = "\(b.marginaliaDue)"
        m["tp.clauses"] = b.state.book.clauses.map { "\($0.text)|\($0.role.rawValue)|\(b.clauseState($0).rawValue)" }
            .joined(separator: ";")
        m["tp.nearest"] = b.state.book.clauses.filter { b.clauseState($0) == .unmet && $0.role != .regardless }
            .sorted { ClauseEvaluator.closeness($0, b.context) > ClauseEvaluator.closeness($1, b.context) }
            .prefix(2).map { "\($0.text)|\(b.clauseHint($0))" }.joined(separator: ";")
        m["th.lines"] = b.threads.map { t in
            "\(t.thread.name)|\(t.progress.label)|\(t.progress.periodLabel)|\(f(t.progress.progress))|\(t.progress.logLabel)|"
                + "\(t.momentum.status.rawValue)|\(f(t.momentum.pace))|\(t.progress.weekDots)"
        }.joined(separator: ";")
        m["veil"] = "\(b.state.book.settings.veilByDefault)/\(b.state.book.settings.showFiguresOnLockScreen)"
        return m
    }

    /// What a widget shows at the same instant, from the board the widget extension builds.
    static func widgetFigures(_ w: LiveBoard) -> [String: String] {
        let s = w.snapshot
        var m = common(ledger: w.ledger, schedule: w.schedule, clarity: w.clarity, price: s.price, at: w.date)
        m["nc.title"] = w.next.flatMap { s.chapterTitles[$0.index] } ?? "nil"
        m["nc.titles"] = s.chapterTitles.sorted { $0.key < $1.key }.map { "\($0.key)=\($0.value)" }.joined(separator: ",")
        m["nc.recto"] = s.mode == .recto ? s.closedAt.map { "\(max(1, Days.between($0, w.date, calendar: cal) + 1))" } ?? "nil" : "nil"
        m["dr.enoughLine"] = f(s.enoughLine)
        m["dr.ticker"] = s.ticker ?? "CRWV"
        m["tp.pulseLine"] = s.pulseLine ?? "nil"
        m["tp.epigraph"] = s.latestSentence ?? "nil"
        m["tp.clauses"] = s.clauses.map { "\($0.text)|\($0.role.rawValue)|\((w.clarity.states[$0.id] ?? .unmet).rawValue)" }
            .joined(separator: ";")
        m["tp.marginaliaDue"] = "\(Board.marginaliaDue(lastWeekOf: s.lastMarginaliaWeekOf, weekday: s.marginaliaWeekday, hour: Board.dueBannerHour, now: w.date, calendar: cal))"
        m["tp.nearest"] = s.nearestClauses.map { "\($0.text)|\($0.hint)" }.joined(separator: ";")
        m["th.lines"] = s.threads.map { t in
            "\(t.name)|\(t.label)|\(t.periodLabel)|\(f(t.progress))|\(t.logLabel)|\(t.status.rawValue)|\(f(t.pace))|\(t.weekDots)"
        }.joined(separator: ";")
        m["veil"] = "\(s.veil.veilByDefault)/\(s.veil.showFiguresOnLockScreen)"
        return m
    }

    /// Figures both sides derive from a ledger, a schedule, a clarity result and the price cache.
    static func common(ledger l: LedgerResult?, schedule s: LedgerResult?, clarity c: ClarityResult,
                       price p: PriceCache, at date: Date) -> [String: String] {
        var m: [String: String] = [:]
        // Next Chapter: every family.
        m["nc.index"] = s?.next.map { "\($0.index)" } ?? "nil"
        m["nc.numeral"] = s?.next?.numeral ?? "nil"
        m["nc.days"] = s?.next.map { "\($0.daysToNext)" } ?? "nil"
        m["nc.date"] = s?.next.map { "\($0.date.timeIntervalSince1970)" } ?? "nil"
        m["nc.progress"] = f(s?.next?.chapterProgress)
        m["nc.vestedToday"] = s?.tranches.first { $0.isVested && cal.isDate($0.date, inSameDayAs: date) }?.numeral ?? "nil"
        m["nc.after"] = f(l?.next?.after)
        m["nc.toc"] = (s?.chapters.prefix(3) ?? []).map { c in
            "\(c.numeral)@\(c.date.timeIntervalSince1970)=\(f(l.flatMap { $0.tranches.indices.contains(c.index) ? $0.tranches[c.index].after : nil }))/\(f(c.ebbWeight))"
        }.joined(separator: ",")
        // Next Chapter large: the whole contents table, read chapters included.
        m["nc.tocAll"] = (s?.tranches ?? []).map { t in
            "\(t.numeral)@\(t.date.timeIntervalSince1970)/\(t.isVested)/\(f(s?.chapter(t.index)?.ebbWeight))"
        }.joined(separator: ",")
        m["nc.finalDate"] = s.map { "\($0.finalDate.timeIntervalSince1970)" } ?? "nil"
        // Day Rate: every family.
        m["dr.rate"] = f(l?.dayRateAfter)
        m["dr.equity"] = f(l?.equityPerDay)
        m["dr.pay"] = f(l?.payPerDay)
        m["dr.chapterRate"] = f(l?.next?.rate)
        m["dr.walkAway"] = f(l?.walkAwayAfter)
        m["dr.ebb"] = f(l?.chapters.first?.ebbWeight)
        m["dr.chaptersLeft"] = s.map { "\($0.chaptersLeft)" } ?? "nil"
        m["dr.complete"] = l.map { "\($0.isComplete)" } ?? "nil"
        // Day Rate large: each chapter ahead on its own.
        m["dr.chapterRates"] = (l?.chapters ?? []).map { "\($0.numeral)=\(f($0.rate))" }.joined(separator: ",")
        m["dr.enough"] = l?.enough.map { "\(f($0.line))/\(f($0.margin))/\($0.isAbove)" } ?? "nil"
        m["dr.price"] = f(p.effective)
        m["dr.priceAge"] = "\(p.ageLabel(now: date))/\(p.isStale(now: date))/\(p.isManual)"
        // Turning Point: every family.
        m["tp.stage"] = c.stage.rawValue
        m["tp.counts"] = "\(c.met)/\(c.holding)/\(c.total)"
        m["tp.ticks"] = c.tickStates.map(\.rawValue).joined(separator: ",")
        m["tp.regardless"] = "\(c.regardlessCount)/\(c.regardlessMet)"
        m["tp.states"] = c.states.sorted { $0.key.uuidString < $1.key.uuidString }.map { "\($0.value.rawValue)" }.joined(separator: ",")
        return m
    }

    /// Put `state` on disk, then compare what the app would show against what the widget extension would show
    /// at every instant. Returns the mismatches (fake values only, so they are safe to print).
    func mismatches(_ state: VersoState) async throws -> [String] {
        let (store, _) = makeStore()
        let st = state
        try await store.mutate(.all) { s, _ in s = st }
        let disk = try await store.load()        // what AppModel.reload() reads
        let source = await WidgetData.source(store: store)   // what every widget provider reads
        guard case .state = source else { return ["widget source is not the store"] }
        let dates = Self.instants.map(\.1)
        let widgets = WidgetData.boards(source, at: dates, calendar: Self.cal)
        var out: [String] = []
        for ((name, t), w) in zip(Self.instants, widgets) {
            guard let w else { out.append("\(name): widget unreadable"); continue }
            let app = Self.appFigures(Board(state: disk, today: t, calendar: Self.cal))
            let wid = Self.widgetFigures(w)
            #expect(Set(app.keys) == Set(wid.keys))
            for k in app.keys.sorted() where app[k] != wid[k] {
                out.append("\(name) \(k): app=\(app[k]!) widget=\(wid[k] ?? "nil")")
            }
        }
        return out
    }

    @Test func everyWidgetFigureMatchesTheApp() async throws {
        let out = try await mismatches(Self.fakeState())
        #expect(out.isEmpty, "\(out.joined(separator: "\n"))")
    }

    @Test func shelteredOverrideNoEnoughLineMatches() async throws {
        var s = Self.fakeState(price: PriceCache(price: 100, overridePrice: 80, overrideEnabled: true))
        s.book.grant?.revestSheltered = true
        s.book.settings.enoughLinePerDay = nil
        s.book.settings.showFiguresOnLockScreen = true
        let out = try await mismatches(s)
        #expect(out.isEmpty, "\(out.joined(separator: "\n"))")
    }

    @Test func noPriceAndNoGrantMatch() async throws {
        var s = Self.fakeState(price: PriceCache())
        #expect(try await mismatches(s).isEmpty)
        s.book.grant = nil
        #expect(try await mismatches(s).isEmpty)
    }

    @Test func rectoMatches() async throws {
        var s = Self.fakeState()
        s.book.state.mode = .recto
        s.book.state.closedAt = SampleData.day(2026, 10, 1)
        #expect(try await mismatches(s).isEmpty)
    }

    /// The old widget froze thread clauses at the last commit, so a lapsed streak stayed met and was even
    /// promoted to holding. The widget now recomputes the streak at each instant, like the app.
    @Test func lapsedStreakLapsesInTheWidgetToo() async throws {
        var s = Self.fakeState()
        s.entries.append(Entry(threadID: SampleData.moveID, date: SampleData.day(2026, 10, 9, hour: 7)))
        let (store, _) = makeStore()
        let st = s
        try await store.mutate(.all) { x, _ in x = st }
        let disk = try await store.load()
        let clause = try #require(disk.book.clauses.first { $0.text == "moving six weeks" })
        let source = await WidgetData.source(store: store)
        let dates = [0, 7, 14, 21].map { Self.cal.date(byAdding: .day, value: $0, to: Self.today)! }
        for (t, w) in zip(dates, WidgetData.boards(source, at: dates, calendar: Self.cal)) {
            let app = Board(state: disk, today: t, calendar: Self.cal)
            #expect(w?.clarity.states[clause.id] == app.clauseState(clause))
            #expect(w?.clarity.met == app.clarity.met)
            #expect(w?.clarity.holding == app.clarity.holding)
            #expect(w?.stage == app.stage)
        }
    }

    // MARK: Freshness

    /// Another process (the widget extension) wrote a new quote and its reload of the other widgets was
    /// dropped. Whatever widget rebuilds next reads price.json itself, so it shows the app's figure.
    @Test func widgetReadsTheQuoteAnotherProcessWrote() async throws {
        let (app, dir) = makeStore()
        try await app.mutate(.all) { s, _ in s = Self.fakeState() }
        let ext = VersoStore(directory: dir, bundle: Bundle(for: RecordingReloader.self), reloader: NoWidgetReloader(),
                             calendar: Self.cal, clock: { Self.today })
        try await ext.setPrice { $0.price = 110; $0.fetchedAt = Self.today; $0.timestamp = Self.today }
        let source = await WidgetData.source(store: app)
        let w = try #require(WidgetData.boards(source, at: [Self.today], calendar: Self.cal)[0])
        let a = Board(state: try await app.load(), today: Self.today, calendar: Self.cal)
        #expect(w.ledger?.price == 110)
        #expect(w.ledger?.next?.after == a.ledger?.next?.after)
        #expect(w.ledger?.dayRateAfter == a.ledger?.dayRateAfter)
    }

    /// The app's foreground push reloads every kind, which retries any reload WidgetKit dropped.
    @Test func foregroundPushReloadsEveryKind() async throws {
        let r = RecordingReloader()
        let (store, _) = makeStore(r)
        try await store.refreshSnapshot()
        #expect(Set(r.kinds) == Set(WidgetKinds.all))
    }

    /// The app's own fetch reloads every price kind even when the quote didn't move (the extension may already
    /// have written it and had its own reload of Next Chapter / Turning Point throttled).
    @Test func appFetchReloadsEveryPriceKind() {
        let t0 = Self.today
        let p2 = PriceCache(price: 110, timestamp: t0, source: "x", fetchedAt: t0)
        let p2b = PriceCache(price: 110, timestamp: t0.addingTimeInterval(300), source: "x", fetchedAt: t0.addingTimeInterval(300))
        #expect(VersoStore.priceReloadKinds(before: p2, after: p2b, excluding: [], reloadAll: true)
            == [WidgetKinds.dayRate, WidgetKinds.nextChapter, WidgetKinds.turningPoint])
        // The budgeted extension path still only reloads what can look different.
        #expect(VersoStore.priceReloadKinds(before: p2, after: p2b, excluding: []) == [WidgetKinds.dayRate])
        #expect(VersoStore.priceReloadKinds(before: p2, after: p2b, excluding: [WidgetKinds.dayRate]) == [])
    }

    /// Every money widget refreshes the quote itself (not only Day Rate), and asks for a new timeline hourly
    /// while the market is open, so none depends on another widget's reload.
    @Test func moneyWidgetsRefreshThemselves() async throws {
        let (store, _) = makeStore()
        let old = PriceCache(price: 100, timestamp: Self.today.addingTimeInterval(-7_200), source: "x",
                             fetchedAt: Self.today.addingTimeInterval(-7_200))
        try await store.mutate(.all) { s, _ in s = Self.fakeState(price: old) }
        let src = await WidgetData.source(store: store)
        #expect(WidgetData.shouldRefreshPrice(src, now: Self.today))     // Friday noon, 2h-old quote
        #expect(WidgetData.reloadDate(src, now: Self.today) == Self.today.addingTimeInterval(3_600))
        // A fresh quote, a manual price or no grant: no fetch.
        try await store.setPrice { $0.fetchedAt = Self.today }
        #expect(!WidgetData.shouldRefreshPrice(await WidgetData.source(store: store), now: Self.today))
        try await store.setOverride(80, enabled: true)
        let manual = await WidgetData.source(store: store)
        #expect(!WidgetData.shouldRefreshPrice(manual, now: Self.today))
        #expect(WidgetData.reloadDate(manual, now: Self.today) > Self.today.addingTimeInterval(3_600))
    }

    /// 14 days of midnights (so every Monday's habit-week roll and streak lapse gets an entry) plus the vest.
    @Test func timelineCoversEveryMidnightForTwoWeeks() async throws {
        let (store, _) = makeStore()
        try await store.mutate(.all) { s, _ in s = Self.fakeState() }
        let dates = WidgetData.timelineDates(await WidgetData.source(store: store), from: Self.today, calendar: Self.cal)
        let start = Self.cal.startOfDay(for: Self.today)
        for d in 1...14 { #expect(dates.contains(Days.add(d, to: start, calendar: Self.cal))) }
        #expect(dates.first == Self.today)
    }

    // MARK: Unreadable store: loud and honest, never sample figures

    @Test func freshInstallIsEmptyNotSample() async throws {
        let (store, _) = makeStore()
        let src = await WidgetData.source(store: store)
        guard case .state(let s) = src else { Issue.record("expected the store"); return }
        #expect(s.book.grant == nil)
        #expect(WidgetData.boards(src, at: [Self.today])[0]?.ledger == nil)
    }

    @Test func brokenStoreFallsBackToSnapshot() async throws {
        let (store, dir) = makeStore()
        try await store.mutate(.all) { s, _ in s = Self.fakeState() }
        let before = Board(state: try await store.load(), today: Self.today, calendar: Self.cal)
        try Data("{ not json".utf8).write(to: dir.appendingPathComponent("book.json"))
        let src = await WidgetData.source(store: store)
        guard case .snapshot = src else { Issue.record("expected the snapshot fallback, got \(src)"); return }
        let w = try #require(WidgetData.boards(src, at: [Self.today], calendar: Self.cal)[0])
        #expect(w.ledger?.dayRateAfter == before.ledger?.dayRateAfter)
        #expect(w.next?.daysToNext == before.ledger?.next?.daysToNext)
        #expect(w.clarity.met == before.clarity.met)
    }

    @Test func brokenStoreAndSnapshotAreUnreadable() async throws {
        let (store, dir) = makeStore()
        try await store.mutate(.all) { s, _ in s = Self.fakeState() }
        try Data("{ not json".utf8).write(to: dir.appendingPathComponent("book.json"))
        try Data("[]".utf8).write(to: dir.appendingPathComponent("snapshot.json"))
        let src = await WidgetData.source(store: store)
        guard case .unreadable = src else { Issue.record("expected unreadable, got \(src)"); return }
        #expect(WidgetData.boards(src, at: [Self.today, Self.today.addingTimeInterval(86_400)]).allSatisfy { $0 == nil })
        #expect(!WidgetData.shouldRefreshPrice(src, now: Self.today))
        #expect(WidgetData.reloadDate(src, now: Self.today) == Self.today.addingTimeInterval(30 * 60))
        if case .failure = VersoStore.readSnapshotResult(directory: dir) {} else { Issue.record("decode error swallowed") }
    }

    /// A snapshot written by an older build (fields missing) still decodes, keeping its grant and price.
    @Test func olderSnapshotSchemaStillDecodes() throws {
        let full = try VersoJSON.encoder().encode(SampleData.snapshot)
        var obj = try #require(try JSONSerialization.jsonObject(with: full) as? [String: Any])
        for k in ["pulseStateWeeks", "marginaliaWeekday", "marginaliaHour", "storedMet", "chapterTitles", "version"] {
            obj.removeValue(forKey: k)
        }
        if var threads = obj["threads"] as? [[String: Any]] {
            for i in threads.indices { threads[i].removeValue(forKey: "pace"); threads[i].removeValue(forKey: "weekDots") }
            obj["threads"] = threads
        }
        if var price = obj["price"] as? [String: Any] { price.removeValue(forKey: "overrideEnabled"); obj["price"] = price }
        let snap = try VersoJSON.decoder().decode(VersoSnapshot.self, from: JSONSerialization.data(withJSONObject: obj))
        #expect(snap.grant == SampleData.snapshot.grant)
        #expect(snap.price.price == SampleData.snapshot.price.price)
        #expect(snap.threads.count == SampleData.snapshot.threads.count)
        #expect(snap.marginaliaWeekday == VersoSnapshot.empty.marginaliaWeekday)
    }

    // MARK: Every family has its own layout

    static var sourceRoot: URL { URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent() }

    /// The families `VersoFamilies.home` offers on the newest SDK, read from its source (the widget views aren't in
    /// VersoCore, so this test reads them rather than renders them).
    static func homeFamilies() throws -> [String] {
        let style = try String(contentsOf: sourceRoot.appendingPathComponent("Shared/WidgetViews/WidgetStyle.swift"), encoding: .utf8)
        let decl = try #require(style.range(of: "static var home"))
        // The first `return [...]` is the newest SDK's list (the `#available` branch).
        let list = try #require(style[decl.upperBound...].range(of: #"return \[[^\]]*\]"#, options: .regularExpression))
        return style[list].matches(of: try Regex(#"\.(system\w+)"#)).map { String($0.output[1].substring ?? "") }
    }

    /// Each `switch f {` / `switch family {` body in `text`, with its line number.
    static func familySwitches(_ text: String) -> [(line: Int, body: String)] {
        var out: [(Int, String)] = []
        var search = text.startIndex
        while let r = text.range(of: #"switch (f|family) \{"#, options: .regularExpression, range: search..<text.endIndex) {
            var depth = 1, i = r.upperBound, body = ""
            // Keep only the switch's own level, so a nested switch's cases can't stand in for this one's.
            while i < text.endIndex, depth > 0 {
                let ch = text[i]
                if ch == "{" { depth += 1 } else if ch == "}" { depth -= 1 } else if depth == 1 { body.append(ch) }
                i = text.index(after: i)
            }
            out.append((text[..<r.lowerBound].filter { $0 == "\n" }.count + 1, body))
            search = i
        }
        return out
    }

    /// A family missing from a view's switch falls to `default:`, which renders the small layout stretched. Every
    /// family the widgets offer must be named in every switch, and the configurations and the gallery must read
    /// the one shared list rather than their own copies.
    @Test func everyOfferedFamilyHasItsOwnBranch() throws {
        let families = try Self.homeFamilies()
        #expect(Set(families) == ["systemSmall", "systemMedium", "systemLarge", "systemExtraLargePortrait"])
        let views = Self.sourceRoot.appendingPathComponent("Shared/WidgetViews")
        let files = try FileManager.default.contentsOfDirectory(atPath: views.path).filter { $0.hasSuffix(".swift") }.sorted()
        var switches = 0
        for file in files {
            let text = try String(contentsOf: views.appendingPathComponent(file), encoding: .utf8)
            for (line, body) in Self.familySwitches(text) {
                switches += 1
                let named = Set(body.matches(of: try Regex(#"case\s+([^:]+):"#))
                    .flatMap { String($0.output[1].substring ?? "").matches(of: /\.(system\w+)/).map { String($0.output.1) } })
                for f in families where !named.contains(f) {
                    Issue.record("\(file):\(line) switch has no explicit .\(f) branch")
                }
            }
        }
        #expect(switches >= 11, "found only \(switches) family switches; did the scan break?")

        let widgets = Self.sourceRoot.appendingPathComponent("Widgets")
        for file in ["NextChapterWidget.swift", "DayRateWidget.swift", "TurningPointWidget.swift", "ThreadWidget.swift"] {
            let text = try String(contentsOf: widgets.appendingPathComponent(file), encoding: .utf8)
            #expect(text.contains(".supportedFamilies(VersoFamilies.supported("), "\(file) doesn't use VersoFamilies")
        }
        let gallery = try String(contentsOf: views.appendingPathComponent("WidgetGalleryView.swift"), encoding: .utf8)
        #expect(gallery.contains("VersoFamilies.supported("), "the gallery keeps its own family list")
        #expect(!gallery.contains("[.systemSmall"), "the gallery keeps its own family list")
    }
}
