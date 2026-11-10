import AppKit
import MetalKit
import ReframeKit
import SwiftUI

/// Live reframed view of the selected clip. Drag to look, scroll or pinch to zoom;
/// in 360° mode, click to aim the flat camera at that point.
struct MetalViewer: NSViewRepresentable {
    let model: ViewerModel
    let renderer: ReframeRenderer

    func makeCoordinator() -> Coordinator { Coordinator(model: model, renderer: renderer) }

    func makeNSView(context: Context) -> InteractiveMTKView {
        let view = InteractiveMTKView(frame: .zero, device: renderer.device)
        view.framebufferOnly = false
        view.colorPixelFormat = .bgra8Unorm
        view.clearColor = MTLClearColor(red: 0.085, green: 0.085, blue: 0.085, alpha: 1)
        view.preferredFramesPerSecond = 60
        view.delegate = context.coordinator
        view.model = model
        return view
    }

    func updateNSView(_ view: InteractiveMTKView, context: Context) {
        view.model = model
        context.coordinator.model = model
    }

    final class Coordinator: NSObject, MTKViewDelegate {
        var model: ViewerModel
        let renderer: ReframeRenderer

        init(model: ViewerModel, renderer: ReframeRenderer) {
            self.model = model
            self.renderer = renderer
        }

        func mtkView(_ view: MTKView, drawableSizeWillChange size: CGSize) {}

        func draw(in view: MTKView) {
            // Present at the footage's own rate while playing (48 fps on a ProMotion display
            // avoids the uneven 48-in-60 cadence that reads as shake); 60 when idle for smooth dragging.
            let want = model.isPlaying ? max(model.fps, 24) : 60
            if view.preferredFramesPerSecond != want { view.preferredFramesPerSecond = want }
            guard let clip = model.clip, let frames = model.engine.latest,
                  let drawable = view.currentDrawable,
                  let cb = renderer.queue.makeCommandBuffer() else { return }
            let framing = model.framing(at: frames.time)
            var settings = ViewSettings()
            settings.projection = model.projection
            settings.hfov = framing.hfov
            settings.distortion = framing.distortion
            settings.applyLUT = model.showLUT && clip.info.colorProfile == "I_Log"
            if let motion = clip.motion {
                let t = frames.time
                settings.bodyFromCamera = motion.bodyFromCamera(
                    at: t, mode: model.stabilization, smoothing: model.smoothing, lockTime: model.lockTime,
                    yaw: framing.yaw, pitch: model.projection == .flat ? framing.pitch : 0, roll: 0)
                if model.rollingShutter {
                    settings.readout = clip.rig.readoutSeconds
                    settings.angularVelocity = motion.angularVelocity(at: t)
                    settings.rowRotations = motion.rowRotations(at: t, readout: clip.rig.readoutSeconds)
                }
            }
            if let path = model.debugCapturePath {  // debug: same view, rendered offscreen
                model.debugCapturePath = nil
                let size = drawable.texture
                if let img = renderer.renderImage(frames: frames, rig: clip.rig, view: settings, width: size.width,
                                                  height: size.height, sixteenBit: false) {
                    try? writePNG(img, to: URL(fileURLWithPath: path))
                }
            }
            let keep = renderer.encode(cb, frames: frames, rig: clip.rig, view: settings, output: drawable.texture)
            cb.addCompletedHandler { _ in _ = keep }
            cb.present(drawable)
            cb.commit()
        }
    }
}

final class InteractiveMTKView: MTKView {
    weak var model: ViewerModel?
    private var dragged = false

    override var acceptsFirstResponder: Bool { true }
    override var mouseDownCanMoveWindow: Bool { false }

    override func resetCursorRects() {
        addCursorRect(bounds, cursor: model?.projection == .equirect ? .crosshair : .openHand)
    }

    override func mouseDown(with event: NSEvent) {
        dragged = false
        window?.makeFirstResponder(self)
    }

    override func mouseDragged(with event: NSEvent) {
        guard let model, bounds.width > 0 else { return }
        if !dragged {
            dragged = true
            model.beginManualEdit()
            if model.projection == .flat { NSCursor.closedHand.set() }
        }
        let degreesPerPoint = model.projection == .flat ? model.manual.hfov / bounds.width : 360 / bounds.width
        model.manual.yaw = wrap(model.manual.yaw + event.deltaX * degreesPerPoint)
        if model.projection == .flat {
            model.manual.pitch = min(max(model.manual.pitch + event.deltaY * degreesPerPoint, -89), 89)
        }
    }

    override func mouseUp(with event: NSEvent) {
        if dragged, model?.projection == .flat { NSCursor.openHand.set() }
        guard let model, !dragged, model.projection == .equirect, bounds.width > 0 else { return }
        // Click in the 360° view: aim the flat camera there (centre of the view = current yaw).
        let p = convert(event.locationInWindow, from: nil)
        model.beginManualEdit()
        model.manual.yaw = wrap(model.manual.yaw - ((p.x / bounds.width) * 360 - 180))
        model.manual.pitch = min(max((p.y / bounds.height) * 180 - 90, -89), 89)
    }

    override func scrollWheel(with event: NSEvent) {
        guard let model else { return }
        model.beginManualEdit()
        model.manual.hfov = min(max(model.manual.hfov * exp(-event.scrollingDeltaY * 0.01), 20), 160)
    }

    override func magnify(with event: NSEvent) {
        guard let model else { return }
        model.beginManualEdit()
        model.manual.hfov = min(max(model.manual.hfov / (1 + event.magnification), 20), 160)
    }

    private func wrap(_ d: Double) -> Double {
        var x = d.truncatingRemainder(dividingBy: 360)
        if x > 180 { x -= 360 } else if x < -180 { x += 360 }
        return x
    }
}
