import WidgetKit
import SwiftUI
import VersoCore

/// One timeline instant for Next Chapter, Day Rate and Turning Point: the stored snapshot plus everything
/// date-derived recomputed for `date`, so a widget stays right even if the app hasn't run for weeks.
struct VersoEntry: TimelineEntry {
    let date: Date
    let live: LiveBoard
    /// The store couldn't be read: views say "Open Verso to refresh" instead of any figure.
    var unreadable = false
    var snapshot: VersoSnapshot { live.snapshot }

    init(live: LiveBoard) { date = live.date; self.live = live }
    private init(date: Date, live: LiveBoard, unreadable: Bool) {
        self.date = date; self.live = live; self.unreadable = unreadable
    }

    static func make(_ snap: VersoSnapshot, at date: Date) -> VersoEntry {
        VersoEntry(live: LiveBoard(snapshot: snap, at: date))
    }
    static func unreadable(at date: Date) -> VersoEntry {
        VersoEntry(date: date, live: LiveBoard(snapshot: .empty, at: date), unreadable: true)
    }
    static var sample: VersoEntry { .make(SampleData.snapshot, at: SampleData.today) }
    static var empty: VersoEntry { .make(.empty, at: Date()) }

    /// WidgetKit's synchronous placeholder (always drawn redacted): the last snapshot when there is one, the
    /// sample's shapes only before anything was ever written, and the refresh notice if snapshot.json is broken.
    static var placeholder: VersoEntry {
        switch VersoStore.readSnapshotResult() {
        case .success(let s?): .make(s, at: Date())
        case .success(nil): .sample
        case .failure: .unreadable(at: Date())
        }
    }

    /// Whether dollars may show in this family. Accessory families (Lock Screen, StandBy, watch-like slots)
    /// obey `showFiguresOnLockScreen`; Home Screen families show figures behind `.privacySensitive()`.
    func moneyVisible(_ family: WidgetFamily) -> Bool {
        family.isAccessory ? snapshot.veil.showFiguresOnLockScreen : true
    }

    var hasGrant: Bool { snapshot.grant != nil }
    var isRecto: Bool { snapshot.mode == .recto }

    /// The tranche that vested today, if today is a vest day (the chapter that "ends today").
    var vestedToday: TrancheRow? {
        guard let s = live.schedule else { return nil }
        return s.tranches.first { $0.isVested && Calendar.current.isDate($0.date, inSameDayAs: date) }
    }

    /// Book Two page number in Recto mode: page 1 is the day he closed Book One.
    var bookTwoPage: Int? {
        guard isRecto, let c = snapshot.closedAt else { return nil }
        return max(1, Days.between(c, date) + 1)
    }

    /// The price, honestly: "CRWV $100 · 1h", with a stale flag when the quote is old or the last fetch failed.
    var priceLine: (text: String, stale: Bool) {
        let p = snapshot.price
        let ticker = snapshot.ticker ?? "CRWV"
        guard p.effective != nil else { return ("\(ticker) — no price yet", true) }
        let stale = p.isStale(now: date)
        let age = p.isManual ? "manual" : p.ageLabel(now: date)
        return ("\(ticker) \(MoneyFormat.price(p.effective)) · \(stale && !p.isManual ? "stale " : "")\(age)", stale)
    }
}

/// One timeline instant for the Thread widget. `threads.first` is the featured thread.
struct ThreadEntry: TimelineEntry {
    let date: Date
    let threads: [ThreadSummary]
    /// The store couldn't be read: the view says "Open Verso to refresh".
    var unreadable = false

    static func unreadable(at date: Date) -> ThreadEntry { ThreadEntry(date: date, threads: [], unreadable: true) }

    /// Orders `snap.threads`: the featured thread first (the configured one, else the most behind-pace),
    /// then the rest by sortIndex.
    static func make(_ snap: VersoSnapshot, at date: Date, featuring id: UUID? = nil) -> ThreadEntry {
        ThreadEntry(date: date, threads: order(snap.threads, featuring: id))
    }

    static func order(_ threads: [ThreadSummary], featuring id: UUID?) -> [ThreadSummary] {
        let featured = id.flatMap { c in threads.first { $0.id == c } } ?? threads.min { $0.pace < $1.pace }
        guard let featured else { return [] }
        return [featured] + threads.filter { $0.id != featured.id }.sorted { $0.sortIndex < $1.sortIndex }
    }

    static var sample: ThreadEntry { .make(SampleData.snapshot, at: SampleData.today, featuring: SampleData.serbianID) }
    static var empty: ThreadEntry { ThreadEntry(date: Date(), threads: []) }
}

/// widgetURL / Link targets. Every one of these is a route App/DeepLink.swift parses.
enum VersoWidgetURL {
    static let contents = URL(string: "verso://contents")!
    static let ledger = URL(string: "verso://ledger")!
    static let turning = URL(string: "verso://turning")!
    static let marginalia = URL(string: "verso://marginalia")!
    static let threads = URL(string: "verso://threads")!
    static func thread(_ id: UUID) -> URL { URL(string: "verso://thread/\(id.uuidString)")! }
}

extension WidgetFamily {
    var isAccessory: Bool {
        switch self {
        case .accessoryCircular, .accessoryRectangular, .accessoryInline: true
        default: false
        }
    }
}

extension ThreadSummary {
    /// Whether today's dot is already filled (a habit logged today).
    func doneToday(_ date: Date, calendar: Calendar = .current) -> Bool {
        guard kindTag == "HABIT", weekDots.count == 7 else { return false }
        let weekday = calendar.component(.weekday, from: date) // 1 Sun … 7 Sat
        return weekDots[(weekday + 5) % 7]
    }
    var initial: String { String(name.trimmingCharacters(in: .whitespaces).prefix(1)).uppercased() }
}
