import Foundation

/// Directional signals: a time × heading grid (4 Hz × 72 bins of 5°), each cell 0...1.
///
/// Heading is the world yaw relative to where the camera faced at the start of the clip, in
/// degrees, left-positive, the same frame the timeline's heading lane uses and that Lock
/// stabilization (lock time 0) renders in. Bin b covers yaw (b·5 − 180) ..< (b·5 − 175).
public struct HeadingGrid: Codable, Sendable {
    public static let bins = 72
    public static let binWidth = 360.0 / Double(bins)

    public var rate: Double
    public var count: Int
    /// Row-major by time: value(t, b) = channel[t * bins + b].
    public var channels: [Signal: [Float]] = [:]

    public init(rate: Double, count: Int) {
        self.rate = rate
        self.count = count
    }

    public static func bin(yaw: Double) -> Int {
        var y = (yaw + 180).truncatingRemainder(dividingBy: 360)
        if y < 0 { y += 360 }
        return min(Int(y / binWidth), bins - 1)
    }

    /// Centre yaw of bin b, in -180..<180.
    public static func yaw(bin b: Int) -> Double { (Double(b) + 0.5) * binWidth - 180 }

    public subscript(_ s: Signal) -> [Float] {
        get { channels[s] ?? [Float](repeating: 0, count: count * HeadingGrid.bins) }
        set { channels[s] = newValue }
    }

    /// Weighted sum of the directional channels, smoothed over `sigma` seconds and one bin of
    /// heading (wrapping at ±180°).
    public func fused(_ w: InterestWeights, sigma: Double = 0.5) -> [Float] {
        let B = HeadingGrid.bins
        var sum = [Float](repeating: 0, count: count * B)
        for (s, v) in channels where w[s] != 0 {
            let k = Float(w[s])
            for i in 0..<min(v.count, sum.count) { sum[i] += k * v[i] }
        }
        // Heading: [1 2 1]/4 with wrap.
        var out = sum
        for t in 0..<count {
            let o = t * B
            for b in 0..<B {
                out[o + b] = 0.25 * sum[o + (b + B - 1) % B] + 0.5 * sum[o + b] + 0.25 * sum[o + (b + 1) % B]
            }
        }
        // Time: Gaussian per bin.
        let r = Int(ceil(sigma * rate * 3)), s2 = 2 * sigma * rate * sigma * rate
        guard r > 0, count > 1 else { return out }
        let kernel = (-r...r).map { Float(exp(-Double($0 * $0) / s2)) }
        var smoothed = out
        for t in 0..<count {
            for b in 0..<B {
                var acc: Float = 0, wsum: Float = 0
                for (k, kv) in kernel.enumerated() {
                    let j = min(max(t + k - r, 0), count - 1)
                    acc += out[j * B + b] * kv; wsum += kv
                }
                smoothed[t * B + b] = acc / wsum
            }
        }
        return smoothed
    }

    // Stored as 8-bit quantized base64: a 6-minute clip is ~300 KB instead of several MB of JSON.
    enum CodingKeys: String, CodingKey { case rate, count, channels }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        rate = try c.decode(Double.self, forKey: .rate)
        count = try c.decode(Int.self, forKey: .count)
        let raw = try c.decode([Signal: String].self, forKey: .channels)
        for (s, b64) in raw {
            guard let d = Data(base64Encoded: b64) else { continue }
            channels[s] = d.map { Float($0) / 255 }
        }
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(rate, forKey: .rate)
        try c.encode(count, forKey: .count)
        var raw: [Signal: String] = [:]
        for (s, v) in channels { raw[s] = Data(v.map { UInt8(min(max($0, 0), 1) * 255 + 0.5) }).base64EncodedString() }
        try c.encode(raw, forKey: .channels)
    }
}

/// A person or face found in one of the detector views, in the heading grid's frame.
public struct Detection: Codable, Sendable, Hashable {
    public enum Kind: String, Codable, Sendable { case face, person }
    public var kind: Kind
    public var time: Double
    /// Degrees, left-positive, relative to the clip-start heading, in -180..<180.
    public var yaw: Double
    public var pitch: Double
    /// Angular size in degrees.
    public var width: Double
    public var height: Double
    public var confidence: Double
}
