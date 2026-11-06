import CoreGraphics
import CoreVideo
import Foundation
import ImageIO
import Metal
import simd
import UniformTypeIdentifiers

/// One decoded moment: either one pixel buffer per lens track (.insv) or a single packed
/// side-by-side buffer (.lrv proxy).
public struct LensFrames: @unchecked Sendable {
    public let time: Double
    public let buffers: [CVPixelBuffer]
    public var packed: Bool { buffers.count == 1 }

    public init(time: Double, buffers: [CVPixelBuffer]) {
        self.time = time
        self.buffers = buffers
    }
}

/// Calibrated two-lens rig for one recording.
public struct Rig: Sendable {
    public let lenses: [Lens]
    public let rotations: [simd_double3x3]
    /// Metadata field 80: 1 => video track 0 carries stream "10" (calibration lens 1).
    public let trackForLens: [Int]
    public let readoutSeconds: Double

    public init(info: InsvInfo) throws {
        lenses = try parseCalibration(info.calibration)
        rotations = lenses.enumerated().map { $1.rotation(index: $0) }
        trackForLens = info.trackOrder == 1 ? [1, 0] : [0, 1]
        readoutSeconds = (info.rollingShutterMs ?? 0) / 1000
    }

    /// Shader parameters for both lenses, given the source layout.
    func lensParameters(_ frames: LensFrames) -> [Float] {
        var p = [Float](repeating: 0, count: 64)
        for i in 0..<2 {
            let lens = lenses[i]
            let buffer = frames.packed ? frames.buffers[0] : frames.buffers[trackForLens[i]]
            let tw = Double(CVPixelBufferGetWidth(buffer)), th = Double(CVPixelBufferGetHeight(buffer))
            let half = lens.canvasWidth / 2
            let scale: Double, offsetX: Double, xMin: Double, xMax: Double
            if frames.packed {  // the proxy is the calibration canvas, scaled
                scale = tw / lens.canvasWidth; offsetX = 0
                xMin = Double(i) * tw / 2; xMax = Double(i + 1) * tw / 2
            } else {
                scale = tw / half; offsetX = Double(i) * half
                xMin = 0; xMax = tw
            }
            let r = rotations[i]
            let o = i * 32
            for row in 0..<3 { for col in 0..<3 { p[o + row * 3 + col] = Float(r[col][row]) } }
            p[o + 9] = Float(lens.xi)
            p[o + 10] = Float(lens.fx * scale); p[o + 11] = Float(lens.fy * scale)
            p[o + 12] = Float((lens.cx - offsetX) * scale); p[o + 13] = Float(lens.cy * scale)
            for (k, d) in lens.distortion.prefix(13).enumerated() { p[o + 14 + k] = Float(d) }
            p[o + 27] = Float(xMin); p[o + 28] = Float(xMax)
            let tenBit = CVPixelBufferGetPixelFormatType(buffer) == kCVPixelFormatType_420YpCbCr10BiPlanarVideoRange
            p[o + 29] = tenBit ? Float(65535.0 / 65472.0) : 1
            p[o + 30] = Float(tw); p[o + 31] = Float(th)
        }
        return p
    }
}

public enum Projection: String, CaseIterable, Identifiable, Sendable {
    case flat, equirect
    public var id: String { rawValue }
}

public struct ViewSettings: Sendable {
    public var projection: Projection = .flat
    public var bodyFromCamera = simd_double3x3(diagonal: [1, 1, 1])
    public var hfov: Double = 90
    public var angularVelocity = SIMD3<Double>.zero
    /// Rolling-shutter readout in seconds; 0 disables correction.
    public var readout: Double = 0
    public var applyLUT = false
    public var seamBelt: Double = 6

    public init() {}
}

public final class ReframeRenderer: @unchecked Sendable {
    public let device: MTLDevice
    public let queue: MTLCommandQueue
    private let pipeline: MTLComputePipelineState
    private let yuvPipeline: MTLComputePipelineState
    private let cache: CVMetalTextureCache
    private var lutTexture: MTLTexture
    private var lutSize = 2
    public private(set) var lut: CubeLUT?

    public init() throws {
        guard let device = MTLCreateSystemDefaultDevice(), let queue = device.makeCommandQueue() else {
            throw NSError(domain: "Reframe", code: 1, userInfo: [NSLocalizedDescriptionKey: "no Metal device"])
        }
        self.device = device
        self.queue = queue
        let library = try device.makeLibrary(source: reframeShaderSource, options: nil)
        pipeline = try device.makeComputePipelineState(function: library.makeFunction(name: "reframe")!)
        yuvPipeline = try device.makeComputePipelineState(function: library.makeFunction(name: "reframeYUV422")!)
        var c: CVMetalTextureCache?
        CVMetalTextureCacheCreate(nil, nil, device, nil, &c)
        cache = c!
        let d = MTLTextureDescriptor()
        d.textureType = .type3D; d.pixelFormat = .rgba32Float; d.width = 2; d.height = 2; d.depth = 2
        lutTexture = device.makeTexture(descriptor: d)!
    }

