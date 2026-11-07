import CoreGraphics
import Foundation
import simd
import Vision

/// One sequential decode of the 2048×1024 proxy that feeds every pixel-based signal:
/// - junk statistics (luma per lens) at `pictureRate`;
/// - world-locked equirect frames at `motionRate`, differenced into per-heading change, and at
///   `sceneryRate` turned into spectral-residual salience per heading and a horizon colour
///   histogram (for novelty);
/// - six world-locked horizon views at `detectorRate`, run through Vision's face and
///   human detectors on the Neural Engine.
/// Views are rendered in the heading grid's frame (Lock stabilization at the clip's first frame),
/// so the scene stands still and anything that changes is really moving.
struct ProxyScan {
    var pictureRate = 2.0
    var motionRate = 8.0
    var detectorRate = 2.0
    /// Detector views: yaw spacing, field of view and size. A direction belongs to the view
    /// whose centre is nearest, and every view overlaps its neighbours by 20° each side.
    var viewSpacing = 60.0
    var viewFOV = 100.0
    var viewSize = 448
    var sceneryRate = 2.0
    /// Motion equirect: rendered at 4× and box-filtered down to `motionWidth` × `motionWidth/2`
    /// (a power of two, for the salience FFT).
    var motionWidth = 128

    struct Result {
        var pictures: [PictureAnalysis.Sample] = []
        /// Mean absolute luma change per (motion frame, heading bin), horizon band only.
        var motionTimes: [Double] = []
        var motionFrames: [[Float]] = []
        var detections: [Detection] = []
        /// Spectral-residual salience per heading bin (each frame scaled to its own maximum).
        var salienceTimes: [Double] = []
        var salienceFrames: [[Float]] = []
        /// 4×4×4 RGB histogram of the horizon band (sums to 1).
        var histogramTimes: [Double] = []
        var histograms: [[Float]] = []
        var timings: [String: Double] = [:]
    }

    func run(clip: Clip, proxy: URL, renderer: ReframeRenderer) async throws -> Result {
        let reader = try await FrameReader(url: proxy)
        guard let motion = clip.motion else {
            return Result(pictures: try await PictureAnalysis.samples(proxy: proxy, rig: clip.rig))
        }
        return try await withCheckedThrowingContinuation { cont in
            DispatchQueue.global(qos: .utility).async {
                do { cont.resume(returning: try scan(reader: reader, clip: clip, motion: motion, renderer: renderer)) }
                catch { cont.resume(throwing: error) }
            }
        }
    }

    private func scan(reader: FrameReader, clip: Clip, motion: Motion, renderer: ReframeRenderer) throws -> Result {
        var result = Result()
        let seq = try reader.sequence(from: 0)
        defer { seq.cancel() }
        let leftIsLens0 = clip.rig.lenses[0].cx < clip.rig.lenses[0].canvasWidth / 2
        let detectorsOn = detectorRate > 0
        // Timelapses too: their baselines absorb the constant churn, and what's left is activity.
        let motionOn = true
        let residual = SpectralResidual(width: motionWidth, height: motionWidth / 2)
        var nextScenery = 0.0, tScenery = 0.0

        // Vision runs on its own queue so decode and rendering keep going.
        let visionQueue = OperationQueue()
        visionQueue.maxConcurrentOperationCount = 3
        let lock = NSLock()
        var detections: [Detection] = []

        var nextPicture = 0.0, nextMotion = 0.0, nextDetect = 0.0
        var prevMotion: [Float]?
        var tDecode = 0.0, tMotion = 0.0, tRender = 0.0
        var clock = Date()
        while let f = seq.next() {
            tDecode += Date().timeIntervalSince(clock)
            let t = f.time
            if t >= nextPicture, let pb = f.buffers.first {
                var halves = PictureAnalysis.stats(pb)
                if !leftIsLens0 { halves.swapAt(0, 1) }
                result.pictures.append(PictureAnalysis.Sample(time: t, lenses: halves))
                nextPicture += 1 / pictureRate
            }
            if motionOn, t >= nextMotion {
                let c0 = Date()
                let scenery = t >= nextScenery
                if let (frame, histogram) = motionFrame(f, clip: clip, motion: motion, renderer: renderer, histogram: scenery) {
                    if let prev = prevMotion {
                        result.motionTimes.append(t)
                        result.motionFrames.append(difference(prev, frame))
                    }
                    prevMotion = frame
                    if scenery, let residual, let histogram {
                        let c1 = Date()
                        result.salienceTimes.append(t)
                        result.salienceFrames.append(salience(residual.saliency(frame)))
                        result.histogramTimes.append(t)
                        result.histograms.append(histogram)
                        tScenery += Date().timeIntervalSince(c1)
                        nextScenery = max(nextScenery + 1 / sceneryRate, t + 0.5 / sceneryRate)
                    }
                }
                tMotion += Date().timeIntervalSince(c0)
                nextMotion = max(nextMotion + 1 / motionRate, t + 0.5 / motionRate)
            }
            if detectorsOn, t >= nextDetect {
                let c0 = Date()
                let views = detectorViews(f, clip: clip, motion: motion, renderer: renderer)
                tRender += Date().timeIntervalSince(c0)
                visionQueue.addOperation {
                    let found = views.flatMap { detect(in: $0.image, viewYaw: $0.yaw, time: t) }
                    lock.withLock { detections += found }
                }
                nextDetect = max(nextDetect + 1 / detectorRate, t + 0.5 / detectorRate)
            }
            clock = Date()
        }
        let c0 = Date()
        visionQueue.waitUntilAllOperationsAreFinished()
        result.detections = detections.sorted { $0.time < $1.time }
        result.timings = ["decode": tDecode, "motion frames": tMotion, "salience": tScenery, "detector views": tRender,
                          "vision wait": Date().timeIntervalSince(c0)]
        return result
    }

