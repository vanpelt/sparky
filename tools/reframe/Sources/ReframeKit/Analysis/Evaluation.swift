import Foundation

/// Scores suggestions against what you actually did with a footage folder (INTEREST_MAP.md,
/// Evaluation): the shots you kept and the suggestions you dismissed are the ground truth.
///
/// - Own shots (marked yourself, not kept from a suggestion) measure **recall**: did a top-k
///   suggestion cover the same moment?
/// - Kept and dismissed suggestions measure **precision**: how often a suggestion you acted on was
///   worth keeping, and whether dismissed moments still rank near the top.
/// - **Aim**: where the suggested path looks versus where your shot looks over the overlap (world
///   heading), for own shots and for kept suggestions you re-aimed.
public struct Evaluation: Codable, Sendable {
    public struct ShotMatch: Codable, Sendable {
        public var clip: String
        public var shot: String
        public var inTime: Double
        public var outTime: Double
        /// 1-based rank of the best-ranked overlapping suggestion, nil if none overlaps.
        public var rank: Int?
        /// Mean and worst heading difference over the overlap (degrees), when matched.
        public var aimMean: Double?
        public var aimWorst: Double?
        /// Looks like the REFRAME_DEMO_SHOT debug shot (0:12–0:30, keyframes at 12, 20, 28 s).
        public var looksLikeDemo = false
    }

    public struct ClipResult: Codable, Sendable {
        public var clip: String
        public var duration: Double
        public var suggestions: Int
        public var ownShots: [ShotMatch]
        public var kept: [ShotMatch]
        public var dismissed: Int
        /// Dismissed suggestions that the current analysis still ranks in its top 3.
        public var dismissedStillTop3: [String]
        public var analysisSeconds: Double
    }

    public var preset: InterestWeights.Preset
    public var clips: [ClipResult]

    // Summary
    public var ownShotCount: Int { clips.reduce(0) { $0 + $1.ownShots.count } }
    public func recall(at k: Int?) -> Double? {
        let all = clips.flatMap(\.ownShots)
        guard !all.isEmpty else { return nil }
        return Double(all.filter { m in m.rank.map { k == nil || $0 <= k! } ?? false }.count) / Double(all.count)
    }
    /// Kept ÷ (kept + dismissed) suggestions.
    public var keepRate: Double? {
        let kept = clips.reduce(0) { $0 + $1.kept.count }, dismissed = clips.reduce(0) { $0 + $1.dismissed }
        return kept + dismissed > 0 ? Double(kept) / Double(kept + dismissed) : nil
    }
    /// Mean aim error over own shots matched to a suggestion.
    public var aimMean: Double? {
        let v = clips.flatMap(\.ownShots).compactMap(\.aimMean)
        return v.isEmpty ? nil : v.reduce(0, +) / Double(v.count)
    }
    /// Share of matched own shots whose suggested path stays within 30° on average (roughly: the
    /// subject is in frame for any of the lenses).
    public var aimWithin30: Double? {
        let v = clips.flatMap(\.ownShots).compactMap(\.aimMean)
        return v.isEmpty ? nil : Double(v.filter { $0 <= 30 }.count) / Double(v.count)
    }
    /// Kept suggestions you re-aimed by more than 10° somewhere (a sign the path was off), of
    /// those whose original path is known.
    public var keptReaimed: (count: Int, of: Int) {
        let k = clips.flatMap(\.kept).filter { $0.aimWorst != nil }
        return (k.filter { ($0.aimWorst ?? 0) > 10 }.count, k.count)
    }
    public var dismissedStillTop3: Int { clips.reduce(0) { $0 + $1.dismissedStillTop3.count } }
    /// Analysis time ÷ footage time, from the cached timings.
    public var speed: Double? {
        let d = clips.reduce(0) { $0 + $1.duration }
        return d > 0 ? clips.reduce(0) { $0 + $1.analysisSeconds } / d : nil
    }
}

public enum Evaluator {
    /// Overlap counts as the same moment when it covers half the shorter of the two.
    static func overlaps(_ a: (Double, Double), _ b: (Double, Double)) -> Bool {
        let o = min(a.1, b.1) - max(a.0, b.0)
        return o > 0 && o >= 0.5 * min(a.1 - a.0, b.1 - b.0)
    }

    /// World heading a shot looks at, at time t (degrees).
    static func heading(_ s: Shot, _ m: Motion, _ t: Double) -> Double {
        m.baseHeading(at: t, mode: s.stabilization, smoothing: s.smoothing, lockTime: s.lockTime) + s.framing(at: t).yaw
    }

