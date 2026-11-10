import Foundation

/// Everything the interest analysis knows about one clip. Signals are stored unweighted, so
/// weights can change (and suggestions re-rank) without re-analysis.
public struct ClipAnalysis: Codable, Sendable {
    /// Bump when signals or their normalization change: cached analyses re-run.
    public static let currentVersion = 3

    public var version = ClipAnalysis.currentVersion
    public var clipName: String
    public var duration: Double
    public var hasAudio: Bool
    public var signals: TimeSignals
    /// Directional signals (faces, people, motion), when the clip has a proxy and gyro.
    public var heading: HeadingGrid?
    public var detections: [Detection]
    /// Where the camera faced per cell (follow heading in the heading grid's frame, degrees).
    public var forward: [Float]
    /// 90th-percentile angular speed per cell (deg/s), for snapping cuts.
    public var spin: [Float]
    public var weights: InterestWeights
    public var suggestions: [Suggestion]
    /// How worth opening this clip is: best suggestion blended with the next two.
    public var score: Double
    /// Where the most interesting moment is (seconds), for the library thumbnail.
    public var bestMoment: Double
    /// Seconds spent per stage.
    public var timings: [String: Double]
    /// `SemanticSignals.version` of the `.appeal`/`.dull` channels, nil without them.
    public var semanticVersion: Int? = nil

    /// Interest over time: time-only signals plus the best direction's interest at each moment.
    public func fused(_ w: InterestWeights? = nil) -> [Float] {
        let w = w ?? weights
        var f = signals.fused(w)
        if let d = heading?.fused(w) {
            let B = HeadingGrid.bins
            for t in 0..<min(f.count, d.count / B) { f[t] += d[(t * B)..<((t + 1) * B)].max() ?? 0 }
        }
        return f
    }

    /// Directional interest, time × 72 heading bins (row-major), or nil without directional analysis.
    public func headingFused(_ w: InterestWeights? = nil) -> [Float]? { heading?.fused(w ?? weights) }

    var inputs: InterestInputs {
        InterestInputs(time: signals, heading: heading, detections: detections, forward: forward, spin: spin)
    }

    /// Re-rank from the cached signals with other weights.
    public func reweighted(_ w: InterestWeights, suggester: ShotSuggester = ShotSuggester()) -> ClipAnalysis {
        var a = self
        a.weights = w
        a.suggestions = suggester.suggest(inputs, weights: w, clipID: clipName, duration: duration)
        (a.score, a.bestMoment) = ClipAnalysis.rank(a.suggestions, fused: a.fused(), rate: signals.rate)
        return a
    }

    static func rank(_ s: [Suggestion], fused: [Float], rate: Double) -> (Double, Double) {
        let top = s.prefix(3).map(\.score)
        let score = top.isEmpty ? 0 : 0.6 * top[0] + 0.4 * top.reduce(0, +) / Double(top.count)
        let best = s.first?.peakTime ?? (fused.indices.max { fused[$0] < fused[$1] }.map { (Double($0) + 0.5) / rate } ?? 0)
        return (score, best)
    }

    /// The strongest weighted contributions at time t (and heading, for directional signals), for
    /// explaining a spot ("2 faces, laughter").
    public func explain(at t: Double, heading yaw: Double? = nil, limit: Int = 3) -> [(signal: Signal, value: Double)] {
        let i = signals.index(t)
        var rows = signals.contributions(weights).map { (signal: $0.key, value: Double($0.value[i])) }
        if let yaw, let h = heading {
            let b = HeadingGrid.bin(yaw: yaw)
            for (s, v) in h.channels where weights[s] != 0 { rows.append((s, Double(v[i * HeadingGrid.bins + b]) * weights[s])) }
        }
        return rows.filter { abs($0.value) > 0.03 }.sorted { abs($0.value) > abs($1.value) }.prefix(limit).map { $0 }
    }

    /// Faces seen within 20° of `yaw` within half a second of t.
    public func faceCount(at t: Double, heading yaw: Double) -> Int {
        var times: [Double: Int] = [:]
        for d in detections where d.kind == .face && abs(d.time - t) <= 0.5 {
            var off = (d.yaw - yaw).truncatingRemainder(dividingBy: 360)
            if off > 180 { off -= 360 } else if off < -180 { off += 360 }
            if abs(off) < 20 { times[d.time, default: 0] += 1 }
        }
        return times.values.max() ?? 0
    }
}

/// Runs the interest analysis for a clip, cached on disk.
public enum Analyzer {
    public static let rate = 4.0
    /// The analysis renderer (no LUT: detectors see the camera's own encoding).
    static let renderer = try? ReframeRenderer()

