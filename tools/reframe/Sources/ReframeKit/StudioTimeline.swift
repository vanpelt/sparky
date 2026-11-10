import Foundation
import simd

/// Reads an Insta360 Studio timeline (.insproj) back into seconds and degrees, so a project edited
/// in Studio can be inspected and previewed with Reframe's renderer. `StudioProject` writes the same
/// format; its header has the mapping. In short: every frame number counts at the project's rate,
/// keyframe `time` is a source frame, and keyframe pan is a world direction whose zero is the
/// camera's heading on the file's first frame.
public struct StudioTimeline: Codable, Sendable {
    public var file: String
    public var name: String
    public var fps: Double
    /// Studio's project length in frames (it grows to the longest track, music included).
    public var frames: Int
    public var video: [Clip]
    public var audio: [Clip]
    /// Clips on effect tracks (Studio's track types other than video 0 and music 2), such as a
    /// "Fade out" at the end.
    public var effects: [Effect]

    public struct Effect: Codable, Sendable {
        public var name: String
        public var trackType: Int
        public var timelineIn: Double
        public var timelineOut: Double
    }

    /// The end of the last video clip, in seconds.
    public var videoEnd: Double { video.map(\.timelineOut).max() ?? 0 }

    public struct Keyframe: Codable, Sendable {
        /// Studio's `time`: the source frame counted on the clip's stretched clock (source frame ÷ the
        /// clip's speed), and real seconds into the source file.
        public var sourceFrame: Int
        public var sourceTime: Double
        /// Degrees: pan left-positive from the file's first-frame heading, tilt up-positive.
        public var pan: Double
        public var tilt: Double
        public var roll: Double
        /// Horizontal field of view (degrees) of the 16:9 frame; Studio stores the vertical one.
        public var hfov: Double
        /// Studio's lens pull-back (Reframe's distortion).
        public var distance: Double
    }

    /// The view Studio shows at one moment of a clip.
    public struct View: Codable, Sendable {
        public var pan, tilt, roll, hfov, distance: Double
        public init(pan: Double, tilt: Double, roll: Double, hfov: Double, distance: Double) {
            self.pan = pan; self.tilt = tilt; self.roll = roll; self.hfov = hfov; self.distance = distance
        }
    }

    public struct Clip: Codable, Sendable {
        public var index: Int
        public var id: String
        public var name: String
        /// Studio's media reference: a file path, or an Insta360 Cloud media ID.
        public var url: String
        public var isCloud: Bool
        /// Frames on the timeline; for a clip with a speed change the trims and length count on its
        /// stretched clock (a 0.25× clip's `sourceFrames` is 4× the source's).
        public var startFrame: Int
        public var leftTrim: Int
        public var rightTrim: Int
        public var sourceFrames: Int
        /// Playback speed (0.25 = four times slower); 1 without a speed change.
        public var rate: Double
        /// Seconds on the timeline, and real seconds into the source file.
        public var timelineIn: Double
        public var timelineOut: Double
        public var sourceIn: Double
        public var sourceOut: Double
        public var keyframes: [Keyframe]
        /// True when an easing node between keyframes isn't Studio's default (linear); previews then
        /// only approximate the move.
        public var customEasing: Bool
        public var stabilized: Bool
        public var directionalLock: Bool
        /// Set when a speed change (play_rate_track) is on the clip.
        public var speedChanged: Bool
        /// Set when the speed varies within the clip (a ramp); `rate` is then only its first speed.
        public var variableSpeed: Bool
        public var volume: Int?
        /// Audio fade lengths in frames (Studio's `currentFadeIn/Out`).
        public var fadeIn: Int = 0
        public var fadeOut: Int = 0
        /// The view without keyframes (camera_transform), used when there are none.
        public var baseView: View?

        public var duration: Double { timelineOut - timelineIn }

