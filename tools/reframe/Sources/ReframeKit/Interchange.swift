import CoreMedia
import Foundation

/// Timeline files that hand exported shots to an editor:
/// OpenTimelineIO for DaVinci Resolve (18.5+), FCPXML for Final Cut Pro.
/// Clip source ranges use each file's embedded time-of-day timecode, so relinking works.
public enum Interchange {
    // MARK: FCPXML

    public static func fcpxml(_ shots: [ExportedShot], title: String) -> String {
        guard !shots.isEmpty else { return "" }
        func t(_ frames: Int, _ fd: CMTime) -> String {
            var v = Int64(frames) * fd.value, d = Int64(fd.timescale)
            if v == 0 { return "0s" }
            var a = v, b = d
            while b != 0 { (a, b) = (b, a % b) }
            v /= a; d /= a
            return d == 1 ? "\(v)s" : "\(v)/\(d)s"
        }
        var formats: [String: String] = [:]  // key -> id
        var resources: [String] = []
        var nextID = 1
        func formatID(_ s: ExportedShot) -> String {
            let key = "\(s.width)x\(s.height)@\(s.frameDuration.value)/\(s.frameDuration.timescale)"
            if let id = formats[key] { return id }
            let id = "r\(nextID)"; nextID += 1
            formats[key] = id
            resources.append(#"<format id="\#(id)" frameDuration="\#(t(1, s.frameDuration))" width="\#(s.width)" height="\#(s.height)" colorSpace="1-1-1 (Rec. 709)"/>"#)
            return id
        }
        var assetIDs: [String] = []
        for s in shots {
            let fmt = formatID(s)
            let id = "r\(nextID)"; nextID += 1
            assetIDs.append(id)
            let audio = s.hasAudio ? #" hasAudio="1" audioSources="1" audioChannels="2" audioRate="48000""# : ""
            resources.append("""
            <asset id="\(id)" name="\(xml(s.name))" start="\(t(s.timecodeFrames, s.frameDuration))" duration="\(t(s.frameCount, s.frameDuration))" hasVideo="1" format="\(fmt)"\(audio)>
                <media-rep kind="original-media" src="\(xml(s.url.absoluteString))"/>
            </asset>
            """)
        }
        let sequenceFormat = formatID(shots[0])
        var eventClips: [String] = []
        var spine: [String] = []
        var offset = 0  // in the first shot's frame units
        for (s, id) in zip(shots, assetIDs) {
            let start = t(s.timecodeFrames, s.frameDuration)
            let dur = t(s.frameCount, s.frameDuration)
            let kw = #"<keyword start="\#(start)" duration="\#(dur)" value="\#(xml(s.keywords.joined(separator: ", ")))"/>"#
            eventClips.append("""
            <asset-clip ref="\(id)" name="\(xml(s.name))" start="\(start)" duration="\(dur)" tcFormat="NDF">
                <note>\(xml(s.note))</note>
                \(kw)
            </asset-clip>
            """)
            spine.append(#"<asset-clip ref="\#(id)" name="\#(xml(s.name))" offset="\#(t(offset, shots[0].frameDuration))" start="\#(start)" duration="\#(dur)" tcFormat="NDF"/>"#)
            offset += Int((Double(s.frameCount) * s.frameDuration.seconds / shots[0].frameDuration.seconds).rounded())
        }
        return """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE fcpxml>
        <fcpxml version="1.10">
            <resources>
                \(resources.joined(separator: "\n        "))
            </resources>
            <library>
                <event name="\(xml(title))">
                    \(eventClips.joined(separator: "\n            "))
                    <project name="\(xml(title))">
                        <sequence format="\(sequenceFormat)" duration="\(t(offset, shots[0].frameDuration))" tcStart="0s" tcFormat="NDF" audioLayout="stereo" audioRate="48k">
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

    public static func otio(_ shots: [ExportedShot], title: String) throws -> Data {
        guard let first = shots.first else { return Data() }
        let rate = 1 / first.frameDuration.seconds
        func rt(_ value: Double, _ r: Double) -> [String: Any] {
            ["OTIO_SCHEMA": "RationalTime.1", "rate": r, "value": value]
        }
        func range(_ start: Double, _ dur: Double, _ r: Double) -> [String: Any] {
            ["OTIO_SCHEMA": "TimeRange.1", "start_time": rt(start, r), "duration": rt(dur, r)]
        }
        func clip(_ s: ExportedShot) -> [String: Any] {
            let r = 1 / s.frameDuration.seconds
            let available = range(Double(s.timecodeFrames), Double(s.frameCount), r)
            return [
                "OTIO_SCHEMA": "Clip.1",
                "name": s.name,
                "source_range": available,
                "media_reference": [
                    "OTIO_SCHEMA": "ExternalReference.1",
                    "name": s.url.lastPathComponent,
                    "target_url": s.url.absoluteString,
                    "available_range": available,
                    "metadata": [:] as [String: Any],
                ] as [String: Any],
                "effects": [] as [Any],
                "markers": [] as [Any],
                "enabled": true,
                "metadata": ["reframe": ["note": s.note, "keywords": s.keywords]],
            ]
        }
        func track(_ name: String, _ kind: String, _ items: [[String: Any]]) -> [String: Any] {
            ["OTIO_SCHEMA": "Track.1", "name": name, "kind": kind, "source_range": NSNull(),
             "effects": [] as [Any], "markers": [] as [Any], "enabled": true, "metadata": [:] as [String: Any],
             "children": items]
        }
        var tracks = [track("V1", "Video", shots.map(clip))]
        if shots.contains(where: \.hasAudio) {
            tracks.append(track("A1", "Audio", shots.filter(\.hasAudio).map(clip)))
        }
        let timeline: [String: Any] = [
            "OTIO_SCHEMA": "Timeline.1",
            "name": title,
            "global_start_time": rt(0, rate),
            "metadata": [:] as [String: Any],
            "tracks": [
                "OTIO_SCHEMA": "Stack.1", "name": "tracks", "source_range": NSNull(),
                "effects": [] as [Any], "markers": [] as [Any], "enabled": true, "metadata": [:] as [String: Any],
                "children": tracks,
            ] as [String: Any],
        ]
        return try JSONSerialization.data(withJSONObject: timeline, options: [.prettyPrinted, .sortedKeys])
    }

    static func xml(_ s: String) -> String {
        s.replacingOccurrences(of: "&", with: "&amp;").replacingOccurrences(of: "\"", with: "&quot;")
            .replacingOccurrences(of: "<", with: "&lt;").replacingOccurrences(of: ">", with: "&gt;")
    }
}
