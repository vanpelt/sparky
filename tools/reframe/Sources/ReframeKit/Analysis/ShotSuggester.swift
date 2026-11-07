import Foundation

extension Signal: CodingKeyRepresentable {}

/// A moment worth keeping: a time range, a camera path, and why.
public struct Suggestion: Codable, Sendable, Hashable, Identifiable {
    /// Stable across re-analysis (clip and in point to the half second), so dismissals stick.
    public var id: String
    public var inTime: Double
    public var outTime: Double
    public var peakTime: Double
    /// Peak and mean interest along the path, blended. Comparable across clips.
    public var score: Double
    /// Positive contributors, strongest first.
    public var reasons: [Signal]
    /// Mean weighted contribution of each signal over the range, along the path for directional
    /// signals (negatives are penalties).
    public var contributions: [Signal: Double]
    public var lens: LensPreset
    /// The camera path as keyframes, relative to `stabilization`'s view base.
    public var keyframes: [Keyframe]
    /// Lock at the clip's first frame (the heading grid's frame) when a path was solved; Follow
    /// when the clip had no directional analysis.
    public var stabilization: Motion.Stabilization
    public var lockTime: Double

    public var duration: Double { outTime - inTime }

    /// The main reason, short enough for a tile: "Laughter".
    public var title: String { reasons.first.map { $0.phrase.prefix(1).uppercased() + $0.phrase.dropFirst() } ?? "Suggested" }

    /// Up to two reasons: "Laughter, voices".
    public var summary: String {
        let s = reasons.prefix(2).map(\.phrase).joined(separator: ", ")
        return s.isEmpty ? "Suggested" : s.prefix(1).uppercased() + s.dropFirst()
    }

    /// An ordinary editable shot with the suggested path.
    public func shot(clipPath: String, name: String) -> Shot {
        var s = Shot(clipPath: clipPath, name: name, inTime: inTime, outTime: outTime,
                     framing: keyframes.first?.framing ?? Framing(yaw: 0, pitch: 0, preset: lens),
                     stabilization: stabilization, smoothing: 0.8, lockTime: lockTime)
        if !keyframes.isEmpty { s.keyframes = keyframes.map { Keyframe(time: $0.time, framing: $0.framing) } }
        s.suggestionID = id
        s.suggestedKeyframes = s.keyframes
        return s
    }
}

/// Everything the suggester reads: cached, so weights can change without re-analysis.
public struct InterestInputs: Sendable {
    public var time: TimeSignals
    public var heading: HeadingGrid?
    public var detections: [Detection]
    /// Where the camera itself faced (follow heading, heading-grid frame, degrees) per cell.
    public var forward: [Float]
    /// 90th-percentile angular speed per cell (deg/s), for snapping cuts.
    public var spin: [Float]
}

/// Turns interest into suggested shots (INTEREST_MAP.md): the moment curve is time-only
/// interest plus the best direction's interest; peaks with hysteresis grow into 3–12 s segments,
/// cuts snap to calm moments, neighbours merge; each segment then gets a camera path.
public struct ShotSuggester: Sendable {
    public var minLength = 3.0
    public var maxLength = 12.0
    /// Below this interest nothing is suggested.
    public var floor: Float = 0.15
    /// A segment extends while interest stays above this fraction of its peak.
    public var hysteresis: Float = 0.3
    /// Reactions (laughter, cheering) lag what caused them: start this much earlier.
    public var audioLead = 1.5
    public var path = CameraPath()

    public init() {}

