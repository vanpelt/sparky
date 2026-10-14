import Foundation

public enum PulseState: String, Codable, Sendable, Hashable {
    case unread, holds, even, pulls

    public func copy(weeks: Int) -> String {
        switch self {
        case .unread: "Not enough pages yet."
        case .holds: "Book One still holds you."
        case .even: "Even. Nothing pulling hard either way."
        case .pulls: "Book Two is pulling, and has for \(weeks) \(weeks == 1 ? "week" : "weeks")."
        }
    }

    static func candidate(_ s: Double) -> PulseState {
        s <= -0.75 ? .holds : (s >= 0.75 ? .pulls : .even)
    }
}

public struct PulsePoint: Sendable, Hashable, Identifiable {
    public let weekOf: Date
    public let toward: Double
    public let away: Double
    public let water: Double
    public let smoothed: Double
    public let candidate: PulseState
    /// The displayed (hysteresis) state as of this record.
    public let displayed: PulseState
    public let divergent: Bool
    public var id: Date { weekOf }
}

/// Strand 2 (§6.2): EWMA(α 0.35) over recorded weeks, 3-week hysteresis, divergence, "the X", burnout.
public struct PulseAnalysis: Sendable, Hashable {
    public static let alpha = 0.35
    public static let pullThreshold = 0.75

    public let points: [PulsePoint]
    public let state: PulseState
    /// Consecutive trailing records whose candidate matches the displayed state.
    public let stateWeeks: Int
    /// Weeks of the last 8 records where pull − energy ≥ 2.
    public let divergenceWeeks: Int
    /// Every week where s crossed from ≤ 0 to > 0, oldest first.
    public let crossings: [Date]
    /// energy ≤ 2 and restlessness ≥ 4 in each of the last 3 records.
    public let burnout: Bool
    /// Anchor reasons from records in the last 8 weeks, most-named first.
    public let anchorRanking: [AnchorCount]
    /// The trailing candidate run when it disagrees with the displayed state (the hysteresis hasn't called it
    /// yet), e.g. two weeks of .pulls under a displayed .even. nil when they agree or nothing is displayed.
    public let leaning: PulseState?
    /// Length of that trailing run (0 when `leaning` is nil).
    public let leanWeeks: Int

    public struct AnchorCount: Sendable, Hashable { public let reason: String; public let count: Int }

    public var count: Int { points.count }

    /// Records a candidate must hold for the displayed state to change.
    public static let hysteresisWeeks = 3

    /// The one-line state, honest about where it is heading: the hysteresis keeps the displayed word steady,
    /// but the line never claims "nothing is pulling" while the last weeks plainly are.
    public var headline: String {
        let n = leanWeeks
        let run = "\(n) \(n == 1 ? "week" : "weeks") running"
        switch (state, leaning) {
        case (.even, .pulls?): return "Leaning toward Book Two, \(run) (\(Self.hysteresisWeeks) to call it)."
        case (.even, .holds?): return "Leaning back toward Book One, \(run) (\(Self.hysteresisWeeks) to call it)."
        case (.pulls, _?): return "Book Two has been pulling; it eased the last \(n == 1 ? "week" : "\(n) weeks")."
        case (.holds, _?): return "Book One has held you; it loosened the last \(n == 1 ? "week" : "\(n) weeks")."
        default: return state.copy(weeks: stateWeeks)
        }
    }
    /// "The X": the first crossing.
    public var theX: Date? { crossings.first }

    /// At least `k` of the last `n` recorded weeks had smoothed s ≥ +0.75.
    public func pullsWeeks(ofLast n: Int) -> Int { points.suffix(n).filter { $0.smoothed >= Self.pullThreshold }.count }
    public func divergence(ofLast n: Int) -> Int { points.suffix(n).filter(\.divergent).count }

    public static func analyze(_ pulses: [Pulse], today: Date = Date(), calendar: Calendar = .current) -> PulseAnalysis {
        let sorted = pulses.sorted { $0.weekOf < $1.weekOf }
        var points: [PulsePoint] = []
        var s: Double? = nil
        var prevWeek: Date? = nil
        var displayed: PulseState = .unread
        var run: (state: PulseState, count: Int) = (.unread, 0)
        var crossings: [Date] = []

        for (i, p) in sorted.enumerated() {
            let w = p.water
            let gapWeeks = prevWeek.map { Days.between($0, p.weekOf, calendar: calendar) / 7 } ?? 0
            let prevS = s
            if let cur = s, gapWeeks <= 3 { s = alpha * w + (1 - alpha) * cur } else { s = w }
            let sv = s!
            if let ps = prevS, ps <= 0, sv > 0 { crossings.append(p.weekOf) }
            let cand = PulseState.candidate(sv)
            run = run.state == cand ? (cand, run.count + 1) : (cand, 1)
            if i + 1 == 3 { displayed = cand }
            else if i + 1 > 3, run.count >= 3 { displayed = cand }
            points.append(PulsePoint(weekOf: p.weekOf, toward: p.toward, away: p.away, water: w, smoothed: sv,
                                     candidate: cand, displayed: displayed, divergent: p.pull - p.energy >= 2))
            prevWeek = p.weekOf
        }

        var stateWeeks = 0
        if displayed != .unread {
            for pt in points.reversed() { if pt.candidate == displayed { stateWeeks += 1 } else { break } }
        }

        var leaning: PulseState? = nil
        var leanWeeks = 0
        if displayed != .unread, let last = points.last, last.candidate != displayed {
            leaning = last.candidate
            for pt in points.reversed() { if pt.candidate == last.candidate { leanWeeks += 1 } else { break } }
        }

        let last3 = sorted.suffix(3)
        let burnout = last3.count == 3 && last3.allSatisfy { $0.energy <= 2 && $0.restlessness >= 4 }

        let cutoff = Days.add(-7 * 8, to: Days.weekStart(today, calendar: calendar), calendar: calendar)
        var tally: [String: Int] = [:]
        for p in sorted where p.weekOf >= cutoff { for r in p.anchorReasons { tally[r, default: 0] += 1 } }
        let ranking = tally.map { AnchorCount(reason: $0.key, count: $0.value) }
            .sorted { $0.count != $1.count ? $0.count > $1.count : $0.reason < $1.reason }

        return PulseAnalysis(points: points, state: displayed, stateWeeks: stateWeeks,
                             divergenceWeeks: points.suffix(8).filter(\.divergent).count,
                             crossings: crossings, burnout: burnout, anchorRanking: ranking,
                             leaning: leaning, leanWeeks: leanWeeks)
    }
}
