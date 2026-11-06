import AVFoundation
import CoreVideo
import Foundation
import Metal

public struct ExportOptions: Sendable {
    public enum Codec: String, CaseIterable, Identifiable, Sendable {
        case proRes422HQ, proRes422, proRes422LT
        public var id: String { rawValue }
        public var label: String {
            switch self {
            case .proRes422HQ: return "ProRes 422 HQ"
            case .proRes422: return "ProRes 422"
            case .proRes422LT: return "ProRes 422 LT"
            }
        }
        var avCodec: AVVideoCodecType {
            switch self {
            case .proRes422HQ: return .proRes422HQ
            case .proRes422: return .proRes422
            case .proRes422LT: return .proRes422LT
            }
        }
    }

    public enum Size: String, CaseIterable, Identifiable, Sendable {
        case uhd, hd, vertical, square
        public var id: String { rawValue }
        public var label: String {
            switch self {
            case .uhd: return "4K UHD (3840 × 2160)"
            case .hd: return "HD (1920 × 1080)"
            case .vertical: return "Vertical (1080 × 1920)"
            case .square: return "Square (1080 × 1080)"
            }
        }
        public var dimensions: (width: Int, height: Int) {
            switch self {
            case .uhd: return (3840, 2160)
            case .hd: return (1920, 1080)
            case .vertical: return (1080, 1920)
            case .square: return (1080, 1080)
            }
        }
    }

    public var codec: Codec = .proRes422HQ
    public var size: Size = .hd
    /// Bake the preview LUT into the pixels (off: footage stays in the camera's log encoding).
    public var bakeLook = false
    public var rollingShutter = true
    public var destination: URL

    public init(destination: URL) { self.destination = destination }
}

/// A finished shot file, described for timeline interchange.
public struct ExportedShot: Sendable {
    public let url: URL
    public let name: String
    public let frameCount: Int
    public let frameDuration: CMTime
    /// Start timecode in frames (time of day the moment was recorded).
    public let timecodeFrames: Int
    public let timecodeQuanta: Int
    public let width: Int
    public let height: Int
    public let hasAudio: Bool
    public let keywords: [String]
    public let note: String
}

public enum ExportError: LocalizedError {
    case noMotion, decode(String), writer(String)
    public var errorDescription: String? {
        switch self {
        case .noMotion: return "This clip has no gyro data, so it can't be stabilized."
        case .decode(let s): return "Couldn't read the source: \(s)"
        case .writer(let s): return "Couldn't write the file: \(s)"
        }
    }
}

/// Renders one shot to a ProRes .mov with source audio and a time-of-day timecode track.
public final class ShotExporter: @unchecked Sendable {
    let renderer: ReframeRenderer

    public init(lut: CubeLUT?) throws {
        renderer = try ReframeRenderer()
        renderer.setLUT(lut)
    }

