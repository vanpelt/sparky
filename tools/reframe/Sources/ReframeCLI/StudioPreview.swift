import AVFoundation
import CoreGraphics
import CoreText
import Foundation
import ReframeKit

// reframe-cli studio list
// reframe-cli studio dump <project> [--json]
// reframe-cli studio frame <project> <timeline time> <out.png> [--width 1280] [--lut file.cube]
// reframe-cli studio sheet <project> <out dir | out.png> [--clip N] [--per-clip 3] [--rows 6] [--width 400] [--lut file.cube]
// reframe-cli studio dump <project> --spec           (the timeline as an edit spec for apply)
// reframe-cli studio apply <edit.json> [--as "New name"] [--replace] [--dry-run] [--quit-studio] [--open]
// reframe-cli studio beats <project | audio file> [--json]
// reframe-cli studio handles <cut project> [--seconds 1] [--as name] [--quit-studio] [--open]  (copy with handles, for export)
// reframe-cli studio conform <handles project> <export.mov> <out dir>   (per-clip files + FCPXML/OTIO for Resolve)
//
// <project> is a Studio project name, its folder, or its .insproj. Times are seconds or m:ss.s.
// Previews are Reframe renders of Studio's keyframes from the LRV proxy: framing and timing match
// Studio, stitching and colour are Reframe's.

