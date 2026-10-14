import Foundation

/// Builds snapshot.json for widgets, and recomputes date-derived values from it per timeline entry.
public enum SnapshotBuilder {
    public static func build(from board: Board) -> VersoSnapshot {
        let s = board.state
        let ctx = board.context
        let unmet = s.book.clauses.filter { board.clauseState($0) == .unmet && $0.role != .regardless }
            .sorted { ClauseEvaluator.closeness($0, ctx) > ClauseEvaluator.closeness($1, ctx) }
            .prefix(2)
            .map { ClauseLine(id: $0.id, text: $0.text, role: $0.role, state: .unmet, hint: board.clauseHint($0)) }
        var stored: [UUID: Bool] = [:]
        for c in s.book.clauses { stored[c.id] = ClauseEvaluator.isMet(c, ctx) }
        let summaries = board.threads.map { line in
            ThreadSummary(id: line.thread.id, name: line.thread.name, binding: line.thread.binding,
                          kindTag: line.thread.kind.tag, progress: line.progress.progress, label: line.progress.label,
                          periodLabel: line.progress.periodLabel, lastEntryAt: line.lastActivity,
                          status: line.momentum.status, pace: line.momentum.pace, logLabel: line.progress.logLabel,
                          defaultAmount: line.thread.defaultAmount, weekDots: line.progress.weekDots,
                          sortIndex: line.thread.sortIndex)
        }
        var titles: [Int: String] = [:]
        for c in s.book.chapters { if let t = c.title, !t.isEmpty { titles[c.trancheIndex] = t } }
        var snap = VersoSnapshot(
            generatedAt: board.today, grant: s.book.grant, price: s.price, stage: board.clarity.stage,
            clausesMet: board.clarity.met, clausesHolding: board.clarity.holding, clausesTotal: board.clarity.total,
            tickStates: board.clarity.tickStates, nearestClauses: Array(unmet), pulseState: board.pulse.state,
            pulseStateWeeks: board.pulse.stateWeeks, divergenceWeeks: board.pulse.divergenceWeeks,
            latestSentence: board.footerEpigraph, lastMarginaliaWeekOf: s.pulses.map(\.weekOf).max(),
            marginaliaWeekday: s.book.settings.marginaliaWeekday, marginaliaHour: s.book.settings.marginaliaHour,
            threads: summaries, enoughLine: s.book.settings.enoughLinePerDay,
            veil: VeilPolicy(veilByDefault: s.book.settings.veilByDefault,
                             showFiguresOnLockScreen: s.book.settings.showFiguresOnLockScreen),
            mode: s.book.state.mode, clauses: s.book.clauses, storedMet: stored, chapterTitles: titles)
        snap.closedAt = s.book.state.closedAt
        snap.ticker = s.book.settings.ticker
        snap.dayRate = s.book.settings.dayRate
        snap.pulseLine = board.pulse.headline
        return snap
    }
}

/// What a widget needs at one timeline instant, recomputed from the snapshot.
public struct LiveBoard: Sendable {
    public let date: Date
    public let snapshot: VersoSnapshot
    /// Money figures; nil without a grant or a price.
    public let ledger: LedgerResult?
    /// Dates and countdowns only (computed even without a price — never read its dollars).
    public let schedule: LedgerResult?
    public let clarity: ClarityResult

    /// The widget's primary path: the app's own `Board` for this instant, so every figure (money, dates,
    /// clauses, streaks, stage) is computed by exactly the code the app renders with. `snapshot` is the
    /// snapshot that board would write, for the display-only fields (titles, veil, ticker, epigraph).
    public init(board: Board) {
        date = board.today
        snapshot = SnapshotBuilder.build(from: board)
        ledger = board.ledger
        schedule = board.ledger ?? board.state.book.grant.map {
            Ledger.compute(grant: $0, price: 0, today: board.today, calendar: board.calendar)
        }
        clarity = board.clarity
    }

    /// The fallback path, when only snapshot.json can be read: date and money clauses re-evaluated live,
    /// thread / pulse / manual clauses frozen at `generatedAt`.
    public init(snapshot: VersoSnapshot, at date: Date, calendar: Calendar = .current) {
        self.date = date; self.snapshot = snapshot
        if let g = snapshot.grant, let px = snapshot.price.effective {
            ledger = Ledger.compute(grant: g, price: px, today: date, calendar: calendar, enoughLine: snapshot.enoughLine)
        } else { ledger = nil }
        schedule = ledger ?? snapshot.grant.map { Ledger.compute(grant: $0, price: 0, today: date, calendar: calendar) }
        // Pulse/thread/manual clauses come from storedMet; date & money kinds are re-evaluated live.
        let ctx = ClauseContext(ledger: ledger, enoughLine: snapshot.enoughLine, threads: [], entries: [],
                                pulse: PulseAnalysis.analyze([]), price: snapshot.price.effective, today: date,
                                calendar: calendar, storedMet: snapshot.storedMet)
        let c = Clarity.evaluate(clauses: snapshot.clauses, context: ctx, mode: snapshot.mode)
        // Crisis via divergence needs the pulse: carry the snapshot's stage if it was crisis and live says lower.
        if c.stage.ordinal < snapshot.stage.ordinal && snapshot.stage == .crisis && snapshot.divergenceWeeks >= 6 {
            clarity = ClarityResult(stage: .crisis, total: c.total, met: c.met, holding: c.holding,
                                    tickStates: c.tickStates, regardlessCount: c.regardlessCount,
                                    regardlessMet: c.regardlessMet, requiredWaiting: c.requiredWaiting, states: c.states)
        } else { clarity = c }
    }

    public var stage: Stage { clarity.stage }
    public var next: NextVest? { schedule?.next }

    public var marginaliaDue: Bool {
        Board.marginaliaDue(lastWeekOf: snapshot.lastMarginaliaWeekOf, weekday: snapshot.marginaliaWeekday,
                            hour: Board.dueBannerHour, now: date, calendar: .current)
    }

    /// Timeline instants: each local midnight for `days`, plus vest days and clause flips in that window.
    public static func timelineDates(snapshot: VersoSnapshot, from now: Date, days: Int = 14,
                                     calendar: Calendar = .current) -> [Date] {
        var out: Set<Date> = [now]
        let start = calendar.startOfDay(for: now)
        for d in 1...max(1, days) { out.insert(Days.add(d, to: start, calendar: calendar)) }
        if let g = snapshot.grant {
            let end = Days.add(days, to: start, calendar: calendar)
            for d in Ledger.trancheDates(g, calendar: calendar) where d > now && d <= end { out.insert(d) }
            let ledger = snapshot.price.effective.map { Ledger.compute(grant: g, price: $0, today: now, calendar: calendar) }
            out.formUnion(ClauseEvaluator.flipDates(snapshot.clauses, ledger: ledger, today: now, horizon: days, calendar: calendar))
        }
        return out.filter { $0 >= now }.sorted()
    }
}