    public func suggest(_ inputs: InterestInputs, weights: InterestWeights, clipID: String, duration: Double) -> [Suggestion] {
        let g = inputs.time, spin = inputs.spin
        let B = HeadingGrid.bins
        let tf = g.fused(weights)
        let n = tf.count, rate = g.rate
        guard n > Int(minLength * rate) else { return [] }
        let D = inputs.heading.map { $0.fused(weights) }
        let dmax: [Float] = (0..<n).map { t in D.map { d in d[(t * B)..<((t + 1) * B)].max() ?? 0 } ?? 0 }
        let c = zip(tf, dmax).map { $0 + $1 }
        let contributions = g.contributions(weights)
        let maxCount = min(8, max(1, Int(duration / 12)))
        let cells = { (s: Double) in Int((s * rate).rounded()) }
        var avail = c
        var picked: [(a: Int, b: Int, peak: Int)] = []
        // Hysteresis is measured above the clip's typical level, so a segment ends when interest
        // falls back toward it (people who are always around don't stretch every shot to 12 s).
        let base = percentile(c, 0.3)

        for _ in 0..<(maxCount * 3) where picked.count < maxCount {
            guard let p = avail.indices.max(by: { avail[$0] < avail[$1] }), avail[p] >= floor else { break }
            let v = c[p], low = base + (v - base) * hysteresis
            var a = p, b = p
            while a > 0, c[a - 1] > low, b - a < cells(maxLength) { a -= 1 }
            while b < n - 1, c[b + 1] > low, b - a < cells(maxLength) { b += 1 }

            // Audio-led moments (reactions) start a beat early.
            if let (s, total) = dominant(contributions, a, b), s.isAudio,
               total > dmax[a...b].reduce(0, +) { a = max(0, a - cells(audioLead)) }
            // At least minLength, padded around the peak (more before it than after).
            while b - a < cells(minLength) {
                if a > 0, (p - a) <= 2 * (b - p) || b == n - 1 { a -= 1 } else if b < n - 1 { b += 1 } else { break }
            }
            // At most maxLength, trimmed toward the peak.
            while b - a > cells(maxLength) { if p - a > b - p { a += 1 } else { b -= 1 } }
            // Snap each cut to the calmest cell within a second, so it doesn't land mid-swing.
            if !spin.isEmpty {
                a = calmest(spin, around: a, radius: cells(1), limit: 0..<max(p - cells(1), 1))
                let lo = min(p + cells(1), n - 1)
                b = calmest(spin, around: b, radius: cells(1), limit: lo..<max(lo + 1, min(n, a + cells(maxLength) + 1)))
            }
            while b - a + 1 > cells(maxLength) { if p - a > b - p { a += 1 } else { b -= 1 } }
            while b - a + 1 < cells(minLength) {  // snapping can also shrink it
                if a > 0 { a -= 1 } else if b < n - 1 { b += 1 } else { break }
            }
            for i in max(0, a - cells(1))...min(n - 1, b + cells(1)) { avail[i] = -.infinity }

            // Merge with a neighbour if the result stays short enough; otherwise skip overlaps.
            if let j = picked.firstIndex(where: { a <= $0.b + cells(1) && b >= $0.a - cells(1) }) {
                let ma = min(a, picked[j].a), mb = max(b, picked[j].b)
                if mb - ma <= cells(maxLength) { picked[j] = (ma, mb, c[p] > c[picked[j].peak] ? p : picked[j].peak) }
                continue
            }
            picked.append((a, b, p))
        }

        let made = picked.map { seg in
            make(seg, inputs: inputs, D: D, tf: tf, contributions: contributions, weights: weights,
                 clipID: clipID, duration: duration)
        }
        .sorted { $0.score > $1.score }
        // Merges can grow a segment into another: keep the better of any overlapping pair.
        var kept: [Suggestion] = []
        for s in made where !kept.contains(where: { min($0.outTime, s.outTime) - max($0.inTime, s.inTime) > 0 }) {
            kept.append(s)
        }
        return kept
    }

