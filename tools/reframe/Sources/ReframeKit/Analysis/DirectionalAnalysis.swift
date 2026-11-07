import Foundation

/// Builds the directional channels (faces, people, motion) of the heading grid from a proxy scan.
enum DirectionalAnalysis {
    /// `forward` is where the camera body faced per cell (heading-grid degrees). Things fixed to
    /// the camera (the operator, a rider's body, a vehicle's interior, travel parallax) stay put
    /// in the camera's frame while they sweep across world headings as it turns, so persistence
    /// and motion baselines are measured relative to `forward`.
    static func build(_ scan: ProxyScan.Result, spin: [Float], forward: [Float], time: TimeSignals) -> HeadingGrid {
        var g = HeadingGrid(rate: time.rate, count: time.count)
        let faces = scan.detections.filter { $0.kind == .face }
        let people = scan.detections.filter { $0.kind == .person }
        // Nearer faces (bigger) count more: a face 15° tall is someone at arm's length.
        g[.faces] = persistentDamped(splat(faces, grid: g) { $0.confidence * (0.6 + 0.4 * min($0.height / 15, 1)) },
                                     forward: forward, grid: g)
        g[.people] = persistentDamped(splat(people, grid: g) { $0.confidence * 0.8 }, forward: forward, grid: g)
        if !scan.motionFrames.isEmpty { g[.motion] = motion(scan, spin: spin, forward: forward, grid: g) }
        if !scan.salienceFrames.isEmpty { g[.salience] = salience(scan, forward: forward, grid: g) }
        return g
    }

    /// What stands out, per heading: each frame's salience above its median heading (so only
    /// clear standouts count, not texture everywhere), averaged into cells, then the camera-fixed
    /// part (a roll cage, the rider) mostly removed: those aren't scenery.
    static func salience(_ scan: ProxyScan.Result, forward: [Float], grid g: HeadingGrid) -> [Float] {
        let B = HeadingGrid.bins
        var sum = [Float](repeating: 0, count: g.count * B), cnt = [Float](repeating: 0, count: g.count)
        for (t, frame) in zip(scan.salienceTimes, scan.salienceFrames) {
            let c = min(max(Int(t * g.rate), 0), g.count - 1)
            let median = percentile(frame, 0.5)
            for b in 0..<B { sum[c * B + b] += clamp01((frame[b] - median) / max(1 - median, 1e-3)) }
            cnt[c] += 1
        }
        // Fill the cells between samples (2 Hz samples, 4 Hz cells) from the nearest sample.
        var last: Int?
        for c in 0..<g.count {
            if cnt[c] > 0 {
                for b in 0..<B { sum[c * B + b] /= cnt[c] }
                last = c
            } else if let l = last {
                for b in 0..<B { sum[c * B + b] = sum[l * B + b] }
            }
        }
        return persistentDamped(sum, forward: forward, grid: g, strength: 0.8)
    }

    /// Arriving somewhere new: how different the horizon's colours are now (±2 s) from the
    /// 3–12 s before. Time-only (a histogram has no direction); 0 for the first few seconds.
    static func novelty(_ scan: ProxyScan.Result, time g: TimeSignals) -> [Float] {
        let times = scan.histogramTimes, hists = scan.histograms
        guard hists.count > 4 else { return [Float](repeating: 0, count: g.count) }
        func mean(_ a: Double, _ b: Double) -> [Float]? {
            let idx = times.indices.filter { times[$0] >= a && times[$0] <= b }
            guard !idx.isEmpty else { return nil }
            var m = [Float](repeating: 0, count: hists[0].count)
            for i in idx { for k in m.indices { m[k] += hists[i][k] } }
            return m.map { $0 / Float(idx.count) }
        }
        var raw = [Float](repeating: 0, count: g.count)
        for c in 0..<g.count {
            let t = g.time(c)
            guard t > 5, let now = mean(t - 2, t + 2), let before = mean(t - 12, t - 3) else { continue }
            raw[c] = 0.5 * zip(now, before).reduce(0) { $0 + abs($1.0 - $1.1) }  // total variation, 0…1
        }
        return raw.map { clamp01(($0 - 0.12) / 0.3) }
    }

    /// Bin offset from world heading to camera-relative heading at a time.
    static func shift(_ forward: [Float], at t: Double, rate: Double) -> Int {
        guard !forward.isEmpty else { return 0 }
        let f = Double(forward[min(max(Int(t * rate), 0), forward.count - 1)])
        return HeadingGrid.bin(yaw: f) - HeadingGrid.bin(yaw: 0)
    }

    /// Running median over ±w samples on a coarse stride, linearly interpolated.
    static func runningMedian(_ x: [Float], radius w: Int, step: Int) -> [Float] {
        let n = x.count
        guard n > 0 else { return [] }
        var out = [Float](repeating: 0, count: n)
        var last: (Int, Float)?
        for i in Swift.stride(from: 0, to: n, by: max(step, 1)) {
            let m = percentile(Array(x[max(0, i - w)...min(n - 1, i + w)]), 0.5)
            if let (li, lm) = last { for k in li..<i { out[k] = lm + (m - lm) * Float(k - li) / Float(i - li) } }
            last = (i, m)
        }
        if let (li, lm) = last { for k in li..<n { out[k] = lm } }
        return out
    }

