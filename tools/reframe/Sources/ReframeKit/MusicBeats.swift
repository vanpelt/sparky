import Accelerate
import AVFoundation
import Foundation

/// Tempo, beats and bars of a music track, for cutting a timeline to it.
///
/// Onsets are log-magnitude spectral flux (hop 512 at 22.05 kHz, ~23 ms); tempo is the strongest
/// autocorrelation lag between 60 and 200 BPM, weighted toward 120; beats come from Ellis's
/// dynamic-programming tracker. Bars assume 4/4, with the downbeat phase picked by bass energy.
public struct MusicBeats: Codable, Sendable {
    public var file: String
    public var duration: Double
    public var bpm: Double
    /// Seconds into the audio file.
    public var beats: [Double]
    /// The first beat of each bar.
    public var downbeats: [Double]
    /// Loudness (RMS) of each bar from `downbeats`, scaled so the loudest is 1.
    public var barEnergy: [Double]
    /// Downbeats where the loudness changes most: likely section starts (intro, drop, breakdown).
    public var sections: [Double]

    static let rate = 22050.0, hop = 512, window = 1024

    public static func analyze(url: URL) async throws -> MusicBeats {
        let samples = try await decode(url)
        let (onset, bass) = envelopes(samples)
        let fr = rate / Double(hop)
        let period = tempoPeriod(onset)
        let beatFrames = track(onset, period: period)
        func time(_ f: Int) -> Double { (Double(f * hop) + Double(window) / 2) / rate }
        let beats = beatFrames.map(time)
        let bpm = fr * 60 / period, barSeconds = 4 * 60 / bpm

        // Downbeat phase: the beat position (of 4) with the most bass and onset strength.
        var phase = 0, bestPhase = -Double.infinity
        for k in 0..<4 {
            var s = 0.0
            for i in stride(from: k, to: beatFrames.count, by: 4) { s += Double(bass[beatFrames[i]] + onset[beatFrames[i]]) }
            s /= Double(max(1, (beatFrames.count - k + 3) / 4))
            if s > bestPhase { bestPhase = s; phase = k }
        }
        let downbeats = stride(from: phase, to: beats.count, by: 4).map { beats[$0] }

        var energy: [Double] = []
        for (i, start) in downbeats.enumerated() {
            let end = i + 1 < downbeats.count ? downbeats[i + 1] : min(start + barSeconds, Double(samples.count) / rate)
            let a = min(Int(start * rate), samples.count), b = min(max(Int(end * rate), a + 1), samples.count)
            guard b > a else { energy.append(0); continue }
            var ms: Float = 0
            samples.withUnsafeBufferPointer { vDSP_measqv($0.baseAddress! + a, 1, &ms, vDSP_Length(b - a)) }
            energy.append(Double(ms).squareRoot())
        }
        let top = max(energy.max() ?? 1, 1e-9)
        energy = energy.map { $0 / top }

        // Section changes: the biggest differences between the 4 bars before and after a downbeat.
        var novelty = [Double](repeating: 0, count: energy.count)
        for b in 2..<max(2, energy.count - 1) {
            let before = energy[max(0, b - 4)..<b], after = energy[b..<min(energy.count, b + 4)]
            novelty[b] = abs(after.reduce(0, +) / Double(after.count) - before.reduce(0, +) / Double(before.count))
        }
        var sections: [Double] = []
        // Strongest changes first, at least 4 bars apart.
        for b in novelty.indices.sorted(by: { novelty[$0] > novelty[$1] }) where novelty[b] > 0.12 {
            if sections.contains(where: { abs($0 - downbeats[b]) < 3.5 * barSeconds }) { continue }
            sections.append(downbeats[b])
        }
        sections.sort()

        return MusicBeats(file: url.path, duration: Double(samples.count) / rate, bpm: bpm,
                          beats: beats, downbeats: downbeats, barEnergy: energy, sections: sections)
    }

    /// Mono float samples at `rate`.
    static func decode(_ url: URL) async throws -> [Float] {
        let asset = AVURLAsset(url: url)
        guard let track = try await asset.loadTracks(withMediaType: .audio).first else {
            throw NSError(domain: "Reframe", code: 30, userInfo: [NSLocalizedDescriptionKey: "no audio in \(url.lastPathComponent)"])
        }
        let reader = try AVAssetReader(asset: asset)
        let output = AVAssetReaderTrackOutput(track: track, outputSettings: [
            AVFormatIDKey: kAudioFormatLinearPCM, AVSampleRateKey: rate, AVNumberOfChannelsKey: 1,
            AVLinearPCMBitDepthKey: 32, AVLinearPCMIsFloatKey: true, AVLinearPCMIsNonInterleaved: false,
            AVLinearPCMIsBigEndianKey: false,
        ])
        reader.add(output)
        guard reader.startReading() else { throw reader.error ?? NSError(domain: "Reframe", code: 31) }
        var samples: [Float] = []
        while let sb = output.copyNextSampleBuffer() {
            guard let bb = CMSampleBufferGetDataBuffer(sb) else { continue }
            let n = CMBlockBufferGetDataLength(bb)
            var chunk = [Float](repeating: 0, count: n / 4)
            _ = chunk.withUnsafeMutableBytes { CMBlockBufferCopyDataBytes(bb, atOffset: 0, dataLength: n, destination: $0.baseAddress!) }
            samples += chunk
        }
        return samples
    }