    private func make(_ seg: (a: Int, b: Int, peak: Int), inputs: InterestInputs, D: [Float]?, tf: [Float],
                      contributions: [Signal: [Float]], weights: InterestWeights, clipID: String, duration: Double) -> Suggestion {
        let g = inputs.time, rate = g.rate, B = HeadingGrid.bins
        let range = seg.a...seg.b
        let tIn = Double(seg.a) / rate, tOut = min(Double(seg.b + 1) / rate, duration)
        let times = range.map { g.time($0) }

        // Camera path through the heading grid (or the camera's own heading without one).
        var yaws: [Double]
        if let D { yaws = path.solve(D, cells: range, forward: inputs.forward) } else { yaws = [] }
        let bins = yaws.map { HeadingGrid.bin(yaw: $0) }
        func pathYaw(_ t: Double) -> Double {
            guard !yaws.isEmpty else { return 0 }
            let i = min(max(Int((t - times[0]) * rate), 0), yaws.count - 1)
            return yaws[i]
        }

        // Interest along the path, and each signal's share of it.
        var along = [Float](), contrib: [Signal: Double] = [:]
        for (k, t) in range.enumerated() { along.append(tf[t] + (D.map { $0[t * B + bins[k]] } ?? 0)) }
        for (s, v) in contributions {
            let m = range.reduce(Float(0)) { $0 + v[$1] } / Float(range.count)
            if abs(m) > 0.005 { contrib[s] = Double(m) }
        }
        if let h = inputs.heading {
            for (s, v) in h.channels where weights[s] != 0 {
                let m = range.enumerated().reduce(Float(0)) { $0 + v[$1.element * B + bins[$1.offset]] } / Float(range.count)
                if abs(m) * Float(weights[s]) > 0.005 { contrib[s] = Double(m) * weights[s] }
            }
        }
        let positive = contrib.filter { $0.value > 0 }.sorted { $0.value > $1.value }
        let top = positive.first?.value ?? 0
        var reasons = positive.filter { $0.value >= max(0.25 * top, 0.02) && $0.key != .calm }.prefix(3).map(\.key)
        if reasons.isEmpty, let first = positive.first { reasons = [first.key] }
        let mean = along.reduce(0, +) / Float(along.count)
        let peakAlong = along.max() ?? 0

        // Keyframes, pitch and lens.
        var keyframes: [Keyframe] = []
        var lens = LensPreset.dewarp
        if let D, !yaws.isEmpty {
            let pitch = path.pitch(inputs.detections, path: pathYaw, from: tIn, to: tOut)
            lens = path.lens(D, cell: seg.peak, dets: inputs.detections, path: pathYaw, from: tIn, to: tOut)
            // Scenery-led with nobody along the path: Linear keeps the horizon and rock lines straight.
            let peopleShare = (contrib[.faces] ?? 0) + (contrib[.people] ?? 0)
            if peopleShare < 0.03, let lead = reasons.first, [.salience, .novelty, .calm, .turn].contains(lead) { lens = .linear }
            // Path cells sit at cell centres; pin the first keyframe to the in point.
            var kt = times
            kt[0] = tIn
            keyframes = path.keyframes(yaws, times: kt).map { k in
                var y = k.yaw.truncatingRemainder(dividingBy: 360)
                if y > 180 { y -= 360 } else if y < -180 { y += 360 }
                return Keyframe(time: k.time, framing: Framing(yaw: y, pitch: pitch, preset: lens))
            }
        }
        return Suggestion(id: "\(clipID)@\(Int((tIn * 2).rounded()))", inTime: tIn, outTime: tOut,
                          peakTime: g.time(seg.peak), score: Double(0.6 * peakAlong + 0.4 * mean),
                          reasons: reasons, contributions: contrib, lens: lens, keyframes: keyframes,
                          stabilization: keyframes.isEmpty ? .follow : .lock, lockTime: keyframes.isEmpty ? tIn : 0)
    }

    /// The time-only signal contributing most (positively) over a range, with its total.
    private func dominant(_ contributions: [Signal: [Float]], _ a: Int, _ b: Int) -> (Signal, Float)? {
        let sums = contributions.map { ($0.key, $0.value[a...b].reduce(0, +)) }
        guard let best = sums.max(by: { $0.1 < $1.1 }), best.1 > 0 else { return nil }
        return best
    }

    private func calmest(_ spin: [Float], around i: Int, radius: Int, limit: Range<Int>) -> Int {
        let lo = max(i - radius, limit.lowerBound, 0), hi = min(i + radius, limit.upperBound - 1, spin.count - 1)
        guard lo <= hi else { return min(max(i, 0), spin.count - 1) }
        // Prefer staying put unless it's clearly calmer elsewhere.
        var best = min(max(i, lo), hi)
        for j in lo...hi where spin[j] < spin[best] * 0.8 { best = j }
        return best
    }
}
