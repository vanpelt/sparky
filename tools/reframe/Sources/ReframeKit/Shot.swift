import Foundation

/// Where the virtual camera looks, relative to the stabilized view base, and through what lens.
public struct Framing: Codable, Hashable, Sendable {
    public var yaw: Double
    public var pitch: Double
    /// Field of view in degrees, as Insta360 Studio defines it: the rectilinear angle that sets
    /// the focal length. With distortion > 0 the frame actually covers more (see `coverage`).
    public var hfov: Double
    /// Projection: 0 = rectilinear (straight lines stay straight) ... 1 = stereographic.
    /// This is the unified camera model's xi, i.e. the virtual camera pulled back from the
    /// sphere's centre by `distortion` radii.
    public var distortion: Double

    public init(yaw: Double, pitch: Double, hfov: Double, distortion: Double = LensPreset.dewarp.distortion) {
        self.yaw = yaw
        self.pitch = pitch
        self.hfov = hfov
        self.distortion = distortion
    }

    public init(yaw: Double, pitch: Double, preset: LensPreset) {
        self.init(yaw: yaw, pitch: pitch, hfov: preset.fov, distortion: preset.distortion)
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        yaw = try c.decode(Double.self, forKey: .yaw)
        pitch = try c.decode(Double.self, forKey: .pitch)
        hfov = try c.decode(Double.self, forKey: .hfov)
        distortion = try c.decodeIfPresent(Double.self, forKey: .distortion) ?? 0  // shots saved before presets
    }

    /// Horizontal angle the frame really covers, in degrees (equals hfov when distortion is 0).
    public var coverage: Double {
        let t = tan(hfov * .pi / 360), d = distortion
        let k = (d + sqrt(1 + (1 - d * d) * t * t)) / (t * t + 1)
        return 2 * atan2(k * t, k - d) * 180 / .pi
    }

    /// Vertical angle covered by a frame of `aspect` = height / width (e.g. 9/16).
    public func verticalCoverage(aspect: Double) -> Double {
        let t = tan(hfov * .pi / 360) * aspect, d = distortion
        let k = (d + sqrt(1 + (1 - d * d) * t * t)) / (t * t + 1)
        return 2 * atan2(k * t, k - d) * 180 / .pi
    }

    public var preset: LensPreset? { LensPreset.allCases.first { $0.matches(self) } }

    public mutating func apply(_ p: LensPreset) {
        hfov = p.fov
        distortion = p.distortion
    }
}

/// Insta360 Studio's reframing lenses (field of view, distortion).
public enum LensPreset: String, CaseIterable, Identifiable, Codable, Sendable {
    case mega, ultra, dewarp, linear
    public var id: String { rawValue }
    public var label: String {
        switch self {
        case .mega: return "Mega"
        case .ultra: return "Ultra"
        case .dewarp: return "Dewarp"
        case .linear: return "Linear"
        }
    }
    public var fov: Double {
        switch self {
        case .mega: return 80
        case .ultra: return 69.5
        case .dewarp: return 88
        case .linear: return 110
        }
    }
    public var distortion: Double {
        switch self {
        case .mega, .ultra: return 0.95
        case .dewarp: return 0.40
        case .linear: return 0
        }
    }
    public var framing: Framing { Framing(yaw: 0, pitch: 0, preset: self) }
    public func matches(_ f: Framing) -> Bool { abs(f.hfov - fov) < 0.05 && abs(f.distortion - distortion) < 0.005 }
}

public struct Keyframe: Codable, Hashable, Sendable, Identifiable {
    public var id = UUID()
    /// Seconds on the clip's timeline.
    public var time: Double
    public var framing: Framing

    public init(time: Double, framing: Framing) {
        self.time = time
        self.framing = framing
    }
}

/// A select: a time range of one clip with a framing path, ready to export.
public struct Shot: Codable, Hashable, Sendable, Identifiable {
    public var id = UUID()
    public var clipPath: String
    public var name: String
    public var inTime: Double
    public var outTime: Double
    /// Sorted by time; never empty.
    public var keyframes: [Keyframe]
    public var stabilization: Motion.Stabilization
    public var smoothing: Double
    public var lockTime: Double
    public var created = Date()
    /// The suggestion this shot was kept from, if any (nil for shots you marked yourself).
    public var suggestionID: String?
    /// The path as suggested, kept for evaluation: how far you re-aimed it.
    public var suggestedKeyframes: [Keyframe]?

    public init(clipPath: String, name: String, inTime: Double, outTime: Double, framing: Framing,
                stabilization: Motion.Stabilization, smoothing: Double, lockTime: Double) {
        self.clipPath = clipPath
        self.name = name
        self.inTime = inTime
        self.outTime = outTime
        keyframes = [Keyframe(time: inTime, framing: framing)]
        self.stabilization = stabilization
        self.smoothing = smoothing
        self.lockTime = lockTime
    }

    public var duration: Double { max(outTime - inTime, 0) }

    public func contains(_ t: Double) -> Bool { t >= inTime - 1e-6 && t <= outTime + 1e-6 }

