import Foundation
import Testing
@testable import VersoCore

struct SeedDecodeTests {
    @Test func decodesExampleShapeWithDefaults() throws {
        let json = """
        { "version": 1,
          "grant": { "revestShares": 100000, "rsuShares": 100000, "rolloverShares": 1000, "rolloverVestDate": null,
                     "vestDayOfMonth": 20, "bookOneStart": null, "salaryAnnual": 200000, "bonusAnnual": 50000,
                     "taxRevest": 0.238, "revestSheltered": false, "taxOrdinary": 0.503 },
          "enoughLinePerDay": null }
        """
        let seed = try SeedFile.decode(Data(json.utf8))
        #expect(seed.grant.revestShares == 100_000)
        #expect(seed.grant.schedule == Tranche.standard)
        #expect(seed.grant.isSample == false)
        #expect(seed.enoughLinePerDay == nil)
    }

    @Test func toleratesMissingKeysAndDateOnlyStrings() throws {
        let json = #"{ "grant": { "revestShares": 10, "rsuShares": 20, "bookOneStart": "2025-03-01" } }"#
        let seed = try SeedFile.decode(Data(json.utf8))
        #expect(seed.version == 1)
        #expect(seed.grant.vestDayOfMonth == 20)
        #expect(seed.grant.taxOrdinary == 0.503)
        #expect(seed.grant.rolloverShares == 0)
        let start = try #require(seed.grant.bookOneStart)
        #expect(Calendar.current.dateComponents([.year, .month, .day], from: start) == DateComponents(year: 2025, month: 3, day: 1))
    }

    @Test func committedExampleFileDecodes() throws {
        // The committed example sits next to the tests' source tree.
        let url = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("Seed/grant.example.json")
        guard let data = try? Data(contentsOf: url) else { return } // not reachable on device; fine
        let seed = try SeedFile.decode(data)
        #expect(seed.grant.revestShares == 100_000)
    }

    @Test func roundTripsBookFile() throws {
        let book = SampleData.book
        let data = try VersoJSON.encoder().encode(book)
        let back = try VersoJSON.decoder().decode(BookFile.self, from: data)
        #expect(back.clauses.map(\.kind) == book.clauses.map(\.kind))
        #expect(back.grant?.revestShares == book.grant?.revestShares)
        #expect(abs((back.grant?.updatedAt.timeIntervalSince1970 ?? 0) - (book.grant?.updatedAt.timeIntervalSince1970 ?? 0)) < 1)
    }
}
