import Foundation
import ReframeKit
import simd

// reframe-cli: headless access to ReframeKit, mainly to check the Metal renderer against render.py.
//
//   reframe-cli info <file.insv>
//   reframe-cli still <file.insv> <seconds> <out.png> [--equirect] [--width N] [--height N]
//                     [--yaw D] [--pitch D] [--hfov D] [--proxy] [--lut file.cube] [--no-level]
//   reframe-cli analyze <file.insv | folder>... [--json] [--no-cache] [--preset blend|people|action|scenery]
//                       [--heat] [--stills dir [--lut file.cube]]
//   reframe-cli evaluate <footage folder> [--preset P] [--json]   (scores suggestions against your shots)
//   reframe-cli cloud login|logout|status|list [--raw]|probe <name|mediaId>   (Insta360 Cloud spike, CLOUD.md)

func fail(_ msg: String) -> Never {
    FileHandle.standardError.write((msg + "\n").data(using: .utf8)!)
    exit(1)
}

var args = Array(CommandLine.arguments.dropFirst())
guard let command = args.first else { fail("usage: reframe-cli info|still ...") }
args.removeFirst()

func option(_ name: String) -> String? {
    guard let i = args.firstIndex(of: name), i + 1 < args.count else { return nil }
    return args[i + 1]
}
func flag(_ name: String) -> Bool { args.contains(name) }

