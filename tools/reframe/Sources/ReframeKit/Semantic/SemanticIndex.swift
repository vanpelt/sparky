import Accelerate
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
            case .missing(let u): return "The search models aren't installed (missing \(u.lastPathComponent)). Download them in Settings › Search."
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
    /// `reload` drops a loaded encoder first (after the models were reinstalled).
    public static func shared(reload: Bool = false) throws -> CLIPEncoder {
        try sharedLock.withLock {
            if !reload, let e = sharedEncoder { return e }
            let e = try CLIPEncoder()
            sharedEncoder = e
            return e
        }
    }

    /// Forget the loaded encoder (its models are about to be deleted).
    public static func unload() { sharedLock.withLock { sharedEncoder = nil } }

    public init(directory: URL = CLIPEncoder.modelsDirectory) throws {
        let config = MLModelConfiguration()
        config.computeUnits = .all
        image = try MLModel(contentsOf: try CLIPEncoder.compiled("\(CLIPEncoder.modelName)_image", in: directory), configuration: config)
        text = try MLModel(contentsOf: try CLIPEncoder.compiled("\(CLIPEncoder.modelName)_text", in: directory), configuration: config)
        let vocab = directory.appendingPathComponent("clip-vocab.json"), merges = directory.appendingPathComponent("clip-merges.txt")
        for u in [vocab, merges] where !FileManager.default.fileExists(atPath: u.path) { throw Failure.missing(u) }
        tokenizer = try CLIPTokenizer(vocab: vocab, merges: merges)
    }

    /// `compiled/<name>.mlmodelc`, compiled from the downloaded `<name>.mlpackage` on first use
    /// (no Xcode needed). Only the compiled model is kept: the package would double the space.
    static func compiled(_ name: String, in dir: URL) throws -> URL {
        let out = dir.appendingPathComponent("compiled/\(name).mlmodelc")
        let pkg = dir.appendingPathComponent("\(name).mlpackage")
        if FileManager.default.fileExists(atPath: out.path) {
            try? FileManager.default.removeItem(at: pkg)
            return out
        }
        guard FileManager.default.fileExists(atPath: pkg.path) else { throw Failure.missing(pkg) }
        let tmp = try MLModel.compileModel(at: pkg)
        try FileManager.default.createDirectory(at: out.deletingLastPathComponent(), withIntermediateDirectories: true)
        try? FileManager.default.removeItem(at: out)
        try FileManager.default.moveItem(at: tmp, to: out)
        try? FileManager.default.removeItem(at: pkg)
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
///
/// On disk it's one memory-mapped file per clip: "RFS1", a little-endian UInt32 header length,
/// the JSON header (everything but the vectors), zero padding to 16 bytes, then the Float16
/// vectors. Derived, write-once, read-in-bulk data like this doesn't need a database: mapping it
/// lets the OS page it in and out, and scoring is one matrix-vector product (Accelerate).
public struct SemanticIndex: Codable, Sendable {
    public static let currentVersion = 2

    public var version = SemanticIndex.currentVersion
    public var model = CLIPEncoder.modelName
    public var clipName: String
    public var duration: Double
    public var times: [Double]
    /// View centres, heading-grid degrees (left-positive), evenly spaced around the horizon.
    public var yaws: [Double]
    public var fov: Double
    public var dimension: Int
    /// Float16, unit length, [time][view][dimension]. Not in the JSON header.
    public var vectors = Data()
    public var timings: [String: Double] = [:]

    enum CodingKeys: String, CodingKey { case version, model, clipName, duration, times, yaws, fov, dimension, timings }

    public init(clipName: String, duration: Double, times: [Double], yaws: [Double], fov: Double, dimension: Int,
                vectors: Data, timings: [String: Double] = [:]) {
        self.clipName = clipName; self.duration = duration; self.times = times; self.yaws = yaws
        self.fov = fov; self.dimension = dimension; self.vectors = vectors; self.timings = timings
    }

    public var viewCount: Int { yaws.count }

    static let magic = Data("RFS1".utf8)

    func write(to url: URL) throws {
        var header = try JSONEncoder().encode(self)
        var out = SemanticIndex.magic
        withUnsafeBytes(of: UInt32(header.count).littleEndian) { out.append(contentsOf: $0) }
        out.append(header)
        while out.count % 16 != 0 { out.append(0) }
        out.append(vectors)
        header.removeAll()
        try out.write(to: url, options: .atomic)
    }

    static func read(_ url: URL) -> SemanticIndex? {
        guard let data = try? Data(contentsOf: url, options: .alwaysMapped), data.count > 8,
              data.prefix(4) == magic else { return nil }
        let n = Int(data.subdata(in: 4..<8).withUnsafeBytes { $0.loadUnaligned(as: UInt32.self) }.littleEndian)
        guard data.count >= 8 + n, var s = try? JSONDecoder().decode(SemanticIndex.self, from: data.subdata(in: 8..<(8 + n)))
        else { return nil }
        let start = (8 + n + 15) / 16 * 16
        let bytes = s.times.count * s.yaws.count * s.dimension * 2
        guard data.count >= start + bytes else { return nil }
        s.vectors = data[start..<(start + bytes)]  // a slice: stays mapped, no copy
        return s
    }

    /// Cosine similarity of every (time, view) to a unit query vector, [time][view]: Float16 rows
    /// converted in blocks and multiplied with vDSP (a few ms per hour of footage).
    public func similarities(_ q: [Float]) -> [Float] {
        let rows = times.count * yaws.count, D = dimension
        guard rows > 0, q.count == D else { return [] }
        var out = [Float](repeating: 0, count: rows)
        let block = 1024
        var scratch = [Float](repeating: 0, count: block * D)
        vectors.withUnsafeBytes { raw in
            guard let base = raw.baseAddress else { return }
            var r0 = 0
            while r0 < rows {
                let n = min(block, rows - r0)
                var src = vImage_Buffer(data: UnsafeMutableRawPointer(mutating: base + r0 * D * 2), height: 1,
                                        width: vImagePixelCount(n * D), rowBytes: n * D * 2)
                scratch.withUnsafeMutableBytes { dst in
                    var d = vImage_Buffer(data: dst.baseAddress, height: 1, width: vImagePixelCount(n * D), rowBytes: n * D * 4)
                    vImageConvert_Planar16FtoPlanarF(&src, &d, 0)
                }
                out.withUnsafeMutableBufferPointer { o in
                    vDSP_mmul(scratch, 1, q, 1, o.baseAddress! + r0, 1, vDSP_Length(n), 1, vDSP_Length(D))
                }
                r0 += n
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
        guard let s = SemanticIndex.read(cacheURL(for: clip)),
              s.version == SemanticIndex.currentVersion, s.model == CLIPEncoder.modelName else { return nil }
        return s
    }

    /// Whether a current index exists, without reading it.
    public static func isIndexed(_ clip: Clip) -> Bool { FileManager.default.fileExists(atPath: cacheURL(for: clip).path) }

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
        try? index.write(to: url)
        return SemanticIndex.read(url) ?? index
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

    public static var directory: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Reframe/semantic", isDirectory: true)
    }

    /// `~/Library/Application Support/Reframe/semantic/<name>-<size>-<mtime>.rfsi`, keyed like
    /// the analysis cache so a replaced file (stub → original) re-indexes. Keyed by the proxy's
    /// identity would survive that, but the original's trailer is what we have for every clip.
    public static func cacheURL(for clip: Clip) -> URL {
        let name = Analyzer.cacheURL(for: clip).deletingPathExtension().lastPathComponent
        return directory.appendingPathComponent("\(name).rfsi")
    }

    /// Deletes every index (and the prototype's .plist ones). Returns how many files went.
    @discardableResult
    public static func clear() -> Int {
        let files = (try? FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil)) ?? []
        return files.filter { ["rfsi", "plist"].contains($0.pathExtension) && (try? FileManager.default.removeItem(at: $0)) != nil }.count
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
    /// `id` keys the suggestions (dismissals are remembered by it), so pass one per query.
    public func searched(_ channel: [Float], id: String? = nil, weight: Double = 2.5, others: Double = 0.25,
                         suggester: ShotSuggester = ShotSuggester()) -> ClipAnalysis {
        var a = self
        var w = weights
        for (s, v) in w.values where v > 0 { w.values[s] = v * others }
        w.values[.query] = weight
        a.weights = w
        var grid = heading ?? HeadingGrid(rate: signals.rate, count: signals.count)
        grid[.query] = channel
        a.heading = grid
        a.suggestions = suggester.suggest(a.inputs, weights: w, clipID: id ?? clipName, duration: duration)
            .filter { $0.reasons.contains(.query) }
        (a.score, a.bestMoment) = ClipAnalysis.rank(a.suggestions, fused: a.fused(), rate: signals.rate)
        return a
    }
}

/// One query against one clip's index.
public struct SemanticMatch: Sendable {
    /// Cosine similarity per (sample, view), [time][view].
    public var similarity: [Float]
    /// 0…1 per (sample, view): `SemanticIndex.relevance`.
    public var relevance: [Float]
    /// Best relevance over views per sample (the clip's match curve).
    public var perTime: [Float]
    /// The best similarity anywhere in the clip (ranks clips).
    public var best: Float
    /// Peaks, best first.
    public var hits: [SemanticHit]
    public var isMatch: Bool { !hits.isEmpty }
}

extension SemanticIndex {
    /// Cosines at or below this aren't matches (MobileCLIP-S2; SEMANTIC.md).
    public static let matchFloor: Float = 0.17
    public static let matchSpread: Float = 0.08
    /// At or above this, it's clearly there; between the floor and this, a lookalike is as likely.
    public static let clearMatch: Float = 0.21

    public func match(_ q: [Float], limit: Int = 8, gap: Double = 6) -> SemanticMatch {
        let sims = similarities(q)
        let rel = sims.map { clamp01(($0 - SemanticIndex.matchFloor) / SemanticIndex.matchSpread) }
        let V = viewCount
        let perTime = (0..<times.count).map { t in rel[(t * V)..<((t + 1) * V)].max() ?? 0 }
        return SemanticMatch(similarity: sims, relevance: rel, perTime: perTime, best: sims.max() ?? -1,
                             hits: hits(similarity: sims, floor: SemanticIndex.matchFloor, limit: limit, gap: gap))
    }

    /// `values` ([time][view]) at a heading-grid yaw and time, interpolated between neighbouring
    /// views and samples.
    public func value(_ values: [Float], time t: Double, yaw: Double) -> Float {
        let V = viewCount, T = times.count
        guard V > 0, T > 0, values.count == V * T else { return 0 }
        var j = 0
        while j < T - 2, times[j + 1] < t { j += 1 }
        let (t0, t1) = (times[j], times[min(j + 1, T - 1)])
        let a = Float(t1 > t0 ? min(max((t - t0) / (t1 - t0), 0), 1) : 0)
        let spacing = 360 / Double(V)
        var off = (yaw - yaws[0]).truncatingRemainder(dividingBy: 360)
        if off < 0 { off += 360 }
        let v0 = Int(off / spacing) % V, v1 = (v0 + 1) % V
        let w = Float((off - Double(v0) * spacing) / spacing)
        func at(_ r: Int) -> Float { values[r * V + v0] * (1 - w) + values[r * V + v1] * w }
        return at(j) * (1 - a) + at(min(j + 1, T - 1)) * a
    }

    /// How well a shot matches: the best `values` along its line of sight (its keyframed aim, in
    /// its stabilization), sampled twice a second, and when that was.
    public func along(_ shot: Shot, motion: Motion, values: [Float]) -> (value: Float, time: Double) {
        let origin = motion.baseHeading(at: 0, mode: .lock, smoothing: 0, lockTime: 0)
        var best: (Float, Double) = (-1, shot.inTime)
        var t = shot.inTime
        while t <= shot.outTime {
            let heading = motion.baseHeading(at: t, mode: shot.stabilization, smoothing: shot.smoothing,
                                             lockTime: shot.lockTime) + shot.framing(at: t).yaw - origin
            let v = value(values, time: t, yaw: heading)
            if v > best.0 { best = (v, t) }
            t += 0.5
        }
        return best
    }
}
