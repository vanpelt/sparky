import WidgetKit
import SwiftUI
import VersoCore

/// Reads the shared App Group store. Every provider goes through here.
///
/// Widgets build from the store's own files (`WidgetData.source`), so each timeline instant is
/// `Board(state:today:)`: the app's figures, computed by the app's code from the app's price.json.
/// snapshot.json is only the fallback, and an unreadable store renders "Open Verso to refresh".
enum SnapshotSource {
    static func source() async -> WidgetSource { await WidgetData.source(store: VersoStore.shared) }

    /// Fetch the quote first if it is over 60 min old and the market may have moved (5s timeout), then read.
    /// Every money widget does this, not just Day Rate, so none of them depends on another one's reload.
    /// `kind` is the caller, which is building its timeline now; reloading it would just build another.
    static func sourceRefreshingPrice(for kind: String) async -> WidgetSource {
        let first = await source()
        guard WidgetData.shouldRefreshPrice(first, now: Date()) else { return first }
        _ = try? await VersoStore.shared.refreshPrice(ttl: PriceService.widgetTTL, timeout: 5, excluding: [kind])
        return await source()
    }

    static func entries(_ src: WidgetSource, at dates: [Date]) -> [VersoEntry] {
        zip(dates, WidgetData.boards(src, at: dates)).map { date, board in
            board.map(VersoEntry.init(live:)) ?? .unreadable(at: date)
        }
    }

    /// The add-widget sheet (and the first render) shows real figures when there are any. Sample figures appear
    /// only in the gallery preview of a store that has no grant; never for a configured or unreadable store.
    static func previewEntry(isPreview: Bool) async -> VersoEntry {
        let src = await source()
        if isPreview, case .state(let s) = src, s.book.grant == nil { return .sample }
        return entries(src, at: [Date()])[0]
    }

    /// Instants at which something visible changes: each local midnight for 14 days, every vest instant and
    /// date-clause flip in that window, plus the next Marginalia-due hour.
    static func dates(_ src: WidgetSource, from now: Date, days: Int = 14) -> [Date] {
        var out = Set(WidgetData.timelineDates(src, from: now, days: days))
        if let weekday = marginaliaWeekday(src), let due = nextMarginaliaInstant(weekday: weekday, after: now),
           due < now.addingTimeInterval(Double(days) * 86_400) {
            out.insert(due)
        }
        return out.sorted()
    }

    static func marginaliaWeekday(_ src: WidgetSource) -> Int? {
        switch src {
        case .state(let s): s.book.settings.marginaliaWeekday
        case .snapshot(let s): s.marginaliaWeekday
        case .unreadable: nil
        }
    }

    static func nextMarginaliaInstant(weekday: Int, after now: Date, calendar: Calendar = .current) -> Date? {
        calendar.nextDate(after: now, matching: DateComponents(hour: Board.dueBannerHour, minute: 0, weekday: weekday),
                          matchingPolicy: .nextTime)
    }

    static func nextMidnight(after now: Date, calendar: Calendar = .current) -> Date {
        calendar.startOfDay(for: calendar.date(byAdding: .day, value: 1, to: now) ?? now.addingTimeInterval(86_400))
    }
}

/// Next Chapter, Day Rate and Turning Point. Each refreshes the quote itself (at most hourly, only while the
/// market may have moved) and asks for a new timeline hourly while the market is open, so no widget's dollars
/// depend on another widget's budgeted reload. The timeline still covers 14 days of midnights, vests and clause
/// flips, so a deferred reload never leaves a stale countdown. `priceAgeEntries` (Day Rate) adds entries every
/// 15 min while open / hourly while closed so its "CRWV $100 · 12 min" age label moves.
struct LiveProvider: TimelineProvider {
    let kind: String
    var priceAgeEntries = false

    func placeholder(in context: Context) -> VersoEntry { .placeholder }

    func getSnapshot(in context: Context, completion: @escaping (VersoEntry) -> Void) {
        nonisolated(unsafe) let completion = completion
        let isPreview = context.isPreview
        Task { completion(await SnapshotSource.previewEntry(isPreview: isPreview)) }
    }

    func getTimeline(in context: Context, completion: @escaping (Timeline<VersoEntry>) -> Void) {
        // WidgetKit's completion isn't Sendable; it is called exactly once, from this task.
        nonisolated(unsafe) let completion = completion
        let kind = kind, priceAgeEntries = priceAgeEntries
        Task {
            let src = await SnapshotSource.sourceRefreshingPrice(for: kind)
            let now = Date()
            let reloadAt = WidgetData.reloadDate(src, now: now)
            var dates = Set(SnapshotSource.dates(src, from: now))
            if priceAgeEntries, src.hasGrant {
                let step: TimeInterval = PriceService.isSessionOpen(at: now) ? 15 * 60 : 3_600
                for t in stride(from: 0, to: reloadAt.timeIntervalSince(now), by: step) {
                    dates.insert(now.addingTimeInterval(t))
                }
            }
            let entries = SnapshotSource.entries(src, at: dates.sorted())
            completion(Timeline(entries: entries, policy: .after(reloadAt)))
        }
    }
}

/// Thread: recomputes thread summaries from the full store at each instant, so habit weeks roll over on
/// Monday and "last 3d ago" stays true without the app running.
struct ThreadProvider: AppIntentTimelineProvider {
    func placeholder(in context: Context) -> ThreadEntry { .sample }

    func snapshot(for configuration: SelectThreadIntent, in context: Context) async -> ThreadEntry {
        let now = Date()
        guard let threads = await Self.summaries(at: [now])[0] else { return .unreadable(at: now) }
        if context.isPreview && threads.isEmpty { return .sample }
        return ThreadEntry(date: now, threads: ThreadEntry.order(threads, featuring: configuration.thread?.id))
    }

    func timeline(for configuration: SelectThreadIntent, in context: Context) async -> Timeline<ThreadEntry> {
        let now = Date()
        var dates = [now]
        var d = now
        for _ in 0..<7 { d = SnapshotSource.nextMidnight(after: d); dates.append(d) }
        let per = await Self.summaries(at: dates)
        let entries = zip(dates, per).map { date, threads in
            threads.map { ThreadEntry(date: date, threads: ThreadEntry.order($0, featuring: configuration.thread?.id)) }
                ?? .unreadable(at: date)
        }
        // An unreadable store retries in 30 min rather than at the end of a week.
        let unreadable = per.first.map { $0 == nil } ?? true
        return Timeline(entries: entries, policy: unreadable ? .after(now.addingTimeInterval(30 * 60)) : .atEnd)
    }

    /// Thread summaries as of each date (nil = unreadable). Falls back to the stored snapshot if the store
    /// can't be read.
    static func summaries(at dates: [Date]) async -> [[ThreadSummary]?] {
        switch await SnapshotSource.source() {
        case .state(let state): dates.map { SnapshotBuilder.build(from: Board(state: state, today: $0)).threads }
        case .snapshot(let snap): dates.map { _ in snap.threads }
        case .unreadable: dates.map { _ in nil }
        }
    }
}
