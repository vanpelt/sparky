import Foundation
import Testing
@testable import VersoCore

struct PriceParseTests {
    @Test func stockAnalysisMilliseconds() throws {
        let q = try PriceService.parseStockAnalysis(Data(#"{"status":200,"data":{"p":100.5,"ts":1760000000000}}"#.utf8))
        #expect(q.price == 100.5)
        #expect(q.timestamp == Date(timeIntervalSince1970: 1_760_000_000))
        #expect(q.source == "StockAnalysis")
    }

    @Test func stockAnalysisSecondsAndMissingTs() throws {
        let now = Date(timeIntervalSince1970: 42)
        let a = try PriceService.parseStockAnalysis(Data(#"{"data":{"p":100,"ts":1760000000}}"#.utf8), now: now)
        #expect(a.timestamp == Date(timeIntervalSince1970: 1_760_000_000))
        let b = try PriceService.parseStockAnalysis(Data(#"{"data":{"p":100}}"#.utf8), now: now)
        #expect(b.timestamp == now)
    }

    @Test func rejectsBadPrices() {
        #expect(throws: (any Error).self) { try PriceService.parseStockAnalysis(Data(#"{"data":{"p":0}}"#.utf8)) }
        #expect(throws: (any Error).self) { try PriceService.parseStockAnalysis(Data(#"{"data":{"p":250000}}"#.utf8)) }
        #expect(throws: (any Error).self) { try PriceService.parseStockAnalysis(Data(#"{"data":{}}"#.utf8)) }
    }

    @Test func yahoo() throws {
        let json = #"{"chart":{"result":[{"meta":{"regularMarketPrice":99.25,"regularMarketTime":1760000000}}],"error":null}}"#
        let q = try PriceService.parseYahoo(Data(json.utf8))
        #expect(q.price == 99.25)
        #expect(q.source == "Yahoo")
    }

    @Test func cacheMergeAndOverride() {
        let now = Date(timeIntervalSince1970: 1_000_000)
        var c = PriceService.apply(PriceQuote(price: 100, timestamp: now, source: "StockAnalysis"), to: PriceCache(), now: now)
        #expect(c.effective == 100)
        #expect(c.isFresh(now: now.addingTimeInterval(60)))
        #expect(!c.needsRefresh(ttl: 300, now: now.addingTimeInterval(60)))
        #expect(c.needsRefresh(ttl: 300, now: now.addingTimeInterval(400)))
        // Failure keeps the value, marks it stale.
        c = PriceService.apply(nil, to: c, now: now.addingTimeInterval(400))
        #expect(c.price == 100)
        #expect(c.lastFailureAt != nil)
        #expect(!c.isFresh(now: now.addingTimeInterval(401)))
        // Manual override wins.
        c.overridePrice = 50; c.overrideEnabled = true
        #expect(c.effective == 50)
        #expect(c.ageLabel() == "manual")
        c.overrideEnabled = false
        #expect(c.effective == 100)
    }
}
