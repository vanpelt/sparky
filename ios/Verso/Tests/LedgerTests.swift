import Foundation
import Testing
@testable import VersoCore

/// §5.9 test vectors. FAKE numbers only: REV = RSU = 100,000; ROLL = 1,000; px = $100;
/// S = $200k; B = $50k; τR = .238 (not sheltered); τO = .503; vd = 20; today = 2026-10-09 (New York).
struct LedgerTests {
    let cal = SampleData.calendar
    var grant: Grant {
        var g = SampleData.grant
        g.bookOneStart = nil
        return g
    }
    func day(_ y: Int, _ m: Int, _ d: Int) -> Date { SampleData.day(y, m, d) }
    func compute(_ g: Grant? = nil, price: Double = 100, on date: Date? = nil, enough: Double? = nil) -> LedgerResult {
        Ledger.compute(grant: g ?? grant, price: price, today: date ?? SampleData.today, calendar: cal, enoughLine: enough)
    }
    func close(_ a: Double, _ b: Double, _ tol: Double = 0.01) -> Bool { abs(a - b) <= tol }

    @Test func standardScheduleShape() {
        let s = Tranche.standard
        #expect(s.count == 13)
        #expect(s.first == Tranche(year: 2026, month: 5, fraction: 0.25))
        #expect(s.last == Tranche(year: 2029, month: 5, fraction: 0.0625))
        #expect(close(s.reduce(0) { $0 + $1.fraction }, 1, 1e-12))
        #expect(s.map(\.month) == [5, 8, 11, 2, 5, 8, 11, 2, 5, 8, 11, 2, 5])
    }

    @Test func defaultVestDayIsTwentieth() {
        let g = Grant(revestShares: 100, rsuShares: 100)
        #expect(g.vestDayOfMonth == 20)
        let dates = Ledger.trancheDates(g, calendar: cal)
        #expect(cal.dateComponents([.year, .month, .day], from: dates[2]) == DateComponents(year: 2026, month: 11, day: 20))
    }

    @Test func remainingAndFuel() {
        let l = compute()
        #expect(l.unvested.count == 11)
        #expect(l.unvested.first?.index == 2)
        #expect(close(l.fuel, 0.6875, 1e-12))
        #expect(close(l.k, 125_900))
        #expect(close(l.unvested[0].after, 786_875))
        #expect(close(l.remAfter, 8_655_625))
        #expect(close(l.remPre, 13_750_000))
        #expect(close(l.remShares, 137_500))
        #expect(close(l.remRevestShares, 68_750))
        #expect(close(l.remRsuShares, 68_750))
        // per pool after-tax: 68,750·100·.762 and 68,750·100·.497
        #expect(close(l.remRevestAfter, 5_238_750))
        #expect(close(l.remRsuAfter, 3_416_875))
    }

    @Test func dayRates() {
        let l = compute()
        #expect(l.daysToFinal == 954)
        #expect(close(l.wageRateAfter, 250_000 * 0.497 / 365, 1e-9))
        #expect(close(l.wagesAfter, 324_752.05, 0.01))
        #expect(close(l.dayRateAfter, 9_413.39, 0.01))
        #expect(close(l.dayRatePre, 15_097.93, 0.01))
        #expect(close(l.equityPerDay + l.payPerDay, l.dayRateAfter, 1e-6))
        #expect(close(LedgerResult.perWorkingDay(l.dayRateAfter), 9_413.39 * 365 / 261, 0.05))
    }

    @Test func nextVest() throws {
        let l = compute()
        let n = try #require(l.next)
        #expect(n.index == 2)
        #expect(n.numeral == "III")
        #expect(cal.dateComponents([.year, .month, .day], from: n.date) == DateComponents(year: 2026, month: 11, day: 20))
        #expect(n.daysToNext == 42)
        #expect(close(n.rate, 19_075.53, 0.01))
        #expect(cal.component(.day, from: n.launchWindow) == 21)
        // prev = Aug 20 (92 days to Nov 20); today is 50 days in.
        #expect(close(n.chapterProgress, 50.0 / 92.0, 1e-9))
    }

