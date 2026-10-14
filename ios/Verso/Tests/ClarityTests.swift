import Foundation
import Testing
@testable import VersoCore

struct ClarityTests {
    let cal = SampleData.calendar
    let today = SampleData.today
    func day(_ y: Int, _ m: Int, _ d: Int) -> Date { SampleData.day(y, m, d, hour: 0) }

    func ctx(pulses: [Pulse] = [], ledger: LedgerResult? = nil, price: Double? = 100, enough: Double? = nil,
             threads: [PlotThread] = [], entries: [Entry] = []) -> ClauseContext {
        ClauseContext(ledger: ledger, enoughLine: enough, threads: threads, entries: entries,
                      pulse: PulseAnalysis.analyze(pulses, today: today, calendar: cal), price: price,
                      today: today, calendar: cal)
    }

    func manual(_ on: Bool, role: ClauseRole = .ordinary, since: Date? = nil) -> Clause {
        Clause(text: "x", kind: .manual(checked: on), role: role, createdAt: day(2026, 8, 1), metSince: on ? since : nil)
    }

    @Test func noClausesIsExposition() {
        let r = Clarity.evaluate(clauses: [], context: ctx(), mode: .bookOne)
        #expect(r.stage == .exposition)
        #expect(r.total == 0)
    }

    @Test func oneMetIsRisingAction() {
        let r = Clarity.evaluate(clauses: [manual(true, since: today), manual(false), manual(false), manual(false)],
                                 context: ctx(), mode: .bookOne)
        #expect(r.stage == .risingAction)
        #expect(r.met == 1)
        #expect(r.holding == 0)
        #expect(r.tickStates == [.met, .unmet, .unmet, .unmet])
    }

    @Test func holdingAfterFourteenDays() {
        let c13 = manual(true, since: Days.add(-13, to: today, calendar: cal))
        let c14 = manual(true, since: Days.add(-14, to: today, calendar: cal))
        #expect(ClauseEvaluator.state(c13, ctx()) == .met)
        #expect(ClauseEvaluator.state(c14, ctx()) == .holding)
    }

    @Test func turningPointNeedsRequiredHoldingAndQuorum() {
        let old = Days.add(-30, to: today, calendar: cal)
        // 4 clauses: q75 = 3. Three holding incl. the required one → Turning Point.
        let tp = [manual(true, role: .required, since: old), manual(true, since: old), manual(true, since: old), manual(false)]
        #expect(Clarity.evaluate(clauses: tp, context: ctx(), mode: .bookOne).stage == .turningPoint)
        // Same but the required one is unmet → The Crisis.
        let crisis = [manual(false, role: .required), manual(true, since: old), manual(true, since: old), manual(true, since: old)]
        #expect(Clarity.evaluate(clauses: crisis, context: ctx(), mode: .bookOne).stage == .crisis)
        // Quorum met but not yet holding → Rising Action.
        let fresh = [manual(true, since: today), manual(true, since: today), manual(true, since: today), manual(false)]
        #expect(Clarity.evaluate(clauses: fresh, context: ctx(), mode: .bookOne).stage == .risingAction)
    }

    @Test func regardlessJumps() {
        let cs = [manual(false), manual(false), manual(true, role: .regardless, since: today)]
        let r = Clarity.evaluate(clauses: cs, context: ctx(), mode: .bookOne)
        #expect(r.stage == .turningPoint)
        #expect(r.total == 2)           // regardless excluded from C
        #expect(r.regardlessMet)
    }

    @Test func rectoWins() {
        #expect(Clarity.evaluate(clauses: [], context: ctx(), mode: .recto).stage == .recto)
    }

    @Test func divergenceCrisis() {
        // 8 weeks of pull − energy ≥ 2 and no clauses met → Crisis.
        let pulses = (0..<8).map { i in
            Pulse(weekOf: Days.add(-7 * (7 - i), to: day(2026, 10, 5), calendar: cal), energy: 2, anchor: 3, pull: 5, restlessness: 4)
        }
        let r = Clarity.evaluate(clauses: [manual(false), manual(false)], context: ctx(pulses: pulses), mode: .bookOne)
        #expect(r.stage == .crisis)
    }

