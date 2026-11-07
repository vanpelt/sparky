import CoreML
import CoreVideo
import Foundation

// Text search over the heading grid: MobileCLIP image embeddings of world-locked horizon views,
// so a typed query ("dog in the water") becomes an interest channel I(t, heading) that the
// shot suggester can aim at. Models live outside the repo in
// ~/Library/Application Support/Reframe/models (see SEMANTIC.md).

/// MobileCLIP-S2's image and text encoders (Core ML, Neural Engine), with the CLIP tokenizer.
public final class CLIPEncoder: @unchecked Sendable {
    public static let modelName = "mobileclip_s2"
    public static let imageSize = 256
    public static let dimension = 512

    public static var modelsDirectory: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Reframe/models", isDirectory: true)
    }

    public enum Failure: Error, CustomStringConvertible {
        case missing(URL)
        public var description: String {
            switch self {
            case .missing(let u): return "missing \(u.lastPathComponent) in \(u.deletingLastPathComponent().path) (see SEMANTIC.md)"
            }
        }
    }

    private let image: MLModel
    private let text: MLModel
    public let tokenizer: CLIPTokenizer
    private var textCache: [String: [Float]] = [:]
    private let lock = NSLock()

    nonisolated(unsafe) private static var sharedEncoder: CLIPEncoder?
    private static let sharedLock = NSLock()

    /// Loaded once per process (compiling the packages the first time, cached next to them).
    public static func shared() throws -> CLIPEncoder {
        try sharedLock.withLock {
            if let e = sharedEncoder { return e }
            let e = try CLIPEncoder()
            sharedEncoder = e
            return e
        }
    }

    public init(directory: URL = CLIPEncoder.modelsDirectory) throws {
        let config = MLModelConfiguration()
        config.computeUnits = .all
        image = try MLModel(contentsOf: try CLIPEncoder.compiled("\(CLIPEncoder.modelName)_image", in: directory), configuration: config)
        text = try MLModel(contentsOf: try CLIPEncoder.compiled("\(CLIPEncoder.modelName)_text", in: directory), configuration: config)
        let vocab = directory.appendingPathComponent("clip-vocab.json"), merges = directory.appendingPathComponent("clip-merges.txt")
        for u in [vocab, merges] where !FileManager.default.fileExists(atPath: u.path) { throw Failure.missing(u) }
        tokenizer = try CLIPTokenizer(vocab: vocab, merges: merges)
    }

    /// `<name>.mlmodelc` next to `<name>.mlpackage`, compiled on first use (no Xcode needed).
    private static func compiled(_ name: String, in dir: URL) throws -> URL {
        let out = dir.appendingPathComponent("compiled/\(name).mlmodelc")
        if FileManager.default.fileExists(atPath: out.path) { return out }
        let pkg = dir.appendingPathComponent("\(name).mlpackage")
        guard FileManager.default.fileExists(atPath: pkg.path) else { throw Failure.missing(pkg) }
        let tmp = try MLModel.compileModel(at: pkg)
        try FileManager.default.createDirectory(at: out.deletingLastPathComponent(), withIntermediateDirectories: true)
        try? FileManager.default.removeItem(at: out)
        try FileManager.default.moveItem(at: tmp, to: out)
        return out
    }

    /// Unit-length text embedding.
    public func embed(text query: String) throws -> [Float] {
        if let v = lock.withLock({ textCache[query] }) { return v }
        let ids = tokenizer.encode(query)
        let arr = try MLMultiArray(shape: [1, 77], dataType: .int32)
        for (i, id) in ids.enumerated() { arr[i] = NSNumber(value: id) }
        let out = try text.prediction(from: MLDictionaryFeatureProvider(dictionary: ["text": arr]))
        let v = CLIPEncoder.normalized(out.featureValue(for: "final_emb_1")!.multiArrayValue!)
        lock.withLock { textCache[query] = v }
        return v
    }

    /// Unit-length image embeddings for 256×256 BGRA buffers, one batch on the Neural Engine.
    public func embed(images: [CVPixelBuffer]) throws -> [[Float]] {
        let batch = MLArrayBatchProvider(array: try images.map {
            try MLDictionaryFeatureProvider(dictionary: ["image": MLFeatureValue(pixelBuffer: $0)])
        })
        let out = try image.predictions(fromBatch: batch)
        return (0..<out.count).map { CLIPEncoder.normalized(out.features(at: $0).featureValue(for: "final_emb_1")!.multiArrayValue!) }
    }

    static func normalized(_ a: MLMultiArray) -> [Float] {
        let n = a.count
        var v = [Float](repeating: 0, count: n)
        for i in 0..<n { v[i] = a[i].floatValue }
        let len = max(sqrt(v.reduce(0) { $0 + $1 * $1 }), 1e-8)
        return v.map { $0 / len }
    }

    /// A 256×256 BGRA buffer from tightly packed RGBA8, auto-levelled: the proxy is often I-Log
    /// (flat and grey), which CLIP never saw in training, so stretch luma's 1st–99th percentile
    /// to full range and restore some saturation.
    static func pixelBuffer(rgba: [UInt8], size: Int, autoLevel: Bool) -> CVPixelBuffer? {
        var pb: CVPixelBuffer?
        let attrs = [kCVPixelBufferIOSurfacePropertiesKey: [:]] as CFDictionary
        guard CVPixelBufferCreate(nil, size, size, kCVPixelFormatType_32BGRA, attrs, &pb) == kCVReturnSuccess, let pb else { return nil }
        var lo: Float = 0, hi: Float = 255, sat: Float = 1
        if autoLevel {
            var hist = [Int](repeating: 0, count: 256)
            for p in Swift.stride(from: 0, to: rgba.count, by: 16) {
                hist[(54 * Int(rgba[p]) + 183 * Int(rgba[p + 1]) + 19 * Int(rgba[p + 2])) >> 8] += 1
            }
            let total = hist.reduce(0, +)
            var acc = 0
            for (i, h) in hist.enumerated() {
                acc += h
                if acc < total / 100 { lo = Float(i) }
                if acc < total * 99 / 100 { hi = Float(i + 1) }
            }
            if hi - lo < 32 { lo = 0; hi = 255 }  // a dark or uniform view: leave it
            sat = 1.25
        }
        let scale = 255 / max(hi - lo, 1)
        CVPixelBufferLockBaseAddress(pb, [])
        let base = CVPixelBufferGetBaseAddress(pb)!.assumingMemoryBound(to: UInt8.self)
        let stride = CVPixelBufferGetBytesPerRow(pb)
        for y in 0..<size {
            for x in 0..<size {
                let s = (y * size + x) * 4, d = y * stride + x * 4
                var r = (Float(rgba[s]) - lo) * scale, g = (Float(rgba[s + 1]) - lo) * scale, b = (Float(rgba[s + 2]) - lo) * scale
                if sat != 1 {
                    let l = 0.2126 * r + 0.7152 * g + 0.0722 * b
                    r = l + (r - l) * sat; g = l + (g - l) * sat; b = l + (b - l) * sat
                }
                base[d] = UInt8(min(max(b, 0), 255)); base[d + 1] = UInt8(min(max(g, 0), 255))
                base[d + 2] = UInt8(min(max(r, 0), 255)); base[d + 3] = 255
            }
        }
        CVPixelBufferUnlockBaseAddress(pb, [])
        return pb
    }
}