    /// Use MobileCLIP when it's installed: build the clip's semantic index during the proxy scan
    /// (if there isn't one) and derive the appeal channels from it. The app turns this off while
    /// search is off; without the models it's a no-op either way.
    nonisolated(unsafe) public static var useSemantic = true

    static var semanticEncoder: CLIPEncoder? {
        guard useSemantic, SemanticModels.isInstalled else { return nil }
        return try? CLIPEncoder.shared()
    }

    /// A cached analysis, if one is current (no work otherwise, beyond adding the appeal channels
    /// to one made before the clip had a semantic index, if it has one now).
    public static func cached(_ clip: Clip, weights: InterestWeights = .default) -> ClipAnalysis? {
        let url = cacheURL(for: clip)
        guard let data = try? Data(contentsOf: url),
              var a = try? JSONDecoder().decode(ClipAnalysis.self, from: data), a.version == ClipAnalysis.currentVersion
        else { return nil }
        if !a.hasSemanticSignals, a.heading != nil, let index = SemanticIndexer.cached(clip), let encoder = semanticEncoder,
           let b = try? a.addingSemanticSignals(index, encoder: encoder) {
            a = b.weights == weights ? b : b.reweighted(weights)
            try? JSONEncoder().encode(a).write(to: url, options: .atomic)
        }
        // Handling depends only on the duration: bring an older shape up to date and re-suggest.
        if a.signals.channels[.handling] != nil {
            let h = MotionAnalysis.handling(grid: a.signals, duration: a.duration)
            if a.signals.channels[.handling] != h {
                a.signals[.handling] = h
                a = a.reweighted(a.weights)
                try? JSONEncoder().encode(a).write(to: url, options: .atomic)
            }
        }
        return a.weights == weights ? a : a.reweighted(weights)
    }

    /// Adds the appeal channels to an analysis from a just-built index (and saves it).
    public static func adding(_ index: SemanticIndex, to a: ClipAnalysis, clip: Clip) -> ClipAnalysis? {
        guard !a.hasSemanticSignals, a.heading != nil, let encoder = semanticEncoder,
              let b = try? a.addingSemanticSignals(index, encoder: encoder) else { return nil }
        try? JSONEncoder().encode(b).write(to: cacheURL(for: clip), options: .atomic)
        return b
    }