    /// Add or replace the keyframe nearest `time` (within half a frame at 48 fps).
    public mutating func setKeyframe(at time: Double, framing: Framing) {
        var framing = framing
        framing.yaw = framing.yaw.truncatingRemainder(dividingBy: 360)
        if framing.yaw > 180 { framing.yaw -= 360 } else if framing.yaw < -180 { framing.yaw += 360 }
        if let i = keyframes.firstIndex(where: { abs($0.time - time) < 0.011 }) {
            keyframes[i].framing = framing
        } else {
            keyframes.append(Keyframe(time: time, framing: framing))
            keyframes.sort { $0.time < $1.time }
        }
    }

    /// Keyframes inside the shot's range (trimming keeps the others, so the path is unchanged).
    public var keyframesInRange: [Keyframe] { keyframes.filter { contains($0.time) } }

    /// Merge keyframes closer than `spacing` seconds: each run becomes one keyframe at its first
    /// time with its last framing (the later one is usually the edit that was meant).
    public mutating func mergeCloseKeyframes(within spacing: Double) {
        var out: [Keyframe] = []
        for k in keyframes {
            if let last = out.last, k.time - last.time < spacing {
                out[out.count - 1].framing = k.framing
            } else {
                out.append(k)
            }
        }
        keyframes = out
    }

    public mutating func removeKeyframe(_ id: Keyframe.ID) {
        guard keyframes.count > 1 else { return }
        keyframes.removeAll { $0.id == id }
    }

    /// Smooth framing path through the keyframes: cubic Hermite with finite-difference tangents
    /// (eases in and out of the first and last keyframe), yaw unwrapped so it takes the short way.
    public func framing(at t: Double) -> Framing {
        let k = keyframes
        guard let first = k.first else { return LensPreset.dewarp.framing }
        if k.count == 1 || t <= first.time { return first.framing }
        if t >= k[k.count - 1].time { return k[k.count - 1].framing }

        var yaws = [first.framing.yaw]
        for i in 1..<k.count {
            var d = (k[i].framing.yaw - k[i - 1].framing.yaw).truncatingRemainder(dividingBy: 360)
            if d > 180 { d -= 360 } else if d < -180 { d += 360 }
            yaws.append(yaws[i - 1] + d)
        }
        let i = (k.lastIndex { $0.time <= t } ?? 0)
        let t0 = k[i].time, t1 = k[i + 1].time, h = t1 - t0
        let u = (t - t0) / h
        func tangent(_ j: Int, _ v: (Int) -> Double) -> Double {
            if j == 0 || j == k.count - 1 { return 0 }
            return (v(j + 1) - v(j - 1)) / (k[j + 1].time - k[j - 1].time)
        }
        func hermite(_ v: (Int) -> Double) -> Double {
            let u2 = u * u, u3 = u2 * u
            return (2 * u3 - 3 * u2 + 1) * v(i) + (u3 - 2 * u2 + u) * h * tangent(i, v)
                + (-2 * u3 + 3 * u2) * v(i + 1) + (u3 - u2) * h * tangent(i + 1, v)
        }
        var yaw = hermite { yaws[$0] }.truncatingRemainder(dividingBy: 360)
        if yaw > 180 { yaw -= 360 } else if yaw < -180 { yaw += 360 }
        return Framing(yaw: yaw,
                       pitch: min(max(hermite { k[$0].framing.pitch }, -89), 89),
                       hfov: min(max(hermite { k[$0].framing.hfov }, 20), 160),
                       distortion: min(max(hermite { k[$0].framing.distortion }, 0), 1))
    }
}

extension Motion.Stabilization: Codable {}

/// Shots for one footage folder, saved in Application Support (the footage folder is never written).
public struct ShotStore: Codable, Sendable {
    public var version = 1
    public var shots: [Shot] = []
    /// Suggestion IDs the user dismissed, so they don't come back after re-analysis.
    public var dismissed: Set<String> = []

    public init() {}

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        version = try c.decodeIfPresent(Int.self, forKey: .version) ?? 1
        shots = try c.decodeIfPresent([Shot].self, forKey: .shots) ?? []
        dismissed = try c.decodeIfPresent(Set<String>.self, forKey: .dismissed) ?? []
    }

    static func url(for folder: URL) -> URL {
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Reframe/projects", isDirectory: true)
        let name = folder.standardizedFileURL.path.replacingOccurrences(of: "/", with: "_")
        return base.appendingPathComponent(String(name.suffix(180)) + ".json")
    }

    public static func load(folder: URL) -> ShotStore {
        guard let data = try? Data(contentsOf: url(for: folder)),
              let store = try? JSONDecoder().decode(ShotStore.self, from: data) else { return ShotStore() }
        return store
    }

    public func save(folder: URL) {
        let u = ShotStore.url(for: folder)
        try? FileManager.default.createDirectory(at: u.deletingLastPathComponent(), withIntermediateDirectories: true)
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys]
        try? enc.encode(self).write(to: u, options: .atomic)
    }
}
