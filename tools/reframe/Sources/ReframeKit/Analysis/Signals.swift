import Foundation

/// One input to the interest map. Most say only *when* something happened; the directional ones
/// (faces, people, motion) also say *where* (see INTEREST_MAP.md).
public enum Signal: String, CaseIterable, Codable, Sendable {
    // Directional (time × heading)
    case faces, people, motion, salience
    // Audio
    case loudness, onset, laughter, cheering, shouting, speech, music, wind
    // Camera motion (IMU)
    case jump, impact, turn, calm, shake
    // Scenery
    case novelty
    // Junk
    case dark, covered, handling

    /// How a reason reads in the UI ("laughter, jump").
    public var phrase: String {
        switch self {
        case .faces: return "faces"
        case .people: return "people"
        case .motion: return "movement"
        case .salience: return "stands out"
        case .novelty: return "new scenery"
        case .loudness: return "loud"
        case .onset: return "sudden sounds"
        case .laughter: return "laughter"
        case .cheering: return "cheering"
        case .shouting: return "shouting"
        case .speech: return "voices"
        case .music: return "music"
        case .wind: return "wind noise"
        case .jump: return "jump"
        case .impact: return "impact"
        case .turn: return "camera turned"
        case .calm: return "steady"
        case .shake: return "shaky"
        case .dark: return "dark"
        case .covered: return "lens covered"
        case .handling: return "handling"
        }
    }

    public var isDirectional: Bool { self == .faces || self == .people || self == .motion || self == .salience }

    public var isAudio: Bool {
        switch self {
        case .loudness, .onset, .laughter, .cheering, .shouting, .speech, .music, .wind: return true
        default: return false
        }
    }
}

/// Signal weights: the one place to tune how signals combine. Negative weights are penalties.
public struct InterestWeights: Codable, Sendable, Hashable {
    public var values: [Signal: Double]

    public init(_ values: [Signal: Double]) { self.values = values }

    public subscript(_ s: Signal) -> Double { values[s] ?? 0 }

    public enum Preset: String, CaseIterable, Codable, Sendable {
        case blend, people, action, scenery
    }

    public static func preset(_ p: Preset) -> InterestWeights {
        var w: [Signal: Double] = [
            .faces: 0.8, .people: 0.35, .motion: 0.5, .salience: 0.2, .novelty: 0.4,
            .laughter: 1.0, .cheering: 0.9, .shouting: 0.7, .speech: 0.2, .music: 0.1,
            .loudness: 0.35, .onset: 0.25,
            .jump: 0.9, .impact: 0.35, .turn: 0.2, .calm: 0.08,
            .shake: -0.45, .dark: -1.0, .covered: -0.6, .handling: -0.6, .wind: -0.15,
        ]
        switch p {
        case .blend: break
        case .people:
            w[.faces] = 1.1; w[.people] = 0.5; w[.motion] = 0.3; w[.salience] = 0.1; w[.novelty] = 0.2
            w[.laughter] = 1.2; w[.speech] = 0.6; w[.cheering] = 1.0
            w[.jump] = 0.4; w[.impact] = 0.15; w[.loudness] = 0.25
        case .action:
            w[.motion] = 0.9; w[.faces] = 0.5; w[.salience] = 0.1; w[.novelty] = 0.3
            w[.jump] = 1.2; w[.impact] = 0.6; w[.onset] = 0.45; w[.loudness] = 0.5
            w[.speech] = 0.1; w[.shake] = -0.25
        case .scenery:
            w[.faces] = 0.3; w[.people] = 0.15; w[.motion] = 0.3; w[.salience] = 0.7; w[.novelty] = 0.9
            w[.calm] = 0.5; w[.turn] = 0.35; w[.speech] = 0.0; w[.loudness] = 0.05; w[.onset] = 0.0
            w[.laughter] = 0.4; w[.cheering] = 0.4; w[.jump] = 0.3; w[.shake] = -0.7
        }
        return InterestWeights(w)
    }

    public static let `default` = preset(.blend)
}

/// Time-only signals on a fixed grid, each normalized to 0...1.
public struct TimeSignals: Codable, Sendable {
    /// Samples per second.
    public var rate: Double
    public var count: Int
    public var channels: [Signal: [Float]] = [:]

    public init(rate: Double, duration: Double) {
        self.rate = rate
        count = max(Int((duration * rate).rounded(.up)), 1)
    }

    public func time(_ i: Int) -> Double { (Double(i) + 0.5) / rate }
    public func index(_ t: Double) -> Int { min(max(Int(t * rate), 0), count - 1) }

    public subscript(_ s: Signal) -> [Float] {
        get { channels[s] ?? [Float](repeating: 0, count: count) }
        set { channels[s] = newValue }
    }

    /// Weighted contributions of each signal at every sample.
    public func contributions(_ w: InterestWeights) -> [Signal: [Float]] {
        var out: [Signal: [Float]] = [:]
        for (s, v) in channels where w[s] != 0 { out[s] = v.map { $0 * Float(w[s]) } }
        return out
    }

    /// Fused interest: the weighted sum, smoothed over `sigma` seconds.
    public func fused(_ w: InterestWeights, sigma: Double = 0.5) -> [Float] {
        var sum = [Float](repeating: 0, count: count)
        for (s, v) in channels where w[s] != 0 {
            let k = Float(w[s])
            for i in 0..<min(v.count, count) { sum[i] += k * v[i] }
        }
        return gaussianSmooth(sum.map(Double.init), sigma: sigma * rate).map(Float.init)
    }
}

// MARK: - Small numeric helpers

func percentile(_ x: [Float], _ p: Double) -> Float {
    guard !x.isEmpty else { return 0 }
    let s = x.sorted()
    return s[min(max(Int(Double(s.count - 1) * p), 0), s.count - 1)]
}

@inline(__always) func clamp01(_ x: Float) -> Float { min(max(x, 0), 1) }

/// Running max over ±r samples (spreads short events so they register on the grid).
func dilate(_ x: [Float], radius r: Int) -> [Float] {
    guard r > 0 else { return x }
    return x.indices.map { i in x[max(0, i - r)...min(x.count - 1, i + r)].max() ?? 0 }
}
