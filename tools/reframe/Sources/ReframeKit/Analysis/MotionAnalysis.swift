import Foundation

/// Camera-motion signals from the IMU: jumps (free fall), impacts, the operator turning toward
/// something, calm stretches, shake, and handling at the start and end of a recording.
enum MotionAnalysis {
    /// Per-cell 90th-percentile angular speed (deg/s): also used to snap cuts to calm moments.
    static func spin(_ m: Motion, grid: TimeSignals) -> [Float] {
        var cells = [[Float]](repeating: [], count: grid.count)
        let stride = max(1, m.times.count / (grid.count * 120))
        for i in Swift.stride(from: 0, to: m.times.count, by: stride) {
            let t = m.times[i]
            guard t >= 0, t < Double(grid.count) / grid.rate else { continue }
            cells[grid.index(t)].append(Float(m.angularSpeed(i)))
        }
        var out = cells.map { percentile($0, 0.9) }
        // Cells without IMU samples (shouldn't happen) borrow their neighbour.
        for i in out.indices where cells[i].isEmpty && i > 0 { out[i] = out[i - 1] }
        return out
    }

    static func analyze(_ m: Motion, timelapse: Bool, duration: Double, into grid: inout TimeSignals) -> [Float] {
        let spin = spin(m, grid: grid)
        grid[.calm] = spin.map { clamp01(1 - $0 / 60) }
        grid[.shake] = spin.map { clamp01(($0 - 90) / 150) }
        grid[.handling] = handling(grid: grid, duration: duration)
        guard !timelapse else { return spin }  // capture time is compressed: events are meaningless
        jumpsAndImpacts(m, into: &grid)
        grid[.turn] = turns(m, grid: grid)
        return spin
    }

    /// Picking up and putting down the camera: a penalty ramp over the first 1.2 s and last 1.5 s.
    static func handling(grid: TimeSignals, duration: Double) -> [Float] {
        (0..<grid.count).map { i in
            let t = grid.time(i)
            return max(clamp01(Float(1 - t / 1.2)), clamp01(Float(1 - (duration - t) / 1.5)))
        }
    }

    /// Free fall (|a| well under 1 g for a sustained run) is airtime; sharp spikes are impacts.
    static func jumpsAndImpacts(_ m: Motion, into grid: inout TimeSignals) {
        let f = m.forceMagnitude, t = m.times
        guard f.count > 10 else { return }
        // 15 ms moving average removes sensor noise without hiding short airtime; impacts use
        // 60 ms, so mount vibration (which can spike past 10 g) averages out.
        let dtMean = (t[t.count - 1] - t[0]) / Double(t.count - 1)
        let smooth = boxFilter(f, radius: max(1, Int(0.0075 / max(dtMean, 1e-4))))
        let heavy = boxFilter(f, radius: max(1, Int(0.03 / max(dtMean, 1e-4))))

        var jump = [Float](repeating: 0, count: grid.count)
        var runStart: Int?
        for i in 0..<smooth.count {
            if smooth[i] < 0.35 {
                if runStart == nil { runStart = i }
            } else if let s = runStart {
                runStart = nil
                let dur = t[i] - t[s]
                guard dur >= 0.15 else { continue }
                let score = clamp01(Float((dur - 0.08) / 0.4))
                // Spread over the run plus the take-off before it and the landing after it.
                for c in grid.index(t[s] - 0.75)...grid.index(t[i] + 0.75) { jump[c] = max(jump[c], score) }
            }
        }
        grid[.jump] = jump

        var peak = [Float](repeating: 0, count: grid.count)
        for i in 0..<heavy.count where t[i] >= 0 {
            let c = grid.index(t[i])
            peak[c] = max(peak[c], heavy[i])
        }
        // Must stand out from the local level, so a bumpy road doesn't read as one long impact.
        let w = Int(10 * grid.rate)
        var impact = [Float](repeating: 0, count: grid.count)
        for c in 0..<grid.count {
            let local = percentile(Array(peak[max(0, c - w)...min(grid.count - 1, c + w)]), 0.9)
            impact[c] = clamp01((peak[c] - max(2.0, 1.4 * local)) / 2)
        }
        grid[.impact] = dilate(impact, radius: Int(0.5 * grid.rate))
    }

    /// Centred moving average over ±radius samples.
    static func boxFilter(_ x: [Float], radius r: Int) -> [Float] {
        var prefix = [Double](repeating: 0, count: x.count + 1)
        for i in 0..<x.count { prefix[i + 1] = prefix[i] + Double(x[i]) }
        return x.indices.map { i in
            let a = max(0, i - r), b = min(x.count, i + r + 1)
            return Float((prefix[b] - prefix[a]) / Double(b - a))
        }
    }

    /// The operator swung the camera's heading by 50° or more within 2.5 s and then held it:
    /// they turned toward something. Marks the 3 s after the turn settles.
    static func turns(_ m: Motion, grid: TimeSignals) -> [Float] {
        let h = (0..<grid.count).map { m.heading(at: grid.time($0), smoothing: 0.4) * 180 / .pi }
        let n = grid.count, span = Int(2.5 * grid.rate), hold = Int(1.5 * grid.rate), mark = Int(3 * grid.rate)
        var rate = [Double](repeating: 0, count: n)
        for i in 1..<max(n - 1, 1) { rate[i] = abs(h[i + 1] - h[i - 1]) * grid.rate / 2 }
        var out = [Float](repeating: 0, count: n)
        var i = span
        while i < n - hold {
            let delta = abs(h[i] - h[i - span])
            let held = rate[i..<(i + hold)].max() ?? 0
            if delta > 50, held < 20 {
                let score = clamp01(Float((delta - 30) / 90))
                for c in i..<min(n, i + mark) { out[c] = max(out[c], score) }
                i += mark
            } else {
                i += 1
            }
        }
        return out
    }
}