    @Test func clauseKinds() {
        let l = Ledger.compute(grant: SampleData.grant, price: 100, today: today, calendar: cal, enoughLine: 10_000)
        let c = ctx(ledger: l, price: 100, enough: 10_000)
        func met(_ k: ClauseKind) -> Bool { ClauseEvaluator.isMet(Clause(text: "", kind: k, createdAt: today), c) }
        #expect(met(.date(after: day(2026, 10, 9))))
        #expect(!met(.date(after: day(2026, 10, 10))))
        #expect(met(.tranche(index: 1)))
        #expect(!met(.tranche(index: 2)))
        #expect(met(.remainingBelow(9_000_000)))
        #expect(!met(.remainingBelow(8_000_000)))
        #expect(met(.dayRateBelowEnough))         // 9,461 < 10,000
        #expect(met(.priceBelow(101)) && !met(.priceBelow(100)))
        #expect(met(.priceAbove(99)) && !met(.priceAbove(100)))
    }

    @Test func metSinceBookkeeping() {
        let cs = [manual(true), manual(false)]
        let refreshed = ClauseEvaluator.refreshMetSince(cs, ctx())
        #expect(refreshed[0].metSince == cal.startOfDay(for: today))
        #expect(refreshed[1].metSince == nil)
        var unmet = refreshed[0]; unmet.kind = .manual(checked: false)
        #expect(ClauseEvaluator.refreshMetSince([unmet], ctx())[0].metSince == nil)
    }

    @Test func loosening() {
        #expect(ClauseKind.pulse(metric: .pulls, weeks: 4, ofLast: 8).loosens(.pulse(metric: .pulls, weeks: 6, ofLast: 8)))
        #expect(!ClauseKind.pulse(metric: .pulls, weeks: 10, ofLast: 12).loosens(.pulse(metric: .pulls, weeks: 6, ofLast: 12)))
        #expect(ClauseKind.priceBelow(40).loosens(.priceBelow(30)))
        #expect(ClauseKind.remainingBelow(5e6).loosens(.remainingBelow(4e6)))
    }

    @Test func readingAlwaysEndsInAQuestionAndNeverSaysGo() {
        let board = SampleData.board
        for veiled in [true, false] {
            let text = board.reading(veiled: veiled)
            #expect(text.hasSuffix("?"))
            #expect(!text.lowercased().contains(" go."))
            #expect(!text.lowercased().contains("you should"))
            #expect(text.contains("clauses are true"))
        }
        #expect(board.reading(veiled: true).contains("·····"))
        #expect(!board.reading(veiled: false).contains("·····"))
        #expect(board.reading(veiled: false).contains("1⁄6"))
    }

    @Test func sampleBoardIsCoherent() {
        let b = SampleData.board
        #expect(b.ledger?.next?.daysToNext == 42)
        #expect(b.runningHead == "BOOK ONE · CHAPTER III")
        #expect(b.threads.count == 4)
        #expect(b.clarity.total == 5)
        // The script milestone is done since 28 Sep (11 days) → met, not holding.
        let script = b.state.book.clauses[1]
        #expect(b.clauseState(script) == .met)
        #expect(b.stage == .risingAction)
    }
}

struct PulseSmoothingTests {
    let cal = SampleData.calendar
    func week(_ i: Int) -> Date { Days.add(7 * i, to: SampleData.day(2026, 6, 1, hour: 0), calendar: cal) }
    func p(_ i: Int, _ e: Int, _ a: Int, _ pl: Int, _ r: Int) -> Pulse {
        Pulse(weekOf: week(i), energy: e, anchor: a, pull: pl, restlessness: r)
    }

    @Test func unreadBelowThree() {
        #expect(PulseAnalysis.analyze([p(0, 3, 3, 3, 3), p(1, 3, 3, 3, 3)]).state == .unread)
    }

    @Test func ewmaValues() {
        // water: +2, +2, +2 → s = 2, 2, 2
        let a = PulseAnalysis.analyze([p(0, 2, 2, 4, 4), p(1, 2, 2, 4, 4), p(2, 2, 2, 4, 4)])
        #expect(a.points.map(\.smoothed) == [2, 2, 2])
        #expect(a.state == .pulls)
        #expect(a.stateWeeks == 3)
        // water 0 then +2: s1 = 0.35·2 + 0.65·0 = 0.7
        let b = PulseAnalysis.analyze([p(0, 3, 3, 3, 3), p(1, 2, 2, 4, 4)])
        #expect(abs(b.points[1].smoothed - 0.7) < 1e-12)
        #expect(b.crossings == [week(1)])
    }