    public static func analyze(_ clip: Clip, weights: InterestWeights = .default, useCache: Bool = true) async throws -> ClipAnalysis {
        if useCache, let a = cached(clip, weights: weights) { return a }
        let cache = cacheURL(for: clip)

        var timings: [String: Double] = [:]
        let duration = clip.duration
        var grid = TimeSignals(rate: rate, duration: duration)

        // Audio and proxy pixels decode in parallel; motion is in memory already.
        // A cloud clip whose original isn't downloaded has its audio only in the LRV.
        let proxy = clip.proxyURL, url = CloudCache.isStub(clip.url) ? (proxy ?? clip.url) : clip.url
        async let audioTask: ([Float]?, Double) = {
            let t0 = Date()
            let s = try await AudioAnalysis.decodeMono(url: url)
            return (s, Date().timeIntervalSince(t0))
        }()
        // The semantic index rides along with the proxy scan unless it's already built.
        let existingIndex = SemanticIndexer.cached(clip)
        let encoder = semanticEncoder
        async let scanTask: (ProxyScan.Result, Double) = {
            let t0 = Date()
            guard let proxy, let renderer else { return (ProxyScan.Result(), 0) }
            var scan = ProxyScan()
            if clip.info.isTimelapse { scan.detectorRate = 0.5 }
            if existingIndex == nil, clip.motion != nil { scan.encoder = encoder }
            let r = (try? await scan.run(clip: clip, proxy: proxy, renderer: renderer)) ?? ProxyScan.Result()
            return (r, Date().timeIntervalSince(t0))
        }()

        var t0 = Date()
        var spin: [Float] = []
        var forward = [Float](repeating: 0, count: grid.count)
        if let m = clip.motion {
            spin = MotionAnalysis.analyze(m, timelapse: clip.info.isTimelapse, duration: duration, into: &grid)
            let origin = m.baseHeading(at: 0, mode: .lock, smoothing: 0, lockTime: 0)
            forward = (0..<grid.count).map { i in
                var y = (m.baseHeading(at: grid.time(i), mode: .follow, smoothing: 0.8, lockTime: 0) - origin)
                    .truncatingRemainder(dividingBy: 360)
                if y >= 180 { y -= 360 } else if y < -180 { y += 360 }
                return Float(y)
            }
        }
        timings["motion"] = Date().timeIntervalSince(t0)

        let (samples, audioDecode) = try await audioTask
        timings["audio decode"] = audioDecode
        if let samples, !samples.isEmpty {
            t0 = Date()
            try AudioAnalysis.analyze(samples: samples, into: &grid)
            timings["audio analysis"] = Date().timeIntervalSince(t0)
        }

        let (scan, scanTime) = await scanTask
        timings["proxy scan"] = scanTime
        for (k, v) in scan.timings { timings["  " + k] = v }
        PictureAnalysis.analyze(scan.pictures, into: &grid)
        if !scan.histograms.isEmpty { grid[.novelty] = DirectionalAnalysis.novelty(scan, time: grid) }
        var heading: HeadingGrid?
        if clip.motion != nil, !scan.pictures.isEmpty {
            t0 = Date()
            heading = DirectionalAnalysis.build(scan, spin: spin, forward: forward, time: grid)
            timings["directional"] = Date().timeIntervalSince(t0)
        }
        var semanticVersion: Int?
        if let index = existingIndex ?? scan.semantic.map({ SemanticIndexer.store($0, for: clip) }),
           let encoder, heading != nil {
            t0 = Date()
            if (try? SemanticSignals.apply(index, to: &heading!, forward: forward, encoder: encoder)) != nil {
                semanticVersion = SemanticSignals.version
            }
            timings["semantic signals"] = Date().timeIntervalSince(t0)
        }

        var a = ClipAnalysis(clipName: clip.name, duration: duration, hasAudio: !(samples ?? []).isEmpty, signals: grid,
                             heading: heading, detections: scan.detections, forward: forward,
                             spin: spin, weights: weights, suggestions: [], score: 0, bestMoment: 0, timings: timings,
                             semanticVersion: semanticVersion)
        t0 = Date()
        a = a.reweighted(weights)
        a.timings["suggest"] = Date().timeIntervalSince(t0)
        try? FileManager.default.createDirectory(at: cache.deletingLastPathComponent(), withIntermediateDirectories: true)
        try? JSONEncoder().encode(a).write(to: cache, options: .atomic)
        return a
    }

    /// `~/Library/Application Support/Reframe/analysis/<name>-<size>-<mtime>.json`
    /// Delete cached analyses left behind by earlier versions of these clips' files (a stub replaced
    /// by its original, a re-fetch, a re-copied file): for each clip's name, every entry except the
    /// ones matching a given clip's current file. Other clips' entries are untouched (still valid if
    /// that folder is opened again). Returns how many files and bytes were removed.
    @discardableResult
    public static func pruneCache(keeping clips: [Clip]) -> (files: Int, bytes: Int64) {
        guard let dir = clips.first.map({ cacheURL(for: $0).deletingLastPathComponent() }) else { return (0, 0) }
        let names = Set(clips.map(\.name))
        let keep = Set(clips.map { cacheURL(for: $0).lastPathComponent })
        let files = (try? FileManager.default.contentsOfDirectory(at: dir, includingPropertiesForKeys: [.fileSizeKey])) ?? []
        var removed = 0, bytes: Int64 = 0
        for f in files where f.pathExtension == "json" && !keep.contains(f.lastPathComponent) {
            // <clip name>-<size>-<mtime>.json; the clip name itself may contain dashes.
            let parts = f.deletingPathExtension().lastPathComponent.split(separator: "-", omittingEmptySubsequences: false)
            guard parts.count >= 3, parts.suffix(2).allSatisfy({ Int64($0) != nil }),
                  names.contains(parts.dropLast(2).joined(separator: "-")) else { continue }
            let size = Int64((try? f.resourceValues(forKeys: [.fileSizeKey]).fileSize) ?? 0)
            if (try? FileManager.default.removeItem(at: f)) != nil { removed += 1; bytes += size }
        }
        return (removed, bytes)
    }

    public static func cacheURL(for clip: Clip) -> URL {
        let attrs = try? FileManager.default.attributesOfItem(atPath: clip.url.path)
        let size = (attrs?[.size] as? NSNumber)?.int64Value ?? 0
        let mtime = Int((attrs?[.modificationDate] as? Date)?.timeIntervalSince1970 ?? 0)
        return FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Reframe/analysis", isDirectory: true)
            .appendingPathComponent("\(clip.name)-\(size)-\(mtime).json")
    }
}