        enum CodingKeys: String, CodingKey {
            case index, id, name, url, isCloud, startFrame, leftTrim, rightTrim, sourceFrames, rate, timelineIn,
                 timelineOut, sourceIn, sourceOut, keyframes, customEasing, stabilized, directionalLock,
                 speedChanged, variableSpeed, volume, fadeIn, fadeOut, baseView
        }

        /// Studio's view at a source time: keyframes interpolated linearly, held past either end.
        public func view(atSource t: Double) -> View {
            guard let first = keyframes.first, let last = keyframes.last else {
                return baseView ?? View(pan: 0, tilt: 0, roll: 0, hfov: 88, distance: 0.4)
            }
            func v(_ k: Keyframe) -> View { View(pan: k.pan, tilt: k.tilt, roll: k.roll, hfov: k.hfov, distance: k.distance) }
            if t <= first.sourceTime { return v(first) }
            if t >= last.sourceTime { return v(last) }
            let j = keyframes.firstIndex { $0.sourceTime > t }!
            let a = keyframes[j - 1], b = keyframes[j]
            let u = (t - a.sourceTime) / max(b.sourceTime - a.sourceTime, 1e-9)
            func mix(_ x: Double, _ y: Double) -> Double { x + (y - x) * u }
            // Studio interpolates the vertical FOV; do the same, then convert.
            let vf = mix(StudioTimeline.verticalFOV(a.hfov), StudioTimeline.verticalFOV(b.hfov))
            return View(pan: mix(a.pan, b.pan), tilt: mix(a.tilt, b.tilt), roll: mix(a.roll, b.roll),
                        hfov: StudioTimeline.horizontalFOV(vf), distance: mix(a.distance, b.distance))
        }

        /// Source time for a timeline time inside this clip.
        public func sourceTime(atTimeline t: Double) -> Double { sourceIn + (t - timelineIn) * rate }

        /// Timeline time for a source time (may fall outside the clip).
        public func timelineTime(atSource t: Double) -> Double { timelineIn + (t - sourceIn) / rate }
    }

    public enum Failure: Error, CustomStringConvertible {
        case notFound(String), unreadable(URL)
        public var description: String {
            switch self {
            case .notFound(let s): return "no Studio project \"\(s)\" (looked in \(StudioProject.timelineRoot.path))"
            case .unreadable(let u): return "couldn't read \(u.path) as a Studio project"
            }
        }
    }

    /// The .insproj for a project name, project folder or file path.
    public static func locate(_ ref: String) throws -> URL {
        let fm = FileManager.default
        var candidates = [URL(fileURLWithPath: (ref as NSString).expandingTildeInPath)]
        candidates.append(StudioProject.timelineRoot.appendingPathComponent(ref, isDirectory: true))
        for c in candidates {
            var isDir: ObjCBool = false
            guard fm.fileExists(atPath: c.path, isDirectory: &isDir) else { continue }
            if !isDir.boolValue { return c }
            let files = (try? fm.contentsOfDirectory(at: c, includingPropertiesForKeys: nil)) ?? []
            if let f = files.first(where: { $0.pathExtension == "insproj" && !$0.lastPathComponent.hasSuffix("_backup.insproj") }) {
                return f
            }
        }
        throw Failure.notFound(ref)
    }

    /// Every timeline project in Studio's folder, newest first.
    public static func all() -> [URL] {
        let fm = FileManager.default
        let dirs = (try? fm.contentsOfDirectory(at: StudioProject.timelineRoot, includingPropertiesForKeys: [.contentModificationDateKey])) ?? []
        let files = dirs.compactMap { try? locate($0.path) }
        func date(_ u: URL) -> Date { (try? u.resourceValues(forKeys: [.contentModificationDateKey]).contentModificationDate) ?? .distantPast }
        return files.sorted { date($0) > date($1) }
    }

