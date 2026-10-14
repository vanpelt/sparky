import Foundation

public struct PriceQuote: Sendable, Hashable {
    public let price: Double
    public let timestamp: Date
    public let source: String
    public init(price: Double, timestamp: Date, source: String) {
        self.price = price; self.timestamp = timestamp; self.source = source
    }
}

public enum PriceError: Error, Sendable { case noPrice, outOfRange, http(Int) }

/// CRWV quote: StockAnalysis first, Yahoo chart as a concurrent fallback. Network is used for this GET only.
public struct PriceService: Sendable {
    public static let appTTL: TimeInterval = 5 * 60
    public static let widgetTTL: TimeInterval = 60 * 60

    public let ticker: String
    public let session: URLSession

    public init(ticker: String = "CRWV", session: URLSession = .shared) {
        self.ticker = ticker; self.session = session
    }

    public var stockAnalysisURL: URL { URL(string: "https://stockanalysis.com/api/quotes/s/\(ticker.lowercased())/")! }
    public var yahooURL: URL { URL(string: "https://query1.finance.yahoo.com/v8/finance/chart/\(ticker)?interval=5m&range=1d")! }

    /// Both sources fire concurrently; StockAnalysis wins if both succeed. nil if neither answered.
    public func fetch(timeout: TimeInterval = 8) async -> PriceQuote? {
        async let sa = get(stockAnalysisURL, timeout: timeout) { try Self.parseStockAnalysis($0, now: Date()) }
        async let yf = get(yahooURL, timeout: timeout) { try Self.parseYahoo($0, now: Date()) }
        let (a, b) = await (sa, yf)
        return a ?? b
    }

    func get(_ url: URL, timeout: TimeInterval, parse: @Sendable (Data) throws -> PriceQuote) async -> PriceQuote? {
        var req = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: timeout)
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        req.setValue("Mozilla/5.0 (iPhone) Verso/1.0", forHTTPHeaderField: "User-Agent")
        do {
            let (data, resp) = try await session.data(for: req)
            if let h = resp as? HTTPURLResponse, !(200..<300).contains(h.statusCode) { throw PriceError.http(h.statusCode) }
            return try parse(data)
        } catch { return nil }
    }

    static func validate(_ p: Double) throws -> Double {
        guard p.isFinite, p > 0, p < 100_000 else { throw PriceError.outOfRange }
        return p
    }

    /// `{ "data": { "p": 100.0, "ts": 1760000000000 } }`. `ts` may be ms or s; missing → now.
    public static func parseStockAnalysis(_ data: Data, now: Date = Date()) throws -> PriceQuote {
        struct Payload: Decodable { struct D: Decodable { let p: Double?; let ts: Double? }; let data: D? }
        let d = try JSONDecoder().decode(Payload.self, from: data)
        guard let p = d.data?.p else { throw PriceError.noPrice }
        let ts = d.data?.ts.map { Date(timeIntervalSince1970: $0 > 1e11 ? $0 / 1000 : $0) } ?? now
        return PriceQuote(price: try validate(p), timestamp: ts, source: "StockAnalysis")
    }

    /// `chart.result[0].meta.regularMarketPrice` / `regularMarketTime` (seconds).
    public static func parseYahoo(_ data: Data, now: Date = Date()) throws -> PriceQuote {
        struct Payload: Decodable {
            struct Chart: Decodable { let result: [R]? }
            struct R: Decodable { let meta: Meta }
            struct Meta: Decodable { let regularMarketPrice: Double?; let regularMarketTime: Double? }
            let chart: Chart
        }
        let d = try JSONDecoder().decode(Payload.self, from: data)
        guard let m = d.chart.result?.first?.meta, let p = m.regularMarketPrice else { throw PriceError.noPrice }
        let ts = m.regularMarketTime.map { Date(timeIntervalSince1970: $0) } ?? now
        return PriceQuote(price: try validate(p), timestamp: ts, source: "Yahoo")
    }

    /// Merge a fetch result into the cache: success replaces the quote; failure keeps it and marks it stale.
    public static func apply(_ quote: PriceQuote?, to cache: PriceCache, now: Date) -> PriceCache {
        var c = cache
        if let q = quote {
            c.price = q.price; c.timestamp = q.timestamp; c.source = q.source; c.fetchedAt = now; c.lastFailureAt = nil
        } else {
            c.lastFailureAt = now
        }
        return c
    }

    // MARK: Market hours

    /// US equity trading, extended hours included (04:00–20:00 New York, Monday–Friday). Exchange holidays
    /// are not modelled: on one, a widget fetches a few times for an unchanged quote, which is harmless.
    public static let sessionOpenHour = 4
    public static let sessionCloseHour = 20
    public static let marketCalendar: Calendar = {
        var c = Calendar(identifier: .gregorian)
        c.timeZone = TimeZone(identifier: "America/New_York") ?? .current
        return c
    }()

    static func isWeekday(_ d: Date, _ cal: Calendar) -> Bool {
        let w = cal.component(.weekday, from: d) // 1 Sun … 7 Sat
        return w != 1 && w != 7
    }

    public static func isSessionOpen(at now: Date, calendar cal: Calendar = marketCalendar) -> Bool {
        guard isWeekday(now, cal) else { return false }
        let h = cal.component(.hour, from: now)
        return h >= sessionOpenHour && h < sessionCloseHour
    }

    /// The most recent session close at or before `now`.
    public static func lastSessionClose(before now: Date, calendar cal: Calendar = marketCalendar) -> Date {
        var day = cal.startOfDay(for: now)
        for _ in 0..<8 {
            if isWeekday(day, cal), let close = cal.date(bySettingHour: sessionCloseHour, minute: 0, second: 0, of: day), close <= now {
                return close
            }
            day = cal.date(byAdding: .day, value: -1, to: day) ?? day.addingTimeInterval(-86_400)
        }
        return now.addingTimeInterval(-7 * 86_400)
    }

    /// The next session open after `now` (now itself if a session is open).
    public static func nextSessionOpen(after now: Date, calendar cal: Calendar = marketCalendar) -> Date {
        if isSessionOpen(at: now, calendar: cal) { return now }
        var day = cal.startOfDay(for: now)
        for _ in 0..<8 {
            if isWeekday(day, cal), let open = cal.date(bySettingHour: sessionOpenHour, minute: 0, second: 0, of: day), open > now {
                return open
            }
            day = cal.date(byAdding: .day, value: 1, to: day) ?? day.addingTimeInterval(86_400)
        }
        return now.addingTimeInterval(86_400)
    }

    /// Whether a quote fetched at `fetchedAt` can be out of date: a session is open now, or one closed since.
    /// Nights and weekends after a post-close fetch answer false, so widgets don't refetch a frozen quote.
    public static func marketMayHaveMoved(since fetchedAt: Date?, now: Date, calendar cal: Calendar = marketCalendar) -> Bool {
        guard let fetchedAt else { return true }
        if isSessionOpen(at: now, calendar: cal) { return true }
        return fetchedAt < lastSessionClose(before: now, calendar: cal)
    }
}
