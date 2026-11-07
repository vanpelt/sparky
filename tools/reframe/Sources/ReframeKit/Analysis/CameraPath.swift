import Foundation

/// Where to point the camera through a suggested shot (AutoCam-style): dynamic programming over
/// heading bins maximizing Σ I(t, h_t) − λ·|Δh|, with a bounded turn rate, a gentle pull toward
/// where the camera itself was facing, then smoothing (standing in for the |Δ²h| term) and
/// Douglas–Peucker simplification into a few editable keyframes.
public struct CameraPath: Sendable {
    /// Cost of moving one 5° bin between cells, in interest units.
    public var lambda: Float = 0.04
    /// Fastest turn: bins per cell (2 bins per 0.25 s = 40°/s).
    public var maxStep = 2
    /// Reward for looking where the camera faced, so empty stretches fall back to follow.
    public var forwardPrior: Float = 0.05
    public var smoothSeconds = 0.8
    /// Keyframe fit tolerance, degrees.
    public var tolerance = 4.0
    /// A path that wanders less than this is one still framing.
    public var stillRange = 12.0

    public init() {}

    /// Unwrapped yaw (degrees, heading-grid frame) per cell over a...b.
    public func solve(_ D: [Float], cells range: ClosedRange<Int>, forward: [Float]) -> [Double] {
        let B = HeadingGrid.bins, n = range.count
        func dist(_ a: Int, _ b: Int) -> Int { let d = abs(a - b) % B; return min(d, B - d) }
        func prior(_ t: Int, _ b: Int) -> Float {
            guard t < forward.count else { return 0 }
            let d = Double(dist(b, HeadingGrid.bin(yaw: Double(forward[t])))) * HeadingGrid.binWidth
            return forwardPrior * Float(exp(-d * d / (2 * 45 * 45)))
        }
        var score = (0..<B).map { D[range.lowerBound * B + $0] + prior(range.lowerBound, $0) }
        var back = [[Int]](repeating: [Int](repeating: 0, count: B), count: n)
        for i in 1..<max(n, 1) {
            let t = range.lowerBound + i
            var next = [Float](repeating: -.infinity, count: B)
            for b in 0..<B {
                var best: Float = -.infinity, arg = b
                for k in -maxStep...maxStep {
                    let p = ((b + k) % B + B) % B
                    let v = score[p] - lambda * Float(abs(k))
                    if v > best { best = v; arg = p }
                }
                next[b] = best + D[t * B + b] + prior(t, b)
                back[i][b] = arg
            }
            score = next
        }
        var b = score.indices.max { score[$0] < score[$1] } ?? 0
        var bins = [Int](repeating: 0, count: n)
        for i in stride(from: n - 1, through: 0, by: -1) { bins[i] = b; if i > 0 { b = back[i][b] } }

        // Unwrap, then smooth.
        var yaw = [HeadingGrid.yaw(bin: bins[0])]
        for i in 1..<n {
            var d = HeadingGrid.yaw(bin: bins[i]) - HeadingGrid.yaw(bin: bins[i - 1])
            if d > 180 { d -= 360 } else if d < -180 { d += 360 }
            yaw.append(yaw[i - 1] + d)
        }
        return gaussianSmooth(yaw, sigma: smoothSeconds * Analyzer.rate)
    }

    /// Keyframe times and yaws: one still framing, or a Douglas–Peucker fit of the path.
    public func keyframes(_ yaw: [Double], times: [Double]) -> [(time: Double, yaw: Double)] {
        guard let lo = yaw.min(), let hi = yaw.max(), yaw.count > 1 else {
            return yaw.first.map { [(times[0], $0)] } ?? []
        }
        if hi - lo < stillRange {
            // Weighted toward the middle of the shot, where the action usually is.
            let mid = yaw.sorted()[yaw.count / 2]
            return [(times[0], mid)]
        }
        var keep = Set([0, yaw.count - 1])
        func fit(_ a: Int, _ b: Int) {
            guard b > a + 1 else { return }
            var worst = 0.0, at = a
            for i in (a + 1)..<b {
                let f = (times[i] - times[a]) / (times[b] - times[a])
                let d = abs(yaw[i] - (yaw[a] + (yaw[b] - yaw[a]) * f))
                if d > worst { worst = d; at = i }
            }
            if worst > tolerance { keep.insert(at); fit(a, at); fit(at, b) }
        }
        fit(0, yaw.count - 1)
        return keep.sorted().map { (times[$0], yaw[$0]) }
    }

    /// Pitch from the people near the path (pulled toward the horizon), else level.
    public func pitch(_ dets: [Detection], path: (Double) -> Double, from t0: Double, to t1: Double) -> Double {
        var sum = 0.0, wsum = 0.0
        for d in dets where d.time >= t0 && d.time <= t1 {
            var off = (d.yaw - path(d.time)).truncatingRemainder(dividingBy: 360)
            if off > 180 { off -= 360 } else if off < -180 { off += 360 }
            guard abs(off) < 25 else { continue }
            let w = d.confidence * (d.kind == .face ? 1 : 0.5)
            sum += w * d.pitch; wsum += w
        }
        return wsum > 0 ? min(max(0.7 * sum / wsum, -20), 15) : 0
    }

    /// Mega for wide interest or people close (a group selfie), otherwise Dewarp.
    public func lens(_ D: [Float], cell: Int, dets: [Detection], path: (Double) -> Double, from t0: Double, to t1: Double) -> LensPreset {
        let B = HeadingGrid.bins
        let row = Array(D[(cell * B)..<((cell + 1) * B)])
        let top = row.max() ?? 0
        let wide = top > 0.1 ? Double(row.filter { $0 >= 0.5 * top }.count) * HeadingGrid.binWidth : 0
        let near = dets.filter { d in
            guard d.kind == .face, d.time >= t0, d.time <= t1 else { return false }
            var off = (d.yaw - path(d.time)).truncatingRemainder(dividingBy: 360)
            if off > 180 { off -= 360 } else if off < -180 { off += 360 }
            return abs(off) < 40
        }.map(\.height).max() ?? 0
        return wide >= 100 || near >= 14 ? .mega : .dewarp
    }
}
