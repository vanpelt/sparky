import Foundation
import simd

/// Camera orientation from the IMU (port of motion.py).
/// Orientation is body -> world. Body: +X forward (lens-0 side), +Y left, +Z up; world: Z up.
public final class Motion: @unchecked Sendable {
    public let times: [Double]  // video-timeline seconds of each IMU sample
    let quats: [simd_quatd]
    let omega: [SIMD3<Double>]  // body rad/s

    // Heading grid for follow mode, sampled at `gridRate`.
    private let gridRate = 200.0
    private var gridStart = 0.0
    private var headingGrid: [Double] = []
    private var smoothedCache: (sigma: Double, values: [Double])?
    private let lock = NSLock()

    /// X5 raw IMU axes -> body (per insta360-rs); verified on X6 for gravity and gyro.
    @inline(__always) static func imuToBody(_ v: SIMD3<Double>) -> SIMD3<Double> { SIMD3(v.y, v.z, v.x) }

    public init(info: InsvInfo, imu: IMUData, kp: Double = 0.6) {
        // Camera clock -> video time. Field 28 (gyro adjust) is deliberately not applied: X6
        // footage was steadiest without it. Timelapse: capture time runs interval*fps faster.
        let base = Double(info.firstFrameTimestamp ?? 0) / (info.rawIMU ? 1e6 : 1e3)
        var speed = 1.0
        if let iv = info.timelapseIntervalMs, let fps = info.nominalFPS, iv > 0, fps > 0 {
            speed = Double(iv) / 1000 * Double(fps)
        }
        let t = imu.time.map { ($0 - base) / speed }
        let acc = imu.accel.map(Motion.imuToBody)
        let gyr = imu.gyro.map(Motion.imuToBody)

        var q = simd_quatd(ix: 0, iy: 0, iz: 0, r: 1)
        if !t.isEmpty {
            var up = SIMD3<Double>.zero
            for k in 0..<t.count where t[k] < t[0] + 0.5 { up += acc[k] }
            if simd_length(up) > 0 { q = simd_quatd(from: simd_normalize(up), to: [0, 0, 1]) }
        }
        var quats = [simd_quatd](repeating: q, count: t.count)
        for k in 1..<max(t.count, 1) {
            let dt = t[k] - t[k - 1]
            var w = gyr[k] / speed  // angular velocity in video time
            let a = acc[k], n = simd_length(a)
            if kp > 0, n > 0.85, n < 1.15 {
                let upEst = q.inverse.act([0, 0, 1])
                w += kp * simd_cross(a / n, upEst)
            }
            let angle = simd_length(w) * dt
            if angle > 1e-12 { q = simd_normalize(q * simd_quatd(angle: angle, axis: simd_normalize(w))) }
            quats[k] = q
        }
        times = t
        self.quats = quats
        omega = gyr.map { $0 / speed }

        if let first = t.first, let last = t.last, last > first {
            gridStart = first
            let n = Int((last - first) * gridRate) + 1
            var grid = [Double](repeating: 0, count: n)
            var prev = 0.0, offset = 0.0
            for i in 0..<n {
                let f = orientation(at: first + Double(i) / gridRate).act([1, 0, 0])
                let y = atan2(f.y, f.x)
                if i > 0 {
                    let d = y + offset - prev
                    if d > .pi { offset -= 2 * .pi } else if d < -.pi { offset += 2 * .pi }
                }
                prev = y + offset
                grid[i] = prev
            }
            headingGrid = grid
        }
    }

    public convenience init?(file: InsvFile) {
        guard let imu = try? file.readIMU(), !imu.time.isEmpty else { return nil }
        self.init(info: file.info, imu: imu)
    }

    private func index(at t: Double) -> Int {
        var lo = 0, hi = times.count - 1
        while lo < hi {
            let mid = (lo + hi + 1) / 2
            if times[mid] <= t { lo = mid } else { hi = mid - 1 }
        }
        return lo
    }

    public func orientation(at t: Double) -> simd_quatd {
        guard let first = times.first, let last = times.last else { return simd_quatd(ix: 0, iy: 0, iz: 0, r: 1) }
        if t <= first { return quats[0] }
        if t >= last { return quats[quats.count - 1] }
        let i = index(at: t)
        let f = (t - times[i]) / (times[i + 1] - times[i])
        return simd_slerp(quats[i], quats[i + 1], f)
    }

    /// Mean body angular velocity within +-window of t (rad/s).
    public func angularVelocity(at t: Double, window: Double = 0.004) -> SIMD3<Double> {
        guard !times.isEmpty else { return .zero }
        var i = index(at: t - window)
        var sum = SIMD3<Double>.zero, n = 0.0
        while i < times.count, times[i] <= t + window {
            if times[i] >= t - window { sum += omega[i]; n += 1 }
            i += 1
        }
        return n > 0 ? sum / n : .zero
    }

