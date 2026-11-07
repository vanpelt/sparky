import Foundation
import ReframeKit

/// reframe-cli evaluate-aim <footage folder | cloud>
///
/// Where would each signal point the camera during the shots you kept, versus where you pointed
/// it? For every kept shot, sampled twice a second: the heading of the strongest cell of
///   - interest: today's fused heading grid (what suggestions aim with),
///   - appeal: MobileCLIP zero-shot, "worth filming" prompts minus "junk" prompts (SemanticSignals),
///   - taste: similarity to the views you aimed at in your *other* kept shots (leave-one-out),
///   - z-scored blends, and the analysis's own fused map with and without `.appeal`/`.dull`
///     (AIM_W=appeal,dull overrides those two weights; AIM_STATS=1 prints the margin distribution),
/// compared with your own heading. Reports mean angular error and how often it's within 30°.
func evaluateAim(_ folder: URL) async throws {
    let store = ShotStore.load(folder: folder)
    let encoder = try CLIPEncoder.shared()
    // The prompts the analysis uses for `.appeal` (Semantic/SemanticSignals.swift).
    let (pos, neg) = try SemanticSignals.promptVectors(encoder)

    struct Sample { var clip: String; var shot: String; var t: Double; var userYaw: Double }
    struct ClipData { var index: SemanticIndex; var analysis: ClipAnalysis; var motion: Motion }
    var clips: [String: ClipData] = [:]
    var samples: [Sample] = []
    // Every shot you made or kept: its keyframed path is where you chose to look.
    for shot in store.shots {
        let url = URL(fileURLWithPath: shot.clipPath)
        guard let clip = try? Clip(url: url), let motion = clip.motion,
              let index = try await SemanticIndexer.index(clip) else { continue }
        let a = try await Analyzer.analyze(clip)
        clips[shot.clipPath] = ClipData(index: index, analysis: a, motion: motion)
        let origin = motion.baseHeading(at: 0, mode: .lock, smoothing: 0, lockTime: 0)
        var t = shot.inTime
        while t <= shot.outTime {
            let yaw = motion.baseHeading(at: t, mode: shot.stabilization, smoothing: shot.smoothing, lockTime: shot.lockTime)
                + shot.framing(at: t).yaw - origin
            samples.append(Sample(clip: shot.clipPath, shot: shot.name, t: t, userYaw: yaw))
            t += 0.5
        }
    }
    guard !samples.isEmpty else { print("no kept shots with an index"); return }

    // The (unit) embedding of the view nearest a heading at a time.
    func vector(_ idx: SemanticIndex, t: Double, yaw: Double) -> [Float] {
        let j = idx.times.indices.min { abs(idx.times[$0] - t) < abs(idx.times[$1] - t) }!
        var off = (yaw - idx.yaws[0]).truncatingRemainder(dividingBy: 360)
        if off < 0 { off += 360 }
        let v = Int((off / (360 / Double(idx.viewCount))).rounded()) % idx.viewCount
        let o = (j * idx.viewCount + v) * idx.dimension
        return idx.vectors.withUnsafeBytes { raw in
            let h = raw.bindMemory(to: Float16.self)
            return (0..<idx.dimension).map { Float(h[o + $0]) }
        }
    }
    func normalize(_ x: [Float]) -> [Float] {
        let n = max(sqrt(x.reduce(0) { $0 + $1 * $1 }), 1e-6)
        return x.map { $0 / n }
    }
    func angle(_ a: Double, _ b: Double) -> Double {
        var d = (a - b).truncatingRemainder(dividingBy: 360)
        if d > 180 { d -= 360 } else if d < -180 { d += 360 }
        return abs(d)
    }
    /// Per-bin values (72) at time t from a per-(time, view) array.
    func bins(_ idx: SemanticIndex, _ values: [Float], t: Double) -> [Float] {
        (0..<HeadingGrid.bins).map { idx.value(values, time: t, yaw: HeadingGrid.yaw(bin: $0)) }
    }
    func zscore(_ x: [Float]) -> [Float] {
        let m = x.reduce(0, +) / Float(x.count)
        let s = sqrt(x.reduce(0) { $0 + ($1 - m) * ($1 - m) } / Float(x.count))
        return x.map { s > 1e-6 ? ($0 - m) / s : 0 }
    }

    var errors: [String: [Double]] = [:]
    var perShot: [String: [String: [Double]]] = [:]
    let shotsByKey = Dictionary(grouping: samples) { "\($0.clip)|\($0.shot)" }
    for (key, ss) in shotsByKey {
        guard let d = clips[ss[0].clip] else { continue }
        let idx = d.index
        // Taste: centroid of what you aimed at in every *other* kept shot.
        var proto = [Float](repeating: 0, count: CLIPEncoder.dimension)
        for s in samples where "\(s.clip)|\(s.shot)" != key {
            guard let o = clips[s.clip] else { continue }
            for (i, x) in vector(o.index, t: s.t, yaw: s.userYaw).enumerated() { proto[i] += x }
        }
        proto = normalize(proto)
        let simPos = idx.similarities(pos), simNeg = idx.similarities(neg), simTaste = idx.similarities(proto)
        let appeal = zip(simPos, simNeg).map { $0 - $1 }
        var without = d.analysis.weights
        without.values[.appeal] = 0; without.values[.dull] = 0
        let grid = d.analysis.headingFused(without)
        var w = d.analysis.weights
        if let o = ProcessInfo.processInfo.environment["AIM_W"]?.split(separator: ",").compactMap({ Double($0) }), o.count == 2 {
            w.values[.appeal] = o[0]; w.values[.dull] = o[1]
        }
        let shipped = d.analysis.headingFused(w)
        for s in ss {
            let c = min(max(Int(s.t * d.analysis.signals.rate), 0), d.analysis.signals.count - 1)
            let B = HeadingGrid.bins
            let interest = grid.map { Array($0[(c * B)..<((c + 1) * B)]) } ?? [Float](repeating: 0, count: B)
            let withCLIP = shipped.map { Array($0[(c * B)..<((c + 1) * B)]) } ?? [Float](repeating: 0, count: B)
            let ap = bins(idx, appeal, t: s.t), ta = bins(idx, simTaste, t: s.t)
            var forward = [Float](repeating: 0, count: B)
            let fwd = Double(d.analysis.forward[min(c, d.analysis.forward.count - 1)])
            forward[HeadingGrid.bin(yaw: fwd)] = 1
            let candidates: [String: [Float]] = [
                "0 camera forward": forward,
                "1 interest": interest,
                "2 appeal": ap,
                "3 taste": ta,
                "4 interest+appeal": zip(zscore(interest), zscore(ap)).map { $0 + $1 },
                "5 interest+taste": zip(zscore(interest), zscore(ta)).map { $0 + $1 },
                "6 all three": zip(zip(zscore(interest), zscore(ap)), zscore(ta)).map { $0.0 + $0.1 + $1 },
                "7 interest, shipped": withCLIP,
            ]
            for (name, v) in candidates {
                guard let b = v.indices.max(by: { v[$0] < v[$1] }), v[b] > v.min()! else { continue }
                let e = angle(HeadingGrid.yaw(bin: b), s.userYaw)
                errors[name, default: []].append(e)
                perShot[key, default: [:]][name, default: []].append(e)
            }
        }
    }
    if ProcessInfo.processInfo.environment["AIM_STATS"] != nil {
        var margins: [Float] = [], above: [Float] = []
        for d in clips.values {
            let m = try SemanticSignals.margin(d.index, encoder: encoder)
            margins += m
            let V = d.index.viewCount
            for t in 0..<d.index.times.count {
                let row = Array(m[(t * V)..<((t + 1) * V)]).sorted()
                let med = row[V / 2]
                above += row.map { $0 - med }.filter { $0 > 0 }
            }
        }
        func pct(_ x: [Float], _ p: Double) -> Float { let s = x.sorted(); return s[Int(Double(s.count - 1) * p)] }
        print("margin (good - junk):", [0.05, 0.25, 0.5, 0.75, 0.95].map { String(format: "p%.0f %.3f", $0 * 100, pct(margins, $0)) }.joined(separator: "  "))
        print("above moment median:", [0.5, 0.75, 0.9, 0.99].map { String(format: "p%.0f %.3f", $0 * 100, pct(above, $0)) }.joined(separator: "  "))
    }
    print("Aim vs your kept shots: \(samples.count) samples (every 0.5 s) in \(shotsByKey.count) shots\n")
    print("signal               mean error   within 30°   within 60°")
    for (name, e) in errors.sorted(by: { $0.key < $1.key }) {
        let m = e.reduce(0, +) / Double(e.count)
        let w30 = Double(e.filter { $0 <= 30 }.count) / Double(e.count), w60 = Double(e.filter { $0 <= 60 }.count) / Double(e.count)
        print(String(name.dropFirst(2)).padding(toLength: 20, withPad: " ", startingAt: 0)
              + String(format: " %8.0f°    %8.0f%%    %8.0f%%", m, w30 * 100, w60 * 100))
    }
    print("\nPer shot (mean error):")
    let names = errors.keys.sorted()
    print("  " + "shot".padding(toLength: 36, withPad: " ", startingAt: 0) + names.map { String($0.dropFirst(2).prefix(9)).padding(toLength: 10, withPad: " ", startingAt: 0) }.joined())
    for (key, m) in perShot.sorted(by: { $0.key < $1.key }) {
        let label = String(URL(fileURLWithPath: String(key.split(separator: "|")[0])).deletingPathExtension().lastPathComponent.suffix(7)) + " " + String(key.split(separator: "|")[1])
        print("  " + label.padding(toLength: 36, withPad: " ", startingAt: 0)
              + names.map { n in m[n].map { String(format: "%4.0f°     ", $0.reduce(0, +) / Double($0.count)) } ?? "  –       " }.joined())
    }
}