    public func export(_ shot: Shot, clip: Clip, options: ExportOptions,
                       progress: @escaping @Sendable (Double) -> Void,
                       isCancelled: @escaping @Sendable () -> Bool) async throws -> ExportedShot {
        guard let motion = clip.motion else { throw ExportError.noMotion }
        let reader = try await FrameReader(url: clip.url)
        let fd = reader.frameDuration
        let seq = try reader.sequence(from: shot.inTime, to: shot.outTime)
        guard let firstFrame = seq.next() else { throw ExportError.decode("no frames in range") }
        let firstTime = firstFrame.time
        let total = max(1, Int(((shot.outTime - firstTime) / fd.seconds).rounded()))

        try FileManager.default.createDirectory(at: options.destination, withIntermediateDirectories: true)
        let url = options.destination.appendingPathComponent(fileName(for: shot, clip: clip))
        try? FileManager.default.removeItem(at: url)
        let writer = try AVAssetWriter(outputURL: url, fileType: .mov)
        let (w, h) = options.size.dimensions

        let videoInput = AVAssetWriterInput(mediaType: .video, outputSettings: [
            AVVideoCodecKey: options.codec.avCodec,
            AVVideoWidthKey: w,
            AVVideoHeightKey: h,
            AVVideoColorPropertiesKey: [
                AVVideoColorPrimariesKey: AVVideoColorPrimaries_ITU_R_709_2,
                AVVideoTransferFunctionKey: AVVideoTransferFunction_ITU_R_709_2,
                AVVideoYCbCrMatrixKey: AVVideoYCbCrMatrix_ITU_R_709_2,
            ],
        ])
        videoInput.expectsMediaDataInRealTime = false
        let adaptor = AVAssetWriterInputPixelBufferAdaptor(assetWriterInput: videoInput, sourcePixelBufferAttributes: [
            kCVPixelBufferPixelFormatTypeKey as String: kCVPixelFormatType_422YpCbCr10BiPlanarVideoRange,
            kCVPixelBufferWidthKey as String: w,
            kCVPixelBufferHeightKey as String: h,
            kCVPixelBufferMetalCompatibilityKey as String: true,
            kCVPixelBufferIOSurfacePropertiesKey as String: [:] as [String: Any],
        ])
        writer.add(videoInput)

        // Time-of-day timecode, so Resolve / FCP can line shots up with the original recording.
        let quanta = Int((1 / fd.seconds).rounded())
        let tcFrames = timeOfDayFrames(clip: clip, at: firstTime, quanta: quanta)
        var tcFormat: CMTimeCodeFormatDescription?
        CMTimeCodeFormatDescriptionCreate(allocator: nil, timeCodeFormatType: kCMTimeCodeFormatType_TimeCode32,
                                          frameDuration: fd, frameQuanta: UInt32(quanta),
                                          flags: kCMTimeCodeFlag_24HourMax, extensions: nil, formatDescriptionOut: &tcFormat)
        let tcInput = AVAssetWriterInput(mediaType: .timecode, outputSettings: nil, sourceFormatHint: tcFormat)
        writer.add(tcInput)
        videoInput.addTrackAssociation(withTrackOf: tcInput, type: AVAssetTrack.AssociationType.timecode.rawValue)

        // Source audio, as 24-bit PCM.
        var audioReader: AVAssetReader?, audioOutput: AVAssetReaderTrackOutput?, audioInput: AVAssetWriterInput?
        if let track = try await reader.asset.loadTracks(withMediaType: .audio).first {
            let ar = try AVAssetReader(asset: reader.asset)
            ar.timeRange = CMTimeRange(start: CMTime(seconds: firstTime, preferredTimescale: 48000),
                                       end: CMTime(seconds: shot.outTime, preferredTimescale: 48000))
            let pcm: [String: Any] = [
                AVFormatIDKey: kAudioFormatLinearPCM, AVLinearPCMBitDepthKey: 24, AVLinearPCMIsFloatKey: false,
                AVLinearPCMIsBigEndianKey: false, AVLinearPCMIsNonInterleaved: false,
                AVSampleRateKey: 48000, AVNumberOfChannelsKey: 2,
            ]
            let out = AVAssetReaderTrackOutput(track: track, outputSettings: pcm)
            ar.add(out)
            var layout = AudioChannelLayout()
            layout.mChannelLayoutTag = kAudioChannelLayoutTag_Stereo
            let input = AVAssetWriterInput(mediaType: .audio, outputSettings: pcm.merging([
                AVChannelLayoutKey: Data(bytes: &layout, count: MemoryLayout<AudioChannelLayout>.size),
            ]) { $1 })
            input.expectsMediaDataInRealTime = false
            writer.add(input)
            audioReader = ar; audioOutput = out; audioInput = input
        }

        guard writer.startWriting() else { throw ExportError.writer(writer.error?.localizedDescription ?? "start failed") }
        writer.startSession(atSourceTime: .zero)
        audioReader?.startReading()

        // Timecode: one sample holding the start frame number, spanning the whole shot.
        var tcValue = Int32(tcFrames).bigEndian
        if let tcFormat {
            var block: CMBlockBuffer?
            CMBlockBufferCreateWithMemoryBlock(allocator: nil, memoryBlock: nil, blockLength: 4, blockAllocator: nil,
                                               customBlockSource: nil, offsetToData: 0, dataLength: 4, flags: 0,
                                               blockBufferOut: &block)
            if let block {
                CMBlockBufferReplaceDataBytes(with: &tcValue, blockBuffer: block, offsetIntoDestination: 0, dataLength: 4)
                var timing = CMSampleTimingInfo(duration: CMTimeMultiply(fd, multiplier: Int32(total)),
                                                presentationTimeStamp: .zero, decodeTimeStamp: .invalid)
                var size = 4
                var sample: CMSampleBuffer?
                CMSampleBufferCreate(allocator: nil, dataBuffer: block, dataReady: true, makeDataReadyCallback: nil,
                                     refcon: nil, formatDescription: tcFormat, sampleCount: 1, sampleTimingEntryCount: 1,
                                     sampleTimingArray: &timing, sampleSizeEntryCount: 1, sampleSizeArray: &size,
                                     sampleBufferOut: &sample)
                if let sample { tcInput.append(sample) }
            }
        }
        tcInput.markAsFinished()

        let group = DispatchGroup()
        let state = ExportState()

        // Video: decode -> reframe (Metal, straight to 10-bit 4:2:2) -> ProRes.
        group.enter()
        let videoQueue = DispatchQueue(label: "reframe.export.video")
        var pending: LensFrames? = firstFrame
        var index = 0
        videoInput.requestMediaDataWhenReady(on: videoQueue) { [renderer] in
            while videoInput.isReadyForMoreMediaData {
                if isCancelled() || state.failed {
                    state.cancelled = isCancelled()
                    videoInput.markAsFinished(); group.leave(); return
                }
                guard let frames = pending ?? seq.next(), index < total else {
                    videoInput.markAsFinished(); group.leave(); return
                }
                pending = nil
                guard let pool = adaptor.pixelBufferPool else { continue }
                var pb: CVPixelBuffer?
                CVPixelBufferPoolCreatePixelBuffer(nil, pool, &pb)
                guard let pb, let cb = renderer.queue.makeCommandBuffer() else {
                    state.fail("no pixel buffer"); continue
                }
                let t = frames.time
                let f = shot.framing(at: t)
                var view = ViewSettings()
                view.hfov = f.hfov
                view.applyLUT = options.bakeLook && clip.info.colorProfile == "I_Log"
                view.bodyFromCamera = motion.bodyFromCamera(at: t, mode: shot.stabilization, smoothing: shot.smoothing,
                                                            lockTime: shot.lockTime, yaw: f.yaw, pitch: f.pitch, roll: 0)
                if options.rollingShutter {
                    view.readout = clip.rig.readoutSeconds
                    view.angularVelocity = motion.angularVelocity(at: t)
                }
                let keep = renderer.encodeYUV422(cb, frames: frames, rig: clip.rig, view: view, into: pb)
                cb.commit()
                cb.waitUntilCompleted()
                _ = keep
                if !adaptor.append(pb, withPresentationTime: CMTimeMultiply(fd, multiplier: Int32(index))) {
                    state.fail(writer.error?.localizedDescription ?? "append failed")
                }
                index += 1
                if index % 8 == 0 || index == total { progress(Double(index) / Double(total)) }
            }
        }

        if let audioInput, let audioOutput {
            group.enter()
            let offset = CMTime(seconds: firstTime, preferredTimescale: 48000)
            let end = CMTimeMultiply(fd, multiplier: Int32(total))
            audioInput.requestMediaDataWhenReady(on: DispatchQueue(label: "reframe.export.audio")) {
                while audioInput.isReadyForMoreMediaData {
                    guard !state.failed, !isCancelled(), let sample = audioOutput.copyNextSampleBuffer() else {
                        audioInput.markAsFinished(); group.leave(); return
                    }
                    let pts = CMSampleBufferGetPresentationTimeStamp(sample) - offset
                    if pts < .zero { continue }
                    if pts >= end { audioInput.markAsFinished(); group.leave(); return }
                    var timing = CMSampleTimingInfo(duration: CMSampleBufferGetDuration(sample),
                                                    presentationTimeStamp: pts, decodeTimeStamp: .invalid)
                    var shifted: CMSampleBuffer?
                    CMSampleBufferCreateCopyWithNewTiming(allocator: nil, sampleBuffer: sample, sampleTimingEntryCount: 1,
                                                          sampleTimingArray: &timing, sampleBufferOut: &shifted)
                    if let shifted { audioInput.append(shifted) }
                }
            }
        }

        await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in
            group.notify(queue: .global()) { c.resume() }
        }
        seq.cancel()
        audioReader?.cancelReading()