    /// Heading (radians, unwrapped) of the camera's forward axis, optionally Gaussian-smoothed.
    public func heading(at t: Double, smoothing: Double) -> Double {
        guard !headingGrid.isEmpty else { return 0 }
        let values: [Double]
        if smoothing <= 0 {
            values = headingGrid
        } else {
            lock.lock()
            if let c = smoothedCache, c.sigma == smoothing {
                values = c.values
            } else {
                values = gaussianSmooth(headingGrid, sigma: smoothing * gridRate)
                smoothedCache = (smoothing, values)
            }
            lock.unlock()
        }
        let x = min(max((t - gridStart) * gridRate, 0), Double(values.count - 1))
        let i = Int(x), j = min(i + 1, values.count - 1)
        return values[i] + (values[j] - values[i]) * (x - Double(i))
    }

    public enum Stabilization: String, CaseIterable, Identifiable, Sendable {
        case none, follow, lock
        public var id: String { rawValue }
        public var label: String {
            switch self {
            case .none: return "Off"
            case .follow: return "Follow"
            case .lock: return "Lock"
            }
        }
    }

    /// World <- view-base rotation for a frame, before the user's yaw/pitch/roll.
    public func viewBase(at t: Double, mode: Stabilization, smoothing: Double, lockTime: Double) -> simd_double3x3 {
        switch mode {
        case .none:
            // The calibration body frame is upside down relative to an upright X6.
            return simd_double3x3(orientation(at: t)) * simd_double3x3(diagonal: [1, -1, -1])
        case .lock:
            return rot(.z, heading(at: lockTime, smoothing: 0) * 180 / .pi)
        case .follow:
            return rot(.z, heading(at: t, smoothing: smoothing) * 180 / .pi)
        }
    }

    /// Heading (degrees, unwrapped, left-positive) of the view base for a mode, matching viewBase.
    public func baseHeading(at t: Double, mode: Stabilization, smoothing: Double, lockTime: Double) -> Double {
        switch mode {
        case .follow: return heading(at: t, smoothing: smoothing) * 180 / .pi
        case .lock: return heading(at: lockTime, smoothing: 0) * 180 / .pi
        case .none: return heading(at: t, smoothing: 0) * 180 / .pi
        }
    }

    /// Angular speed (deg/s) in `bins` equal time bins over [0, duration], each the bin's 90th percentile.
    public func speedProfile(bins: Int, duration: Double) -> [Double] {
        guard !times.isEmpty, duration > 0, bins > 0 else { return [] }
        var buckets = [[Double]](repeating: [], count: bins)
        let stride = max(1, times.count / (bins * 40))
        for i in Swift.stride(from: 0, to: times.count, by: stride) {
            let b = Int(times[i] / duration * Double(bins))
            if b >= 0 && b < bins { buckets[b].append(simd_length(omega[i]) * 180 / .pi) }
        }
        return buckets.map { b in
            guard !b.isEmpty else { return 0 }
            let s = b.sorted()
            return s[Int(Double(s.count - 1) * 0.9)]
        }
    }

    /// Body <- camera rotation for rendering: R_body(t)^T * viewBase * userRotation.
    public func bodyFromCamera(at t: Double, mode: Stabilization, smoothing: Double, lockTime: Double,
                               yaw: Double, pitch: Double, roll: Double) -> simd_double3x3 {
        let user = rot(.z, yaw) * rot(.y, -pitch) * rot(.x, roll)
        let body = simd_double3x3(orientation(at: t))
        return body.transpose * viewBase(at: t, mode: mode, smoothing: smoothing, lockTime: lockTime) * user
    }

    /// Median and 95th-percentile angular speed (deg/s): a cheap "how shaky is this" signal.
    public func shakiness() -> (median: Double, p95: Double) {
        guard !omega.isEmpty else { return (0, 0) }
        let stride = max(1, omega.count / 5000)
        var speeds: [Double] = []
        for i in Swift.stride(from: 0, to: omega.count, by: stride) { speeds.append(simd_length(omega[i]) * 180 / .pi) }
        speeds.sort()
        return (speeds[speeds.count / 2], speeds[Int(Double(speeds.count - 1) * 0.95)])
    }
}

func gaussianSmooth(_ x: [Double], sigma: Double) -> [Double] {
    guard sigma > 0.5, x.count > 1 else { return x }
    let r = Int(ceil(sigma * 3))
    let kernel = (-r...r).map { exp(-Double($0 * $0) / (2 * sigma * sigma)) }
    var out = [Double](repeating: 0, count: x.count)
    for i in 0..<x.count {
        var s = 0.0, w = 0.0
        for (k, kv) in kernel.enumerated() {
            let j = min(max(i + k - r, 0), x.count - 1)  // "nearest" edge mode
            s += x[j] * kv; w += kv
        }
        out[i] = s / w
    }
    return out
}