switch command {
case "info":
    guard let path = args.first else { fail("info <file.insv>") }
    let clip = try Clip(url: URL(fileURLWithPath: path))
    let i = clip.info
    print("camera      \(i.camera)  fw \(i.firmware)  serial \(i.serial)")
    print("profile     \(i.colorLabel)  fps \(i.nominalFPS ?? 0)  duration \(clip.duration)s  timelapse \(i.isTimelapse)")
    print("readout     \(i.rollingShutterMs ?? 0) ms  raw imu \(i.rawIMU)  track order \(i.trackOrder ?? 0)")
    print("proxy       \(clip.proxyURL?.lastPathComponent ?? "none")")
    for (n, lens) in clip.rig.lenses.enumerated() {
        print("lens \(n)      xi \(lens.xi) f \(lens.fx) c (\(lens.cx), \(lens.cy)) type \(lens.lensType) V\(lens.generation)")
    }
    let t0 = Date()
    if let m = clip.motion {
        let s = m.shakiness()
        print(String(format: "motion      %d samples in %.2fs  median %.1f°/s  p95 %.1f°/s",
                     m.times.count, Date().timeIntervalSince(t0), s.median, s.p95))
    }

case "still":
    guard args.count >= 3, let t = Double(args[1]) else { fail("still <file.insv> <seconds> <out.png> [options]") }
    let clip = try Clip(url: URL(fileURLWithPath: args[0]))
    let out = URL(fileURLWithPath: args[2])
    let useProxy = flag("--proxy")
    guard let source = useProxy ? clip.proxyURL : clip.url else { fail("no proxy for this clip") }
    let reader = try await FrameReader(url: source)
    guard let frames = try reader.frame(at: t) else { fail("no frame at \(t)") }
    let renderer = try ReframeRenderer()
    if let lutPath = option("--lut") { renderer.setLUT(try CubeLUT(url: URL(fileURLWithPath: lutPath))) }

    var view = ViewSettings()
    view.projection = flag("--equirect") ? .equirect : .flat
    view.hfov = Double(option("--hfov") ?? "90") ?? 90
    view.distortion = Double(option("--distortion") ?? "0") ?? 0
    view.applyLUT = option("--lut") != nil
    let yaw = Double(option("--yaw") ?? "0") ?? 0, pitch = Double(option("--pitch") ?? "0") ?? 0
    let user = rot(.z, yaw) * rot(.y, -pitch)
    if flag("--no-level") {
        view.bodyFromCamera = user
    } else if let mode = option("--stab").flatMap(Motion.Stabilization.init(rawValue:)), let m = clip.motion {
        // Same view as a shot in that stabilization mode (smoothing 0.8, as the app defaults).
        view.bodyFromCamera = m.bodyFromCamera(at: frames.time, mode: mode, smoothing: 0.8, lockTime: 0,
                                               yaw: yaw, pitch: pitch, roll: 0)
        print(String(format: "view base heading %.1f° (raw %.1f°)",
                     m.baseHeading(at: frames.time, mode: mode, smoothing: 0.8, lockTime: 0),
                     m.heading(at: frames.time, smoothing: 0) * 180 / .pi))
    } else if let m = clip.motion {
        // Same as render.py stills: world-levelled, heading from the gyro integration.
        view.bodyFromCamera = simd_double3x3(m.orientation(at: frames.time)).transpose * user
    }
    let width = Int(option("--width") ?? "") ?? (view.projection == .flat ? 1920 : 3840)
    let height = Int(option("--height") ?? "") ?? (view.projection == .flat ? 1080 : width / 2)
    let start = Date()
    guard let image = renderer.renderImage(frames: frames, rig: clip.rig, view: view, width: width, height: height) else {
        fail("render failed")
    }
    try writePNG(image, to: out)
    print(String(format: "wrote %@ %dx%d (frame %.3fs, render %.0f ms, %@)", out.path, width, height, frames.time,
                 Date().timeIntervalSince(start) * 1000, useProxy ? "proxy" : "master"))

case "export":
    // reframe-cli export <file.insv> <in> <out> <dest-dir> [--yaw --pitch --hfov --stab follow|lock|none
    //                    --size hd|uhd|vertical|square --codec proRes422HQ --name N --bake-lut file.cube]
    guard args.count >= 4, let tIn = Double(args[1]), let tOut = Double(args[2]) else {
        fail("export <file.insv> <in> <out> <dest-dir> [options]")
    }
    let clip = try Clip(url: URL(fileURLWithPath: args[0]))
    var options = ExportOptions(destination: URL(fileURLWithPath: args[3]))
    options.size = ExportOptions.Size(rawValue: option("--size") ?? "hd") ?? .hd
    options.codec = ExportOptions.Codec(rawValue: option("--codec") ?? "proRes422HQ") ?? .proRes422HQ
    var lut: CubeLUT?
    if let path = option("--bake-lut") { lut = try CubeLUT(url: URL(fileURLWithPath: path)); options.bakeLook = true }
    let framing = Framing(yaw: Double(option("--yaw") ?? "0") ?? 0, pitch: Double(option("--pitch") ?? "0") ?? 0,
                          hfov: Double(option("--hfov") ?? "88") ?? 88,
                          distortion: Double(option("--distortion") ?? "0.4") ?? 0.4)
    let shot = Shot(clipPath: clip.url.path, name: option("--name") ?? "\(clip.name) shot", inTime: tIn, outTime: tOut,
                    framing: framing, stabilization: Motion.Stabilization(rawValue: option("--stab") ?? "follow") ?? .follow,
                    smoothing: 0.8, lockTime: tIn)
    let exporter = try ShotExporter(lut: lut)
    let start = Date()
    let result = try await exporter.export(shot, clip: clip, options: options, progress: { _ in }, isCancelled: { false })
    let secs = Date().timeIntervalSince(start)
    print(String(format: "wrote %@: %d frames in %.1fs (%.1f fps), timecode frame %d, audio %@",
                 result.url.lastPathComponent, result.frameCount, secs, Double(result.frameCount) / secs,
                 result.timecodeFrames, result.hasAudio ? "yes" : "no"))
    let dest = URL(fileURLWithPath: args[3])
    try Interchange.fcpxml([result], title: "Reframe selects").write(to: dest.appendingPathComponent("Reframe selects.fcpxml"),
                                                                   atomically: true, encoding: .utf8)
    try Interchange.otio([result], title: "Reframe selects").write(to: dest.appendingPathComponent("Reframe selects.otio"))
    print("wrote timelines to \(dest.path)")

case "studio":
    // reframe-cli studio <footage-folder> <project-name> [--register] [--replace] [--root dir] [--tolerance deg] [--fps 24]
    // Writes the folder's saved shots as an Insta360 Studio timeline project.
    guard args.count >= 2 else { fail("studio <footage-folder> <project-name> [--register] [--replace] [--root dir]") }
    // "cloud" = the shots on Insta360 Cloud clips (their own store).
    let store = ShotStore.load(folder: args[0] == "cloud" ? CloudCache.root : URL(fileURLWithPath: args[0]))
    var clips: [String: Clip] = [:]
    var items: [StudioProject.Item] = []
    for shot in store.shots.sorted(by: { $0.created < $1.created }) {
        let clip = try clips[shot.clipPath] ?? Clip(url: URL(fileURLWithPath: shot.clipPath))
        clips[shot.clipPath] = clip
        guard let item = StudioProject.Item(shot: shot, clip: clip) else { fail("no frame rate for \(shot.clipPath)") }
        items.append(item)
        print(String(format: "  %@  %@  %.2f-%.2fs  %d keyframes  (%.0f fps in Studio)", shot.name,
                     clip.name, shot.inTime, shot.outTime, shot.keyframes.count, item.fps))
    }
    let root = option("--root").map { URL(fileURLWithPath: $0) }
    if let t = option("--tolerance").flatMap(Double.init) { StudioProject.tolerance = t }
    do {
        let file = try StudioProject.write(name: args[1], items: items,
                                           fps: option("--fps").flatMap(Double.init), register: flag("--register"),
                                           replace: flag("--replace"), root: root)
        print("wrote \(file.path)" + (flag("--register") ? " and added it to Studio's project list" : ""))
    } catch let e as StudioProject.Failure {
        fail(e.description)
    }

case "analyze":
    var paths: [String] = []
    var skip = false
    for a in args {
        if skip { skip = false; continue }
        if ["--preset", "--stills", "--lut"].contains(a) { skip = true; continue }
        if !a.hasPrefix("--") { paths.append(a) }
    }
    guard !paths.isEmpty else { fail("analyze <file.insv | folder>... [--json] [--no-cache] [--preset P]") }
    var urls: [URL] = []
    for p in paths {
        var isDir: ObjCBool = false
        let u = URL(fileURLWithPath: p)
        if FileManager.default.fileExists(atPath: p, isDirectory: &isDir), isDir.boolValue { urls += Clip.scan(folder: u) } else { urls.append(u) }
    }
    let weights = InterestWeights.preset(InterestWeights.Preset(rawValue: option("--preset") ?? "blend") ?? .blend)
    var results: [ClipAnalysis] = []
    for u in urls {
        let clip = try Clip(url: u)
        let start = Date()
        let a = try await Analyzer.analyze(clip, weights: weights, useCache: !flag("--no-cache"))
        let secs = Date().timeIntervalSince(start)
        results.append(a)
        if flag("--json") { continue }
        print(String(format: "\n%@  %@, %.1fs  analyzed in %.1fs (%.2f× real time)  score %.2f  best %@",
                     clip.name, clip.info.isTimelapse ? "timelapse" : clip.info.colorLabel, a.duration, secs,
                     secs / max(a.duration, 1), a.score, clock(a.bestMoment)))
        print("  " + a.timings.sorted { $0.key < $1.key }.map { String(format: "%@ %.2fs", $0.key.trimmingCharacters(in: .whitespaces), $0.value) }.joined(separator: ", "))
        print("  interest " + sparkline(a.fused(), width: 100))
        for s in Signal.allCases {
            let v = a.signals[s]
            guard let mx = v.max(), mx > 0.05 else { continue }
            print(String(format: "  %-9@", s.rawValue) + "" + sparkline(v, width: 100, fixedMax: 1))
        }
        if let h = a.heading {
            let faces = a.detections.filter { $0.kind == .face }.count, people = a.detections.count - faces
            print("  detections: \(faces) faces, \(people) people")
            let B = HeadingGrid.bins
            for ch in [Signal.faces, .people, .motion, .salience] {
                let v = h[ch]
                let perT = (0..<h.count).map { t in v[(t * B)..<((t + 1) * B)].max() ?? 0 }
                print(String(format: "  %-9@", ch.rawValue) + sparkline(perT, width: 100, fixedMax: 1))
            }
            if flag("--heat"), let d = a.headingFused() {
                // Rows of 30° from Left (+90) through Ahead (0) to Behind, like the heading lane.
                print("  heat (rows: yaw +165 at top … -165 at bottom; x = time)")
                let top = max(d.max() ?? 1, 1e-3)
                for row in 0..<12 {
                    let y0 = 180 - Double(row + 1) * 30
                    let bins = (0..<6).map { HeadingGrid.bin(yaw: y0 + Double($0) * 5 + 2.5) }
                    let rowVals = (0..<h.count).map { t in bins.map { d[t * B + $0] }.max() ?? 0 }
                    print(String(format: "  %+4.0f ", y0 + 15) + sparkline(rowVals, width: 100, fixedMax: top))
                }
            }
        }
        if let dir = option("--stills") {
            // Each suggestion's peak frame, seen through its own path: a contact sheet for tuning.
            let out = URL(fileURLWithPath: dir)
            try FileManager.default.createDirectory(at: out, withIntermediateDirectories: true)
            let renderer = try ReframeRenderer()
            if let lut = option("--lut") { renderer.setLUT(try CubeLUT(url: URL(fileURLWithPath: lut))) }
            if let proxy = clip.proxyURL, let m = clip.motion {
                let reader = try await FrameReader(url: proxy)
                for (i, sg) in a.suggestions.enumerated() {
                    guard let frames = try reader.frame(at: sg.peakTime) else { continue }
                    let shot = sg.shot(clipPath: clip.url.path, name: "")
                    let f = shot.framing(at: frames.time)
                    var view = ViewSettings()
                    view.hfov = f.hfov; view.distortion = f.distortion
                    view.applyLUT = option("--lut") != nil && clip.info.colorProfile == "I_Log"
                    view.bodyFromCamera = m.bodyFromCamera(at: frames.time, mode: shot.stabilization, smoothing: shot.smoothing,
                                                           lockTime: shot.lockTime, yaw: f.yaw, pitch: f.pitch, roll: 0)
                    if let img = renderer.renderImage(frames: frames, rig: clip.rig, view: view, width: 480, height: 270, sixteenBit: false) {
                        try writePNG(img, to: out.appendingPathComponent("\(clip.name)-\(i + 1).png"))
                    }
                }
            }
        }
        for (i, s) in a.suggestions.enumerated() {
            print(String(format: "  %d. %@–%@ (%4.1fs)  score %.2f  peak %@  %@", i + 1, clock(s.inTime), clock(s.outTime),
                         s.duration, s.score, clock(s.peakTime), s.summary))
            if !s.keyframes.isEmpty {
                print("       " + s.lens.label + ", pitch " + String(format: "%+.0f°", s.keyframes[0].framing.pitch) + ", path "
                      + s.keyframes.map { String(format: "%@ %+.0f°", clock($0.time), $0.framing.yaw) }.joined(separator: " → "))
            }
        }
    }
    if flag("--json") {
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys]
        FileHandle.standardOutput.write(try enc.encode(results))
    } else if results.count > 1 {
        print("\nRanking")
        for a in results.sorted(by: { $0.score > $1.score }) {
            print(String(format: "  %.2f  %@  (%d suggestions)", a.score, a.clipName, a.suggestions.count))
        }
    }

