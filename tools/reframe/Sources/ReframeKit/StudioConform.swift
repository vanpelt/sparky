import Foundation

/// Hands a Studio cut to DaVinci Resolve or Final Cut Pro.
///
/// Studio's "Multiple Clips" export trims each clip exactly to the cut (no room to adjust in the
/// next editor) and isn't reliable on long timelines. So instead:
///
/// 1. `handles` writes a copy of the cut with every clip widened by a few seconds on each side
///    (clamped to its source), no music and camera audio at 100 %, plus a manifest
///    (`reframe-conform.json`) recording where each clip of the cut sits inside it.
/// 2. The user exports that project from Studio as ONE file (ProRes, log).
/// 3. `conform` cuts the export into one file per clip at those frames (ProRes is all-intra, so a
///    stream copy is frame-exact), and writes FCPXML and OpenTimelineIO timelines that use each
///    file trimmed back to the cut, with the camera audio at the cut's levels and the cut's music
///    on its own track.
public enum StudioConform {
    public static let manifestName = "reframe-conform.json"

    public struct Manifest: Codable, Sendable {
        /// The cut this was made from.
        public var cut: String
        public var cutFile: String
        public var fps: Double
        /// Handle length asked for, timeline seconds.
        public var handles: Double
        public var width: Int
        public var height: Int
        public var clips: [Clip]
        public var music: [Music]

        public struct Clip: Codable, Sendable {
            /// 1-based position in the cut.
            public var index: Int
            public var name: String
            /// Where it sits in the cut, timeline frames.
            public var cutStart: Int
            public var length: Int
            /// Extra frames before and after the cut's in/out, and where the widened clip starts in
            /// the handles project (and so in its export).
            public var head: Int
            public var tail: Int
            public var exportStart: Int
            /// The cut's camera audio level, percent.
            public var volume: Int
            public var sourceIn: Double
            public var rate: Double
        }

        public struct Music: Codable, Sendable {
            public var name: String
            public var file: String
            /// Timeline frames.
            public var start: Int
            public var length: Int
            /// Seconds into the music file.
            public var sourceIn: Double
            /// Length of the whole music file, seconds.
            public var fileDuration: Double
            public var volume: Int
            public var fadeIn: Int
            public var fadeOut: Int
        }

        public var exportFrames: Int { clips.map { $0.exportStart + $0.head + $0.length + $0.tail }.max() ?? 0 }
        public var cutFrames: Int { clips.map { $0.cutStart + $0.length }.max() ?? 0 }
    }

    /// The edit that widens every clip of `cut` by `seconds` on each side, music removed.
    public static func handlesEdit(_ cut: StudioTimeline, ref: String, seconds: Double) -> StudioEdit {
        let clips = cut.video.map { c -> StudioEdit.ClipSpec in
            let sourceLength = Double(c.sourceFrames) * c.rate / cut.fps
            var spec = StudioEdit.ClipSpec(from: c.index, name: c.name,
                                           in: max(0, c.sourceIn - seconds * c.rate),
                                           out: min(sourceLength, c.sourceOut + seconds * c.rate))
            spec.volume = 100
            return spec
        }
        var edit = StudioEdit(project: ref, clips: clips, music: [])
        edit.effects = "drop"
        return edit
    }

    /// The manifest pairing each clip of `cut` with its widened copy in `handles` (same order).
    public static func manifest(cut: StudioTimeline, handles: StudioTimeline, seconds: Double) throws -> Manifest {
        guard cut.video.count == handles.video.count else {
            throw StudioEdit.Failure.bad("the handles project has \(handles.video.count) clips, the cut \(cut.video.count)")
        }
        let clips = zip(cut.video, handles.video).map { c, h in
            let head = c.leftTrim - h.leftTrim
            let length = c.sourceFrames - c.leftTrim - c.rightTrim
            let hLength = h.sourceFrames - h.leftTrim - h.rightTrim
            return Manifest.Clip(index: c.index, name: c.name, cutStart: c.startFrame, length: length, head: head,
                                 tail: hLength - length - head, exportStart: h.startFrame, volume: c.volume ?? 100,
                                 sourceIn: c.sourceIn, rate: c.rate)
        }
        let music = cut.audio.map { a in
            Manifest.Music(name: a.name, file: a.url, start: a.startFrame, length: a.sourceFrames - a.leftTrim - a.rightTrim,
                           sourceIn: a.sourceIn, fileDuration: Double(a.sourceFrames) / cut.fps, volume: a.volume ?? 100,
                           fadeIn: a.fadeIn, fadeOut: a.fadeOut)
        }
        return Manifest(cut: cut.name, cutFile: cut.file, fps: cut.fps, handles: seconds, width: 3840, height: 2160,
                        clips: clips, music: music)
    }