func studioCommand(_ sub: String, _ args: [String]) async throws {
    let positional = positionalArgs(args, valued: ["--width", "--lut", "--clip", "--per-clip", "--rows", "--as", "--root", "--seconds"])
    func timeline(_ i: Int) throws -> StudioTimeline {
        guard i < positional.count else { fail("studio \(sub) needs a project (name, folder or .insproj); see studio list") }
        do { return try StudioTimeline(url: try StudioTimeline.locate(positional[i])) } catch let e as StudioTimeline.Failure { fail(e.description) }
    }
    switch sub {
    case "list":
        for url in StudioTimeline.all() {
            guard let t = try? StudioTimeline(url: url) else { continue }
            let date = (try? url.resourceValues(forKeys: [.contentModificationDateKey]).contentModificationDate).map {
                DateFormatter.localizedString(from: $0, dateStyle: .short, timeStyle: .short)
            } ?? ""
            print(String(format: "  %@  %2d clips  %@ video  %@ fps%@  saved %@", t.name, t.video.count, clock(t.videoEnd),
                         fmt(t.fps), t.audio.isEmpty ? "" : "  + music", date))
        }
    case "dump":
        let t = try timeline(0)
        if flag("--spec") {
            // The edit that reproduces this timeline: a starting point for `studio apply`.
            let enc = JSONEncoder()
            enc.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
            FileHandle.standardOutput.write(try enc.encode(StudioEdit.current(t, ref: t.name)))
            print("")
        } else if flag("--json") {
            let enc = JSONEncoder()
            enc.outputFormatting = [.prettyPrinted, .sortedKeys]
            FileHandle.standardOutput.write(try enc.encode(t))
            print("")
        } else {
            printDump(t)
        }
        if StudioProject.isStudioRunning {
            FileHandle.standardError.write("note: Insta360 Studio is running; this is the project as last saved.\n".data(using: .utf8)!)
        }
    case "frame":
        guard positional.count >= 3, let time = parseTime(positional[1]) else { fail("studio frame <project> <timeline time> <out.png>") }
        let t = try timeline(0)
        guard let clip = t.videoClip(at: time) else { fail("no video at \(clock(time)) (video runs to \(clock(t.videoEnd)))") }
        let width = Int(option("--width") ?? "") ?? 1280
        let preview = try StudioPreviewer(lut: option("--lut"))
        let src = clip.sourceTime(atTimeline: time)
        guard let (img, v) = try await preview.render(clip, atSource: src, width: width, height: width * 9 / 16) else {
            fail("couldn't render \(clip.name) (is its file or proxy on this Mac?)")
        }
        try writePNG(img, to: URL(fileURLWithPath: positional[2]))
        print(String(format: "wrote %@: #%d %@ at %@ (source %@)  %@", positional[2], clip.index, clip.name, clock(time),
                     clock(src), describe(v)))
    case "sheet":
        guard positional.count >= 2 else { fail("studio sheet <project> <out dir | out.png> [--clip N] [--per-clip 3]") }
        let t = try timeline(0)
        var clips = t.video
        if let n = option("--clip").flatMap(Int.init) {
            clips = clips.filter { $0.index == n }
            if clips.isEmpty { fail("no clip #\(n): the timeline has \(t.video.count)") }
        }
        let perClip = max(1, Int(option("--per-clip") ?? "") ?? (option("--clip") == nil ? 3 : 8))
        let tileW = Int(option("--width") ?? "") ?? 400
        let rows = max(1, Int(option("--rows") ?? "") ?? 6)
        let columns = min(perClip, 4)
        let preview = try StudioPreviewer(lut: option("--lut"))
        let files = try await preview.sheets(name: t.name, clips: clips, perClip: perClip, columns: columns, tileWidth: tileW,
                                              rowsPerPage: rows, out: URL(fileURLWithPath: positional[1]))
        for f in files { print("wrote \(f.path)") }
    case "beats":
        // studio beats <project | audio file> [--json]: tempo, bars and section changes of the music.
        guard let ref = positional.first else { fail("studio beats <project | audio file> [--json]") }
        var file = URL(fileURLWithPath: (ref as NSString).expandingTildeInPath)
        var place: (start: Double, sourceIn: Double, end: Double)?
        if !FileManager.default.fileExists(atPath: file.path) || file.pathExtension == "insproj" {
            let t = try timeline(0)
            guard let m = t.audio.first else { fail("\(t.name) has no music") }
            file = URL(fileURLWithPath: m.url)
            place = (m.timelineIn, m.sourceIn, m.timelineOut)
        }
        let b = try await MusicBeats.analyze(url: file)
        if flag("--json") {
            let enc = JSONEncoder()
            enc.outputFormatting = [.prettyPrinted, .sortedKeys]
            FileHandle.standardOutput.write(try enc.encode(b))
            print("")
            break
        }
        printBeats(b, place: place)
    case "apply":
        // studio apply <edit.json> [--as "New name"] [--replace] [--dry-run] [--quit-studio] [--open]
        guard let path = positional.first else { fail("studio apply <edit.json> [--as name] [--dry-run]") }
        let edit: StudioEdit
        do {
            edit = try JSONDecoder().decode(StudioEdit.self, from: Data(contentsOf: URL(fileURLWithPath: path)))
        } catch let e as DecodingError {
            fail("\(path): \(describe(e))")
        }
        var beats: [MusicBeats?] = []
        if edit.snap != nil {
            for f in try musicFiles(edit) {
                if let f { beats.append(try await MusicBeats.analyze(url: f)) } else { beats.append(nil) }
            }
        }
        let result: StudioEdit.Result
        do { result = try edit.build(beats: beats) } catch let e as StudioEdit.Failure { fail(e.description) }
        for w in result.warnings { print("warning: \(w)") }
        if flag("--dry-run") {
            let tmp = FileManager.default.temporaryDirectory.appendingPathComponent("reframe-dry-run-\(UUID().uuidString).insproj")
            try StudioProject.json(result.project).write(to: tmp)
            defer { try? FileManager.default.removeItem(at: tmp) }
            printDump(try StudioTimeline(url: tmp))
            print("(dry run: nothing written)")
            break
        }
        let root = option("--root").map { URL(fileURLWithPath: $0) }
        if root == nil { await quitStudioIfAsked() }
        do {
            let file = try StudioEdit.write(result, newName: option("--as"), replace: flag("--replace"), root: root)
            let t = try StudioTimeline(url: file)
            print(String(format: "wrote %@: %d clips, %@ of video", file.path, t.video.count, clock(t.videoEnd)))
        } catch let e as StudioProject.Failure { fail(e.description) }
        if flag("--open") { try openStudio() }
    case "handles":
        // studio handles <cut project> [--seconds 1] [--as name] [--replace] [--root dir] [--quit-studio] [--open]
        guard let ref = positional.first else { fail("studio handles <cut project> [--seconds 1] [--as name]") }
        let cut = try StudioTimeline(url: try StudioTimeline.locate(ref))
        let seconds = option("--seconds").flatMap(Double.init) ?? 1
        let name = option("--as") ?? "\(cut.name) handles"
        let result: StudioEdit.Result
        do { result = try StudioConform.handlesEdit(cut, ref: cut.file, seconds: seconds).build() } catch let e as StudioEdit.Failure { fail(e.description) }
        for w in result.warnings where !w.contains("music") { print("warning: \(w)") }
        let root = option("--root").map { URL(fileURLWithPath: $0) }
        if root == nil { await quitStudioIfAsked() }
        let file: URL
        do { file = try StudioEdit.write(result, newName: name, replace: flag("--replace"), root: root) } catch let e as StudioProject.Failure { fail(e.description) }
        let handles = try StudioTimeline(url: file)
        let manifest = try StudioConform.manifest(cut: cut, handles: handles, seconds: seconds)
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys]
        try enc.encode(manifest).write(to: file.deletingLastPathComponent().appendingPathComponent(StudioConform.manifestName))
        let short = manifest.clips.filter { $0.head < Int(seconds * cut.fps) || $0.tail < Int(seconds * cut.fps) }.count
        print(String(format: "wrote \"%@\": %d clips, %@ (the cut is %@), %.1fs handles%@", name, handles.video.count,
                     clock(handles.videoEnd), clock(Double(manifest.cutFrames) / cut.fps), seconds,
                     short > 0 ? " (\(short) shorter, at the ends of their recordings)" : ""))
        print("export it from Studio as ONE file (Export Mode: single), then: reframe-cli studio conform \"\(name)\" <export.mov> <out dir>")
        if flag("--open") { try openStudio() }
    case "conform":
        // studio conform <handles project> <export.mov> <out dir>
        guard positional.count >= 3 else { fail("studio conform <handles project> <export.mov> <out dir>") }
        let project = try StudioTimeline.locate(positional[0])
        let manifestURL = project.deletingLastPathComponent().appendingPathComponent(StudioConform.manifestName)
        guard let data = try? Data(contentsOf: manifestURL) else { fail("no \(StudioConform.manifestName) next to \(project.path): make the project with `studio handles`") }
        let m = try JSONDecoder().decode(StudioConform.Manifest.self, from: data)
        let export = URL(fileURLWithPath: (positional[1] as NSString).expandingTildeInPath)
        guard FileManager.default.fileExists(atPath: export.path) else { fail("no export at \(export.path)") }
        let out = URL(fileURLWithPath: positional[2], isDirectory: true)
        let asset = AVURLAsset(url: export)
        let frames = Int((try await asset.load(.duration).seconds * m.fps).rounded())
        if abs(frames - m.exportFrames) > 1 {
            fail("\(export.lastPathComponent) is \(frames) frames; the handles project is \(m.exportFrames). Export the whole project as one file at \(Int(m.fps)) fps.")
        }
        // ProRes is all-intra, so a stream copy cuts it frame-exactly. Anything else (Studio's cloud
        // export is H.264/HEVC) is long-GOP: decode and re-encode each clip to ProRes 422 instead.
        var intra = false
        var codec = "?"
        if let track = try await asset.loadTracks(withMediaType: .video).first {
            let size = try await track.load(.naturalSize)
            if Int(size.width) != m.width || Int(size.height) != m.height { print("note: export is \(Int(size.width))×\(Int(size.height))") }
            if let desc = try await track.load(.formatDescriptions).first {
                let t = CMFormatDescriptionGetMediaSubType(desc)
                codec = String([24, 16, 8, 0].map { Character(UnicodeScalar(UInt8((t >> $0) & 0xff))) })
                intra = ["apcn", "apch", "apcs", "apco", "ap4h", "ap4x"].contains(codec)
            }
        }
        if !intra { print("export is \(codec), not ProRes: re-encoding each clip to ProRes 422 (slower, and no better than the export)") }
        let videoCodec = intra ? ["-c:v", "copy"] : ["-c:v", "prores_ks", "-profile:v", "2", "-pix_fmt", "yuv422p10le", "-vendor", "apl0"]
        let fm = FileManager.default
        let clipsDir = out.appendingPathComponent("clips", isDirectory: true), musicDir = out.appendingPathComponent("music", isDirectory: true)
        try fm.createDirectory(at: clipsDir, withIntermediateDirectories: true)
        try fm.createDirectory(at: musicDir, withIntermediateDirectories: true)
        var clipFiles: [URL] = []
        for c in m.clips {
            let stem = (c.name as NSString).deletingPathExtension
            let dest = clipsDir.appendingPathComponent(String(format: "%03d %@.mov", c.index, stem))
            let n = c.head + c.length + c.tail
            // ProRes is all-intra: seeking the input and copying the video is frame-exact. Audio goes
            // to PCM so it cuts at the same sample, not the nearest AAC packet.
            try run("/opt/homebrew/bin/ffmpeg", ["-v", "error", "-y", "-ss", String(format: "%.6f", Double(c.exportStart) / m.fps),
                                                 "-i", export.path, "-frames:v", "\(n)", "-t", String(format: "%.6f", Double(n) / m.fps),
                                                 "-map", "0:v:0", "-map", "0:a:0?"] + videoCodec + ["-c:a", "pcm_s24le",
                                                 "-avoid_negative_ts", "make_zero", dest.path])
            clipFiles.append(dest)
            if c.index % 10 == 0 { print("cut \(c.index) of \(m.clips.count)") }
        }
        var musicFiles: [URL] = []
        for mu in m.music {
            let src = URL(fileURLWithPath: mu.file)
            let dest = musicDir.appendingPathComponent((mu.name as NSString).lastPathComponent)
            if !fm.fileExists(atPath: dest.path) { try fm.copyItem(at: src, to: dest) }
            musicFiles.append(dest)
        }
        let title = m.cut
        let xmlURL = out.appendingPathComponent("\(title).fcpxml"), otioURL = out.appendingPathComponent("\(title).otio")
        try StudioConform.fcpxml(m, clipFiles: clipFiles, musicFiles: musicFiles).write(to: xmlURL, atomically: true, encoding: .utf8)
        try StudioConform.otio(m, clipFiles: clipFiles, musicFiles: musicFiles).write(to: otioURL)
        print(String(format: "wrote %d clips (%.1fs handles) and %d music files to %@", clipFiles.count, m.handles, Set(musicFiles).count, out.path))
        print("timelines: \(xmlURL.lastPathComponent) (Resolve: File › Import › Timeline; Final Cut: File › Import › XML), \(otioURL.lastPathComponent)")
    default:
        fail("studio list | dump | frame | sheet | beats | apply | handles | conform")
    }
}