case "evaluate":
    // reframe-cli evaluate <footage folder> [--preset P] [--json]
    // Scores suggestions against the shots you kept and the suggestions you dismissed in the app.
    guard let path = args.first(where: { !$0.hasPrefix("--") && args.firstIndex(of: $0).map { $0 == 0 || args[$0 - 1] != "--preset" } ?? true })
    else { fail("evaluate <footage folder> [--preset P] [--json]") }
    let folder = URL(fileURLWithPath: path)
    let store = ShotStore.load(folder: folder)
    let clips = Clip.scan(folder: folder).compactMap { try? Clip(url: $0) }
        .filter { c in store.shots.contains { $0.clipPath == c.url.path } || store.dismissed.contains { $0.hasPrefix(c.name + "@") } }
    guard !clips.isEmpty else { fail("no kept shots or dismissed suggestions for \(folder.path) yet: keep or dismiss some in the app first") }
    let presets = option("--preset").flatMap(InterestWeights.Preset.init(rawValue:)).map { [$0] } ?? InterestWeights.Preset.allCases
    var evaluations: [Evaluation] = []
    for p in presets {
        var analyses: [URL: ClipAnalysis] = [:]
        for c in clips { analyses[c.url] = try await Analyzer.analyze(c, weights: .preset(p)) }
        evaluations.append(Evaluator.evaluate(clips: clips, analyses: analyses, store: store, preset: p))
    }
    if flag("--json") {
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys]
        FileHandle.standardOutput.write(try enc.encode(evaluations))
        exit(0)
    }
    func pct(_ x: Double?) -> String { x.map { String(format: "%3.0f%%", $0 * 100) } ?? "  –" }
    func deg(_ x: Double?) -> String { x.map { String(format: "%4.0f°", $0) } ?? "   –" }
    let first = evaluations[0]
    let kept = first.clips.reduce(0) { $0 + $1.kept.count }, dismissed = first.clips.reduce(0) { $0 + $1.dismissed }
    print("Ground truth: \(first.ownShotCount) shots you marked yourself, \(kept) kept suggestions, \(dismissed) dismissed, across \(first.clips.count) clips")
    if first.ownShotCount < 10 { print("(Too few to tune on: aim for 5–10 clips with your real picks. Numbers below are indicative only.)") }
    print("")
    func col(_ s: String, _ w: Int) -> String { s.count >= w ? s : s + String(repeating: " ", count: w - s.count) }
    print(col("preset", 10) + col("recall @1", 11) + col("@3", 6) + col("any", 6) + col("aim off", 9) + col("≤30°", 7)
          + col("keep rate", 11) + col("re-aimed", 10) + "dismissed in top 3")
    for e in evaluations {
        let r = e.keptReaimed
        print(col(e.preset.rawValue, 10) + col(pct(e.recall(at: 1)), 11) + col(pct(e.recall(at: 3)), 6)
              + col(pct(e.recall(at: nil)), 6) + col(deg(e.aimMean), 9) + col(pct(e.aimWithin30), 7)
              + col(pct(e.keepRate), 11) + col("\(r.count) of \(r.of)", 10) + "\(e.dismissedStillTop3)")
    }
    let demos = first.clips.flatMap { $0.ownShots + $0.kept }.filter(\.looksLikeDemo)
    for d in demos {
        print("Note: \(d.clip) \(d.shot) looks like the REFRAME_DEMO_SHOT debug shot, not a real pick; delete it in the app for honest numbers.")
    }
    if let sp = first.speed { print(String(format: "\nAnalysis speed: %.2f× real time", sp)) }
    // Detail for the first (or chosen) preset.
    print("\nPer shot (\(first.preset.rawValue)):")
    for c in first.clips {
        for m in c.ownShots {
            print(String(format: "  %@ %@ %@–%@  yours     %@  %@", c.clip, m.shot, clock(m.inTime), clock(m.outTime),
                         m.rank.map { "suggestion #\($0)" } ?? "missed      ", m.aimMean.map { String(format: "aim off %.0f° (worst %.0f°)", $0, m.aimWorst ?? 0) } ?? ""))
        }
        for m in c.kept {
            print(String(format: "  %@ %@ %@–%@  kept      %@  %@", c.clip, m.shot, clock(m.inTime), clock(m.outTime),
                         m.rank.map { "now #\($0)" } ?? "no longer suggested", m.aimMean.map { String(format: "re-aimed %.0f° (worst %.0f°)", $0, m.aimWorst ?? 0) } ?? "kept before paths were recorded"))
        }
        for id in c.dismissedStillTop3 { print("  \(c.clip) dismissed \(id) is still in the top 3") }
    }

case "cloud":
    // Sign-in runs AppKit, which must stay on the main thread (top-level code is; async code isn't).
    if args.first == "login" { cloudLogin() }
    await runCloud(args)

default:
    fail("unknown command \(command)")
}


func clock(_ t: Double) -> String { String(format: "%d:%04.1f", Int(t) / 60, t.truncatingRemainder(dividingBy: 60)) }

/// Block-character sparkline, resampled (max) to `width` columns.
func sparkline(_ x: [Float], width: Int, fixedMax: Float? = nil) -> String {
    guard !x.isEmpty else { return "" }
    let blocks = Array(" ▁▂▃▄▅▆▇█")
    let mx = fixedMax ?? max(x.max() ?? 1, 1e-6)
    return String((0..<width).map { c in
        let a = c * x.count / width, b = max(a + 1, (c + 1) * x.count / width)
        let v = max(x[a..<min(b, x.count)].max() ?? 0, 0)
        return blocks[min(Int(v / mx * 8 + 0.5), 8)]
    })
}
