import Foundation

/// Everything derived from a `VersoState` at one instant: the four strands plus display figures.
/// Pure and cheap (a few hundred records) — recompute freely.
public struct Board: Sendable {
    public let today: Date
    public let calendar: Calendar
    public let state: VersoState
    /// nil when there is no grant or no price.
    public let ledger: LedgerResult?
    public let pulse: PulseAnalysis
    public let clarity: ClarityResult
    public let context: ClauseContext
    /// Active threads in sort order with their figures.
    public let threads: [ThreadLine]

    public struct ThreadLine: Sendable, Hashable, Identifiable {
        public let thread: PlotThread
        public let progress: ThreadProgress
        public let momentum: MomentumReading
        public let lastActivity: Date?
        public var id: UUID { thread.id }
    }

    public init(state: VersoState, today: Date = Date(), calendar: Calendar = .current, priceOverride: Double? = nil) {
        self.today = today; self.calendar = calendar; self.state = state
        let px = priceOverride ?? state.price.effective
        let E = state.book.settings.enoughLinePerDay
        if let g = state.book.grant, let px {
            ledger = Ledger.compute(grant: g, price: px, today: today, calendar: calendar, enoughLine: E)
        } else { ledger = nil }
        pulse = PulseAnalysis.analyze(state.pulses, today: today, calendar: calendar)
        // Clauses are always judged on the real effective price, never a what-if.
        let realLedger: LedgerResult? = priceOverride == nil ? ledger : {
            guard let g = state.book.grant, let p = state.price.effective else { return nil }
            return Ledger.compute(grant: g, price: p, today: today, calendar: calendar, enoughLine: E)
        }()
        context = ClauseContext(ledger: realLedger, enoughLine: E, threads: state.threads, entries: state.entries,
                                pulse: pulse, price: state.price.effective, today: today, calendar: calendar)
        clarity = Clarity.evaluate(clauses: state.book.clauses, context: context, mode: state.book.state.mode)
        threads = state.activeThreads.map { t in
            ThreadLine(thread: t,
                       progress: ThreadProgress.compute(thread: t, entries: state.entries, today: today, calendar: calendar),
                       momentum: Momentum.status(thread: t, entries: state.entries, today: today, calendar: calendar),
                       lastActivity: Momentum.lastActivity(thread: t, entries: state.entries))
        }
    }

    public var stage: Stage { clarity.stage }

    /// The weekly Reading paragraph.
    public func reading(veiled: Bool) -> String {
        Reading.compose(Reading.Input(
            clarity: clarity, clauses: state.book.clauses, ledger: context.ledger,
            enoughLine: state.book.settings.enoughLinePerDay, veiled: veiled, pulse: pulse,
            latestRestlessness: state.pulses.max(by: { $0.weekOf < $1.weekOf })?.restlessness,
            threads: threads.map { Reading.ThreadNote(name: $0.thread.name, reading: $0.momentum, lastActivity: $0.lastActivity) },
            turningPointSince: state.book.state.turningPointSince, today: today, calendar: calendar))
    }

    public func clauseState(_ clause: Clause) -> TickState { clarity.states[clause.id] ?? .unmet }
    public func clauseHint(_ clause: Clause) -> String { ClauseEvaluator.hint(clause, context) }

    /// The folio: "p. 412" — days since Book One start (or first launch); in Recto, days since closing.
    public var folio: Int {
        let b = state.book
        if b.state.mode == .recto, let closed = b.state.closedAt { return max(1, Days.between(closed, today, calendar: calendar) + 1) }
        let start = b.grant?.bookOneStart ?? b.state.firstLaunchAt
        return max(1, Days.between(start, today, calendar: calendar) + 1)
    }

    /// "BOOK ONE · CHAPTER VI" or "BOOK TWO · PAGE 12"
    public var runningHead: String {
        if state.book.state.mode == .recto { return "BOOK TWO · PAGE \(folio)" }
        guard let l = ledger else { return "BOOK ONE" }
        return "BOOK ONE · CHAPTER \(Roman.chapter(l.currentIndex))"
    }

    /// Marginalia is due on the chosen weekday (Sunday) after 17:00, or 7+ days since the last record.
    public var marginaliaDue: Bool {
        Board.marginaliaDue(lastWeekOf: state.pulses.map(\.weekOf).max(), weekday: state.book.settings.marginaliaWeekday,
                            hour: Board.dueBannerHour, now: today, calendar: calendar)
    }

    public static let dueBannerHour = 17

    public static func marginaliaDue(lastWeekOf: Date?, weekday: Int, hour: Int, now: Date, calendar: Calendar) -> Bool {
        let thisWeek = Days.weekStart(now, calendar: calendar)
        if let last = lastWeekOf, last >= thisWeek { return false }
        guard let last = lastWeekOf else { return true }
        if Days.between(last, now, calendar: calendar) >= 14 { return true } // last record's Monday ≥ 2 weeks back ⇒ ≥ 7 days since
        let wd = calendar.component(.weekday, from: now), h = calendar.component(.hour, from: now)
        return wd == weekday && h >= hour
    }

    /// The latest Marginalia sentence, else the epigraph.
    public var footerEpigraph: String? {
        state.pulses.sorted { $0.weekOf > $1.weekOf }.compactMap(\.sentence).first { !$0.isEmpty } ?? state.book.settings.epigraph
    }

    /// The board as a Marginalia snapshot.
    public var boardSnapshot: BoardSnapshot {
        BoardSnapshot(stage: clarity.stage, met: clarity.met, holding: clarity.holding, total: clarity.total,
                      dayRateAfter: ledger?.dayRateAfter, price: state.price.effective)
    }
}