/// The music file an edit's snapping should follow: its first music clip, else the project's.
/// Quits Studio before a write when `--quit-studio` is given; fails if it's running otherwise.
func quitStudioIfAsked() async {
    guard StudioProject.isStudioRunning else { return }
    guard flag("--quit-studio") else { fail("Insta360 Studio is running: quit it (or pass --quit-studio) so it doesn't overwrite the edit") }
    print("quitting Insta360 Studio…")
    guard await StudioProject.quitStudio() else { fail("Studio didn't quit (is a save prompt open?)") }
}

/// Relaunches Studio. NSWorkspace launches asynchronously and this process exits right away; `open` waits.
func openStudio() throws { try run("/usr/bin/open", ["-b", StudioProject.bundleID]) }

/// Runs a tool and fails with its exit status if it fails.
func run(_ tool: String, _ arguments: [String]) throws {
    let p = Process()
    p.executableURL = URL(fileURLWithPath: tool)
    p.arguments = arguments
    try p.run()
    p.waitUntilExit()
    if p.terminationStatus != 0 { fail("\((tool as NSString).lastPathComponent) failed (\(p.terminationStatus))") }
}

/// The audio file of each music clip of an edit (of the project's first music clip without `music`).
func musicFiles(_ edit: StudioEdit) throws -> [URL?] {
    guard let music = edit.music else {
        let t = try StudioTimeline(url: try StudioTimeline.locate(edit.project))
        return [t.audio.first.map { URL(fileURLWithPath: $0.url) }]
    }
    return try music.map { m in
        let t = try StudioTimeline(url: try StudioTimeline.locate(m.project ?? edit.project))
        guard m.from >= 1 && m.from <= t.audio.count else { return nil }
        return URL(fileURLWithPath: t.audio[m.from - 1].url)
    }
}