    // MARK: Motion frames

    /// World-locked equirect luma at motionWidth × motionWidth/2 (2.8° per pixel at 128), plus
    /// (when asked) a 4×4×4 RGB histogram of the -30…+50° band from the full-size render.
    private func motionFrame(_ f: LensFrames, clip: Clip, motion: Motion, renderer: ReframeRenderer,
                             histogram wantHistogram: Bool) -> ([Float], [Float]?)? {
        let W = motionWidth * 4, H = W / 2
        var view = ViewSettings()
        view.projection = .equirect
        view.bodyFromCamera = motion.bodyFromCamera(at: f.time, mode: .lock, smoothing: 0, lockTime: 0,
                                                    yaw: 0, pitch: 0, roll: 0)
        guard let rgba = renderer.renderRGBA8(frames: f, rig: clip.rig, view: view, width: W, height: H) else { return nil }
        let w = motionWidth, h = motionWidth / 2
        var out = [Float](repeating: 0, count: w * h)
        for y in 0..<h {
            for x in 0..<w {
                var s = 0
                for yy in 0..<4 {
                    let row = ((y * 4 + yy) * W + x * 4) * 4
                    for xx in 0..<4 {
                        let p = row + xx * 4
                        s += 54 * Int(rgba[p]) + 183 * Int(rgba[p + 1]) + 19 * Int(rgba[p + 2])
                    }
                }
                out[y * w + x] = Float(s) / (16 * 256 * 255)
            }
        }
        guard wantHistogram else { return (out, nil) }
        var hist = [Float](repeating: 0, count: 64)
        let rowLo = Int(Double(H) * (90 - 50) / 180), rowHi = Int(Double(H) * (90 + 30) / 180)
        var n: Float = 0
        for y in Swift.stride(from: rowLo, to: rowHi, by: 2) {
            for x in Swift.stride(from: 0, to: W, by: 2) {
                let p = (y * W + x) * 4
                hist[Int(rgba[p] >> 6) * 16 + Int(rgba[p + 1] >> 6) * 4 + Int(rgba[p + 2] >> 6)] += 1
                n += 1
            }
        }
        return (out, hist.map { $0 / max(n, 1) })
    }

    /// Salience per heading bin over the -30…+50° band (landmarks sit above the horizon more
    /// often than below; lower down is usually the operator or the vehicle), scaled to the frame max.
    private func salience(_ map: [Float]) -> [Float] {
        let w = motionWidth, h = motionWidth / 2
        let rowLo = Int(Double(h) * (90 - 50) / 180), rowHi = Int(Double(h) * (90 + 30) / 180)
        var bins = [Float](repeating: 0, count: HeadingGrid.bins), counts = bins
        for x in 0..<w {
            let bin = HeadingGrid.bin(yaw: -((Double(x) + 0.5) / Double(w) * 360 - 180))
            for y in rowLo..<rowHi { bins[bin] += map[y * w + x]; counts[bin] += 1 }
        }
        let means = zip(bins, counts).map { $1 > 0 ? $0 / $1 : 0 }
        let top = max(means.max() ?? 0, 1e-12)
        return means.map { $0 / top }
    }