    static func decibels(_ percent: Int) -> Double { percent <= 0 ? -96 : 20 * log10(Double(percent) / 100) }

    // MARK: FCPXML

    /// FCPXML 1.10: the clips on the primary storyline (gaps kept), camera audio at the cut's
    /// levels, the music as connected clips on the lane below.
    public static func fcpxml(_ m: Manifest, clipFiles: [URL], musicFiles: [URL]) -> String {
        let fps = Int(m.fps.rounded())
        func t(_ frames: Int) -> String {
            if frames == 0 { return "0s" }
            var a = frames, b = fps
            while b != 0 { (a, b) = (b, a % b) }
            return fps / a == 1 ? "\(frames / a)s" : "\(frames / a)/\(fps / a)s"
        }
        func seconds(_ s: Double) -> String { t(Int((s * m.fps).rounded())) }
        func volume(_ percent: Int) -> String {
            percent == 100 ? "" : String(format: #"<adjust-volume amount="%.1fdB"/>"#, decibels(percent))
        }
        var resources = [#"<format id="r1" name="FFVideoFormat\#(m.height)p\#(fps)" frameDuration="\#(t(1))" width="\#(m.width)" height="\#(m.height)" colorSpace="1-1-1 (Rec. 709)"/>"#]
        for (i, (c, url)) in zip(m.clips, clipFiles).enumerated() {
            resources.append(#"<asset id="c\#(i + 1)" name="\#(Interchange.xml(url.deletingPathExtension().lastPathComponent))" start="0s" duration="\#(t(c.head + c.length + c.tail))" hasVideo="1" format="r1" hasAudio="1" audioSources="1" audioChannels="2" audioRate="48000"><media-rep kind="original-media" src="\#(Interchange.xml(url.absoluteString))"/></asset>"#)
        }
        for (i, (mu, url)) in zip(m.music, musicFiles).enumerated() where !m.music.isEmpty {
            resources.append(#"<asset id="m\#(i + 1)" name="\#(Interchange.xml(mu.name))" start="0s" duration="\#(seconds(mu.fileDuration))" hasAudio="1" audioSources="1" audioChannels="2" audioRate="44100"><media-rep kind="original-media" src="\#(Interchange.xml(url.absoluteString))"/></asset>"#)
        }
        // Each music piece hangs off the spine clip it starts under; its offset is in that clip's
        // own time (which begins at the clip's `start`).
        var connected: [Int: [String]] = [:]
        for (i, mu) in m.music.enumerated() {
            let parent = m.clips.lastIndex { $0.cutStart <= mu.start } ?? 0
            let p = m.clips[parent]
            let offset = p.head + (mu.start - p.cutStart)
            var fades = ""
            if mu.fadeIn > 0 || mu.fadeOut > 0 {
                let fi = mu.fadeIn > 0 ? #"<fadeIn type="easeIn" duration="\#(t(mu.fadeIn))"/>"# : ""
                let fo = mu.fadeOut > 0 ? #"<fadeOut type="easeIn" duration="\#(t(mu.fadeOut))"/>"# : ""
                fades = #"<adjust-volume amount="\#(String(format: "%.1f", decibels(mu.volume)))dB"><param name="amount">\#(fi)\#(fo)</param></adjust-volume>"#
            }
            connected[parent, default: []].append(#"<asset-clip ref="m\#(i + 1)" lane="-1" name="\#(Interchange.xml(mu.name))" offset="\#(t(offset))" start="\#(seconds(mu.sourceIn))" duration="\#(t(mu.length))" audioRole="music">\#(fades.isEmpty ? volume(mu.volume) : fades)</asset-clip>"#)
        }
        var spine: [String] = []
        var position = 0
        for (i, c) in m.clips.enumerated() {
            if c.cutStart > position {
                spine.append(#"<gap name="Gap" offset="\#(t(position))" start="0s" duration="\#(t(c.cutStart - position))"/>"#)
                position = c.cutStart
            }
            let inner = volume(c.volume) + (connected[i] ?? []).joined()
            spine.append(#"<asset-clip ref="c\#(i + 1)" name="\#(Interchange.xml(c.name))" offset="\#(t(position))" start="\#(t(c.head))" duration="\#(t(c.length))" tcFormat="NDF">\#(inner)</asset-clip>"#)
            position += c.length
        }
        return """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE fcpxml>
        <fcpxml version="1.10">
            <resources>
                \(resources.joined(separator: "\n        "))
            </resources>
            <library>
                <event name="\(Interchange.xml(m.cut))">
                    <project name="\(Interchange.xml(m.cut))">
                        <sequence format="r1" duration="\(t(position))" tcStart="0s" tcFormat="NDF" audioLayout="stereo" audioRate="48k">
                            <spine>
                                \(spine.joined(separator: "\n                        "))
                            </spine>
                        </sequence>
                    </project>
                </event>
            </library>
        </fcpxml>

        """
    }

    // MARK: OpenTimelineIO

    /// OTIO: V1 and A1 are the clips (gaps kept), A2 the music. Levels ride along as metadata only;
    /// OTIO has no standard volume.
    public static func otio(_ m: Manifest, clipFiles: [URL], musicFiles: [URL]) throws -> Data {
        let r = m.fps
        func rt(_ v: Double) -> [String: Any] { ["OTIO_SCHEMA": "RationalTime.1", "rate": r, "value": v] }
        func range(_ s: Double, _ d: Double) -> [String: Any] { ["OTIO_SCHEMA": "TimeRange.1", "start_time": rt(s), "duration": rt(d)] }
        func gap(_ frames: Int) -> [String: Any] {
            ["OTIO_SCHEMA": "Gap.1", "name": "", "source_range": range(0, Double(frames)), "effects": [Any](),
             "markers": [Any](), "enabled": true, "metadata": [String: Any]()]
        }
        func clip(_ name: String, _ url: URL, available: [String: Any], used: [String: Any], meta: [String: Any]) -> [String: Any] {
            ["OTIO_SCHEMA": "Clip.1", "name": name, "source_range": used,
             "media_reference": ["OTIO_SCHEMA": "ExternalReference.1", "name": url.lastPathComponent,
                                 "target_url": url.absoluteString, "available_range": available,
                                 "metadata": [String: Any]()] as [String: Any],
             "effects": [Any](), "markers": [Any](), "enabled": true, "metadata": ["reframe": meta]]
        }
        func track(_ name: String, _ kind: String, _ items: [[String: Any]]) -> [String: Any] {
            ["OTIO_SCHEMA": "Track.1", "name": name, "kind": kind, "source_range": NSNull(), "effects": [Any](),
             "markers": [Any](), "enabled": true, "metadata": [String: Any](), "children": items]
        }
        var video: [[String: Any]] = [], audio: [[String: Any]] = []
        var position = 0
        for (c, url) in zip(m.clips, clipFiles) {
            if c.cutStart > position { video.append(gap(c.cutStart - position)); audio.append(gap(c.cutStart - position)); position = c.cutStart }
            let item = clip(c.name, url, available: range(0, Double(c.head + c.length + c.tail)),
                            used: range(Double(c.head), Double(c.length)), meta: ["volume": c.volume, "cut_index": c.index])
            video.append(item); audio.append(item)
            position += c.length
        }
        var music: [[String: Any]] = []
        var mpos = 0
        for (mu, url) in zip(m.music, musicFiles).sorted(by: { $0.0.start < $1.0.start }) {
            if mu.start > mpos { music.append(gap(mu.start - mpos)) }
            music.append(clip(mu.name, url, available: range(0, (mu.fileDuration * r).rounded()),
                              used: range((mu.sourceIn * r).rounded(), Double(mu.length)),
                              meta: ["volume": mu.volume, "fade_in_frames": mu.fadeIn, "fade_out_frames": mu.fadeOut]))
            mpos = max(mpos, mu.start) + mu.length
        }
        var tracks = [track("V1", "Video", video), track("A1", "Audio", audio)]
        if !music.isEmpty { tracks.append(track("A2", "Audio", music)) }
        let timeline: [String: Any] = [
            "OTIO_SCHEMA": "Timeline.1", "name": m.cut, "global_start_time": rt(0), "metadata": [String: Any](),
            "tracks": ["OTIO_SCHEMA": "Stack.1", "name": "tracks", "source_range": NSNull(), "effects": [Any](),
                       "markers": [Any](), "enabled": true, "metadata": [String: Any](), "children": tracks] as [String: Any],
        ]
        return try JSONSerialization.data(withJSONObject: timeline, options: [.prettyPrinted, .sortedKeys])
    }
}
