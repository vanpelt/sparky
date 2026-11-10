import Accelerate
import Foundation

/// Spectral residual saliency (Hou & Zhang, CVPR 2007) on a world-locked equirect luma frame:
/// what stands out from the scene's usual texture statistics. The FFT is periodic in x, so the
/// ±180° seam needs no special handling. Size must be powers of two (128 × 64 here).
final class SpectralResidual {
    let width: Int, height: Int
    private let log2w: vDSP_Length, log2h: vDSP_Length
    private let setup: FFTSetup
    private var re: [Float], im: [Float]

    init?(width: Int, height: Int) {
        guard width.nonzeroBitCount == 1, height.nonzeroBitCount == 1,
              let s = vDSP_create_fftsetup(vDSP_Length(max(width, height).trailingZeroBitCount), FFTRadix(kFFTRadix2))
        else { return nil }
        self.width = width; self.height = height
        log2w = vDSP_Length(width.trailingZeroBitCount); log2h = vDSP_Length(height.trailingZeroBitCount)
        setup = s
        re = [Float](repeating: 0, count: width * height)
        im = re
    }

    deinit { vDSP_destroy_fftsetup(setup) }

    /// Saliency map (same size, row-major, non-negative; scale is per frame).
    func saliency(_ luma: [Float]) -> [Float] {
        let n = width * height
        precondition(luma.count == n)
        re = luma
        for i in 0..<n { im[i] = 0 }
        fft(forward: true)
        // Log amplitude, its 3×3 local mean (wrapping), and the residual.
        var logA = [Float](repeating: 0, count: n), phaseC = [Float](repeating: 0, count: n), phaseS = phaseC
        for i in 0..<n {
            let a = (re[i] * re[i] + im[i] * im[i]).squareRoot()
            logA[i] = log(a + 1e-6)
            phaseC[i] = a > 0 ? re[i] / a : 1
            phaseS[i] = a > 0 ? im[i] / a : 0
        }
        for y in 0..<height {
            for x in 0..<width {
                var m: Float = 0
                for dy in -1...1 {
                    for dx in -1...1 {
                        m += logA[((y + dy + height) % height) * width + (x + dx + width) % width]
                    }
                }
                let i = y * width + x
                let r = exp(logA[i] - m / 9)
                re[i] = r * phaseC[i]
                im[i] = r * phaseS[i]
            }
        }
        fft(forward: false)
        var s = [Float](repeating: 0, count: n)
        for i in 0..<n { s[i] = re[i] * re[i] + im[i] * im[i] }
        return blur(s, passes: 2)
    }

    private func fft(forward: Bool) {
        re.withUnsafeMutableBufferPointer { r in
            im.withUnsafeMutableBufferPointer { i in
                var split = DSPSplitComplex(realp: r.baseAddress!, imagp: i.baseAddress!)
                vDSP_fft2d_zip(setup, &split, 1, 0, log2w, log2h, forward ? FFTDirection(kFFTDirection_Forward)
                               : FFTDirection(kFFTDirection_Inverse))
            }
        }
    }

    /// Box blur (wrapping in x, clamped in y); two passes ≈ Gaussian of a couple of pixels.
    private func blur(_ x: [Float], passes: Int) -> [Float] {
        var a = x, b = x
        for _ in 0..<passes {
            for y in 0..<height {
                for xx in 0..<width {
                    var m: Float = 0
                    for dy in -1...1 {
                        let yy = min(max(y + dy, 0), height - 1)
                        for dx in -1...1 { m += a[yy * width + (xx + dx + width) % width] }
                    }
                    b[y * width + xx] = m / 9
                }
            }
            swap(&a, &b)
        }
        return a
    }
}
