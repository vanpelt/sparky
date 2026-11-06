import AppKit
import MetalKit
import ReframeKit
import SwiftUI

/// Live reframed view of the selected clip. Drag to look, scroll or pinch to zoom;
/// in 360 mode, click to aim the flat camera at that point.
struct MetalViewer: NSViewRepresentable {
    let model: ViewerModel
    let renderer: ReframeRenderer

    func makeCoordinator() -> Coordinator { Coordinator(model: model, renderer: renderer) }

    func makeNSView(context: Context) -> InteractiveMTKView {
        let view = InteractiveMTKView(frame: .zero, device: renderer.device)
        view.framebufferOnly = false
        view.colorPixelFormat = .bgra8Unorm
        view.clearColor = MTLClearColor(red: 0.05, green: 0.05, blue: 0.06, alpha: 1)
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
            guard let clip = model.clip, let frames = model.engine.latest,
                  let drawable = view.currentDrawable,
                  let cb = renderer.queue.makeCommandBuffer() else { return }
            var settings = ViewSettings()
            settings.projection = model.projection
            settings.hfov = model.hfov
            settings.applyLUT = model.showLUT && clip.info.colorProfile == "I_Log"
            if let motion = clip.motion {
                let t = frames.time
                settings.bodyFromCamera = motion.bodyFromCamera(
                    at: t, mode: model.stabilization, smoothing: model.smoothing, lockTime: model.lockTime,
                    yaw: model.yaw, pitch: model.projection == .flat ? model.pitch : 0, roll: 0)
                if model.rollingShutter {
                    settings.readout = clip.rig.readoutSeconds
                    settings.angularVelocity = motion.angularVelocity(at: t)
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

    override func mouseDown(with event: NSEvent) {
        dragged = false
        window?.makeFirstResponder(self)
    }

    override func mouseDragged(with event: NSEvent) {
        guard let model, bounds.width > 0 else { return }
        dragged = true
        let degreesPerPoint = model.projection == .flat ? model.hfov / bounds.width : 360 / bounds.width
        model.yaw = wrap(model.yaw + event.deltaX * degreesPerPoint)
        if model.projection == .flat {
            model.pitch = min(max(model.pitch + event.deltaY * degreesPerPoint, -89), 89)
        }
    }

    override func mouseUp(with event: NSEvent) {
        guard let model, !dragged, model.projection == .equirect, bounds.width > 0 else { return }
        // Click in the 360 view: aim the flat camera there (centre of the 360 view = current yaw).
        let p = convert(event.locationInWindow, from: nil)
        let lon = (p.x / bounds.width) * 360 - 180
        let lat = (p.y / bounds.height) * 180 - 90
        model.yaw = wrap(model.yaw - lon)
        model.pitch = min(max(lat, -89), 89)
    }

    override func scrollWheel(with event: NSEvent) {
        guard let model else { return }
        model.hfov = min(max(model.hfov * exp(-event.scrollingDeltaY * 0.01), 20), 160)
    }

    override func magnify(with event: NSEvent) {
        guard let model else { return }
        model.hfov = min(max(model.hfov / (1 + event.magnification), 20), 160)
    }

    override func keyDown(with event: NSEvent) {
        guard let model else { return super.keyDown(with: event) }
        switch event.keyCode {
        case 49: model.togglePlay()  // space
        case 123: model.step(frames: -1)  // left
        case 124: model.step(frames: 1)  // right
        default: super.keyDown(with: event)
        }
    }

    private func wrap(_ d: Double) -> Double {
        var x = d.truncatingRemainder(dividingBy: 360)
        if x > 180 { x -= 360 } else if x < -180 { x += 360 }
        return x
    }
}
