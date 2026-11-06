import AVFoundation
import CoreVideo
import Foundation

/// Hardware-decodes the lens tracks of an .insv (or the packed .lrv proxy) via AVFoundation.
public final class FrameReader: @unchecked Sendable {
    public let url: URL
    public let asset: AVURLAsset
    public let tracks: [AVAssetTrack]
    public let duration: Double
    public let frameRate: Double
    let pixelFormat: OSType

    public init(url: URL) async throws {
        self.url = url
        asset = AVURLAsset(url: try mp4Alias(for: url))
        let video = try await asset.loadTracks(withMediaType: .video).sorted { $0.trackID < $1.trackID }
        guard let first = video.first else {
            throw NSError(domain: "Reframe", code: 10, userInfo: [NSLocalizedDescriptionKey: "no video tracks"])
        }
        tracks = video
        duration = try await asset.load(.duration).seconds
        frameRate = Double(try await first.load(.nominalFrameRate))
        let desc = try await first.load(.formatDescriptions).first
        let bits = desc.flatMap { CMFormatDescriptionGetExtension($0, extensionKey: "BitsPerComponent" as CFString) as? Int } ?? 8
        pixelFormat = bits > 8 ? kCVPixelFormatType_420YpCbCr10BiPlanarVideoRange : kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange
    }

    /// Sequential decoder starting at `start` (the first frame at or after it).
    public func sequence(from start: Double, to end: Double? = nil) throws -> FrameSequence {
        try FrameSequence(reader: self, start: start, end: end ?? duration)
    }

    /// Decode the single frame at (or just after) `time`.
    public func frame(at time: Double) throws -> LensFrames? {
        try sequence(from: time, to: min(duration, time + 2 / max(frameRate, 1))).next()
    }
}

public final class FrameSequence: @unchecked Sendable {
    private let reader: AVAssetReader
    private let outputs: [AVAssetReaderTrackOutput]

    init(reader source: FrameReader, start: Double, end: Double) throws {
        reader = try AVAssetReader(asset: source.asset)
        let ts = CMTimeScale(48000)
        reader.timeRange = CMTimeRange(start: CMTime(seconds: max(0, start), preferredTimescale: ts),
                                       end: CMTime(seconds: end, preferredTimescale: ts))
        let settings: [String: Any] = [
            kCVPixelBufferPixelFormatTypeKey as String: source.pixelFormat,
            kCVPixelBufferMetalCompatibilityKey as String: true,
            kCVPixelBufferIOSurfacePropertiesKey as String: [:] as [String: Any],
        ]
        outputs = source.tracks.map {
            let o = AVAssetReaderTrackOutput(track: $0, outputSettings: settings)
            o.alwaysCopiesSampleData = false
            return o
        }
        for o in outputs { reader.add(o) }
        guard reader.startReading() else { throw reader.error ?? NSError(domain: "Reframe", code: 11) }
    }

    public func next() -> LensFrames? {
        var buffers: [CVPixelBuffer] = []
        var time = 0.0
        for (i, o) in outputs.enumerated() {
            guard let sample = o.copyNextSampleBuffer(), let pb = CMSampleBufferGetImageBuffer(sample) else { return nil }
            if i == 0 { time = CMSampleBufferGetPresentationTimeStamp(sample).seconds }
            buffers.append(pb)
        }
        return LensFrames(time: time, buffers: buffers)
    }

    public func cancel() { reader.cancelReading() }
}

/// AVFoundation won't open the .insv/.lrv extensions; a .mp4-named symlink works (same bytes).
func mp4Alias(for url: URL) throws -> URL {
    let ext = url.pathExtension.lowercased()
    if ext == "mp4" || ext == "mov" { return url }
    let dir = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("Reframe/aliases", isDirectory: true)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    let safe = url.path.replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: " ", with: "-")
    let alias = dir.appendingPathComponent(String(safe.suffix(200)) + ".mp4")
    if (try? FileManager.default.destinationOfSymbolicLink(atPath: alias.path)) != url.path {
        try? FileManager.default.removeItem(at: alias)
        try FileManager.default.createSymbolicLink(at: alias, withDestinationURL: url)
    }
    return alias
}