    @Test func ebbWeights() {
        let l = compute()
        #expect(close(l.rolloverValue, 49_700))
        #expect(close(l.chapters.first!.ebbWeight, 0.16493, 0.00001))
        #expect(UnitFraction.render(l.chapters.first!.ebbWeight) == "1\u{2044}6")
        #expect(close(l.chapters.last!.ebbWeight, 0.06225, 0.00001))
        // shareOfRemaining rises to 1 on the last chapter
        #expect(close(l.chapters.last!.shareOfRemaining, 1, 1e-12))
        #expect(l.chapters.map(\.ebbWeight) == l.chapters.map(\.ebbWeight).sorted(by: >))
        // first chapter's gap is today → Nov 20; later gaps are vest → vest
        #expect(l.chapters.first!.gap == 42)
        #expect(l.chapters[1].gap == 92) // Nov 20 → Feb 20
        #expect(close(l.chapters.first!.value, 786_875 + l.wageRateAfter * 42, 1e-6))
    }

    @Test func crossover() {
        let l = compute(enough: 3_000)
        let e = l.enough!
        guard case .price(let px) = e.crossover else { Issue.record("expected a crossover price"); return }
        #expect(close(px, 29.31, 0.005))
        #expect(close(e.margin, 9_413.39 - 3_000, 0.01))
        #expect(close(e.priceSensitivity, 86_556.25))
        // At the crossover price, the day rate equals the Enough Line.
        let atCross = compute(price: px, enough: 3_000)
        #expect(close(atCross.dayRateAfter, 3_000, 1e-6))
        // Pay alone clears a low line.
        #expect(compute(enough: 100).enough?.crossover == .payAloneClears)
    }

    @Test func sheltered() {
        var g = grant
        g.revestSheltered = true
        #expect(close(compute(g).unvested[0].after, 935_625))
    }

    @Test func onVestDay() {
        // Nov 20 itself: Nov '26 is vested (date-precise), N = 10.
        let l = compute(on: day(2026, 11, 20))
        #expect(l.tranches[2].isVested)
        #expect(l.unvested.count == 10)
        #expect(l.next?.index == 3)
        #expect(l.next?.daysToNext == 92)
        #expect(close(l.fuel, 0.625, 1e-12))
        // The day before, it's still chapter III with 1 day left.
        let eve = compute(on: day(2026, 11, 19))
        #expect(eve.next?.index == 2)
        #expect(eve.next?.daysToNext == 1)
    }

    @Test func beforeFirstVest() throws {
        let l = compute(on: day(2026, 3, 1))
        #expect(l.unvested.count == 13)
        let n = try #require(l.next)
        #expect(n.index == 0)
        // No bookOneStart: prevDate = d0 − 91 days.
        #expect(Days.between(n.prevDate, n.date, calendar: cal) == 91)
        #expect(close(l.fuel, 1, 1e-12))
        #expect(close(l.unvested[0].after, 0.25 * 100 * 125_900))
        // With bookOneStart, progress runs from it.
        var g = grant; g.bookOneStart = day(2025, 5, 20)
        let l2 = compute(g, on: day(2026, 3, 1))
        #expect(Days.between(l2.next!.prevDate, l2.next!.date, calendar: cal) == 365)
    }

    @Test func afterFinalVest() {
        let l = compute(on: day(2029, 6, 1))
        #expect(l.isComplete)
        #expect(l.next == nil)
        #expect(l.remAfter == 0)
        #expect(l.wagesAfter == 0)
        #expect(l.dayRateAfter == 0)
        #expect(l.chapters.isEmpty)
        #expect(l.lastPage.count == 1)
        #expect(l.lastPage[0].forfeit == 0)
        #expect(l.currentIndex == 12)
        // Final vest day itself is complete too.
        #expect(compute(on: day(2029, 5, 20)).isComplete)
        // Day before final: one chapter, D = 1.
        let eve = compute(on: day(2029, 5, 19))
        #expect(eve.unvested.count == 1)
        #expect(eve.daysToFinal == 1)
    }