    /// Mean and worst wrapped heading difference between two shots over their overlap, at 4 Hz.
    static func aim(_ a: Shot, _ b: Shot, motion: Motion) -> (Double, Double)? {
        let t0 = max(a.inTime, b.inTime), t1 = min(a.outTime, b.outTime)
        guard t1 > t0 else { return nil }
        var sum = 0.0, worst = 0.0, n = 0.0
        var t = t0
        while t <= t1 {
            var d = (heading(a, motion, t) - heading(b, motion, t)).truncatingRemainder(dividingBy: 360)
            if d > 180 { d -= 360 } else if d < -180 { d += 360 }
            sum += abs(d); worst = max(worst, abs(d)); n += 1
            t += 0.25
        }
        return (sum / n, worst)
    }

    /// Shots without a recorded suggestion (made before shots remembered where they came from) that
    /// still sit exactly on a suggestion's in point are treated as kept suggestions.
    /// Shots kept before shots remembered where they came from are recognised by their path: Lock
    /// at the clip's first frame with every later keyframe on an analysis cell centre (x.125, x.375,
    /// …), which hand-made keyframes essentially never are.
    static func suggestionID(of shot: Shot, among all: [Suggestion]) -> String? {
        if let id = shot.suggestionID { return id }
        if let s = all.first(where: { abs($0.inTime - shot.inTime) < 0.02 && abs($0.outTime - shot.outTime) < 0.3 }) { return s.id }
        let later = shot.keyframes.dropFirst()
        let onCells = later.allSatisfy { k in
            let f = (k.time * 4).truncatingRemainder(dividingBy: 1)
            return abs(f - 0.5) < 0.01
        }
        if shot.stabilization == .lock, shot.lockTime == 0, later.count >= 1, onCells { return "legacy" }
        return nil
    }

    public static func evaluate(clips: [Clip], analyses: [URL: ClipAnalysis], store: ShotStore,
                                preset: InterestWeights.Preset) -> Evaluation {
        var results: [Evaluation.ClipResult] = []
        for clip in clips {
            guard let a = analyses[clip.url] else { continue }
            let shots = store.shots.filter { $0.clipPath == clip.url.path }
            let prefix = clip.name + "@"
            let dismissed = store.dismissed.filter { $0.hasPrefix(prefix) }
            guard !shots.isEmpty || !dismissed.isEmpty else { continue }
            let ranked = a.suggestions  // best first
            var own: [Evaluation.ShotMatch] = [], kept: [Evaluation.ShotMatch] = []
            for shot in shots.sorted(by: { $0.inTime < $1.inTime }) {
                var m = Evaluation.ShotMatch(clip: clip.name, shot: shot.name, inTime: shot.inTime, outTime: shot.outTime)
                m.looksLikeDemo = shot.inTime == 12 && shot.keyframes.map(\.time) == [12, 20, 28]
                if let id = suggestionID(of: shot, among: ranked) {
                    // Kept suggestion: how far did you move it from what we suggest now?
                    let current = ranked.first { $0.id == id }
                        ?? ranked.first { overlaps(($0.inTime, $0.outTime), (shot.inTime, shot.outTime)) }
                    m.rank = current.flatMap { c in ranked.firstIndex { $0.id == c.id } }.map { $0 + 1 }
                    // Re-aiming is measured against the path as it was suggested when kept (today's
                    // analysis may differ). Shots kept before that was recorded can't say.
                    if let original = shot.suggestedKeyframes, let motion = clip.motion {
                        var was = shot
                        was.keyframes = original
                        if let (mean, worst) = aim(shot, was, motion: motion) { m.aimMean = mean; m.aimWorst = worst }
                    }
                    kept.append(m)
                } else {
                    if let i = ranked.firstIndex(where: { overlaps(($0.inTime, $0.outTime), (shot.inTime, shot.outTime)) }) {
                        m.rank = i + 1
                        if let motion = clip.motion,
                           let (mean, worst) = aim(shot, ranked[i].shot(clipPath: clip.url.path, name: ""), motion: motion) {
                            m.aimMean = mean; m.aimWorst = worst
                        }
                    }
                    own.append(m)
                }
            }
            let top3 = Set(ranked.prefix(3).map(\.id))
            results.append(Evaluation.ClipResult(
                clip: clip.name, duration: a.duration, suggestions: ranked.count, ownShots: own, kept: kept,
                dismissed: dismissed.count, dismissedStillTop3: dismissed.filter { top3.contains($0) }.sorted(),
                // Audio decodes alongside the proxy scan, so it isn't added.
                analysisSeconds: ["motion", "proxy scan", "audio analysis", "directional"]
                    .reduce(0) { $0 + (a.timings[$1] ?? 0) }))
        }
        return Evaluation(preset: preset, clips: results)
    }
}
