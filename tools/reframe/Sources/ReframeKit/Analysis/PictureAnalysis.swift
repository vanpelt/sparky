import AVFoundation
import CoreVideo
import Foundation

/// Junk detection from the proxy's pixels: frames too dark to use, and the forward lens covered
/// (a hand, a pocket, the selfie stick's mount). Reads the luma plane of the 2048×1024 LRV at
/// about 2 Hz; no rendering.
enum PictureAnalysis {
    struct LensStats: Codable, Sendable {
        /// Mean luma, 0...1 of the video range.
        var mean: Float
        /// Mean absolute luma difference between neighbouring samples 8 px apart: texture.
        var detail: Float
    }

    struct Sample: Codable, Sendable {
        var time: Double
        /// Indexed by calibration lens (0 is the forward lens follow mode looks through).
        var lenses: [LensStats]
    }

    static func samples(proxy: URL, rig: Rig, rate: Double = 2) async throws -> [Sample] {
        let reader = try await FrameReader(url: proxy)
        let seq = try reader.sequence(from: 0)
        defer { seq.cancel() }
        let every = max(1, Int((reader.frameRate / rate).rounded()))
        // Which half of the packed frame holds each calibration lens.
        let leftIsLens0 = rig.lenses[0].cx < rig.lenses[0].canvasWidth / 2
        var out: [Sample] = []
        var n = 0
        while let f = seq.next() {
            defer { n += 1 }
            guard n % every == 0, let pb = f.buffers.first else { continue }
            var halves = stats(pb)
            if !leftIsLens0 { halves.swapAt(0, 1) }
            out.append(Sample(time: f.time, lenses: halves))
        }
        return out
    }

    /// Luma statistics of the left and right halves of a packed proxy frame (each lens circle's
    /// central region).
    static func stats(_ pb: CVPixelBuffer) -> [LensStats] {
        CVPixelBufferLockBaseAddress(pb, .readOnly)
        defer { CVPixelBufferUnlockBaseAddress(pb, .readOnly) }
        guard let base = CVPixelBufferGetBaseAddressOfPlane(pb, 0) else { return [] }
        let w = CVPixelBufferGetWidthOfPlane(pb, 0), h = CVPixelBufferGetHeightOfPlane(pb, 0)
        let stride = CVPixelBufferGetBytesPerRowOfPlane(pb, 0)
        let tenBit = CVPixelBufferGetPixelFormatType(pb) == kCVPixelFormatType_420YpCbCr10BiPlanarVideoRange
        let step = 8
        return (0..<2).map { lens in
            let cx = w / 4 + lens * w / 2, cy = h / 2, r = min(w / 4, h / 2) * 7 / 10
            var sum = 0.0, diff = 0.0, count = 0
            for y in Swift.stride(from: cy - r, to: cy + r, by: step) {
                let row = base + y * stride
                var prev = -1.0
                for x in Swift.stride(from: cx - r, to: cx + r, by: step) {
                    let dx = x - cx, dy = y - cy
                    guard dx * dx + dy * dy <= r * r else { prev = -1; continue }
                    let v: Double
                    if tenBit {
                        v = (Double(row.load(fromByteOffset: x * 2, as: UInt16.self) >> 6) - 64) / 876
                    } else {
                        v = (Double(row.load(fromByteOffset: x, as: UInt8.self)) - 16) / 219
                    }
                    sum += v; count += 1
                    if prev >= 0 { diff += abs(v - prev) }
                    prev = v
                }
            }
            let c = Double(max(count, 1))
            return LensStats(mean: Float(sum / c), detail: Float(diff / c))
        }
    }

    static func analyze(_ samples: [Sample], into grid: inout TimeSignals) {
        guard !samples.isEmpty else { return }
        var dark = [Float](repeating: 0, count: grid.count)
        var covered = [Float](repeating: 0, count: grid.count)
        for c in 0..<grid.count {
            // Nearest sample (they're 0.5 s apart; cells are 0.25 s).
            let t = grid.time(c)
            var lo = 0, hi = samples.count - 1
            while lo < hi { let mid = (lo + hi) / 2; if samples[mid].time < t { lo = mid + 1 } else { hi = mid } }
            if lo > 0, abs(samples[lo - 1].time - t) < abs(samples[lo].time - t) { lo -= 1 }
            let s = samples[lo].lenses
            guard s.count == 2 else { continue }
            let bright = max(s[0].mean, s[1].mean)
            dark[c] = clamp01((0.10 - bright) / 0.06)
            // The forward lens is much darker *and* flatter than the back one.
            let ratioMean = s[0].mean / max(s[1].mean, 1e-3), ratioDetail = s[0].detail / max(s[1].detail, 1e-4)
            covered[c] = min(clamp01((0.5 - ratioMean) / 0.3), clamp01((0.45 - ratioDetail) / 0.3))
        }
        grid[.dark] = dark
        grid[.covered] = covered
    }
}