    public func setLUT(_ lut: CubeLUT?) {
        self.lut = lut
        if let lut, let tex = lut.makeTexture(device: device) {
            lutTexture = tex; lutSize = lut.size
        }
    }

    private func planes(_ pb: CVPixelBuffer) -> (luma: CVMetalTexture, chroma: CVMetalTexture)? {
        let tenBit = CVPixelBufferGetPixelFormatType(pb) == kCVPixelFormatType_420YpCbCr10BiPlanarVideoRange
        func plane(_ i: Int, _ fmt: MTLPixelFormat) -> CVMetalTexture? {
            var t: CVMetalTexture?
            CVMetalTextureCacheCreateTextureFromImage(nil, cache, pb, nil, fmt,
                                                      CVPixelBufferGetWidthOfPlane(pb, i),
                                                      CVPixelBufferGetHeightOfPlane(pb, i), i, &t)
            return t
        }
        guard let y = plane(0, tenBit ? .r16Unorm : .r8Unorm), let c = plane(1, tenBit ? .rg16Unorm : .rg8Unorm) else {
            return nil
        }
        return (y, c)
    }

    private func sourcePlanes(_ frames: LensFrames, rig: Rig) -> [(luma: CVMetalTexture, chroma: CVMetalTexture)]? {
        if frames.packed {
            guard let p = planes(frames.buffers[0]) else { return nil }
            return [p, p]
        }
        guard let a = planes(frames.buffers[rig.trackForLens[0]]),
              let b = planes(frames.buffers[rig.trackForLens[1]]) else { return nil }
        return [a, b]
    }

    private func parameters(_ frames: LensFrames, rig: Rig, view: ViewSettings, width: Int) -> [Float] {
        var p = rig.lensParameters(frames) + [Float](repeating: 0, count: 32)
        let m = view.bodyFromCamera
        for row in 0..<3 { for col in 0..<3 { p[64 + row * 3 + col] = Float(m[col][row]) } }
        p[73] = Float(Double(width) / 2 / tan(view.hfov * .pi / 360))
        p[74] = view.projection == .flat ? 0 : 1
        p[75] = Float(view.readout)
        p[76] = Float(view.angularVelocity.x); p[77] = Float(view.angularVelocity.y); p[78] = Float(view.angularVelocity.z)
        p[79] = Float(view.seamBelt)
        p[80] = view.applyLUT && lut != nil ? 1 : 0
        p[81] = Float(lutSize)
        return p
    }

    private func dispatch(_ cb: MTLCommandBuffer, pipeline: MTLComputePipelineState, textures: [Int: MTLTexture],
                          sources: [(luma: CVMetalTexture, chroma: CVMetalTexture)], params: [Float], grid: MTLSize) {
        guard let enc = cb.makeComputeCommandEncoder() else { return }
        enc.setComputePipelineState(pipeline)
        for (i, t) in textures { enc.setTexture(t, index: i) }
        enc.setTexture(CVMetalTextureGetTexture(sources[0].luma), index: 1)
        enc.setTexture(CVMetalTextureGetTexture(sources[0].chroma), index: 2)
        enc.setTexture(CVMetalTextureGetTexture(sources[1].luma), index: 3)
        enc.setTexture(CVMetalTextureGetTexture(sources[1].chroma), index: 4)
        enc.setTexture(lutTexture, index: 5)
        params.withUnsafeBytes { enc.setBytes($0.baseAddress!, length: $0.count, index: 0) }
        enc.dispatchThreads(grid, threadsPerThreadgroup: MTLSize(width: 16, height: 16, depth: 1))
        enc.endEncoding()
    }

    /// Encodes the reframe pass into an RGBA texture. Keep the returned objects alive until the
    /// command buffer completes.
    @discardableResult
    public func encode(_ cb: MTLCommandBuffer, frames: LensFrames, rig: Rig, view: ViewSettings,
                       output: MTLTexture) -> [AnyObject] {
        guard let sources = sourcePlanes(frames, rig: rig) else { return [] }
        dispatch(cb, pipeline: pipeline, textures: [0: output], sources: sources,
                 params: parameters(frames, rig: rig, view: view, width: output.width),
                 grid: MTLSize(width: output.width, height: output.height, depth: 1))
        return sources.flatMap { [$0.luma, $0.chroma] } + frames.buffers
    }

