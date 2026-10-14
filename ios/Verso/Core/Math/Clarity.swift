import Foundation

/// Output 1 (§6.5): the ordinal stage and the tick arc. Never a score, never a percentage.
public struct ClarityResult: Sendable, Hashable {
    public let stage: Stage
    /// Clauses excluding .regardless (C).
    public let total: Int
    public let met: Int
    public let holding: Int
    /// One tick per clause in C, in clause order.
    public let tickStates: [TickState]
    public let regardlessCount: Int
    public let regardlessMet: Bool
    /// Required clauses that are not yet holding, in order.
    public let requiredWaiting: [Clause]
    public let states: [UUID: TickState]

    public var tallyLabel: String { "\(met) of \(total) \(total == 1 ? "clause" : "clauses") true" }
    /// "Holding" = true for 14 days running (ClauseEvaluator.holdingDays); spelled out, never bare.
    public var holdingLabel: String { "\(holding) held \(ClauseEvaluator.holdingDays) days" }
}

public enum Clarity {
    public static func evaluate(clauses: [Clause], context ctx: ClauseContext, mode: BookState.Mode) -> ClarityResult {
        var states: [UUID: TickState] = [:]
        for c in clauses { states[c.id] = ClauseEvaluator.state(c, ctx) }

        let C = clauses.filter { $0.role != .regardless }
        let R = clauses.filter { $0.role == .regardless }
        let req = C.filter { $0.role == .required }
        let m = C.filter { states[$0.id] != .unmet }.count
        let h = C.filter { states[$0.id] == .holding }.count
        let regardlessMet = R.contains { states[$0.id] != .unmet }
        let q75 = Int((0.75 * Double(C.count)).rounded(.up))
        let q50 = Int((0.5 * Double(C.count)).rounded(.up))
        let allReqHolding = req.allSatisfy { states[$0.id] == .holding }
        let someReqUnmet = req.contains { states[$0.id] == .unmet }

        let stage: Stage
        if mode == .recto { stage = .recto }
        else if regardlessMet || (!C.isEmpty && allReqHolding && m >= q75 && h >= q75) { stage = .turningPoint }
        else if (!C.isEmpty && m >= q75 && someReqUnmet) || (ctx.pulse.divergenceWeeks >= 6 && m < q50) { stage = .crisis }
        else if m >= 1 { stage = .risingAction }
        else { stage = .exposition }

        return ClarityResult(stage: stage, total: C.count, met: m, holding: h,
                             tickStates: C.map { states[$0.id] ?? .unmet },
                             regardlessCount: R.count, regardlessMet: regardlessMet,
                             requiredWaiting: req.filter { states[$0.id] != .holding }, states: states)
    }
}