/// Image embeddings of one clip: `views` world-locked horizon views (heading-grid frame, the
/// same Lock-at-clip-start frame as the interest map) every 1/rate seconds.
public struct SemanticIndex: Codable, Sendable {
    public static let currentVersion = 1

    public var version = SemanticIndex.currentVersion
    public var model = CLIPEncoder.modelName
    public var clipName: String
    public var duration: Double
    public var times: [Double]
    /// View centres, heading-grid degrees (left-positive), evenly spaced around the horizon.
    public var yaws: [Double]
    public var fov: Double
    public var dimension: Int
    /// Float16, unit length, [time][view][dimension].
    public var vectors: Data
    public var timings: [String: Double] = [:]

    public var viewCount: Int { yaws.count }

    /// Cosine similarity of every (time, view) to a unit query vector, [time][view].
    public func similarities(_ q: [Float]) -> [Float] {
        let n = times.count * yaws.count, D = dimension
        var out = [Float](repeating: 0, count: n)
        vectors.withUnsafeBytes { raw in
            let h = raw.bindMemory(to: Float16.self)
            for i in 0..<n {
                var s: Float = 0
                let o = i * D
                for k in 0..<D { s += Float(h[o + k]) * q[k] }
                out[i] = s
            }
        }
        return out
    }

