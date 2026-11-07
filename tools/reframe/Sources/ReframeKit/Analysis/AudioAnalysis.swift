import Accelerate
import AVFoundation
import Foundation
import SoundAnalysis

/// Audio signals: loudness, onsets (spectral flux) and Apple's on-device sound classifier
/// (laughter, cheering, shouting, speech, music, wind). The file's audio is stereo, so these
/// say *when*, never *where*.
enum AudioAnalysis {
    static let sampleRate = 16_000.0

    /// Classifier labels pooled into our signals (max confidence within a group).
    static let groups: [Signal: [String]] = [
        .laughter: ["laughter", "belly_laugh", "giggling", "chuckle_chortle", "snicker", "baby_laughter"],
        .cheering: ["cheering", "applause", "crowd", "clapping", "battle_cry"],
        .shouting: ["screaming", "shout", "yell", "children_shouting"],
        .speech: ["speech", "chatter"],
        .music: ["music", "singing"],
        .wind: ["wind_noise_microphone", "wind"],
    ]

    /// Decode the first audio track to 16 kHz mono float. Nil when the file has no audio.
    static func decodeMono(url: URL) async throws -> [Float]? {
        let asset = AVURLAsset(url: try mp4Alias(for: url))
        guard let track = try await asset.loadTracks(withMediaType: .audio).first else { return nil }
        let reader = try AVAssetReader(asset: asset)
        let output = AVAssetReaderTrackOutput(track: track, outputSettings: [
            AVFormatIDKey: kAudioFormatLinearPCM,
            AVSampleRateKey: sampleRate,
            AVNumberOfChannelsKey: 1,
            AVLinearPCMBitDepthKey: 32,
            AVLinearPCMIsFloatKey: true,
            AVLinearPCMIsNonInterleaved: false,
            AVLinearPCMIsBigEndianKey: false,
        ])
        output.alwaysCopiesSampleData = false
        reader.add(output)
        guard reader.startReading() else { throw reader.error ?? NSError(domain: "Reframe", code: 20) }
        var samples: [Float] = []
        var first = true
        while let sb = output.copyNextSampleBuffer() {
            if first {
                // Align to the video timeline if the audio starts late.
                let pts = CMSampleBufferGetPresentationTimeStamp(sb).seconds
                if pts.isFinite, pts > 0 { samples.append(contentsOf: [Float](repeating: 0, count: Int(pts * sampleRate))) }
                first = false
            }
            guard let block = CMSampleBufferGetDataBuffer(sb) else { continue }
            let n = CMBlockBufferGetDataLength(block) / MemoryLayout<Float>.size
            let start = samples.count
            samples.append(contentsOf: [Float](repeating: 0, count: n))
            samples.withUnsafeMutableBytes { p in
                _ = CMBlockBufferCopyDataBytes(block, atOffset: 0, dataLength: n * 4, destination: p.baseAddress! + start * 4)
            }
        }
        if reader.status == .failed { throw reader.error ?? NSError(domain: "Reframe", code: 21) }
        return samples
    }

    static func analyze(samples: [Float], into grid: inout TimeSignals) throws {
        loudness(samples, into: &grid)
        onsets(samples, into: &grid)
        try classify(samples, into: &grid)
        // Wind noise makes things loud without anything happening.
        let wind = grid[.wind]
        grid[.loudness] = zip(grid[.loudness], wind).map { $0 * (1 - $1) }
        grid[.onset] = zip(grid[.onset], wind).map { $0 * (1 - 0.6 * $1) }
    }

    /// RMS level per grid cell, in dB above the clip's median; 15 dB above = 1.
    static func loudness(_ x: [Float], into grid: inout TimeSignals) {
        let hop = sampleRate / grid.rate
        var db = [Float](repeating: -120, count: grid.count)
        for i in 0..<grid.count {
            let a = Int(Double(i) * hop), b = min(Int(Double(i + 1) * hop), x.count)
            guard b > a else { continue }
            var ms: Float = 0
            x.withUnsafeBufferPointer { p in vDSP_measqv(p.baseAddress! + a, 1, &ms, vDSP_Length(b - a)) }
            db[i] = 10 * log10(max(ms, 1e-12))
        }
        let base = percentile(db.filter { $0 > -100 }, 0.5)
        grid[.loudness] = db.map { clamp01(($0 - base) / 15) }
    }

