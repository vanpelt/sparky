import Foundation
import Observation
import SwiftUI
import VersoCore

enum AppTab: Hashable { case contents, ledger, threads, turning }

enum ContentsRoute: Hashable {
    case chapter(Int)
    case thread(UUID)
    case colophon
    #if DEBUG
    case widgetGallery
    #endif
}

enum AppSheet: Identifiable, Hashable {
    case marginalia
    /// nil = pick a thread first.
    case log(UUID?)
    case newThread
    case editThread(UUID)
    case price
    case newClause
    case editClause(UUID)
    case closeBook
    case editEntry(UUID)

    var id: String {
        switch self {
        case .marginalia: "marginalia"
        case .log(let id): "log-\(id?.uuidString ?? "pick")"
        case .newThread: "newThread"
        case .editThread(let id): "editThread-\(id)"
        case .price: "price"
        case .newClause: "newClause"
        case .editClause(let id): "editClause-\(id)"
        case .closeBook: "closeBook"
        case .editEntry(let id): "editEntry-\(id)"
        }
    }
}

/// The UI's mirror of the store. Refreshed after every store mutation.
@Observable @MainActor
final class AppModel {
    let store: VersoStore
    private(set) var state: VersoState
    private(set) var board: Board
    private(set) var bootstrap: VersoStore.BootstrapResult?
    private(set) var loaded = false
    var lastError: String?
    /// Session-level: two-finger tap / eye toggle lifts the veil until the app is relaunched.
    var veilLifted = false
    /// What-if price on the Ledger (never persisted, never reaches widgets).
    var whatIfPrice: Double?
    var route: DeepLink?
    /// Overridable clock for previews.
    let now: () -> Date
    let calendar: Calendar

    // Navigation
    var tab: AppTab = .contents
    var contentsPath: [ContentsRoute] = []
    var threadsPath: [UUID] = []
    var sheet: AppSheet?
    var titleCard: TitleCardScript?
    /// The window is an open book (`Measure.layout(…).isSpread`): Contents and Threads show their list on the left
    /// page and what's open on the right. RootView keeps it current as the window folds and unfolds.
    var isSpread = false
    /// What is open or half-written on the pages (expanded lists, drafts, picks, the keyboard), held here so a
    /// fold, which rebuilds the pages, loses none of it.
    let pages = PageState()

    /// Open a Contents page. On a spread the right page turns to it (the path is replaced: the list stays on the
    /// left, so there is nothing to go back to); in one column it pushes, as it always has.
    func openContents(_ route: ContentsRoute) {
        if isSpread { contentsPath = [route] } else { contentsPath.append(route) }
    }

    /// Open a thread's scenes, the same way: the right page on a spread, a push in one column.
    func openThread(_ id: UUID) {
        if isSpread { threadsPath = [id] } else { threadsPath.append(id) }
    }

    /// Bumped per thread on every log: drives the pen-stroke underline and the selection haptic.
    var logTicks: [UUID: Int] = [:]
    var logCount = 0
    /// Bumped when a count target / milestone completes (success haptic).
    var successCount = 0
    var stampCount = 0

    init(store: VersoStore = .shared, initial: VersoState? = nil, now: @escaping () -> Date = { Date() },
         calendar: Calendar = .current) {
        self.store = store; self.now = now; self.calendar = calendar
        let s = initial ?? VersoState(book: BookFile(state: BookState(firstLaunchAt: now())))
        state = s
        board = Board(state: s, today: now(), calendar: calendar)
        loaded = initial != nil
    }

    var veiled: Bool { state.book.settings.veilByDefault && !veilLifted }
    var isRecto: Bool { state.book.state.mode == .recto }
    var needsOnboarding: Bool {
        #if DEBUG
        if DebugLaunch.skipOnboarding { return false }
        #endif
        return loaded && state.book.state.onboardedAt == nil
    }

    /// The Ledger's board, honouring the what-if price.
    var ledgerBoard: Board {
        whatIfPrice.map { Board(state: state, today: now(), calendar: calendar, priceOverride: $0) } ?? board
    }