func describe(_ e: DecodingError) -> String {
    switch e {
    case .keyNotFound(let k, let c): return "missing \"\(k.stringValue)\" at \(path(c))"
    case .typeMismatch(_, let c), .valueNotFound(_, let c), .dataCorrupted(let c):
        return "\(c.debugDescription) at \(path(c))"
    @unknown default: return "\(e)"
    }
}

func path(_ c: DecodingError.Context) -> String {
    c.codingPath.map { $0.intValue.map { "[\($0)]" } ?? ".\($0.stringValue)" }.joined()
}

/// Beats as timeline times when `place` (the music clip's start, in-point and end) is given.
func printBeats(_ b: MusicBeats, place: (start: Double, sourceIn: Double, end: Double)?) {
    func tl(_ s: Double) -> Double { place.map { $0.start + s - $0.sourceIn } ?? s }
    func visible(_ s: Double) -> Bool { place.map { tl(s) >= $0.start - 1e-6 && tl(s) <= $0.end + 1e-6 } ?? true }
    let bar = 4 * 60 / b.bpm
    print(String(format: "%@  %.1f BPM, %d beats, bar %.2fs (4/4), %@ long", URL(fileURLWithPath: b.file).lastPathComponent, b.bpm,
                 b.beats.count, bar, clock(b.duration)))
    if place != nil { print("times are on the timeline (music placed at \(clock(place!.start)), from \(clock(place!.sourceIn)) into the file)") }
    print("first beat " + clock(tl(b.beats.first ?? 0)) + ", first downbeat " + clock(tl(b.downbeats.first ?? 0)))
    print("loudness per bar " + sparkline(b.barEnergy.map(Float.init), width: min(b.barEnergy.count, 100), fixedMax: 1))
    print("section changes: " + b.sections.filter(visible).map { clock(tl($0)) }.joined(separator: ", "))
    print("bars (downbeat, loudness):")
    var line = ""
    for (i, d) in b.downbeats.enumerated() where visible(d) {
        let e = i < b.barEnergy.count ? b.barEnergy[i] : 0
        line += String(format: "  %3d %@ %3.0f%%", i + 1, clock(tl(d)), e * 100)
        if (i + 1) % 6 == 0 { print(line); line = "" }
    }
    if !line.isEmpty { print(line) }
}