        if state.cancelled || isCancelled() {
            writer.cancelWriting()
            try? FileManager.default.removeItem(at: url)
            throw CancellationError()
        }
        if let message = state.error {
            writer.cancelWriting()
            throw ExportError.writer(message)
        }
        writer.endSession(atSourceTime: CMTimeMultiply(fd, multiplier: Int32(index)))
        await writer.finishWriting()
        if writer.status != .completed {
            throw ExportError.writer(writer.error?.localizedDescription ?? "finish failed")
        }

        let info = clip.info
        var keywords = [info.colorLabel, "Reframe"]
        if info.isTimelapse { keywords.append("Timelapse") }
        let note = String(format: "%@ · %.2f–%.2fs · %@ · %d keyframe%@",
                          clip.name, shot.inTime, shot.outTime, shot.stabilization.label,
                          shot.keyframes.count, shot.keyframes.count == 1 ? "" : "s")
        return ExportedShot(url: url, name: shot.name, frameCount: index, frameDuration: fd, timecodeFrames: tcFrames,
                            timecodeQuanta: quanta, width: w, height: h, hasAudio: audioInput != nil,
                            keywords: keywords, note: note)
    }

    func fileName(for shot: Shot, clip: Clip) -> String {
        let safe = shot.name.components(separatedBy: CharacterSet(charactersIn: "/:\\?%*|\"<>")).joined(separator: "-")
        return "\(safe).mov"
    }

    /// Frames since midnight at the recording's local start time plus `t`.
    func timeOfDayFrames(clip: Clip, at t: Double, quanta: Int) -> Int {
        let s = clip.info.captureTime
        guard s.count == 14, let hh = Int(s.dropFirst(8).prefix(2)), let mm = Int(s.dropFirst(10).prefix(2)),
              let ss = Int(s.dropFirst(12).prefix(2)) else { return Int((t * Double(quanta)).rounded()) }
        let seconds = Double(hh * 3600 + mm * 60 + ss) + t
        return Int((seconds * Double(quanta)).rounded()) % (24 * 3600 * quanta)
    }
}

private final class ExportState: @unchecked Sendable {
    private let lock = NSLock()
    private var _error: String?
    private var _cancelled = false
    var error: String? { lock.withLock { _error } }
    var failed: Bool { lock.withLock { _error != nil } }
    var cancelled: Bool {
        get { lock.withLock { _cancelled } }
        set { lock.withLock { _cancelled = newValue } }
    }
    func fail(_ message: String) { lock.withLock { if _error == nil { _error = message } } }
}