    /// DEBUG: `-versoSample YES` runs on a throwaway store filled with SampleData (fake figures only).
    private(set) var isSampleStore = false
    /// DEBUG: `-versoSeededSample YES` is the same throwaway store, but with the REAL bundled seed as the grant
    /// (SampleData supplies only threads, entries, clauses and pulses) and a live price. Never touches the real store.
    private(set) var isSeededSample = false
    /// DEBUG: `-versoDayOne YES` is what "Start plotting" leaves behind, on a throwaway store: the bundled seed
    /// (or no grant), the four default threads with no lines, the one onboarding clause, no Marginalia.
    private(set) var isDayOne = false

    static func launch() -> AppModel {
        #if DEBUG
        if UserDefaults.standard.bool(forKey: "versoSample") {
            let dir = FileManager.default.temporaryDirectory.appendingPathComponent("verso-sample", isDirectory: true)
            try? FileManager.default.removeItem(at: dir)
            // The sample state has a grant, so bootstrap never imports the local seed into it.
            let m = AppModel(store: VersoStore(directory: dir, reloader: NoWidgetReloader()))
            m.isSampleStore = true
            return m
        }
        if UserDefaults.standard.bool(forKey: "versoFirstRun") {
            // A fresh install's empty store, every launch, without uninstalling (the App Group outlives the app).
            let dir = FileManager.default.temporaryDirectory.appendingPathComponent("verso-first-run", isDirectory: true)
            try? FileManager.default.removeItem(at: dir)
            return AppModel(store: VersoStore(directory: dir, reloader: NoWidgetReloader()))
        }
        if UserDefaults.standard.bool(forKey: "versoDayOne") {
            let dir = FileManager.default.temporaryDirectory.appendingPathComponent("verso-day-one", isDirectory: true)
            try? FileManager.default.removeItem(at: dir)
            let m = AppModel(store: VersoStore(directory: dir, reloader: NoWidgetReloader()))
            m.isDayOne = true
            return m
        }
        if UserDefaults.standard.bool(forKey: "versoSeededSample") {
            let dir = FileManager.default.temporaryDirectory.appendingPathComponent("verso-seeded-sample", isDirectory: true)
            try? FileManager.default.removeItem(at: dir)
            let m = AppModel(store: VersoStore(directory: dir, reloader: NoWidgetReloader()))
            m.isSeededSample = true
            return m
        }
        #endif
        return AppModel()
    }

    func start() async {
        if isSampleStore {
            let s = SampleData.state
            let x = VersoExport(book: s.book, threads: s.threads, entries: s.entries, pulses: s.pulses)
            if let data = try? VersoJSON.encoder().encode(x) {
                try? await store.importJSON(data)
                try? await store.setPrice { $0 = SampleData.priceCache; $0.fetchedAt = Date(); $0.timestamp = Date() }
            }
        }
        if isSeededSample {
            var s = SampleData.state
            s.book.settings.enoughLinePerDay = nil
            s.book.grant = nil // bootstrap imports the bundled local seed into the empty grant
            let x = VersoExport(book: s.book, threads: s.threads, entries: s.entries, pulses: s.pulses)
            if let data = try? VersoJSON.encoder().encode(x) { try? await store.importJSON(data) }
        }
        do { bootstrap = try await store.bootstrap() } catch { lastError = "\(error)" }
        #if DEBUG
        if isDayOne {
            let threads = PlotThread.presets().filter { OnboardingView.defaultPicks.contains($0.name) }
            do {
                for t in threads { try await store.upsertThread(t) }
                try await store.upsertClause(Clause(text: "the script is drafted and the restlessness hasn't faded",
                                                    kind: .manual(checked: false), role: .ordinary))
                try await store.updateBook(.all) { $0.state.onboardedAt = Date() }
            } catch { lastError = "\(error)" }
        }
        #endif
        await reload()
        loaded = true
        if state.book.state.onboardedAt == nil && bootstrap?.isFirstLaunch == true { titleCard = .firstLaunch }
        checkVestTitleCard()
        #if DEBUG
        // `simctl launch … -versoRoute verso://ledger` opens a route without the "Open in Verso?" prompt.
        if let r = UserDefaults.standard.string(forKey: "versoRoute"), let url = URL(string: r) {
            titleCard = nil
            route = DeepLink(url: url)
        }
        if UserDefaults.standard.bool(forKey: "versoUnveil") { veilLifted = true }
        if let screen = DebugLaunch.screen { openDebugScreen(screen) }
        #endif
        if !isSampleStore { await refreshPrice() }
        Notifications.reschedule(for: state, calendar: calendar)
    }