func printDump(_ t: StudioTimeline) {
    print("\(t.name): \(t.video.count) clips, \(clock(t.videoEnd)) of video, \(fmt(t.fps)) fps")
    print("file \(t.file)")
    for a in t.audio {
        print(String(format: "music  %@  %@–%@ on the timeline (source %@–%@)%@", a.name, clock(a.timelineIn), clock(a.timelineOut),
                     clock(a.sourceIn), clock(a.sourceOut), a.volume.map { "  volume \($0)" } ?? ""))
    }
    for e in t.effects {
        print(String(format: "effect %@  %@–%@ on the timeline (track type %d)", e.name, clock(e.timelineIn), clock(e.timelineOut), e.trackType))
    }
    if let music = t.audio.map(\.timelineOut).max(), abs(music - t.videoEnd) > 0.5 {
        print(String(format: "note: music runs to %@, video to %@", clock(music), clock(t.videoEnd)))
    }
    var previousEnd = 0.0
    for c in t.video {
        let gap = c.timelineIn - previousEnd
        if gap > 0.5 / t.fps { print(String(format: "     (gap %.2fs)", gap)) }
        if gap < -0.5 / t.fps { print(String(format: "     (overlaps the previous clip by %.2fs)", -gap)) }
        previousEnd = max(previousEnd, c.timelineOut)
        var flags: [String] = []
        if c.isCloud { flags.append("cloud") }
        if StudioTimeline.localFile(for: c) == nil { flags.append("no local file") }
        if !c.stabilized { flags.append("stabilization off") }
        if c.directionalLock { flags.append("direction lock") }
        if c.customEasing { flags.append("custom easing") }
        if c.speedChanged { flags.append(String(format: "%@%g× speed", c.variableSpeed ? "variable, " : "", c.rate)) }
        print(String(format: "#%-2d %@–%@ (%5.2fs)  %@  source %@–%@%@", c.index, clock(c.timelineIn), clock(c.timelineOut),
                     c.duration, c.name, clock(c.sourceIn), clock(c.sourceOut),
                     flags.isEmpty ? "" : "  [" + flags.joined(separator: ", ") + "]"))
        if c.keyframes.isEmpty {
            print("      no keyframes" + (c.baseView.map { ": " + describe($0) } ?? ""))
        }
        for k in c.keyframes {
            let v = StudioTimeline.View(pan: k.pan, tilt: k.tilt, roll: k.roll, hfov: k.hfov, distance: k.distance)
            let rel = (k.sourceTime - c.sourceIn) / c.rate
            let inside = rel >= -0.01 && rel <= c.duration + 0.01
            print(String(format: "      %@ %+6.2fs  %@%@", inside ? clock(c.timelineIn + rel) : "  ---  ", rel, describe(v),
                         inside ? "" : "  (outside the trim)"))
        }
    }
}

