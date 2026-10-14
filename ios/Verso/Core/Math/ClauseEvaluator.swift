import Foundation

/// Everything a clause can be judged against. Never includes a what-if price.
public struct ClauseContext: Sendable {
    public var ledger: LedgerResult?
    public var enoughLine: Double?
    public var threads: [PlotThread]
    public var entries: [Entry]
    public var pulse: PulseAnalysis
    /// The effective price (override or live).
    public var price: Double?
    public var today: Date
    public var calendar: Calendar
    /// When set (widgets), non-date kinds read their met-ness from here instead of re-deriving it.
    public var storedMet: [UUID: Bool]?

    public init(ledger: LedgerResult?, enoughLine: Double?, threads: [PlotThread], entries: [Entry],
                pulse: PulseAnalysis, price: Double?, today: Date, calendar: Calendar = .current,
                storedMet: [UUID: Bool]? = nil) {
        self.ledger = ledger; self.enoughLine = enoughLine; self.threads = threads; self.entries = entries
        self.pulse = pulse; self.price = price; self.today = today; self.calendar = calendar; self.storedMet = storedMet
    }
}

/// Strand 4 (§6.4).
public enum ClauseEvaluator {
    public static let holdingDays = 14

    /// True for kinds that depend only on the date, the grant and the price (recomputable in widgets).
    public static func isLive(_ kind: ClauseKind) -> Bool {
        switch kind {
        case .date, .tranche, .remainingBelow, .dayRateBelowEnough, .priceBelow, .priceAbove: true
        default: false
        }
    }

    public static func isMet(_ clause: Clause, _ ctx: ClauseContext) -> Bool {
        if let stored = ctx.storedMet, !isLive(clause.kind) { return stored[clause.id] ?? false }
        switch clause.kind {
        case .date(let after):
            return ctx.calendar.startOfDay(for: ctx.today) >= ctx.calendar.startOfDay(for: after)
        case .tranche(let index):
            return ctx.ledger?.tranches.first { $0.index == index }?.isVested ?? false
        case .remainingBelow(let amount):
            return ctx.ledger.map { $0.remAfter < amount } ?? false
        case .dayRateBelowEnough:
            guard let E = ctx.enoughLine, let l = ctx.ledger else { return false }
            return l.dayRateAfter < E
        case .priceBelow(let x): return ctx.price.map { $0 < x } ?? false
        case .priceAbove(let x): return ctx.price.map { $0 > x } ?? false
        case .threadTarget(let id):
            guard let t = ctx.threads.first(where: { $0.id == id }) else { return false }
            let p = ThreadProgress.compute(thread: t, entries: ctx.entries, today: ctx.today, calendar: ctx.calendar)
            switch t.kind {
            case .count, .project: return p.targetReached
            case .habit, .practice: return p.streakWeeks >= 6
            }
        case let .milestone(threadID, milestoneID):
            return ctx.threads.first { $0.id == threadID }?.milestones.first { $0.id == milestoneID }?.doneAt != nil
        case let .pulse(metric, weeks, ofLast):
            switch metric {
            case .pulls: return ctx.pulse.pullsWeeks(ofLast: ofLast) >= weeks
            case .divergence: return ctx.pulse.divergence(ofLast: ofLast) >= weeks
            }
        case .manual(let checked): return checked
        }
    }

    /// Met and met continuously for ≥ 14 days.
    public static func isHolding(_ clause: Clause, met: Bool, today: Date, calendar: Calendar = .current) -> Bool {
        guard met, let since = clause.metSince else { return false }
        return Days.between(since, today, calendar: calendar) >= holdingDays
    }

    public static func state(_ clause: Clause, _ ctx: ClauseContext) -> TickState {
        let met = isMet(clause, ctx)
        if isHolding(clause, met: met, today: ctx.today, calendar: ctx.calendar) { return .holding }
        return met ? .met : .unmet
    }

    /// Persisted bookkeeping: set `metSince` the first day a clause is met; clear it when unmet.
    public static func refreshMetSince(_ clauses: [Clause], _ ctx: ClauseContext) -> [Clause] {
        clauses.map { c in
            var c = c
            if isMet(c, ctx) { if c.metSince == nil { c.metSince = ctx.calendar.startOfDay(for: ctx.today) } }
            else { c.metSince = nil }
            return c
        }
    }

