import Foundation

/// price.json — the last good quote plus the manual override.
public struct PriceCache: Codable, Sendable, Hashable {
    public var version: Int = 1
    public var price: Double?
    /// When the quote was struck (exchange time).
    public var timestamp: Date?
    public var source: String?
    /// When we last fetched successfully.
    public var fetchedAt: Date?
    /// When the last fetch attempt failed (stale marker). Cleared on success.
    public var lastFailureAt: Date?
    public var overridePrice: Double?
    public var overrideEnabled: Bool = false

    public init(price: Double? = nil, timestamp: Date? = nil, source: String? = nil, fetchedAt: Date? = nil,
                lastFailureAt: Date? = nil, overridePrice: Double? = nil, overrideEnabled: Bool = false) {
        self.price = price; self.timestamp = timestamp; self.source = source; self.fetchedAt = fetchedAt
        self.lastFailureAt = lastFailureAt; self.overridePrice = overridePrice; self.overrideEnabled = overrideEnabled
    }

    /// px: the manual override if enabled, otherwise the live/cached quote.
    public var effective: Double? { overrideEnabled ? overridePrice : price }
    public var isManual: Bool { overrideEnabled && overridePrice != nil }

    /// Age of the quote (manual prices have no age).
    public func age(now: Date = Date()) -> TimeInterval? {
        guard !isManual, let t = timestamp ?? fetchedAt else { return nil }
        return max(0, now.timeIntervalSince(t))
    }

    /// Fresh = under 5 minutes old.
    public func isFresh(now: Date = Date()) -> Bool {
        guard let a = age(now: now) else { return isManual }
        return a < 5 * 60 && lastFailureAt == nil
    }

    /// Stale = more than one trading day old (approximated as 26h; weekend-aware), or the last fetch failed.
    public func isStale(now: Date = Date(), calendar: Calendar = .current) -> Bool {
        if isManual { return false }
        guard let a = age(now: now) else { return true }
        let weekday = calendar.component(.weekday, from: now) // 1 Sun … 7 Sat
        let allowance: TimeInterval = (weekday == 1 || weekday == 2) ? 3.2 * 86_400 : 26 * 3_600
        return a > allowance || lastFailureAt != nil
    }

    /// Whether the app (5 min) or widget (60 min) should refetch.
    public func needsRefresh(ttl: TimeInterval, now: Date = Date()) -> Bool {
        // Back off on failures too, so a dead network isn't hammered.
        guard let last = [fetchedAt, lastFailureAt].compactMap({ $0 }).max() else { return true }
        return now.timeIntervalSince(last) > ttl
    }

    /// "4 min", "3h", "2d" — for labels like "· 3h".
    public func ageLabel(now: Date = Date()) -> String {
        if isManual { return "manual" }
        guard let a = age(now: now) else { return "—" }
        switch a {
        case ..<60: return "now"
        case ..<3_600: return "\(Int(a / 60)) min"
        case ..<86_400: return "\(Int(a / 3_600))h"
        default: return "\(Int(a / 86_400))d"
        }
    }
}

extension PriceCache {
    /// Tolerant: a missing key takes its default, so a schema change never loses the quote.
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        version = try c.decodeIfPresent(Int.self, forKey: .version) ?? 1
        price = try c.decodeIfPresent(Double.self, forKey: .price)
        timestamp = try c.decodeIfPresent(Date.self, forKey: .timestamp)
        source = try c.decodeIfPresent(String.self, forKey: .source)
        fetchedAt = try c.decodeIfPresent(Date.self, forKey: .fetchedAt)
        lastFailureAt = try c.decodeIfPresent(Date.self, forKey: .lastFailureAt)
        overridePrice = try c.decodeIfPresent(Double.self, forKey: .overridePrice)
        overrideEnabled = try c.decodeIfPresent(Bool.self, forKey: .overrideEnabled) ?? false
    }
}