    @Test func lastPage() {
        let l = compute()
        #expect(l.lastPage.count == 12)
        let now = l.lastPage[0]
        #expect(now.index == nil)
        #expect(close(now.forfeit, l.remAfter + l.wagesAfter, 1e-6))
        #expect(now.daysBack == 954)
        // Ending after chapter III (Nov 20): keep one tranche + 42 days' pay.
        let c3 = l.lastPage[1]
        #expect(c3.index == 2)
        #expect(close(c3.kept, 786_875 + l.wageRateAfter * 42, 1e-6))
        #expect(c3.daysBack == 954 - 42)
        #expect(close(c3.kept + c3.forfeit, l.remAfter + l.wagesAfter, 1e-6))
        // Ending after the last chapter forfeits nothing.
        #expect(close(l.lastPage.last!.forfeit, 0, 1e-6))
        #expect(l.lastPage.last!.daysBack == 0)
    }

    @Test func rolloverNeverWalksAway() {
        var g = grant; g.rolloverShares = 0
        let without = compute(g), with = compute()
        #expect(close(without.remAfter, with.remAfter, 1e-9))
        #expect(close(without.dayRateAfter, with.dayRateAfter, 1e-9))
        #expect(with.rolloverPending)          // dR nil and it's 2026
        #expect(!compute(on: day(2027, 1, 5)).rolloverPending)
        var dated = grant; dated.rolloverVestDate = day(2026, 12, 1)
        #expect(compute(dated).rolloverPending)
        #expect(!compute(dated, on: day(2026, 12, 2)).rolloverPending)
    }

    @Test func vestDayClampsToMonthLength() {
        var g = grant
        g.vestDayOfMonth = 31
        let dates = Ledger.trancheDates(g, calendar: cal)
        #expect(cal.component(.day, from: dates[3]) == 28) // Feb 2027
        #expect(cal.component(.day, from: dates[1]) == 31) // Aug
        #expect(cal.component(.day, from: dates[2]) == 30) // Nov
        // Per-tranche override wins.
        g.schedule[2].day = 1
        #expect(cal.component(.day, from: Ledger.trancheDates(g, calendar: cal)[2]) == 1)
    }
}

struct FormatTests {
    @Test func compactMatchesFutr() {
        #expect(MoneyFormat.compact(786_875) == "$787k")
        #expect(MoneyFormat.compact(8_655_625) == "$8.7M")
        #expect(MoneyFormat.compact(1_234_000_000) == "$1.23B")
        #expect(MoneyFormat.compact(950) == "$950")
        #expect(MoneyFormat.compact(-2_500) == "−$3k")
        #expect(MoneyFormat.short(9_461.2) == "$9.5k")
        #expect(MoneyFormat.full(9_461.2) == "$9,461")
        #expect(MoneyFormat.string(9_461.2, style: .full) == "≈ $9,461")
        #expect(MoneyFormat.string(9_461.2, veiled: true) == "·····")
        #expect(MoneyFormat.string(nil) == "—")
    }

    @Test func roman() {
        #expect((1...13).map(Roman.numeral) == ["I", "II", "III", "IV", "V", "VI", "VII", "VIII", "IX", "X", "XI", "XII", "XIII"])
        #expect(Roman.chapter(5) == "VI")
        #expect(Roman.spelled(6) == "Six")
    }

    @Test func fractions() {
        #expect(UnitFraction.render(0.16493) == "1⁄6")
        #expect(UnitFraction.render(0.125) == "1⁄8")
        #expect(UnitFraction.render(0.9) == "all")
        #expect(UnitFraction.render(0) == "0")
    }
}