    /// Small-caps kind hint, e.g. "DATE · 1 Mar", "THREAD · Read 12 books 7/12", "PULSE 4 of 6 wks".
    public static func hint(_ clause: Clause, _ ctx: ClauseContext) -> String {
        switch clause.kind {
        case .date(let d): return "DATE · " + d.formatted(.dateTime.day().month(.abbreviated).year())
        case .tranche(let i): return "CHAPTER \(Roman.chapter(i))"
        case .remainingBelow(let a): return "REMAINING < " + MoneyFormat.compact(a)
        case .dayRateBelowEnough: return "DAY RATE < ENOUGH"
        case .priceBelow(let x): return "PRICE < " + MoneyFormat.price(x)
        case .priceAbove(let x): return "PRICE > " + MoneyFormat.price(x)
        case .threadTarget(let id):
            guard let t = ctx.threads.first(where: { $0.id == id }) else { return "THREAD" }
            let p = ThreadProgress.compute(thread: t, entries: ctx.entries, today: ctx.today, calendar: ctx.calendar)
            switch t.kind {
            case .habit, .practice: return "THREAD · \(t.name) \(p.streakWeeks)/6 wks"
            default: return "THREAD · \(t.name) \(p.label.replacingOccurrences(of: " ", with: ""))"
            }
        case let .milestone(tid, mid):
            let t = ctx.threads.first { $0.id == tid }
            return "SCENE · " + (t?.milestones.first { $0.id == mid }?.title ?? "—")
        case let .pulse(metric, weeks, ofLast):
            let got = metric == .pulls ? ctx.pulse.pullsWeeks(ofLast: ofLast) : ctx.pulse.divergence(ofLast: ofLast)
            return "PULSE \(got) of \(weeks) wks"
        case .manual: return "YOURS"
        }
    }

    /// How close an unmet clause is to being met, 0…1 (for "nearest clauses").
    public static func closeness(_ clause: Clause, _ ctx: ClauseContext) -> Double {
        if isMet(clause, ctx) { return 1 }
        let c = ctx.calendar
        switch clause.kind {
        case .date(let d):
            let total = max(1, Days.between(clause.createdAt, d, calendar: c))
            return min(1, max(0, Double(Days.between(clause.createdAt, ctx.today, calendar: c)) / Double(total)))
        case .tranche(let i):
            guard let l = ctx.ledger, let t = l.tranches.first(where: { $0.index == i }) else { return 0 }
            return min(1, max(0, 1 - Double(Days.between(ctx.today, t.date, calendar: c)) / 365))
        case .remainingBelow(let a):
            guard let l = ctx.ledger, l.remAfter > 0 else { return 0 }
            return min(1, a / l.remAfter)
        case .dayRateBelowEnough:
            guard let E = ctx.enoughLine, let l = ctx.ledger, l.dayRateAfter > 0 else { return 0 }
            return min(1, E / l.dayRateAfter)
        case .priceBelow(let x): return ctx.price.map { min(1, x / $0) } ?? 0
        case .priceAbove(let x): return ctx.price.map { min(1, $0 / x) } ?? 0
        case .threadTarget(let id):
            guard let t = ctx.threads.first(where: { $0.id == id }) else { return 0 }
            let p = ThreadProgress.compute(thread: t, entries: ctx.entries, today: ctx.today, calendar: c)
            switch t.kind {
            case .habit, .practice: return Double(p.streakWeeks) / 6
            default: return p.progress
            }
        case .milestone: return 0.5
        case let .pulse(metric, weeks, ofLast):
            let got = metric == .pulls ? ctx.pulse.pullsWeeks(ofLast: ofLast) : ctx.pulse.divergence(ofLast: ofLast)
            return weeks > 0 ? Double(got) / Double(weeks) : 0
        case .manual: return 0.25
        }
    }

    /// The next date within `horizon` days on which a date or tranche clause flips (widget timelines).
    public static func flipDates(_ clauses: [Clause], ledger: LedgerResult?, today: Date, horizon: Int = 14,
                                 calendar: Calendar = .current) -> [Date] {
        let start = calendar.startOfDay(for: today), end = Days.add(horizon, to: start, calendar: calendar)
        var out: [Date] = []
        for c in clauses {
            switch c.kind {
            case .date(let d):
                let day = calendar.startOfDay(for: d)
                if day > start && day <= end { out.append(day) }
                let hold = Days.add(holdingDays, to: day, calendar: calendar)
                if hold > start && hold <= end { out.append(hold) }
            case .tranche(let i):
                if let t = ledger?.tranches.first(where: { $0.index == i }), t.date > start, t.date <= end { out.append(t.date) }
            default: break
            }
            if let since = c.metSince {
                let hold = Days.add(holdingDays, to: since, calendar: calendar)
                if hold > start && hold <= end { out.append(hold) }
            }
        }
        return Array(Set(out)).sorted()
    }
}