    /// Encodes the reframe pass straight into a 10-bit 4:2:2 ('x422') pixel buffer for ProRes.
    @discardableResult
    public func encodeYUV422(_ cb: MTLCommandBuffer, frames: LensFrames, rig: Rig, view: ViewSettings,
                             into buffer: CVPixelBuffer) -> [AnyObject]? {
        guard let sources = sourcePlanes(frames, rig: rig) else { return nil }
        func plane(_ i: Int, _ fmt: MTLPixelFormat) -> CVMetalTexture? {
            var t: CVMetalTexture?
            let attrs = [kCVMetalTextureUsage as String: MTLTextureUsage.shaderWrite.rawValue] as CFDictionary
            CVMetalTextureCacheCreateTextureFromImage(nil, cache, buffer, attrs, fmt,
                                                      CVPixelBufferGetWidthOfPlane(buffer, i),
                                                      CVPixelBufferGetHeightOfPlane(buffer, i), i, &t)
            return t
        }
        guard let y = plane(0, .r16Unorm), let c = plane(1, .rg16Unorm),
              let yt = CVMetalTextureGetTexture(y), let ct = CVMetalTextureGetTexture(c) else { return nil }
        let w = CVPixelBufferGetWidth(buffer), h = CVPixelBufferGetHeight(buffer)
        dispatch(cb, pipeline: yuvPipeline, textures: [0: yt, 6: ct], sources: sources,
                 params: parameters(frames, rig: rig, view: view, width: w),
                 grid: MTLSize(width: w / 2, height: h, depth: 1))
        return sources.flatMap { [$0.luma, $0.chroma] } + [y, c] + frames.buffers + [buffer]
    }

    /// Offscreen render to tightly packed RGBA8 bytes.
    public func renderRGBA8(frames: LensFrames, rig: Rig, view: ViewSettings, width: Int, height: Int) -> [UInt8]? {
        let d = MTLTextureDescriptor.texture2DDescriptor(pixelFormat: .rgba8Unorm, width: width, height: height,
                                                         mipmapped: false)
        d.usage = [.shaderWrite, .shaderRead]
        d.storageMode = .shared
        guard let tex = device.makeTexture(descriptor: d), let cb = queue.makeCommandBuffer() else { return nil }
        let keep = encode(cb, frames: frames, rig: rig, view: view, output: tex)
        cb.commit()
        cb.waitUntilCompleted()
        _ = keep
        var bytes = [UInt8](repeating: 0, count: width * height * 4)
        tex.getBytes(&bytes, bytesPerRow: width * 4, from: MTLRegionMake2D(0, 0, width, height), mipmapLevel: 0)
        return bytes
    }

    /// Offscreen render to a 16-bit RGB CGImage (CLI, thumbnails, tests).
    public func renderImage(frames: LensFrames, rig: Rig, view: ViewSettings, width: Int, height: Int,
                            sixteenBit: Bool = true) -> CGImage? {
        let d = MTLTextureDescriptor.texture2DDescriptor(pixelFormat: sixteenBit ? .rgba16Unorm : .rgba8Unorm,
                                                         width: width, height: height, mipmapped: false)
        d.usage = [.shaderWrite, .shaderRead]
        d.storageMode = .shared
        guard let tex = device.makeTexture(descriptor: d), let cb = queue.makeCommandBuffer() else { return nil }
        let keep = encode(cb, frames: frames, rig: rig, view: view, output: tex)
        cb.commit()
        cb.waitUntilCompleted()
        _ = keep
        let bpp = sixteenBit ? 8 : 4
        var bytes = [UInt8](repeating: 0, count: width * height * bpp)
        tex.getBytes(&bytes, bytesPerRow: width * bpp, from: MTLRegionMake2D(0, 0, width, height), mipmapLevel: 0)
        let info: CGBitmapInfo = sixteenBit
            ? [CGBitmapInfo(rawValue: CGImageAlphaInfo.noneSkipLast.rawValue), .byteOrder16Little]
            : CGBitmapInfo(rawValue: CGImageAlphaInfo.noneSkipLast.rawValue)
        guard let provider = CGDataProvider(data: Data(bytes) as CFData) else { return nil }
        return CGImage(width: width, height: height, bitsPerComponent: sixteenBit ? 16 : 8, bitsPerPixel: bpp * 8,
                       bytesPerRow: width * bpp, space: CGColorSpace(name: CGColorSpace.itur_709)!,
                       bitmapInfo: info, provider: provider, decode: nil, shouldInterpolate: false,
                       intent: .defaultIntent)
    }
}

public func writePNG(_ image: CGImage, to url: URL) throws {
    guard let dest = CGImageDestinationCreateWithURL(url as CFURL, UTType.png.identifier as CFString, 1, nil) else {
        throw NSError(domain: "Reframe", code: 2)
    }
    CGImageDestinationAddImage(dest, image, nil)
    guard CGImageDestinationFinalize(dest) else { throw NSError(domain: "Reframe", code: 3) }
}