    /// How well each (time, view) matches `query`, 0 (not there) … 1 (clearly there).
    /// MobileCLIP cosines sit in a narrow band, and an absolute floor beats any per-image baseline:
    /// on our footage things in view score 0.22–0.30 and absent ones top out near 0.15, whatever the
    /// query (a bland caption like "a photo." scores ~0.19 everywhere, so subtracting it erases real
    /// matches). Calibrated for MobileCLIP-S2 on auto-levelled 90° views; see SEMANTIC.md.
    public func relevance(query: [Float], floor: Float = 0.17, spread: Float = 0.08) -> [Float] {
        similarities(query).map { clamp01(($0 - floor) / spread) }
    }

    /// A heading-grid channel (count × 72, row-major) from per-(time, view) values: linear in
    /// heading between neighbouring views (wrapping) and in time between samples.
    public func headingChannel(_ values: [Float], rate: Double, count: Int) -> [Float] {
        let B = HeadingGrid.bins, V = yaws.count, T = times.count
        guard V > 0, T > 0 else { return [Float](repeating: 0, count: count * B) }
        let spacing = 360 / Double(V)
        // Per bin: the two views either side and the weight of the second.
        let bins: [(Int, Int, Float)] = (0..<B).map { b in
            var off = (HeadingGrid.yaw(bin: b) - yaws[0]).truncatingRemainder(dividingBy: 360)
            if off < 0 { off += 360 }
            let i = Int(off / spacing) % V
            return (i, (i + 1) % V, Float((off - Double(i) * spacing) / spacing))
        }
        var out = [Float](repeating: 0, count: count * B)
        var j = 0
        for c in 0..<count {
            let t = (Double(c) + 0.5) / rate
            while j < T - 2, times[j + 1] < t { j += 1 }
            let (t0, t1) = (times[j], times[min(j + 1, T - 1)])
            let a = Float(t1 > t0 ? min(max((t - t0) / (t1 - t0), 0), 1) : 0)
            let r0 = j * V, r1 = min(j + 1, T - 1) * V
            for b in 0..<B {
                let (v0, v1, w) = bins[b]
                let x0 = values[r0 + v0] * (1 - w) + values[r0 + v1] * w
                let x1 = values[r1 + v0] * (1 - w) + values[r1 + v1] * w
                out[c * B + b] = x0 * (1 - a) + x1 * a
            }
        }
        return out
    }
}

/// Builds (and caches) a clip's semantic index from its proxy: one sequential decode, eight
/// 90° views per sample rendered at 256×256 and embedded in one Neural Engine batch.
public enum SemanticIndexer {
    public static var rate = 1.0
    public static var viewCount = 8
    public static var fov = 90.0

    public static func cached(_ clip: Clip) -> SemanticIndex? {
        guard let d = try? Data(contentsOf: cacheURL(for: clip)),
              let s = try? PropertyListDecoder().decode(SemanticIndex.self, from: d),
              s.version == SemanticIndex.currentVersion, s.model == CLIPEncoder.modelName else { return nil }
        return s
    }

    public static func index(_ clip: Clip, useCache: Bool = true) async throws -> SemanticIndex? {
        if useCache, let s = cached(clip) { return s }
        guard let proxy = clip.proxyURL, let motion = clip.motion else { return nil }
        let encoder = try CLIPEncoder.shared()
        let renderer = try ReframeRenderer()
        let reader = try await FrameReader(url: proxy)
        let index: SemanticIndex = try await withCheckedThrowingContinuation { cont in
            DispatchQueue.global(qos: .utility).async {
                do { cont.resume(returning: try build(clip: clip, motion: motion, reader: reader, renderer: renderer, encoder: encoder)) }
                catch { cont.resume(throwing: error) }
            }
        }
        let url = cacheURL(for: clip)
        try? FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        let enc = PropertyListEncoder()
        enc.outputFormat = .binary
        try? enc.encode(index).write(to: url, options: .atomic)
        return index
    }

