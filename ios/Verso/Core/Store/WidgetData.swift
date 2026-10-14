import Foundation
import os

/// What the widget extension renders from. The store's own files come first, so a widget builds
/// `Board(state:today:)` for each timeline instant: the same function, the same inputs and the same
/// price.json the app reads, and therefore the same figures. snapshot.json is only the fallback.
public enum WidgetSource: Sendable {
    /// The store's files. Figures are the app's, recomputed per instant (streaks, habit weeks, pulse and all).
    case state(VersoState)
    /// The store could not be read but snapshot.json could: the app's last word, with date and money
    /// figures recomputed per instant.
    case snapshot(VersoSnapshot)
    /// Neither decoded. Widgets must say "Open Verso to refresh", never fall back to sample figures.
    case unreadable(String)

    public var price: PriceCache? {
        switch self {
        case .state(let s): s.price
        case .snapshot(let s): s.price
        case .unreadable: nil
        }
    }

    public var hasGrant: Bool {
        switch self {
        case .state(let s): s.book.grant != nil
        case .snapshot(let s): s.grant != nil
        case .unreadable: false
        }
    }
}

public enum WidgetData {
    static let log = Logger(subsystem: "verso", category: "widgets")

    /// Read the store for a widget. Every failure is logged (publicly, so it shows in Console) and
    /// surfaces as `.snapshot` or `.unreadable`, never as an empty or sample state.
    public static func source(store: VersoStore) async -> WidgetSource {
        do {
            return .state(try await store.load())
        } catch {
            log.error("widget: store unreadable, trying snapshot.json: \(String(describing: error), privacy: .public)")
        }
        switch VersoStore.readSnapshotResult(directory: store.directory) {
        case .success(let snap?):
            return .snapshot(snap)
        case .success(nil):
            log.error("widget: store unreadable and no snapshot.json")
            return .unreadable("no snapshot")
        case .failure(let error):
            return .unreadable(String(describing: error))
        }
    }

    /// One board per instant, or nil for every instant when the source is unreadable.
    public static func boards(_ source: WidgetSource, at dates: [Date], calendar: Calendar = .current) -> [LiveBoard?] {
        switch source {
        case .state(let s): dates.map { LiveBoard(board: Board(state: s, today: $0, calendar: calendar)) }
        case .snapshot(let snap): dates.map { LiveBoard(snapshot: snap, at: $0, calendar: calendar) }
        case .unreadable: dates.map { _ in nil }
        }
    }

    /// Instants at which something visible changes: every local midnight for `days` (countdowns, habit weeks,
    /// streaks), every vest instant and date / money clause flip in that window.
    public static func timelineDates(_ source: WidgetSource, from now: Date, days: Int = 14,
                                     calendar: Calendar = .current) -> [Date] {
        switch source {
        case .state(let s):
            let snap = SnapshotBuilder.build(from: Board(state: s, today: now, calendar: calendar))
            return LiveBoard.timelineDates(snapshot: snap, from: now, days: days, calendar: calendar)
        case .snapshot(let snap):
            return LiveBoard.timelineDates(snapshot: snap, from: now, days: days, calendar: calendar)
        case .unreadable:
            return [now]
        }
    }

    /// Whether a widget should fetch the quote before building its timeline (§9.2, now for every money widget).
    public static func shouldRefreshPrice(_ source: WidgetSource, now: Date) -> Bool {
        guard source.hasGrant, let p = source.price, !p.overrideEnabled else { return false }
        return p.needsRefresh(ttl: PriceService.widgetTTL, now: now)
            && PriceService.marketMayHaveMoved(since: p.fetchedAt, now: now)
    }

    /// When a money widget should ask for a new timeline: hourly while the quote can move, at the next session
    /// open (at most 12h) while it can't, soon after a failed read.
    public static func reloadDate(_ source: WidgetSource, now: Date) -> Date {
        if case .unreadable = source { return now.addingTimeInterval(30 * 60) }
        let tracksQuote = source.hasGrant && !(source.price?.isManual ?? false)
        // A manual price or no grant: nothing outside the app can change, and the app reloads on every change.
        guard tracksQuote else { return now.addingTimeInterval(12 * 3_600) }
        if PriceService.isSessionOpen(at: now) { return now.addingTimeInterval(60 * 60) }
        return min(PriceService.nextSessionOpen(after: now), now.addingTimeInterval(12 * 3_600))
    }
}