    /// Spectral flux of log magnitudes (32 ms frames, 16 ms hop), max per grid cell,
    /// scaled between the clip's 60th and 99.5th percentiles.
    static func onsets(_ x: [Float], into grid: inout TimeSignals) {
        let n = 512, hop = 256, half = n / 2
        guard x.count > n, let dft = try? vDSP.DiscreteFourierTransform(
            previous: nil, count: n, direction: .forward, transformType: .complexComplex, ofType: Float.self) else { return }
        let window = vDSP.window(ofType: Float.self, usingSequence: .hanningDenormalized, count: n, isHalfWindow: false)
        let frames = (x.count - n) / hop
        var prev = [Float](repeating: 0, count: half)
        var flux = [Float](repeating: 0, count: frames)
        let zeros = [Float](repeating: 0, count: n)
        var re = [Float](repeating: 0, count: n), im = [Float](repeating: 0, count: n)
        var mag = [Float](repeating: 0, count: half)
        for f in 0..<frames {
            let seg = Array(x[(f * hop)..<(f * hop + n)])
            let w = vDSP.multiply(seg, window)
            dft.transform(inputReal: w, inputImaginary: zeros, outputReal: &re, outputImaginary: &im)
            for k in 0..<half { mag[k] = log1p(10 * (re[k] * re[k] + im[k] * im[k]).squareRoot()) }
            var s: Float = 0
            for k in 1..<half { s += max(0, mag[k] - prev[k]) }
            flux[f] = f == 0 ? 0 : s
            swap(&prev, &mag)
        }
        var cell = [Float](repeating: 0, count: grid.count)
        for f in 0..<frames {
            let t = (Double(f * hop) + Double(n) / 2) / sampleRate
            let i = grid.index(t)
            cell[i] = max(cell[i], flux[f])
        }
        let lo = percentile(cell, 0.6), hi = max(percentile(cell, 0.995), lo + 1e-3)
        grid[.onset] = cell.map { clamp01(($0 - lo) / (hi - lo)) }
    }

    /// Apple's sound classifier on 1.5 s windows every 0.75 s.
    static func classify(_ x: [Float], into grid: inout TimeSignals) throws {
        let request = try SNClassifySoundRequest(classifierIdentifier: .version1)
        request.windowDuration = CMTime(seconds: 1.5, preferredTimescale: 48000)
        request.overlapFactor = 0.5
        let format = AVAudioFormat(standardFormatWithSampleRate: sampleRate, channels: 1)!
        let analyzer = SNAudioStreamAnalyzer(format: format)
        let collector = Collector()
        try analyzer.add(request, withObserver: collector)
        let chunk = 16_384
        var pos = 0
        while pos < x.count {
            let n = min(chunk, x.count - pos)
            guard let buf = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: AVAudioFrameCount(n)) else { break }
            buf.frameLength = AVAudioFrameCount(n)
            x.withUnsafeBufferPointer { p in buf.floatChannelData![0].update(from: p.baseAddress! + pos, count: n) }
            analyzer.analyze(buf, atAudioFramePosition: AVAudioFramePosition(pos))
            pos += n
        }
        analyzer.completeAnalysis()

        for (signal, labels) in groups {
            var cell = [Float](repeating: 0, count: grid.count)
            for r in collector.results {
                let c = labels.map { r.confidences[$0] ?? 0 }.max() ?? 0
                // Each window speaks for the cells within half a hop of its centre (with overlap).
                let a = grid.index(r.center - 0.5), b = grid.index(r.center + 0.5)
                for i in a...b { cell[i] = max(cell[i], c) }
            }
            // Confidences around 0.05 are noise; ~0.55 is a clear detection.
            grid[signal] = cell.map { clamp01(($0 - 0.05) / 0.5) }
        }
    }

    final class Collector: NSObject, SNResultsObserving, @unchecked Sendable {
        struct Window { let center: Double; let confidences: [String: Float] }
        private let lock = NSLock()
        private var _results: [Window] = []
        var results: [Window] { lock.withLock { _results } }
        private let wanted = Set(AudioAnalysis.groups.values.flatMap { $0 })

        func request(_ request: SNRequest, didProduce result: SNResult) {
            guard let r = result as? SNClassificationResult else { return }
            var c: [String: Float] = [:]
            for k in r.classifications where wanted.contains(k.identifier) { c[k.identifier] = Float(k.confidence) }
            let center = r.timeRange.start.seconds + r.timeRange.duration.seconds / 2
            lock.withLock { _results.append(Window(center: center, confidences: c)) }
        }
    }
}