    /// Mean |Δluma| per heading bin over the horizon band (±35° pitch: below that is usually the
    /// operator, the stick or the vehicle; above it, sky).
    private func difference(_ a: [Float], _ b: [Float]) -> [Float] {
        let w = motionWidth, h = motionWidth / 2
        let rowLo = Int(Double(h) * (90 - 35) / 180), rowHi = Int(Double(h) * (90 + 35) / 180)
        var bins = [Float](repeating: 0, count: HeadingGrid.bins)
        var counts = [Float](repeating: 0, count: HeadingGrid.bins)
        for x in 0..<w {
            // Column centre longitude -> yaw (left-positive is minus longitude).
            let lon = (Double(x) + 0.5) / Double(w) * 360 - 180
            let bin = HeadingGrid.bin(yaw: -lon)
            for y in rowLo..<rowHi {
                bins[bin] += abs(a[y * w + x] - b[y * w + x])
                counts[bin] += 1
            }
        }
        return zip(bins, counts).map { $1 > 0 ? $0 / $1 : 0 }
    }

    // MARK: Detectors

    struct View { let yaw: Double; let image: CGImage }

    private func detectorViews(_ f: LensFrames, clip: Clip, motion: Motion, renderer: ReframeRenderer) -> [View] {
        stride(from: -180.0, to: 180.0, by: viewSpacing).compactMap { yaw in
            var view = ViewSettings()
            view.projection = .flat
            view.hfov = viewFOV
            view.distortion = 0
            view.bodyFromCamera = motion.bodyFromCamera(at: f.time, mode: .lock, smoothing: 0, lockTime: 0,
                                                        yaw: yaw, pitch: 0, roll: 0)
            return renderer.renderImage(frames: f, rig: clip.rig, view: view, width: viewSize, height: viewSize,
                                        sixteenBit: false).map { View(yaw: yaw, image: $0) }
        }
    }

    /// Faces and people in one rectilinear view, converted to heading-grid directions. Only
    /// detections centred within half the view spacing are kept, so overlaps aren't double-counted.
    private func detect(in image: CGImage, viewYaw: Double, time: Double) -> [Detection] {
        let faces = VNDetectFaceRectanglesRequest()
        let people = VNDetectHumanRectanglesRequest()
        people.upperBodyOnly = false
        do { try VNImageRequestHandler(cgImage: image).perform([faces, people]) } catch { return [] }

        let W = Double(image.width), H = Double(image.height)
        let focal = W / 2 / tan(viewFOV * .pi / 360)
        func direction(_ x: Double, _ y: Double) -> (yaw: Double, pitch: Double) {
            let dx = x - W / 2, dy = H / 2 - y
            return (atan2(-dx, focal) * 180 / .pi, atan2(dy, hypot(focal, dx)) * 180 / .pi)
        }
        func convert(_ box: CGRect, _ kind: Detection.Kind, _ confidence: Float) -> Detection? {
            // Vision boxes are normalized with the origin at the bottom left.
            let x0 = Double(box.minX) * W, x1 = Double(box.maxX) * W
            let y0 = (1 - Double(box.maxY)) * H, y1 = (1 - Double(box.minY)) * H
            let c = direction((x0 + x1) / 2, (y0 + y1) / 2)
            guard abs(c.yaw) <= viewSpacing / 2 + 0.01 else { return nil }
            let l = direction(x0, (y0 + y1) / 2), r = direction(x1, (y0 + y1) / 2)
            let top = direction((x0 + x1) / 2, y0), bottom = direction((x0 + x1) / 2, y1)
            var yaw = viewYaw + c.yaw
            if yaw >= 180 { yaw -= 360 } else if yaw < -180 { yaw += 360 }
            return Detection(kind: kind, time: time, yaw: yaw, pitch: c.pitch, width: abs(l.yaw - r.yaw),
                             height: abs(top.pitch - bottom.pitch), confidence: Double(confidence))
        }
        var out: [Detection] = []
        for o in faces.results ?? [] where o.confidence > 0.5 {
            if let d = convert(o.boundingBox, .face, o.confidence) { out.append(d) }
        }
        for o in people.results ?? [] where o.confidence > 0.5 {
            if let d = convert(o.boundingBox, .person, o.confidence) { out.append(d) }
        }
        return out
    }
}