    private static func build(clip: Clip, motion: Motion, reader: FrameReader, renderer: ReframeRenderer,
                              encoder: CLIPEncoder) throws -> SemanticIndex {
        let size = CLIPEncoder.imageSize, D = CLIPEncoder.dimension
        let yaws = (0..<viewCount).map { -180 + Double($0) * 360 / Double(viewCount) }
        var times: [Double] = [], data = Data()
        data.reserveCapacity(Int(clip.duration * rate + 2) * viewCount * D * 2)
        var next = 0.0, tDecode = 0.0, tRender = 0.0, tEmbed = 0.0
        let seq = try reader.sequence(from: 0)
        defer { seq.cancel() }
        var clock = Date()
        while let f = seq.next() {
            tDecode += Date().timeIntervalSince(clock)
            if f.time >= next {
                let c0 = Date()
                let buffers: [CVPixelBuffer] = yaws.compactMap { yaw in
                    var view = ViewSettings()
                    view.projection = .flat
                    view.hfov = fov
                    view.distortion = 0
                    view.bodyFromCamera = motion.bodyFromCamera(at: f.time, mode: .lock, smoothing: 0, lockTime: 0,
                                                                yaw: yaw, pitch: 0, roll: 0)
                    return renderer.renderRGBA8(frames: f, rig: clip.rig, view: view, width: size, height: size)
                        .flatMap { CLIPEncoder.pixelBuffer(rgba: $0, size: size, autoLevel: true) }
                }
                let c1 = Date()
                tRender += c1.timeIntervalSince(c0)
                if buffers.count == yaws.count {
                    for v in try encoder.embed(images: buffers) {
                        v.map { Float16($0) }.withUnsafeBufferPointer { data.append(Data(buffer: $0)) }
                    }
                    times.append(f.time)
                }
                tEmbed += Date().timeIntervalSince(c1)
                next = max(next + 1 / rate, f.time + 0.5 / rate)
            }
            clock = Date()
        }
        return SemanticIndex(clipName: clip.name, duration: clip.duration, times: times, yaws: yaws, fov: fov,
                             dimension: D, vectors: data,
                             timings: ["decode": tDecode, "render": tRender, "embed": tEmbed])
    }

    /// `~/Library/Application Support/Reframe/semantic/<name>-<size>-<mtime>.plist`, keyed like
    /// the analysis cache so a replaced file (stub → original) re-indexes.
    public static func cacheURL(for clip: Clip) -> URL {
        let name = Analyzer.cacheURL(for: clip).deletingPathExtension().lastPathComponent
        return FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Reframe/semantic", isDirectory: true)
            .appendingPathComponent("\(name).plist")
    }
}

/// One place a query matched: a clip, a moment and a direction.
public struct SemanticHit: Sendable {
    public var clipName: String
    public var time: Double
    public var yaw: Double
    /// Cosine similarity to the query (MobileCLIP-S2: ~0.22+ is a clear match).
    public var score: Float
}

extension SemanticIndex {
    /// The best moments for a query: peaks of the per-time best heading, at least `gap` seconds
    /// apart, ranked by raw similarity (relevance saturates at 1 for broad queries), above `floor`.
    public func hits(similarity r: [Float], floor: Float = 0.17, limit: Int = 5, gap: Double = 6) -> [SemanticHit] {
        let V = yaws.count
        var best: [(t: Int, v: Int, s: Float)] = (0..<times.count).map { t in
            let row = r[(t * V)..<((t + 1) * V)]
            let v = row.indices.max { row[$0] < row[$1] }! - t * V
            return (t, v, row.max()!)
        }
        best.sort { $0.s > $1.s }
        var out: [SemanticHit] = []
        for b in best where out.count < limit {
            guard b.s > floor, !out.contains(where: { abs($0.time - times[b.t]) < gap }) else { continue }
            out.append(SemanticHit(clipName: clipName, time: times[b.t], yaw: yaws[b.v], score: b.s))
        }
        return out
    }
}

extension ClipAnalysis {
    /// Suggestions for a text query: the query's heading channel added to the interest map and
    /// weighted to dominate, so shots start where it matches and the camera path aims at it.
    /// Other signals still shape the cut points and break ties.
    public func searched(_ channel: [Float], weight: Double = 2.5, others: Double = 0.25,
                         suggester: ShotSuggester = ShotSuggester()) -> ClipAnalysis {
        var a = self
        var w = weights
        for (s, v) in w.values where v > 0 { w.values[s] = v * others }
        w.values[.query] = weight
        a.weights = w
        var grid = heading ?? HeadingGrid(rate: signals.rate, count: signals.count)
        grid[.query] = channel
        a.heading = grid
        a.suggestions = suggester.suggest(a.inputs, weights: w, clipID: clipName, duration: duration)
            .filter { $0.reasons.contains(.query) }
        (a.score, a.bestMoment) = ClipAnalysis.rank(a.suggestions, fused: a.fused(), rate: signals.rate)
        return a
    }
}
