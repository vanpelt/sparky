import Foundation

/// A declarative edit of an Insta360 Studio timeline: the clips in the order wanted, each copied
/// from a clip of an existing project (by its number in `studio dump`), with optional new trims,
/// keyframes or aim adjustments. Applying it packs the clips end to end on the main video track and
/// keeps every field of each clip it doesn't touch (stabilization, colour, Studio's caches...).
///
/// Times are seconds (or "m:ss.s" strings). `in`/`out`/`t` are real seconds into the SOURCE file, as
/// `studio dump --spec` prints them; `duration`, `at` and `start` are seconds on the timeline (`at`
/// from the clip's start). They differ only on a clip with a speed change: 2 s of a 0.25× clip
/// fills 8 s of timeline. Angles are degrees: pan left-positive from the file's first-frame
/// heading, tilt up-positive, `fov` the horizontal field of view of the 16:9 frame.
public struct StudioEdit: Codable, Sendable {
    /// The project the clip numbers refer to (name, folder or .insproj), and the one updated in place.
    public var project: String
    public var clips: [ClipSpec]
    /// Music clips; nil keeps the project's music as it is, [] removes it.
    public var music: [MusicSpec]?
    /// "beat" or "bar": move each cut (by changing the clip's out-point) to the nearest beat or bar
    /// of the music playing at that moment, so the edit cuts on the music.
    public var snap: String?
    /// Music volume (percent of its own) under clips marked `"music": "duck"`; default 25.
    public var duck: Int?
    /// "keep" (default) leaves effect tracks (a "Fade out"...) as they are, at their old times;
    /// "drop" removes them.
    public var effects: String?

    /// How loud the music is under a clip: full, ducked (`duck`), off, or a percent.
    public enum MusicLevel: Codable, Sendable, Equatable {
        case full, duck, off, percent(Double)

        public init(from decoder: Decoder) throws {
            let c = try decoder.singleValueContainer()
            if let n = try? c.decode(Double.self) { self = .percent(n); return }
            switch try c.decode(String.self) {
            case "duck": self = .duck
            case "off", "none": self = .off
            case "full": self = .full
            case let s: throw DecodingError.dataCorruptedError(in: c, debugDescription: "music is \"duck\", \"off\", \"full\" or a percent, not \"\(s)\"")
            }
        }
        public func encode(to encoder: Encoder) throws {
            var c = encoder.singleValueContainer()
            switch self {
            case .full: try c.encode("full")
            case .duck: try c.encode("duck")
            case .off: try c.encode("off")
            case .percent(let p): try c.encode(p)
            }
        }
        func fraction(duck: Int) -> Double {
            switch self {
            case .full: return 1
            case .duck: return Double(duck) / 100
            case .off: return 0
            case .percent(let p): return max(0, p) / 100
            }
        }
    }

    public struct ClipSpec: Codable, Sendable {
        public var from: Int
        /// Take the clip from another project (same frame rate) instead of `project`.
        public var project: String?
        /// Informational (dump --spec fills it in).
        public var name: String?
        public var `in`: Double?
        public var out: Double?
        /// Alternative to `out`: `in` + duration.
        public var duration: Double?
        /// Replaces the clip's keyframes.
        public var keyframes: [KeySpec]?
        /// Offsets every keyframe (or, with none, sets one): degrees of pan, tilt and fov.
        public var adjust: Adjust?
        /// The clip's own (camera) audio, percent.
        public var volume: Int?
        /// The music under this clip: "duck", "off", "full" or a percent of the music's volume.
        public var music: MusicLevel?
        /// false keeps this clip's out-point exactly where given (dialogue) when the edit snaps.
        public var snap: Bool?
        /// Free-form notes (what the shot is, why it's here); ignored by apply.
        public var note: String?