func describe(_ v: StudioTimeline.View) -> String {
    String(format: "pan %+5.0f°  tilt %+4.0f°%@  fov %3.0f°  distance %.2f", v.pan, v.tilt,
           abs(v.roll) > 0.5 ? String(format: "  roll %+.0f°", v.roll) : "", v.hfov, v.distance)
}

func fmt(_ x: Double) -> String { x == x.rounded() ? String(Int(x)) : String(format: "%.3f", x) }

func parseTime(_ s: String) -> Double? { StudioTimeline.parseTime(s) }

/// Arguments that aren't flags or flag values.
func positionalArgs(_ args: [String], valued: Set<String>) -> [String] {
    var out: [String] = [], skip = false
    for a in args {
        if skip { skip = false; continue }
        if valued.contains(a) { skip = true; continue }
        if !a.hasPrefix("--") { out.append(a) }
    }
    return out
}

/// Renders Studio's view of timeline clips with Reframe's renderer, from each file's LRV proxy.
final class StudioPreviewer {
    let renderer: ReframeRenderer
    let hasLUT: Bool
    var clips: [String: ReframeKit.Clip] = [:]
    var readers: [URL: FrameReader] = [:]

    init(lut: String?) throws {
        renderer = try ReframeRenderer()
        hasLUT = lut != nil
        if let lut { renderer.setLUT(try CubeLUT(url: URL(fileURLWithPath: lut))) }
    }

    func render(_ c: StudioTimeline.Clip, atSource t: Double, width: Int, height: Int) async throws -> (CGImage, StudioTimeline.View)? {
        guard let file = StudioTimeline.localFile(for: c) else { return nil }
        let clip = try clips[file.path] ?? ReframeKit.Clip(url: file)
        clips[file.path] = clip
        guard let source = clip.proxyURL ?? (CloudCache.isStub(file) ? nil : file), let motion = clip.motion else { return nil }
        let reader: FrameReader
        if let r = readers[source] { reader = r } else { reader = try await FrameReader(url: source); readers[source] = reader }
        guard let frames = try reader.frame(at: t) else { return nil }
        let (m, v) = StudioTimeline.bodyFromCamera(c, motion: motion, at: frames.time)
        var view = ViewSettings()
        view.bodyFromCamera = m
        view.hfov = v.hfov
        view.distortion = v.distance
        view.applyLUT = hasLUT && clip.info.colorProfile == "I_Log"
        guard let img = renderer.renderImage(frames: frames, rig: clip.rig, view: view, width: width, height: height, sixteenBit: false) else { return nil }
        return (img, v)
    }