    /// Per-frame onset strength (normalized spectral flux) and bass energy (below ~150 Hz).
    static func envelopes(_ x: [Float]) -> (onset: [Float], bass: [Float]) {
        let n = max(0, (x.count - window) / hop)
        // A real FFT of `window` samples: evens as real input, odds as imaginary; bin k is k·rate/window Hz.
        guard n > 0, let dft = try? vDSP.DiscreteFourierTransform(count: window, direction: .forward,
                                                                transformType: .complexReal, ofType: Float.self)
        else { return ([], []) }
        let hann = vDSP.window(ofType: Float.self, usingSequence: .hanningDenormalized, count: window, isHalfWindow: false)
        var flux = [Float](repeating: 0, count: n), bass = [Float](repeating: 0, count: n)
        let half = window / 2
        var previous = [Float](repeating: 0, count: half)
        var evens = [Float](repeating: 0, count: half), odds = [Float](repeating: 0, count: half)
        for f in 0..<n {
            let frame = vDSP.multiply(x[(f * hop)..<(f * hop + window)], hann)
            for k in 0..<half { evens[k] = frame[2 * k]; odds[k] = frame[2 * k + 1] }
            var (re, im) = dft.transform(real: evens, imaginary: odds)
            im[0] = 0  // holds the Nyquist bin
            var mags = vDSP.hypot(re, im)
            mags = mags.map { log1p(100 * $0) }
            var s: Float = 0
            for k in 1..<half { s += max(0, mags[k] - previous[k]) }
            flux[f] = s
            bass[f] = mags[1...7].reduce(0, +)
            previous = mags
        }
        // Remove the local mean, half-wave rectify, scale to unit deviation.
        let w = 16
        var onset = [Float](repeating: 0, count: n)
        var sum: Float = 0
        for i in 0..<n {
            sum += flux[i]
            if i >= w { sum -= flux[i - w] }
            onset[i] = max(0, flux[i] - sum / Float(min(i + 1, w)))
        }
        let sd = max(vDSP.standardDeviation(onset), 1e-6)
        let bassMax = max(bass.max() ?? 1, 1e-6)
        return (onset.map { $0 / sd }, bass.map { $0 / bassMax })
    }

    /// Beat period in frames (fractional), from the autocorrelation of the onset envelope.
    static func tempoPeriod(_ onset: [Float]) -> Double {
        let fr = rate / Double(hop)
        let minLag = Int(fr * 60 / 200), maxLag = Int(fr * 60 / 60) + 1
        guard onset.count > maxLag * 2 else { return fr * 60 / 120 }
        var ac = [Double](repeating: 0, count: maxLag + 2)
        for lag in max(1, minLag - 1)...(maxLag + 1) {
            var s: Float = 0
            onset.withUnsafeBufferPointer { p in vDSP_dotpr(p.baseAddress!, 1, p.baseAddress! + lag, 1, &s, vDSP_Length(onset.count - lag)) }
            ac[lag] = Double(s) / Double(onset.count - lag)
        }
        var best = minLag, bestScore = -Double.infinity
        for lag in minLag...maxLag {
            let bpm = fr * 60 / Double(lag)
            let weight = exp(-0.5 * pow(log2(bpm / 120), 2))
            if ac[lag] * weight > bestScore { bestScore = ac[lag] * weight; best = lag }
        }
        // Parabolic refinement around the peak.
        let a = ac[best - 1], b = ac[best], c = ac[best + 1]
        let d = a - 2 * b + c
        return Double(best) + (d != 0 ? 0.5 * (a - c) / d : 0)
    }

    /// Ellis (2007) dynamic-programming beat tracker: beats on strong onsets, spaced near `period`.
    static func track(_ onset: [Float], period: Double, tightness: Double = 100) -> [Int] {
        let n = onset.count
        guard n > 0 else { return [] }
        var score = onset.map(Double.init), back = [Int](repeating: -1, count: n)
        for t in 0..<n {
            let lo = max(0, t - Int(2 * period)), hi = t - Int(period / 2)
            guard hi >= lo else { continue }
            var best = -Double.infinity, arg = -1
            for p in lo...hi {
                let v = score[p] - tightness * pow(log(Double(t - p) / period), 2)
                if v > best { best = v; arg = p }
            }
            if arg >= 0 && best > 0 { score[t] += best; back[t] = arg }
        }
        // End on the best-scoring frame in the final period.
        var t = (max(0, n - Int(period))..<n).max { score[$0] < score[$1] } ?? n - 1
        var beats: [Int] = []
        while t >= 0 { beats.append(t); t = back[t] }
        return beats.reversed()
    }
}
