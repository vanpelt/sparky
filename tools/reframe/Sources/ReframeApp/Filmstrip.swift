import CoreGraphics
import Observation
import ReframeKit

/// Tiles across a clip, rendered through a framing rule from the low-res proxy.
@Observable
final class Filmstrip {
    struct FramingRule: Sendable {
        var shot: Shot?
        var manual: Framing
        var stabilization: Motion.Stabilization
        var smoothing: Double
        var lockTime: Double
    }

    var tiles: [CGImage?] = []

    /// Renders `count` tiles; cancelled automatically when the caller's task is (SwiftUI .task(id:)).
    func render(clip: Clip, isLog: Bool, renderer: ReframeRenderer, count: Int, duration: Double,
                framing rule: FramingRule) async {
        if tiles.count != count { tiles = Array(repeating: nil, count: count) }
        try? await Task.sleep(for: .milliseconds(180))  // let rapid edits settle
        guard !Task.isCancelled, duration > 0, let motion = clip.motion,
              let reader = try? await FrameReader(url: clip.proxyURL ?? clip.url) else { return }
        for i in 0..<count {
            if Task.isCancelled { return }
            let t = (Double(i) + 0.5) / Double(count) * duration
            let image: CGImage? = await Task.detached(priority: .utility) {
                guard let frames = try? reader.frame(at: t) else { return nil }
                let f = (rule.shot?.contains(frames.time) == true) ? rule.shot!.framing(at: frames.time) : rule.manual
                let shot = rule.shot?.contains(frames.time) == true ? rule.shot : nil
                var view = ViewSettings()
                view.hfov = f.hfov
                view.distortion = f.distortion
                view.applyLUT = isLog
                view.bodyFromCamera = motion.bodyFromCamera(
                    at: frames.time, mode: shot?.stabilization ?? rule.stabilization,
                    smoothing: shot?.smoothing ?? rule.smoothing, lockTime: shot?.lockTime ?? rule.lockTime,
                    yaw: f.yaw, pitch: f.pitch, roll: 0)
                return renderer.renderImage(frames: frames, rig: clip.rig, view: view, width: 192, height: 108,
                                            sixteenBit: false)
            }.value
            if Task.isCancelled { return }
            if i < tiles.count { tiles[i] = image }
        }
    }
}