    /// Contact sheets: a header and `perClip` frames for each clip, `rowsPerPage` clips to a page.
    func sheets(name: String, clips: [StudioTimeline.Clip], perClip: Int, columns: Int, tileWidth: Int,
                rowsPerPage: Int, out: URL) async throws -> [URL] {
        let tileH = tileWidth * 9 / 16, header = 26, pad = 6
        let linesPerClip = (perClip + columns - 1) / columns
        let rowH = header + linesPerClip * (tileH + pad)
        let pageW = columns * tileWidth + (columns + 1) * pad
        let pages = stride(from: 0, to: clips.count, by: rowsPerPage).map { Array(clips[$0..<min($0 + rowsPerPage, clips.count)]) }
        let single = out.pathExtension.lowercased() == "png"
        if !single { try FileManager.default.createDirectory(at: out, withIntermediateDirectories: true) }
        var written: [URL] = []
        for (p, page) in pages.enumerated() {
            let pageH = page.count * rowH + pad
            guard let ctx = CGContext(data: nil, width: pageW, height: pageH, bitsPerComponent: 8, bytesPerRow: 0,
                                      space: CGColorSpace(name: CGColorSpace.sRGB)!,
                                      bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue) else { continue }
            ctx.setFillColor(CGColor(gray: 0.08, alpha: 1))
            ctx.fill(CGRect(x: 0, y: 0, width: pageW, height: pageH))
            // CoreGraphics counts y from the bottom; lay out from the top.
            func top(_ y: Int, _ h: Int) -> CGFloat { CGFloat(pageH - y - h) }
            for (r, c) in page.enumerated() {
                let y0 = pad + r * rowH
                var flags = c.keyframes.isEmpty ? "no keyframes" : "\(c.keyframes.count) keyframes"
                if c.customEasing { flags += ", custom easing" }
                if c.speedChanged { flags += String(format: ", %g× speed", c.rate) }
                draw(ctx, String(format: "#%d  %@  %@–%@ (%.1fs)  source %@  %@", c.index, c.name, clock(c.timelineIn),
                                 clock(c.timelineOut), c.duration, clock(c.sourceIn), flags),
                     x: CGFloat(pad), y: top(y0 + 4, 16), size: 14, color: CGColor(gray: 0.95, alpha: 1))
                for i in 0..<perClip {
                    let u = perClip == 1 ? 0.5 : 0.04 + 0.92 * Double(i) / Double(perClip - 1)
                    let tl = c.timelineIn + c.duration * u
                    let x = pad + (i % columns) * (tileWidth + pad)
                    let y = y0 + header + (i / columns) * (tileH + pad)
                    let rect = CGRect(x: CGFloat(x), y: top(y, tileH), width: CGFloat(tileWidth), height: CGFloat(tileH))
                    if let (img, v) = try await render(c, atSource: c.sourceTime(atTimeline: tl), width: tileWidth, height: tileH) {
                        ctx.draw(img, in: rect)
                        let label = String(format: "%@  pan %+.0f° tilt %+.0f° fov %.0f°", clock(tl), v.pan, v.tilt, v.hfov)
                        ctx.setFillColor(CGColor(gray: 0, alpha: 0.6))
                        ctx.fill(CGRect(x: rect.minX, y: rect.minY, width: rect.width, height: 18))
                        draw(ctx, label, x: rect.minX + 4, y: rect.minY + 4, size: 11, color: CGColor(gray: 1, alpha: 1))
                    } else {
                        ctx.setFillColor(CGColor(gray: 0.2, alpha: 1))
                        ctx.fill(rect)
                        draw(ctx, "\(clock(tl))  no local file or proxy", x: rect.minX + 6, y: rect.midY, size: 12,
                             color: CGColor(gray: 0.8, alpha: 1))
                    }
                }
            }
            guard let image = ctx.makeImage() else { continue }
            let url = single && pages.count == 1 ? out
                : (single ? out.deletingLastPathComponent() : out)
                    .appendingPathComponent(String(format: "%@-%02d.png", single ? out.deletingPathExtension().lastPathComponent : "sheet", p + 1))
            try writePNG(image, to: url)
            written.append(url)
        }
        return written
    }

    func draw(_ ctx: CGContext, _ text: String, x: CGFloat, y: CGFloat, size: CGFloat, color: CGColor) {
        let font = CTFontCreateWithName("Menlo" as CFString, size, nil)
        let s = NSAttributedString(string: text, attributes: [
            NSAttributedString.Key(kCTFontAttributeName as String): font,
            NSAttributedString.Key(kCTForegroundColorAttributeName as String): color,
        ])
        ctx.textPosition = CGPoint(x: x, y: y)
        CTLineDraw(CTLineCreateWithAttributedString(s), ctx)
    }
}
