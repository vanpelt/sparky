import CoreGraphics
import Foundation

/// The whole horizon over time: x is time, y is heading. Each column is the horizon band
/// (latitude-weighted around 0°) of a levelled, heading-locked 360° frame.
///
/// Heading convention: row fraction `0.5 - rel/360`, where `rel` is the heading relative to the
/// camera's forward direction at the start of the clip, positive to the left. So the middle row
/// is "where the camera pointed at the start", and up is left.
public struct HorizonScan: @unchecked Sendable {
    public let image: CGImage
    public let duration: Double
    /// Heading (degrees) of the scan's centre row; see `Motion.baseHeading`.
    public let originHeading: Double
    /// Gyro speed per time bin, 0...1 (1 = 180°/s or more).
    public let shake: [Double]

    /// Row fraction (0 top ... 1 bottom) for a world heading, wrapped.
    public func rowFraction(heading: Double) -> Double {
        var f = 0.5 - (heading - originHeading) / 360
        f -= floor(f)
        return f
    }

    /// World heading at a row fraction (inverse of rowFraction, before wrapping).
    public func heading(rowFraction f: Double) -> Double { originHeading + (0.5 - f) * 360 }

    public static func make(clip: Clip, renderer: ReframeRenderer, applyLUT: Bool) async -> HorizonScan? {
        guard let motion = clip.motion, let source = clip.proxyURL ?? Optional(clip.url),
              let reader = try? await FrameReader(url: source) else { return nil }
        let duration = max(reader.duration, clip.duration)
        let samples = min(max(Int(duration * 4), 80), 480)
        let lonCount = 192, latCount = 96
        let origin = motion.baseHeading(at: 0, mode: .lock, smoothing: 0, lockTime: 0)

        // Latitude weights: favour the horizon, ignore sky and ground.
        let weights: [Double] = (0..<latCount).map { r in
            let lat = 90 - (Double(r) + 0.5) / Double(latCount) * 180
            return abs(lat) < 50 ? exp(-pow(lat / 22, 2)) : 0
        }
        let wsum = weights.reduce(0, +)

        var columns = [[UInt8]](repeating: [UInt8](repeating: 0, count: lonCount * 4), count: samples)
        guard let seq = try? reader.sequence(from: 0) else { return nil }
        var next = 0
        while next < samples, let frames = seq.next() {
            let target = (Double(next) + 0.5) / Double(samples) * duration
            if frames.time + 0.5 / max(reader.frameRate, 1) < target { continue }
            var view = ViewSettings()
            view.projection = .equirect
            view.applyLUT = applyLUT
            view.bodyFromCamera = motion.bodyFromCamera(at: frames.time, mode: .lock, smoothing: 0, lockTime: 0,
                                                        yaw: 0, pitch: 0, roll: 0)
            guard let px = renderer.renderRGBA8(frames: frames, rig: clip.rig, view: view, width: lonCount,
                                                height: latCount) else { break }
            var col = [UInt8](repeating: 255, count: lonCount * 4)
            for c in 0..<lonCount {
                var r = 0.0, g = 0.0, b = 0.0
                for (row, w) in weights.enumerated() where w > 0 {
                    let o = (row * lonCount + c) * 4
                    r += Double(px[o]) * w; g += Double(px[o + 1]) * w; b += Double(px[o + 2]) * w
                }
                // Averaging the band mutes it; restore some contrast and color (navigation aid only).
                var rgb = [r / wsum / 255, g / wsum / 255, b / wsum / 255]
                let luma = 0.2126 * rgb[0] + 0.7152 * rgb[1] + 0.0722 * rgb[2]
                for k in 0..<3 {
                    let sat = luma + (rgb[k] - luma) * 1.45
                    rgb[k] = min(max((sat - 0.5) * 1.3 + 0.5, 0), 1)
                }
                col[c * 4] = UInt8(rgb[0] * 255); col[c * 4 + 1] = UInt8(rgb[1] * 255); col[c * 4 + 2] = UInt8(rgb[2] * 255)
            }
            // Fill every sample this frame satisfies (sparse proxies, short clips).
            while next < samples && (Double(next) + 0.5) / Double(samples) * duration <= frames.time + 0.5 / max(reader.frameRate, 1) {
                columns[next] = col
                next += 1
            }
        }
        seq.cancel()
        if next == 0 { return nil }
        for i in next..<samples { columns[i] = columns[next - 1] }

        // Equirect column c is longitude -180...180 (left to right = increasingly to the right);
        // scan row y uses the same order, which matches rowFraction (up = left).
        var pixels = [UInt8](repeating: 0, count: samples * lonCount * 4)
        for x in 0..<samples {
            for y in 0..<lonCount {
                let s = y * 4, d = (y * samples + x) * 4
                pixels[d] = columns[x][s]; pixels[d + 1] = columns[x][s + 1]; pixels[d + 2] = columns[x][s + 2]
                pixels[d + 3] = 255
            }
        }
        guard let provider = CGDataProvider(data: Data(pixels) as CFData),
              let image = CGImage(width: samples, height: lonCount, bitsPerComponent: 8, bitsPerPixel: 32,
                                  bytesPerRow: samples * 4, space: CGColorSpace(name: CGColorSpace.sRGB)!,
                                  bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.noneSkipLast.rawValue),
                                  provider: provider, decode: nil, shouldInterpolate: true, intent: .defaultIntent)
        else { return nil }
        let shake = motion.speedProfile(bins: 240, duration: duration).map { min($0 / 180, 1) }
        return HorizonScan(image: image, duration: duration, originHeading: origin, shake: shake)
    }
}