    func reload() async {
        do { apply(try await store.load()) } catch { lastError = "\(error)" }
    }

    /// The app's fetches reload every price widget (foreground reloads aren't budgeted), so no widget can be
    /// left on a quote the app has already moved past.
    func refreshPrice(force: Bool = false) async {
        do { _ = try await store.refreshPrice(force: force, reloadAllPriceKinds: true); await reload() }
        catch { lastError = "\(error)" }
    }

    // MARK: Keeping the widgets and the mirror current

    /// Scene became active. Re-read the store, then re-send every widget its timeline: WidgetKit doesn't budget
    /// reloads made while the app is in the foreground, and this is what retries any reload that was throttled
    /// or dropped while the app was away (an extension's price refresh, a +1 from the Home Screen). The work
    /// runs under a background-task assertion so a quick glance-and-leave still lands the price commit.
    func foreground() async {
        await reload()
        checkVestTitleCard()
        await withBackgroundTime("Verso widget refresh") {
            await self.pushWidgets()
            if !self.isSampleStore { await self.refreshPrice() }
        }
    }

    /// Rewrite snapshot.json and reload every widget kind.
    func pushWidgets() async {
        do { try await store.refreshSnapshot() } catch { lastError = "\(error)" }
    }

    /// The day rolled over (or the clock / time zone changed) while the app stayed open: rebuild the board so the
    /// countdown and day rate move at midnight like the widgets do, and re-send the widgets.
    func dayChanged() async {
        await reload()
        await pushWidgets()
    }

    /// Every minute while active: pick up anything another process wrote (the widget extension's price refresh,
    /// a +1 from a widget or Siri) and push it to every widget, roll the board at midnight, and keep the quote
    /// within the app's 5-minute TTL.
    func tick() async {
        if !calendar.isDate(board.today, inSameDayAs: now()) { await dayChanged(); return }
        if let disk = try? await store.load(), disk != state {
            apply(disk)
            await pushWidgets()
        }
        if !isSampleStore, state.price.needsRefresh(ttl: PriceService.appTTL, now: now()) { await refreshPrice() }
    }

    /// Keeps the app running long enough to finish `body` if the user leaves mid-way.
    private func withBackgroundTime(_ name: String, _ body: @MainActor () async -> Void) async {
        let task = BackgroundTime(name)
        await body()
        task.end()
    }

    /// Run a store call, then refresh the mirror.
    func perform(_ op: @escaping @Sendable (VersoStore) async throws -> Void) {
        Task {
            do { try await op(store) } catch { lastError = "\(error)" }
            await reload()
        }
    }

    /// Run a store call and wait for the mirror to refresh.
    func run(_ op: @escaping @Sendable (VersoStore) async throws -> Void) async {
        do { try await op(store) } catch { lastError = "\(error)" }
        await reload()
    }

    private func apply(_ s: VersoState) {
        state = s
        withAnimation(.smooth(duration: 0.5)) { board = Board(state: s, today: now(), calendar: calendar) }
    }

    // MARK: Logging (the critical path)