    /// Each detection adds a Gaussian bump over heading (σ = half its width, at least 5°) to the
    /// cells within ±0.3 s; sums then saturate softly (1 - e^-x), so a crowd isn't 10× a face.
    static func splat(_ dets: [Detection], grid g: HeadingGrid, value: (Detection) -> Double) -> [Float] {
        let B = HeadingGrid.bins
        var sum = [Double](repeating: 0, count: g.count * B)
        let cell = { (t: Double) in min(max(Int(t * g.rate), 0), g.count - 1) }
        for d in dets {
            let v = value(d)
            let sigma = max(d.width / 2, 5) / HeadingGrid.binWidth
            let centre = (d.yaw + 180) / HeadingGrid.binWidth - 0.5
            let reach = Int(ceil(3 * sigma))
            for c in cell(d.time - 0.3)...cell(d.time + 0.3) {
                for k in -reach...reach {
                    let b = Int(centre.rounded()) + k
                    let dist = Double(b) - centre
                    sum[c * B + ((b % B) + B) % B] += v * exp(-dist * dist / (2 * sigma * sigma))
                }
            }
        }
        return sum.map { Float(1 - exp(-1.2 * $0)) }
    }

    /// Something that is always there relative to the camera (the operator's own face on a
    /// selfie stick, a passenger beside you, a rider's body) counts half: subtract half its median
    /// over ±10 s at the same camera-relative heading.
    static func persistentDamped(_ x: [Float], forward: [Float], grid g: HeadingGrid, strength: Float = 0.5) -> [Float] {
        let B = HeadingGrid.bins, w = Int(10 * g.rate), step = max(1, Int(g.rate / 2))
        let shifts = (0..<g.count).map { shift(forward, at: (Double($0) + 0.5) / g.rate, rate: g.rate) }
        var out = x
        for r in 0..<B {  // camera-relative bin
            let world = (0..<g.count).map { t in ((r + shifts[t]) % B + B) % B }
            let column = (0..<g.count).map { x[$0 * B + world[$0]] }
            guard (column.max() ?? 0) > 0.05 else { continue }
            let medians = runningMedian(column, radius: w, step: step)
            for t in 0..<g.count { out[t * B + world[t]] = max(0, column[t] - strength * medians[t]) }
        }
        return out
    }

    /// Change per heading in the world-locked frame, minus what is always changing at that
    /// camera-relative heading (travel parallax, a vehicle's interior, pedalling legs), minus what
    /// changes everywhere at once (exposure), and discounted while the camera spins fast.
    static func motion(_ scan: ProxyScan.Result, spin: [Float], forward: [Float], grid g: HeadingGrid) -> [Float] {
        let B = HeadingGrid.bins
        let frames = scan.motionFrames, times = scan.motionTimes
        let n = frames.count
        // Temporal baseline per camera-relative bin: running median over ±4 s of frames.
        let fr = n > 1 ? Double(n - 1) / max(times[n - 1] - times[0], 1e-3) : 8
        let shifts = times.map { shift(forward, at: $0, rate: g.rate) }
        var residual = [[Float]](repeating: [Float](repeating: 0, count: B), count: n)
        for r in 0..<B {
            let world = (0..<n).map { i in ((r + shifts[i]) % B + B) % B }
            let column = (0..<n).map { frames[$0][world[$0]] }
            let base = runningMedian(column, radius: Int(4 * fr), step: max(1, Int(fr / 2)))
            for i in 0..<n { residual[i][world[i]] = column[i] - base[i] }
        }
        // Global change at an instant (exposure, a bump): remove the median across headings.
        for i in 0..<n {
            let m = percentile(residual[i], 0.5)
            residual[i] = residual[i].map { max(0, $0 - m) }
        }
        // Frames -> cells (mean), discounted by spin.
        var sum = [Float](repeating: 0, count: g.count * B), cnt = [Float](repeating: 0, count: g.count)
        for i in 0..<n {
            let c = min(max(Int(times[i] * g.rate), 0), g.count - 1)
            for b in 0..<B { sum[c * B + b] += residual[i][b] }
            cnt[c] += 1
        }
        for c in 0..<g.count where cnt[c] > 0 {
            let calm = spin.isEmpty ? 1 : clamp01(1 - spin[min(c, spin.count - 1)] / 150)
            for b in 0..<B { sum[c * B + b] = sum[c * B + b] / cnt[c] * calm }
        }
        // Scale: the clip's 99.5th percentile, but never below an absolute floor, so a still
        // scene doesn't blow its noise up to full scale.
        let positive = sum.filter { $0 > 0 }
        let scale = max(percentile(positive, 0.995), 0.02)
        return sum.map { clamp01($0 / scale) }
    }
}
