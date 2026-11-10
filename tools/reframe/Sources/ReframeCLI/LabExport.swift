import CoreGraphics
import Foundation
import ImageIO
import ReframeKit
import UniformTypeIdentifiers

/// reframe-cli lab-export <footage folder | cloud> --out <dir> [--every 8] [--shot-every 2] [--size 384] [--lut file.cube]
///
/// Dumps what the reframe lab (tools/reframe-lab) labels and scores: for sampled moments of every
/// clip with a semantic index, the index's eight world-locked 90° views as JPEGs (with the LUT, so
/// people and VLMs see normal colour), each view's MobileCLIP embedding (the index row itself, so
/// prompt scores in the lab match the app's exactly), what today's analysis says about that
/// direction, and where you aimed if the moment is inside one of your shots.
///
/// Writes <out>/manifest.json, <out>/embeddings.f16 (row-major [view][512] Float16) and
/// <out>/views/<clip>/<time>_<view>.jpg. Re-running replaces the manifest and embeddings and only
/// renders images that aren't there yet.
func labExport(_ folder: URL, out: URL) async throws {
    let every = Double(option("--every") ?? "") ?? 8
    let shotEvery = Double(option("--shot-every") ?? "") ?? 2
    let size = Int(option("--size") ?? "") ?? 384
    let lutURL = option("--lut").map { URL(fileURLWithPath: $0) }
        ?? CubeLUT.find(in: FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Movies/X6_I-Log_LUTs")).first
    let lut = try lutURL.map { try CubeLUT(url: $0) }

    let store = ShotStore.load(folder: folder)
    let renderer = try ReframeRenderer()
    if let lut { renderer.setLUT(lut) }
    try FileManager.default.createDirectory(at: out.appendingPathComponent("views"), withIntermediateDirectories: true)

    // Keep every moment the previous export had (unless --fresh), so labels and model scores keyed
    // by view id survive re-exports that add shots.
    var previous: [String: Set<String>] = [:]
    if !CommandLine.arguments.contains("--fresh"),
       let data = try? Data(contentsOf: out.appendingPathComponent("manifest.json")),
       let old = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
       let oldViews = old["views"] as? [[String: Any]] {
        for v in oldViews {
            if let c = v["clip"] as? String, let t = v["time"] as? Double {
                previous[c, default: []].insert(String(format: "%.2f", t))
            }
        }
    }

    var views: [[String: Any]] = []
    var clipsOut: [[String: Any]] = []
    var embeddings = Data()
    var rendered = 0
    let started = Date()

    for url in Clip.scan(folder: folder) {
        guard let clip = try? Clip(url: url), let motion = clip.motion, let proxy = clip.proxyURL,
              let index = SemanticIndexer.cached(clip) else { continue }
        let a = try await Analyzer.analyze(clip)
        let shots = store.shots.filter { $0.clipPath == clip.url.path }
        let origin = motion.baseHeading(at: 0, mode: .lock, smoothing: 0, lockTime: 0)
        func userYaw(_ shot: Shot, _ t: Double) -> Double {
            var y = (motion.baseHeading(at: t, mode: shot.stabilization, smoothing: shot.smoothing, lockTime: shot.lockTime)
                + shot.framing(at: t).yaw - origin).truncatingRemainder(dividingBy: 360)
            if y >= 180 { y -= 360 } else if y < -180 { y += 360 }
            return y
        }

        // Moments: every `every` seconds, and every `shotEvery` inside your shots (index rows, 1 Hz).
        var rows: [Int] = []
        var lastPicked = -Double.infinity
        let kept = previous[clip.name] ?? []
        for (j, t) in index.times.enumerated() {
            let inShot = shots.contains { t >= $0.inTime && t <= $0.outTime }
            if kept.contains(String(format: "%.2f", t)) || t - lastPicked >= (inShot ? shotEvery : every) - 0.01 {
                rows.append(j); lastPicked = t
            }
        }
        guard !rows.isEmpty else { continue }

        // Analysis values per view direction: interest without and with the CLIP channels.
        var plain = a.weights
        plain.values[.appeal] = 0; plain.values[.dull] = 0
        let fusedPlain = a.headingFused(plain), fusedFull = a.headingFused()
        let margin = try SemanticSignals.margin(index, encoder: try CLIPEncoder.shared())
        let B = HeadingGrid.bins
        let topPlain = max(fusedPlain?.max() ?? 1, 1e-6), topFull = max(fusedFull?.max() ?? 1, 1e-6)

        // Render what's missing, in one pass over the proxy.
        let clipDir = out.appendingPathComponent("views/\(clip.name)", isDirectory: true)
        try FileManager.default.createDirectory(at: clipDir, withIntermediateDirectories: true)
        func file(_ j: Int, _ v: Int) -> String { String(format: "views/%@/%07.2f_%d.jpg", clip.name, index.times[j], v) }
        let todo = rows.filter { j in (0..<index.viewCount).contains { !FileManager.default.fileExists(atPath: out.appendingPathComponent(file(j, $0)).path) } }
        if !todo.isEmpty {
            let reader = try await FrameReader(url: proxy)
            let seq = try reader.sequence(from: 0)
            var k = 0
            while k < todo.count, let f = seq.next() {
                // The index sampled the first frame at or after each 1 Hz tick: match it exactly.
                guard f.time >= index.times[todo[k]] - 1e-3 else { continue }
                let j = todo[k]
                for (v, yaw) in index.yaws.enumerated() {
                    var view = ViewSettings()
                    view.projection = .flat
                    view.hfov = index.fov
                    view.distortion = 0
                    view.applyLUT = lut != nil && clip.info.colorProfile == "I_Log"
                    view.bodyFromCamera = motion.bodyFromCamera(at: f.time, mode: .lock, smoothing: 0, lockTime: 0,
                                                                yaw: yaw, pitch: 0, roll: 0)
                    if let image = renderer.renderImage(frames: f, rig: clip.rig, view: view, width: size, height: size,
                                                        sixteenBit: false) {
                        try writeJPEG(image, to: out.appendingPathComponent(file(j, v)))
                        rendered += 1
                    }
                }
                k += 1
            }
            seq.cancel()
        }

        for j in rows {
            let t = index.times[j]
            let c = min(max(Int(t * a.signals.rate), 0), a.signals.count - 1)
            let shot = shots.first { t >= $0.inTime && t <= $0.outTime }
            let aim = shot.map { userYaw($0, t) }
            for (v, yaw) in index.yaws.enumerated() {
                let row = j * index.viewCount + v
                let b = HeadingGrid.bin(yaw: yaw)
                // A view spans 90°: take the best bin within ±20° of its centre for the grid values.
                let span = (-4...4).map { (b + $0 + B) % B }
                func gridMax(_ g: [Float]?) -> Double { g.map { g in Double(span.map { g[c * B + $0] }.max() ?? 0) } ?? 0 }
                var rec: [String: Any] = [
                    "id": "\(clip.name)|\(String(format: "%.2f", t))|\(v)",
                    "clip": clip.name, "time": t, "view": v, "yaw": yaw, "row": views.count,
                    "file": file(j, v),
                    "pred": [
                        "clip_margin": Double(margin[row]),
                        "appeal": gridMax(a.heading?.channels[.appeal]),
                        "dull": gridMax(a.heading?.channels[.dull]),
                        "faces": Double(a.faceCount(at: t, heading: yaw)),
                        "interest": gridMax(fusedPlain) / Double(topPlain),
                        "interest_clip": gridMax(fusedFull) / Double(topFull),
                    ] as [String: Double],
                ]
                if let shot, let aim {
                    var d = abs(yaw - aim).truncatingRemainder(dividingBy: 360)
                    if d > 180 { d = 360 - d }
                    rec["shot"] = shot.name
                    rec["shot_source"] = shotSource(shot)
                    rec["aim"] = aim
                    rec["aim_err"] = d
                }
                views.append(rec)
                index.vectors.withUnsafeBytes { raw in
                    let o = row * index.dimension * 2
                    embeddings.append(contentsOf: raw[o..<(o + index.dimension * 2)])
                }
            }
        }
        clipsOut.append(["name": clip.name, "path": clip.url.path, "duration": clip.duration,
                         "color": clip.info.colorLabel, "shots": shots.map { ["name": $0.name, "in": $0.inTime, "out": $0.outTime, "source": shotSource($0)] }])
        print(String(format: "%@  %d moments  %d shot views", clip.name, rows.count,
                     views.filter { ($0["clip"] as? String) == clip.name && $0["shot"] != nil }.count))
    }

    let manifest: [String: Any] = [
        "version": 1, "created": ISO8601DateFormatter().string(from: Date()),
        "source": folder.path, "lut": lutURL?.lastPathComponent ?? NSNull(),
        "model": CLIPEncoder.modelName, "dimension": CLIPEncoder.dimension, "viewsPerMoment": SemanticIndexer.viewCount,
        "fov": SemanticIndexer.fov, "clips": clipsOut, "views": views,
    ]
    try JSONSerialization.data(withJSONObject: manifest, options: [.sortedKeys]).write(to: out.appendingPathComponent("manifest.json"), options: .atomic)
    try embeddings.write(to: out.appendingPathComponent("embeddings.f16"), options: .atomic)
    print(String(format: "%d clips, %d views (%d rendered) in %.0fs -> %@", clipsOut.count, views.count, rendered,
                 Date().timeIntervalSince(started), out.path))
}

/// Where a shot's aim came from: "manual" (you marked it), "reaimed" (a kept suggestion you
/// re-aimed) or "suggested" (a kept suggestion with its aim untouched, so it says little about
/// your taste in direction and grades the app against itself).
func shotSource(_ shot: Shot) -> String {
    guard shot.suggestionID != nil else { return "manual" }
    return shot.suggestedKeyframes == shot.keyframes ? "suggested" : "reaimed"
}

/// reframe-cli clip-text: reads a JSON array of strings on stdin, writes a JSON array of unit
/// MobileCLIP text embeddings (for the lab's prompt playground).
func clipText() throws {
    let input = FileHandle.standardInput.readDataToEndOfFile()
    guard let texts = try JSONSerialization.jsonObject(with: input) as? [String] else { fail("expected a JSON array of strings on stdin") }
    let encoder = try CLIPEncoder.shared()
    let vectors = try texts.map { try encoder.embed(text: $0).map(Double.init) }
    FileHandle.standardOutput.write(try JSONSerialization.data(withJSONObject: vectors))
}

func writeJPEG(_ image: CGImage, to url: URL, quality: Double = 0.82) throws {
    guard let dest = CGImageDestinationCreateWithURL(url as CFURL, UTType.jpeg.identifier as CFString, 1, nil) else {
        throw NSError(domain: "Reframe", code: 2)
    }
    CGImageDestinationAddImage(dest, image, [kCGImageDestinationLossyCompressionQuality: quality] as CFDictionary)
    guard CGImageDestinationFinalize(dest) else { throw NSError(domain: "Reframe", code: 3) }
}