    /// Log a thread's default (or a given amount). Fires the pen stroke and haptics.
    func log(_ threadID: UUID, amount: Double? = nil, note: String? = nil, title: String? = nil, milestoneID: UUID? = nil) {
        let before = board.threads.first { $0.id == threadID }?.progress.targetReached ?? false
        Task {
            var saved = false
            do {
                saved = try await store.log(threadID: threadID, amount: amount, note: note, title: title, milestoneID: milestoneID) != nil
            } catch { lastError = "\(error)" }
            await reload()
            // The pen stroke and haptic only celebrate a line that was actually set down.
            guard saved else { return }
            logTicks[threadID, default: 0] += 1
            logCount += 1
            let line = board.threads.first { $0.id == threadID }
            let milestoneDone = line.map { if case .project = $0.thread.kind { return true }; return false } ?? false
            if (line?.progress.targetReached ?? false) && !before || milestoneDone { successCount += 1 }
        }
    }

    // MARK: Deep links

    func handle(_ link: DeepLink) {
        sheet = nil
        switch link {
        case .contents: tab = .contents; contentsPath = []
        case .ledger: tab = .ledger
        case .threads: tab = .threads; threadsPath = []
        case .turning: tab = .turning
        case .colophon: tab = .contents; contentsPath = [.colophon]
        case .marginalia: sheet = .marginalia
        case .chapter(let i): tab = .contents; contentsPath = [.chapter(i)]
        case .thread(let id): tab = .threads; threadsPath = [id]
        case .log(let id): sheet = .log(id)
        }
    }

    #if DEBUG
    func openDebugScreen(_ name: String) {
        titleCard = nil
        let firstThread = board.threads.first?.id
        let firstClause = state.book.clauses.first?.id
        switch name {
        case "gallery": tab = .contents; contentsPath = [.widgetGallery]
        case "price": tab = .ledger; sheet = .price
        case "quicklog": sheet = .log(nil)
        case "newThread": tab = .threads; sheet = .newThread
        case "editThread": tab = .threads; sheet = firstThread.map { .editThread($0) }
        case "newClause": tab = .turning; sheet = .newClause
        case "editClause": tab = .turning; sheet = firstClause.map { .editClause($0) }
        case "closeBook": tab = .turning; sheet = .closeBook
        case "marginalia": sheet = .marginalia
        case "thread": if let firstThread { handle(.thread(firstThread)) } // a sample thread's id isn't known up front
        default: break
        }
    }
    #endif

    // MARK: Title cards

    /// The first open on or after a vest date plays "END OF CHAPTER V ❦".
    func checkVestTitleCard() {
        guard !isRecto, let l = board.ledger ?? scheduleLedger else { return }
        guard let lastVested = l.tranches.last(where: \.isVested)?.index else { return }
        let seen = state.book.state.lastTitleCardVestIndex
        guard seen == nil || seen! < lastVested else { return }
        // Don't play for vests long before Verso was installed: only when the vest is within 7 days.
        let vestDate = l.tranches[lastVested].date
        let recent = Days.between(vestDate, now(), calendar: calendar) <= 7
        if recent && seen != nil && titleCard == nil {
            let nextTitle = l.next.map { state.book.chapter($0.index).title ?? "Chapter \($0.numeral)" }
            titleCard = .vest(ended: Roman.chapter(lastVested), next: nextTitle)
        }
        perform { store in try await store.updateBook { $0.state.lastTitleCardVestIndex = lastVested } }
    }

    /// Dates without dollars, for when there is a grant but no price yet.
    var scheduleLedger: LedgerResult? {
        state.book.grant.map { Ledger.compute(grant: $0, price: 0, today: now(), calendar: calendar) }
    }

    // MARK: Previews

    static var preview: AppModel {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("verso-preview", isDirectory: true)
        let m = AppModel(store: VersoStore(directory: dir, reloader: NoWidgetReloader()), initial: SampleData.state,
                         now: { SampleData.today }, calendar: SampleData.calendar)
        return m
    }
}

/// A UIApplication background-task assertion that ends once, whether `end()` or the expiration handler wins.
@MainActor
private final class BackgroundTime {
    private var id: UIBackgroundTaskIdentifier = .invalid

    init(_ name: String) {
        id = UIApplication.shared.beginBackgroundTask(withName: name) { [weak self] in
            MainActor.assumeIsolated { self?.end() }
        }
    }

    func end() {
        guard id != .invalid else { return }
        UIApplication.shared.endBackgroundTask(id)
        id = .invalid
    }
}