    @Test func hysteresisNeedsThreeWeeks() {
        // Three pulling weeks establish .pulls; a swing to "holds" must repeat 3 recorded weeks to flip it.
        var ps = (0..<3).map { p($0, 1, 1, 5, 5) }                 // water +4 → s = 4
        ps += (3..<7).map { p($0, 5, 5, 1, 1) }                    // water −4 → s = 1.2, −0.62, −1.80, −2.57
        var a = PulseAnalysis.analyze(ps)
        #expect(a.points.map(\.candidate).suffix(4) == [.pulls, .even, .holds, .holds])
        #expect(a.state == .pulls)                                 // only two holds so far
        ps.append(p(7, 5, 5, 1, 1))
        a = PulseAnalysis.analyze(ps)
        #expect(a.state == .holds)
        #expect(a.stateWeeks == 3)
    }

    @Test func gapResetsSmoothing() {
        // A gap > 3 weeks resets s to the raw value.
        let a = PulseAnalysis.analyze([p(0, 1, 1, 5, 5), p(5, 5, 5, 1, 1)])
        #expect(a.points[1].smoothed == -4)
        // A 3-week gap does not.
        let b = PulseAnalysis.analyze([p(0, 1, 1, 5, 5), p(3, 5, 5, 1, 1)])
        #expect(abs(b.points[1].smoothed - (0.35 * -4 + 0.65 * 4)) < 1e-12)
    }

    @Test func divergenceAndBurnout() {
        let ps = (0..<8).map { p($0, 2, 3, 4, 4) }
        let a = PulseAnalysis.analyze(ps)
        #expect(a.divergenceWeeks == 8)
        #expect(a.burnout)
        #expect(a.pullsWeeks(ofLast: 8) >= 0)
    }

    @Test func anchorRanking() {
        let today = SampleData.today
        let ps = SampleData.pulses
        let a = PulseAnalysis.analyze(ps, today: today, calendar: cal)
        #expect(a.anchorRanking.first?.reason == "the money")
        #expect(a.anchorRanking.first?.count == 8)
    }
}

struct MomentumTests {
    let cal = SampleData.calendar
    let today = SampleData.today
    var threads: [PlotThread] { SampleData.threads }
    var entries: [Entry] { SampleData.entries }

    @Test func habit() {
        let t = threads[0]
        let p = ThreadProgress.compute(thread: t, entries: entries, today: today, calendar: cal)
        #expect(p.label == "3 / 4")
        #expect(p.periodLabel == "3/4 this week")
        #expect(p.logLabel == "Done today")
        #expect(p.weekDots == [true, true, false, true, false, false, false])
        #expect(p.streakWeeks == 5)
        let m = Momentum.status(thread: t, entries: entries, today: today, calendar: cal)
        #expect(m.pace == 1)
        #expect(m.status == .moving)
    }

    @Test func count() {
        let t = threads[1]
        let p = ThreadProgress.compute(thread: t, entries: entries, today: today, calendar: cal)
        #expect(p.label == "7 / 12")
        #expect(abs(p.progress - 7.0 / 12) < 1e-12)
        #expect(!p.targetReached)
    }

    @Test func practice() {
        let t = threads[3]
        let p = ThreadProgress.compute(thread: t, entries: entries, today: today, calendar: cal)
        #expect(p.label == "80 / 120 min")
        #expect(p.logLabel == "+20 min")
        let m = Momentum.status(thread: t, entries: entries, today: today, calendar: cal)
        // last 28 days: 80 + 120·3 (weeks −1..−3 fall inside; −4 is 28 days back from 7 Oct = 9 Sep, outside)
        #expect(m.pace > 0.75)
    }

    @Test func project() {
        let t = threads[2]
        let p = ThreadProgress.compute(thread: t, entries: entries, today: today, calendar: cal)
        #expect(p.label == "2 / 6")
        #expect(p.periodLabel == "next scene: Shot list")
        #expect(Momentum.status(thread: t, entries: entries, today: today, calendar: cal).pace == 1)
        // 61+ days after the last milestone with no entries: stalled and idle.
        let later = Days.add(80, to: today, calendar: cal)
        let m = Momentum.status(thread: t, entries: entries, today: later, calendar: cal)
        #expect(m.status == .stalled)
        #expect(m.isIdle)
    }
}