    public init(url: URL) throws {
        guard let data = try? Data(contentsOf: url),
              let o = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw Failure.unreadable(url) }
        file = url.path
        name = o["name"] as? String ?? url.deletingLastPathComponent().lastPathComponent
        fps = Self.num(o["fps"]) ?? 30
        frames = Int(Self.num(o["frames"]) ?? 0)
        let tracks = (o["tracks"] as? [[String: Any]] ?? []).sorted { (Self.num($0["z"]) ?? 0) > (Self.num($1["z"]) ?? 0) }
        var video: [Clip] = [], audio: [Clip] = [], effects: [Effect] = []
        for t in tracks {
            let type = Int(Self.num(t["type"]) ?? 0)
            for c in t["clips"] as? [[String: Any]] ?? [] {
                switch type {
                case 0: video.append(Self.clip(c, fps: fps, index: 0))
                case 2: audio.append(Self.clip(c, fps: fps, index: audio.count + 1))
                default:
                    let e = Self.clip(c, fps: fps, index: 0)
                    effects.append(Effect(name: e.name, trackType: type, timelineIn: e.timelineIn, timelineOut: e.timelineOut))
                }
            }
        }
        self.effects = effects.sorted { $0.timelineIn < $1.timelineIn }
        video.sort { $0.startFrame < $1.startFrame }
        for i in video.indices { video[i].index = i + 1 }
        self.video = video
        self.audio = audio
    }

    static func clip(_ c: [String: Any], fps: Double, index: Int) -> Clip {
        let start = Int(num(c["startFrame"]) ?? 0), left = Int(num(c["leftTrim"]) ?? 0)
        let right = Int(num(c["rightTrim"]) ?? 0), total = Int(num(c["sourceFrames"]) ?? 0)
        let length = max(total - left - right, 0)
        let (rate, variable) = playRate(c)
        var keys: [Keyframe] = []
        var eased = false
        for n in (c["key_frame_track"] as? [String: Any])?["node_list"] as? [[String: Any]] ?? [] {
            if Int(num(n["node_type"]) ?? 0) == 1 {
                // Studio's default easing node has both handles at 0.5 (a straight line).
                if ["point1X", "point1Y", "point2X", "point2Y"].contains(where: { abs((num(n[$0]) ?? 0.5) - 0.5) > 1e-6 }) { eased = true }
                continue
            }
            let f = Int(num(n["time"]) ?? 0)
            keys.append(Keyframe(sourceFrame: f, sourceTime: Double(f) * rate / fps,
                                 pan: deg(n["pan"]), tilt: deg(n["tilt"]), roll: deg(n["roll"]),
                                 hfov: horizontalFOV(num(n["fov"]) ?? verticalFOV(88)), distance: num(n["distance"]) ?? 0.4))
        }
        keys.sort { $0.sourceFrame < $1.sourceFrame }
        let stab = c["stab_setting"] as? [String: Any]
        var base: View?
        if let ct = c["camera_transform"] as? [String: Any] {
            let e = ct["eulers"] as? [String: Any] ?? [:]
            func euler(_ k: String) -> Double { deg((e[k] as? [String: Any])?["value"]) }
            let fov = num((ct["fov"] as? [String: Any])?["value"]) ?? verticalFOV(88)
            base = View(pan: euler("yaw"), tilt: euler("pitch"), roll: euler("roll"),
                        hfov: horizontalFOV(fov), distance: num(ct["distance"]) ?? 0.4)
        }
        let url = c["url"] as? String ?? ""
        var clip = Clip(index: index, id: c["id"] as? String ?? "", name: c["name"] as? String ?? "", url: url,
                        isCloud: !url.hasPrefix("/"), startFrame: start, leftTrim: left, rightTrim: right, sourceFrames: total,
                        rate: rate,
                        timelineIn: Double(start) / fps, timelineOut: Double(start + length) / fps,
                        sourceIn: Double(left) * rate / fps, sourceOut: Double(left + length) * rate / fps,
                        keyframes: keys, customEasing: eased,
                        stabilized: stab?["enableStab"] as? Bool ?? true,
                        directionalLock: stab?["enableDirectionalLock"] as? Bool ?? false,
                        speedChanged: rate != 1 || variable, variableSpeed: variable,
                        volume: ((c["audio_setting"] as? [String: Any])?["currentVolume"]).flatMap(num).map { Int($0) })
        clip.baseView = base
        if let a = c["audio_setting"] as? [String: Any] {
            clip.fadeIn = Int(num(a["currentFadeIn"]) ?? 0)
            clip.fadeOut = Int(num(a["currentFadeOut"]) ?? 0)
        }
        return clip
    }

    /// A clip's playback speed from its play_rate_track, and whether it varies. Studio stores the
    /// trims, length and keyframe `time` of a slowed clip on a stretched clock (source frames ÷ speed)
    /// and keeps the real source frames in `source_left_trim`, `source_total_frames` and `src_time`.
    static func playRate(_ c: [String: Any]) -> (Double, Bool) {
        let models = (c["play_rate_track"] as? [String: Any])?["play_rate_model_list"] as? [[String: Any]] ?? []
        let rates = models.flatMap { [num($0["minPlayRate"]), num($0["maxPlayRate"])].compactMap { $0 } }.filter { $0 > 0 }
        guard let first = rates.first else { return (1, false) }
        return (first, rates.contains { abs($0 - first) > 1e-6 })
    }

    /// The video clip under a timeline time, if any.
    public func videoClip(at t: Double) -> Clip? {
        video.last { $0.timelineIn <= t + 1e-9 && t < $0.timelineOut }
    }

    /// The local .insv a clip refers to: its path, or the Reframe cloud cache entry for its media ID.
    public static func localFile(for clip: Clip) -> URL? {
        let fm = FileManager.default
        if !clip.isCloud { return fm.fileExists(atPath: clip.url) ? URL(fileURLWithPath: clip.url) : nil }
        let dir = CloudCache.dir(clip.url)
        if let d = try? Data(contentsOf: dir.appendingPathComponent("cloud.json")),
           let r = try? JSONDecoder().decode(CloudRecord.self, from: d) {
            let u = dir.appendingPathComponent(r.name)
            if fm.fileExists(atPath: u.path) { return u }
        }
        return nil
    }

    /// The camera-from-view rotation that shows Studio's view on a frame at source time `t`.
    /// Keyframed (or direction-locked) clips are world-anchored to the first-frame heading, i.e.
    /// Reframe's Lock mode locked at t = 0; a clip with neither follows the camera's heading.
    public static func bodyFromCamera(_ clip: Clip, motion: Motion, at t: Double) -> (simd_double3x3, View) {
        let v = clip.view(atSource: t)
        let follow = clip.keyframes.isEmpty && !clip.directionalLock
        let m = motion.bodyFromCamera(at: t, mode: follow ? .follow : .lock, smoothing: 0.8, lockTime: 0,
                                      yaw: v.pan, pitch: v.tilt, roll: v.roll)
        return (m, v)
    }

    /// "83.5", "1:23.5" or "1:02:03.5" -> seconds.
    public static func parseTime(_ s: String) -> Double? {
        var total = 0.0
        for part in s.trimmingCharacters(in: .whitespaces).split(separator: ":", omittingEmptySubsequences: false) {
            guard let v = Double(part) else { return nil }
            total = total * 60 + v
        }
        return total
    }

    static func verticalFOV(_ hfov: Double) -> Double { 2 * atan(tan(hfov * .pi / 360) * 9 / 16) }
    static func horizontalFOV(_ vfov: Double) -> Double { 2 * atan(tan(vfov / 2) * 16 / 9) * 180 / .pi }
    static func deg(_ x: Any?) -> Double { (num(x) ?? 0) * 180 / .pi }
    static func num(_ x: Any?) -> Double? {
        if let n = x as? NSNumber { return n.doubleValue }
        if let s = x as? String { return Double(s) }
        return nil
    }
}