        public init(from: Int, project: String? = nil, name: String? = nil, in i: Double? = nil, out: Double? = nil) {
            self.from = from; self.project = project; self.name = name; self.in = i; self.out = out
        }
        public init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            from = try c.decode(Int.self, forKey: .from)
            project = try c.decodeIfPresent(String.self, forKey: .project)
            name = try c.decodeIfPresent(String.self, forKey: .name)
            self.in = try c.time(.in)
            out = try c.time(.out)
            duration = try c.time(.duration)
            keyframes = try c.decodeIfPresent([KeySpec].self, forKey: .keyframes)
            adjust = try c.decodeIfPresent(Adjust.self, forKey: .adjust)
            volume = try c.decodeIfPresent(Int.self, forKey: .volume)
            music = try c.decodeIfPresent(MusicLevel.self, forKey: .music)
            snap = try c.decodeIfPresent(Bool.self, forKey: .snap)
            note = try c.decodeIfPresent(String.self, forKey: .note)
        }
    }

    public struct KeySpec: Codable, Sendable {
        /// Source seconds, or `at` seconds after the clip's start on the timeline.
        public var t: Double?
        public var at: Double?
        public var pan: Double
        public var tilt: Double
        /// Missing values carry over from the clip's view at that moment.
        public var fov: Double?
        public var distance: Double?
        public var roll: Double?

        public init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            t = try c.time(.t)
            at = try c.time(.at)
            pan = try c.decode(Double.self, forKey: .pan)
            tilt = try c.decodeIfPresent(Double.self, forKey: .tilt) ?? 0
            fov = try c.decodeIfPresent(Double.self, forKey: .fov)
            distance = try c.decodeIfPresent(Double.self, forKey: .distance)
            roll = try c.decodeIfPresent(Double.self, forKey: .roll)
        }
    }

    public struct Adjust: Codable, Sendable {
        public var pan: Double?
        public var tilt: Double?
        public var fov: Double?
    }

    public struct MusicSpec: Codable, Sendable {
        public var from: Int
        public var project: String?
        public var name: String?
        /// Timeline seconds where it begins.
        public var start: Double?
        public var `in`: Double?
        public var out: Double?
        public var volume: Int?
        /// End the music with the video.
        public var fit: Bool?
        /// Start with this clip of `clips` (1-based) instead of at `start`; end with the end of
        /// `untilClip` (instead of `out` or `fit`). Survives re-timing the clips before it.
        public var atClip: Int?
        public var untilClip: Int?
        /// Fade lengths in seconds (Studio's audio fade, at most 300 frames).
        public var fadeIn: Double?
        public var fadeOut: Double?
        public var note: String?

        public init(from: Int, name: String?, start: Double, in i: Double, out: Double, volume: Int?) {
            self.from = from; self.name = name; self.start = start; self.in = i; self.out = out; self.volume = volume
        }
        public init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            from = try c.decodeIfPresent(Int.self, forKey: .from) ?? 1
            project = try c.decodeIfPresent(String.self, forKey: .project)
            name = try c.decodeIfPresent(String.self, forKey: .name)
            start = try c.time(.start)
            self.in = try c.time(.in)
            out = try c.time(.out)
            volume = try c.decodeIfPresent(Int.self, forKey: .volume)
            fit = try c.decodeIfPresent(Bool.self, forKey: .fit)
            atClip = try c.decodeIfPresent(Int.self, forKey: .atClip)
            untilClip = try c.decodeIfPresent(Int.self, forKey: .untilClip)
            fadeIn = try c.time(.fadeIn)
            fadeOut = try c.time(.fadeOut)
            note = try c.decodeIfPresent(String.self, forKey: .note)
        }
    }

    public init(project: String, clips: [ClipSpec], music: [MusicSpec]?) {
        self.project = project; self.clips = clips; self.music = music
    }

    /// The edit that reproduces a timeline as it is (`studio dump --spec`).
    public static func current(_ t: StudioTimeline, ref: String) -> StudioEdit {
        func r(_ x: Double) -> Double { (x * 1000).rounded() / 1000 }
        return StudioEdit(project: ref,
                          clips: t.video.map { ClipSpec(from: $0.index, name: $0.name, in: r($0.sourceIn), out: r($0.sourceOut)) },
                          music: t.audio.map { MusicSpec(from: $0.index, name: $0.name, start: r($0.timelineIn), in: r($0.sourceIn),
                                                         out: r($0.sourceOut), volume: $0.volume) })
    }

    public enum Failure: Error, CustomStringConvertible {
        case bad(String)
        public var description: String { if case .bad(let s) = self { return s }; return "" }
    }

    // MARK: Applying

    /// A project loaded both raw (to copy from) and parsed (for its views), clips in dump order.
    struct Source {
        var url: URL
        var raw: [String: Any]
        var timeline: StudioTimeline
        var video: [[String: Any]]
        var audio: [[String: Any]]

        init(_ ref: String) throws {
            url = try StudioTimeline.locate(ref)
            guard let d = try? Data(contentsOf: url), let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else {
                throw StudioTimeline.Failure.unreadable(url)
            }
            raw = o
            timeline = try StudioTimeline(url: url)
            let tracks = o["tracks"] as? [[String: Any]] ?? []
            func start(_ c: [String: Any]) -> Double { StudioTimeline.num(c["startFrame"]) ?? 0 }
            // Same order as StudioTimeline: tracks by z, then video by start (stable).
            let sorted = tracks.sorted { (StudioTimeline.num($0["z"]) ?? 0) > (StudioTimeline.num($1["z"]) ?? 0) }
            var v: [[String: Any]] = [], a: [[String: Any]] = []
            for t in sorted {
                let clips = t["clips"] as? [[String: Any]] ?? []
                switch Int(StudioTimeline.num(t["type"]) ?? 0) {
                case 0: v += clips
                case 2: a += clips
                default: break  // effect tracks
                }
            }
            video = v.enumerated().sorted { (start($0.element), $0.offset) < (start($1.element), $1.offset) }.map(\.element)
            audio = a
        }
    }

    public struct Result {
        public var project: [String: Any]
        /// The base project's .insproj.
        public var base: URL
        /// Every project the clips came from (for merging media bins).
        public var sources: [URL]
        public var warnings: [String]
    }

    /// Builds the edited project. `beats[i]` is the analysis of music clip i's file (of the
    /// project's first music clip when `music` is nil), needed for `snap`.
    public func build(beats: [MusicBeats?] = []) throws -> Result {
        var sources: [String: Source] = [:]
        func source(_ ref: String?) throws -> Source {
            let key = ref ?? project
            if let s = sources[key] { return s }
            let s = try Source(key)
            sources[key] = s
            return s
        }
        let base = try source(nil)
        let fps = base.timeline.fps
        var warnings: [String] = []
        var usedIDs = Set<String>()

        // Where each music clip's beats fall on the timeline, for snapping. A clip placed by
        // `atClip` is only known once the video reaches that clip.
        struct Placement { var start: Double?; var atClip: Int?; var sourceIn: Double; var length: Double; var beats: [Double] }
        var placements: [Placement] = []
        if let kind = snap {
            guard kind == "beat" || kind == "bar" else { throw Failure.bad("snap is \"beat\" or \"bar\"") }
            func grid(_ b: MusicBeats?) -> [Double] { b.map { kind == "bar" ? $0.downbeats : $0.beats } ?? [] }
            if let music {
                for (i, m) in music.enumerated() {
                    let s = try source(m.project)
                    guard m.from >= 1 && m.from <= s.audio.count else { continue }
                    let c = s.timeline.audio[m.from - 1]
                    let sin = m.in ?? c.sourceIn
                    let len = (m.out ?? c.sourceOut) - sin
                    placements.append(Placement(start: m.atClip == nil ? (m.start ?? c.timelineIn) : nil, atClip: m.atClip,
                                                sourceIn: sin, length: m.untilClip != nil || m.fit == true ? .infinity : len,
                                                beats: grid(i < beats.count ? beats[i] : nil)))
                }
            } else if let c = base.timeline.audio.first {
                placements.append(Placement(start: c.timelineIn, atClip: nil, sourceIn: c.sourceIn, length: c.sourceOut - c.sourceIn,
                                            beats: grid(beats.first ?? nil)))
            }
            if placements.allSatisfy({ $0.beats.isEmpty }) { throw Failure.bad("snap needs music on the timeline and its beats") }
        }

        // Video clips, packed end to end. Frames here count on each clip's stretched clock, which
        // is the timeline's: a source second of a 0.25× clip is 4 s of frames.
        var videoClips: [[String: Any]] = []
        var spans: [(start: Int, end: Int)] = []
        var position = 0
        /// Beat (or bar) times on the timeline of every music clip placed by clip `i`.
        func grid(upTo i: Int) -> [Double] {
            placements.flatMap { p -> [Double] in
                let start: Double
                if let s = p.start { start = s } else if let k = p.atClip, k - 1 <= i, k >= 1 {
                    start = Double(k - 1 < spans.count ? spans[k - 1].start : position) / fps
                } else { return [] }
                return p.beats.filter { $0 >= p.sourceIn && $0 - p.sourceIn <= p.length }.map { start + $0 - p.sourceIn }
            }
        }
        for (i, spec) in clips.enumerated() {
            let s = try source(spec.project)
            if s.timeline.fps != fps { throw Failure.bad("clip \(i + 1): \(s.timeline.name) runs at \(s.timeline.fps) fps, the edit at \(fps)") }
            guard spec.from >= 1 && spec.from <= s.video.count else {
                throw Failure.bad("clip \(i + 1): from \(spec.from), but \(s.timeline.name) has \(s.video.count) clips")
            }
            var c = s.video[spec.from - 1]
            let parsed = s.timeline.video[spec.from - 1]
            let rate = parsed.rate
            if parsed.variableSpeed { warnings.append("clip \(i + 1) (\(parsed.name)): its speed varies; trims assume \(rate)×") }
            let total = parsed.sourceFrames
            let inF = spec.in.map { frame($0 / rate, fps) } ?? parsed.leftTrim
            var outF = spec.out.map { frame($0 / rate, fps) }
                ?? spec.duration.map { inF + frame($0, fps) }
                ?? (total - parsed.rightTrim)
            guard inF >= 0, outF > inF, outF <= total else {
                throw Failure.bad(String(format: "clip %d (%@): in %.2f / out %.2f outside its source 0–%.2f s", i + 1, parsed.name,
                                         Double(inF) * rate / fps, Double(outF) * rate / fps, Double(total) * rate / fps))
            }
            let grid = snap != nil && spec.snap != false ? grid(upTo: i) : []
            if !grid.isEmpty {
                // The nearest grid time to the planned cut that leaves at least half a second and
                // stays within the source.
                let startT = Double(position) / fps
                let planned = startT + Double(outF - inF) / fps
                let maxEnd = startT + Double(total - inF) / fps
                if let g = grid.filter({ $0 >= startT + 0.5 && $0 <= maxEnd + 1e-6 }).min(by: { abs($0 - planned) < abs($1 - planned) }) {
                    outF = inF + max(1, frame(g, fps) - position)
                    if abs(g - planned) > 1.5 {
                        warnings.append(String(format: "clip %d (%@): snapped %.1fs from its planned length", i + 1, parsed.name, g - planned))
                    }
                } else {
                    warnings.append("clip \(i + 1) (\(parsed.name)): no \(snap!) to snap to within its source")
                }
            }
            setTrims(&c, inF, total - outF, rate: rate)
            c["startFrame"] = position
            c["id"] = fresh(c["id"] as? String, &usedIDs)
            if let v = spec.volume { setVolume(&c, v) }
            if let keys = spec.keyframes {
                let sourceIn = Double(inF) * rate / fps
                let built = try keys.map { k -> StudioTimeline.Keyframe in
                    guard let t = k.t ?? k.at.map({ sourceIn + $0 * rate }) else { throw Failure.bad("clip \(i + 1): a keyframe needs t or at") }
                    let v = parsed.view(atSource: t)
                    return StudioTimeline.Keyframe(sourceFrame: frame(t / rate, fps), sourceTime: t, pan: k.pan, tilt: k.tilt,
                                                   roll: k.roll ?? 0, hfov: k.fov ?? v.hfov, distance: k.distance ?? v.distance)
                }
                c["key_frame_track"] = ["node_list": Self.nodes(built.sorted { $0.sourceFrame < $1.sourceFrame }, rate: rate)]
            }
            if let a = spec.adjust {
                if spec.keyframes == nil && parsed.keyframes.isEmpty {
                    // Nothing to offset: pin the clip's current view, adjusted, at its in-point.
                    let t = Double(inF) * rate / fps
                    let v = parsed.view(atSource: t)
                    c["key_frame_track"] = ["node_list": Self.nodes([StudioTimeline.Keyframe(
                        sourceFrame: inF, sourceTime: t, pan: v.pan, tilt: v.tilt, roll: v.roll,
                        hfov: v.hfov, distance: v.distance)], rate: rate)]
                }
                Self.adjust(&c, a)
            }
            spans.append((position, position + outF - inF))
            position += outF - inF
            videoClips.append(c)
        }

        // Music, placed by time or by clip, then split wherever the clips under it want it ducked
        // or silent (Studio has no volume keyframes on music, so each level is its own piece).
        var musicClips: [[String: Any]] = []
        let duckLevel = duck ?? 25
        for (i, m) in (music ?? []).enumerated() {
            let s = try source(m.project)
            guard m.from >= 1 && m.from <= s.audio.count else {
                throw Failure.bad("music from \(m.from): \(s.timeline.name) has \(s.audio.count) music clip(s)")
            }
            func clipIndex(_ n: Int?, _ what: String) throws -> Int? {
                guard let n else { return nil }
                guard n >= 1 && n <= spans.count else { throw Failure.bad("music \(i + 1): \(what) \(n), but the edit has \(spans.count) clips") }
                return n - 1
            }
            let c = s.audio[m.from - 1]
            let total = Int(StudioTimeline.num(c["sourceFrames"]) ?? 0)
            let inF = m.in.map { frame($0, fps) } ?? Int(StudioTimeline.num(c["leftTrim"]) ?? 0)
            let startF = try clipIndex(m.atClip, "atClip").map { spans[$0].start }
                ?? m.start.map { frame($0, fps) } ?? Int(StudioTimeline.num(c["startFrame"]) ?? 0)
            var outF = m.out.map { frame($0, fps) } ?? (total - Int(StudioTimeline.num(c["rightTrim"]) ?? 0))
            if let end = try clipIndex(m.untilClip, "untilClip").map({ spans[$0].end }) ?? (m.fit == true ? position : nil) {
                let wanted = inF + max(1, end - startF)
                if wanted > total {
                    warnings.append(String(format: "music %d (%@) ends %.1fs before it should", i + 1, c["name"] as? String ?? "",
                                           Double(wanted - total) / fps))
                }
                outF = min(total, wanted)
            }
            guard inF >= 0, outF > inF, outF <= total else {
                throw Failure.bad("music \(i + 1): in/out outside its \(String(format: "%.1f", Double(total) / fps))s")
            }
            let volume = m.volume ?? Int(StudioTimeline.num((c["audio_setting"] as? [String: Any])?["currentVolume"]) ?? 100)
            let endF = startF + outF - inF
            // Level changes at clip boundaries inside [startF, endF).
            var pieces: [(start: Int, end: Int, level: Double)] = []
            var cuts = Set([startF, endF])
            for sp in spans where sp.start > startF && sp.start < endF { cuts.insert(sp.start) }
            if position > startF && position < endF { cuts.insert(position) }
            let edges = cuts.sorted()
            for (a, b) in zip(edges, edges.dropFirst()) {
                let k = spans.firstIndex { $0.start <= a && a < $0.end }
                let level = k.map { (clips[$0].music ?? .full).fraction(duck: duckLevel) } ?? 1
                if let last = pieces.last, last.end == a, abs(last.level - level) < 1e-6 { pieces[pieces.count - 1].end = b } else { pieces.append((a, b, level)) }
            }
            for p in pieces where p.level > 0 {
                var piece = c
                let left = inF + (p.start - startF)
                setTrims(&piece, left, total - (left + p.end - p.start), rate: 1)
                piece["startFrame"] = p.start
                setVolume(&piece, Int((Double(volume) * p.level).rounded()))
                setFades(&piece, fadeIn: p.start == startF ? m.fadeIn.map { frame($0, fps) } ?? 0 : 0,
                         fadeOut: p.end == endF ? m.fadeOut.map { frame($0, fps) } ?? 0 : 0)
                piece["id"] = fresh(c["id"] as? String, &usedIDs)
                musicClips.append(piece)
            }
        }
        musicClips.sort { (StudioTimeline.num($0["startFrame"]) ?? 0) < (StudioTimeline.num($1["startFrame"]) ?? 0) }
        if musicClips.count > 1 {
            for (a, b) in zip(musicClips, musicClips.dropFirst()) {
                let aEnd = Int(StudioTimeline.num(a["startFrame"]) ?? 0) + Int(StudioTimeline.num(a["sourceFrames"]) ?? 0)
                    - Int(StudioTimeline.num(a["leftTrim"]) ?? 0) - Int(StudioTimeline.num(a["rightTrim"]) ?? 0)
                if aEnd > Int(StudioTimeline.num(b["startFrame"]) ?? 0) {
                    warnings.append("music overlaps at \(String(format: "%.1f", StudioTimeline.num(b["startFrame"])! / fps))s; Studio's music track holds one clip at a time")
                }
            }
        }

        // Tracks: the main video track gets every clip; other video tracks are emptied; effect
        // tracks are kept or dropped.
        var edited = base.raw
        var tracks = edited["tracks"] as? [[String: Any]] ?? []
        func type(_ t: [String: Any]) -> Int { Int(StudioTimeline.num(t["type"]) ?? 0) }
        let mainVideo = tracks.firstIndex { type($0) == 0 && Int(StudioTimeline.num($0["z"]) ?? 0) == 0 }
            ?? tracks.firstIndex { type($0) == 0 }
        for i in tracks.indices {
            if !((tracks[i]["transitions"] as? [Any]) ?? []).isEmpty {
                warnings.append("removed the transitions on track \(i + 1) (Reframe doesn't know their format yet)")
                tracks[i]["transitions"] = [Any]()
            }
            let isAudio = type(tracks[i]) == 2
            let clipsHere = (tracks[i]["clips"] as? [[String: Any]]) ?? []
            if type(tracks[i]) != 0 && !isAudio {
                if effects == "drop" {
                    tracks[i]["clips"] = [Any]()
                } else if !clipsHere.isEmpty {
                    let names = clipsHere.compactMap { $0["name"] as? String }.joined(separator: ", ")
                    warnings.append("kept \(clipsHere.count) effect(s) (\(names)) at their old times; move them in Studio if the edit changed length")
                }
                continue
            }
            if i == mainVideo {
                tracks[i]["clips"] = videoClips
            } else if isAudio {
                if music != nil { tracks[i]["clips"] = [Any]() }
            } else if !((tracks[i]["clips"] as? [Any]) ?? []).isEmpty {
                warnings.append("emptied video track \(i + 1); every clip is now on the main track")
                tracks[i]["clips"] = [Any]()
            }
        }
        if mainVideo == nil {
            tracks.append(["audible": true, "clips": videoClips, "id": StudioProject.uuid(), "locked": false,
                           "transitions": [Any](), "type": 0, "visible": true, "z": 0])
        }
        if music != nil {
            if let a = tracks.firstIndex(where: { Int(StudioTimeline.num($0["type"]) ?? 0) == 2 }) {
                tracks[a]["clips"] = musicClips
            } else if !musicClips.isEmpty {
                tracks.append(["audible": true, "clips": musicClips, "id": StudioProject.uuid(), "locked": false,
                               "transitions": [Any](), "type": 2, "visible": true, "z": -1])
            }
        }
        edited["tracks"] = tracks

        var frames = position
        for t in tracks {
            for c in t["clips"] as? [[String: Any]] ?? [] {
                let len = Int(StudioTimeline.num(c["sourceFrames"]) ?? 0) - Int(StudioTimeline.num(c["leftTrim"]) ?? 0)
                    - Int(StudioTimeline.num(c["rightTrim"]) ?? 0)
                frames = max(frames, Int(StudioTimeline.num(c["startFrame"]) ?? 0) + len)
            }
        }
        if frames > position + 1 { warnings.append(String(format: "music runs %.1fs past the video", Double(frames - position) / fps)) }
        edited["frames"] = frames
        edited["markTrimIn"] = 0
        edited["markTrimEnd"] = frames
        edited["lastModifiedTime"] = StudioProject.timestamp(Date())
        return Result(project: edited, base: base.url, sources: sources.values.map(\.url), warnings: warnings)
    }

    // MARK: Writing

    /// Writes `result` over its base project (keeping the previous version in `.reframe-history`)
    /// or, with `newName`, as a new project. Studio must be quit; returns the .insproj written.
    /// With `root`, a new project goes there instead and isn't added to Studio's list (for tests).
    @discardableResult
    public static func write(_ result: Result, newName: String? = nil, replace: Bool = false, root: URL? = nil) throws -> URL {
        let register = root == nil
        if register && StudioProject.isStudioRunning { throw StudioProject.Failure.studioRunning }
        let fm = FileManager.default
        var project = result.project
        let folder: URL, file: URL
        if let newName {
            folder = (root ?? StudioProject.timelineRoot).appendingPathComponent(newName, isDirectory: true)
            if fm.fileExists(atPath: folder.path) {
                guard replace else { throw StudioProject.Failure.exists(folder) }
                try fm.trashItem(at: folder, resultingItemURL: nil)
            }
            try fm.createDirectory(at: folder, withIntermediateDirectories: true)
            let id = StudioProject.uuid()
            let now = StudioProject.timestamp(Date())
            project["id"] = id
            project["name"] = newName
            project["projectPath"] = folder.path
            project["creationTime"] = now
            project["lastModifiedTime"] = now
            file = folder.appendingPathComponent("\(id).insproj")
        } else {
            guard register else { throw Failure.bad("a test root only takes new projects (give a name)") }
            file = result.base
            folder = file.deletingLastPathComponent()
            let history = folder.appendingPathComponent(".reframe-history", isDirectory: true)
            try fm.createDirectory(at: history, withIntermediateDirectories: true)
            let f = DateFormatter()
            f.dateFormat = "yyyyMMdd-HHmmss"
            try? fm.copyItem(at: file, to: history.appendingPathComponent("\(f.string(from: Date())).insproj"))
        }
        let data = try StudioProject.json(project)
        try data.write(to: file, options: .atomic)
        try data.write(to: folder.appendingPathComponent(file.deletingPathExtension().lastPathComponent + "_backup.insproj"), options: .atomic)
        try mergeAlbums(into: folder, from: [result.base] + result.sources)
        if register { try StudioProject.register(project, folder: folder) }
        return file
    }

    /// Makes `folder`'s media bin list every recording used by the given projects.
    static func mergeAlbums(into folder: URL, from projects: [URL]) throws {
        let target = folder.appendingPathComponent("project_album_info.data")
        func load(_ u: URL) -> [String: Any]? {
            (try? Data(contentsOf: u)).flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }
        }
        var album = load(target) ?? ["album_tree": ["nodes": [Any](), "root_uuids": [Any](), "version": 1],
                                     "media_card_snapshots": [String: Any](), "tab_media_uuids": [Any](), "version": 1]
        var tree = album["album_tree"] as? [String: Any] ?? [:]
        var nodes = tree["nodes"] as? [[String: Any]] ?? []
        var roots = tree["root_uuids"] as? [Any] ?? []
        var snaps = album["media_card_snapshots"] as? [String: Any] ?? [:]
        var tabs = album["tab_media_uuids"] as? [String] ?? []
        var changed = false
        for p in Set(projects.map { $0.deletingLastPathComponent() }) where p != folder {
            guard let other = load(p.appendingPathComponent("project_album_info.data")) else { continue }
            let otherNodes = (other["album_tree"] as? [String: Any])?["nodes"] as? [[String: Any]] ?? []
            let otherSnaps = other["media_card_snapshots"] as? [String: Any] ?? [:]
            for n in otherNodes {
                guard let ref = n["media_card_uuid"] as? String, snaps[ref] == nil else { continue }
                nodes.append(n)
                if let id = n["node_uuid"] { roots.append(id) }
                snaps[ref] = otherSnaps[ref] ?? [:]
                tabs.append(ref)
                changed = true
            }
        }
        guard changed || !FileManager.default.fileExists(atPath: target.path) else { return }
        tree["nodes"] = nodes
        tree["root_uuids"] = roots
        album["album_tree"] = tree
        album["media_card_snapshots"] = snaps
        album["tab_media_uuids"] = tabs
        try StudioProject.json(album).write(to: target, options: .atomic)
    }

    // MARK: Helpers

    func frame(_ seconds: Double, _ fps: Double) -> Int { Int((seconds * fps).rounded()) }

    /// Trims on the clip's stretched clock; Studio also keeps them in real source frames.
    func setTrims(_ c: inout [String: Any], _ left: Int, _ right: Int, rate: Double) {
        c["leftTrim"] = left
        c["rightTrim"] = right
        if let real = StudioTimeline.num(c["source_total_frames"]).map(Int.init), rate != 1 {
            c["source_left_trim"] = min(real, Int((Double(left) * rate).rounded()))
            c["source_right_trim"] = min(real, Int((Double(right) * rate).rounded()))
        } else {
            c["source_left_trim"] = left
            c["source_right_trim"] = right
        }
    }

    /// Studio's audio fades, in frames (its `fade_in_frame`), capped at its maximum.
    func setFades(_ c: inout [String: Any], fadeIn: Int, fadeOut: Int) {
        var a = c["audio_setting"] as? [String: Any] ?? [:]
        a["currentFadeIn"] = min(fadeIn, Int(StudioTimeline.num(a["maxFadeIn"]) ?? 300))
        a["currentFadeOut"] = min(fadeOut, Int(StudioTimeline.num(a["maxFadeOut"]) ?? 300))
        c["audio_setting"] = a
    }

    func setVolume(_ c: inout [String: Any], _ v: Int) {
        var a = c["audio_setting"] as? [String: Any] ?? [:]
        a["currentVolume"] = max(0, min(v, 200))
        c["audio_setting"] = a
    }

    /// The clip's own ID the first time it is used, a new one for each repeat.
    func fresh(_ id: String?, _ used: inout Set<String>) -> String {
        if let id, !used.contains(id) { used.insert(id); return id }
        let n = StudioProject.uuid()
        used.insert(n)
        return n
    }

    /// Studio keyframe nodes, with the default (linear) easing node between each pair.
    /// `sourceFrame` is Studio's `time` (stretched clock); `src_time` is the real source frame.
    static func nodes(_ keys: [StudioTimeline.Keyframe], rate: Double = 1) -> [[String: Any]] {
        var nodes: [[String: Any]] = []
        var previous: String?
        for k in keys {
            let name = StudioProject.uuid()
            if let p = previous {
                nodes.append(["name": "\(p)-\(name)", "node_type": 1, "point1X": 0.5, "point1Y": 0.5,
                              "point2X": 0.5, "point2Y": 0.5, "type": 1])
            }
            nodes.append(["auto_fov": 0, "distance": k.distance, "fov": StudioTimeline.verticalFOV(k.hfov), "is_headtrack": 0,
                          "name": name, "node_type": 0, "pan": k.pan * .pi / 180, "roll": k.roll * .pi / 180,
                          "src_time": Int((Double(k.sourceFrame) * rate).rounded()), "state": 7, "tilt": k.tilt * .pi / 180, "time": k.sourceFrame])
            previous = name
        }
        return nodes
    }

    static func adjust(_ c: inout [String: Any], _ a: Adjust) {
        var track = c["key_frame_track"] as? [String: Any] ?? [:]
        let list = (track["node_list"] as? [[String: Any]] ?? []).map { n -> [String: Any] in
            guard Int(StudioTimeline.num(n["node_type"]) ?? 0) == 0 else { return n }
            var n = n
            if let d = a.pan { n["pan"] = (StudioTimeline.num(n["pan"]) ?? 0) + d * .pi / 180 }
            if let d = a.tilt { n["tilt"] = (StudioTimeline.num(n["tilt"]) ?? 0) + d * .pi / 180 }
            if let d = a.fov {
                let h = StudioTimeline.horizontalFOV(StudioTimeline.num(n["fov"]) ?? StudioTimeline.verticalFOV(88))
                n["fov"] = StudioTimeline.verticalFOV(min(max(h + d, 1), 170))
            }
            return n
        }
        track["node_list"] = list
        c["key_frame_track"] = track
    }
}

extension KeyedDecodingContainer {
    /// A time given as seconds or as "m:ss.s".
    func time(_ key: Key) throws -> Double? {
        if let d = try? decodeIfPresent(Double.self, forKey: key) { return d }
        guard let s = try decodeIfPresent(String.self, forKey: key) else { return nil }
        guard let v = StudioTimeline.parseTime(s) else {
            throw DecodingError.dataCorruptedError(forKey: key, in: self, debugDescription: "\"\(s)\" isn't seconds or m:ss.s")
        }
        return v
    }
}
