import Foundation
import Observation
import QuartzCore
import ReframeKit

/// Decodes frames on a background queue and exposes the latest one to the renderer.
/// Scrubbing decodes single frames from the proxy; playback streams the 8K master.
final class PlaybackEngine: @unchecked Sendable {
    private let queue = DispatchQueue(label: "reframe.playback", qos: .userInteractive)
    private let lock = NSLock()
    private var _latest: LensFrames?
    private var generation = 0
    private var master: FrameReader?
    private var proxy: FrameReader?

    var onTime: ((Double) -> Void)?
    var onEnd: (() -> Void)?

    var latest: LensFrames? { lock.withLock { _latest } }

    private func bump() -> Int { lock.withLock { generation += 1; return generation } }
    private func isCurrent(_ g: Int) -> Bool { lock.withLock { generation == g } }

    func load(clip: Clip, completion: @escaping @Sendable (Double) -> Void) {
        let g = bump()
        lock.withLock { _latest = nil; master = nil; proxy = nil }
        Task.detached(priority: .userInitiated) { [self] in
            let m = try? await FrameReader(url: clip.url)
            let p: FrameReader? = if let u = clip.proxyURL { try? await FrameReader(url: u) } else { nil }
            guard isCurrent(g) else { return }
            lock.withLock { master = m; proxy = p }
            let duration = m?.duration ?? clip.duration
            DispatchQueue.main.async { completion(duration) }
            seek(to: 0, preview: false)
        }
    }

    /// Show the frame at `t`. Rapid calls coalesce: only the newest request decodes.
    func seek(to t: Double, preview: Bool) {
        let g = bump()
        queue.async { [self] in
            guard isCurrent(g) else { return }
            let reader = lock.withLock { preview ? (proxy ?? master) : master }
            if let f = try? reader?.frame(at: t), isCurrent(g) { lock.withLock { _latest = f } }
        }
    }

    func play(from t: Double) {
        let g = bump()
        queue.async { [self] in
            guard let reader = lock.withLock({ master }), let seq = try? reader.sequence(from: t) else { return }
            defer { seq.cancel() }
            var wallStart = 0.0, firstTime = 0.0, lastReport = 0.0
            var statsStart = CACurrentMediaTime(), statsFrames = 0, late = 0
            let debug = ProcessInfo.processInfo.environment["REFRAME_DEBUG"] != nil
            while isCurrent(g) {
                if debug {
                    statsFrames += 1
                    let now = CACurrentMediaTime()
                    if now - statsStart >= 2 {
                        FileHandle.standardError.write(String(format: "playback: %.1f fps decoded, %d late\n",
                                                              Double(statsFrames) / (now - statsStart), late).data(using: .utf8)!)
                        statsStart = now; statsFrames = 0; late = 0
                    }
                }
                guard let f = seq.next() else {
                    DispatchQueue.main.async { self.onEnd?() }
                    return
                }
                let now = CACurrentMediaTime()
                if wallStart == 0 { wallStart = now; firstTime = f.time }
                let due = wallStart + (f.time - firstTime)
                if due > now { Thread.sleep(forTimeInterval: due - now) } else if now - due > 0.05 { late += 1 }
                guard isCurrent(g) else { return }
                lock.withLock { _latest = f }
                if now - lastReport > 0.05 {
                    lastReport = now
                    DispatchQueue.main.async { self.onTime?(f.time) }
                }
            }
        }
    }

    func stop() { _ = bump() }
}

/// Everything the viewer shows for the selected clip.
@Observable
final class ViewerModel {
    var clip: Clip?
    var yaw = 0.0
    var pitch = 0.0
    var hfov = 100.0
    var projection: Projection = .flat
    var stabilization: Motion.Stabilization = .follow {
        didSet { if stabilization == .lock { lockTime = currentTime } }
    }
    var smoothing = 0.8
    var rollingShutter = true
    var showLUT = true
    var currentTime = 0.0
    var duration = 0.0
    var isPlaying = false
    var lockTime = 0.0

    @ObservationIgnored let engine = PlaybackEngine()
    @ObservationIgnored var debugCapturePath: String?

    init() {
        engine.onTime = { [weak self] t in self?.currentTime = t }
        engine.onEnd = { [weak self] in self?.isPlaying = false }
    }

    func load(_ clip: Clip?) {
        engine.stop()
        isPlaying = false
        self.clip = clip
        currentTime = 0
        duration = clip?.duration ?? 0
        guard let clip else { return }
        engine.load(clip: clip) { [weak self] d in self?.duration = d }
    }

    func togglePlay() {
        guard clip != nil else { return }
        if isPlaying {
            engine.stop()
            isPlaying = false
            engine.seek(to: currentTime, preview: false)
        } else {
            if currentTime >= duration - 0.1 { currentTime = 0 }
            isPlaying = true
            engine.play(from: currentTime)
        }
    }

    func scrub(to t: Double, final: Bool) {
        if isPlaying { engine.stop(); isPlaying = false }
        currentTime = t
        engine.seek(to: t, preview: !final)
    }

    func step(frames n: Int) {
        let fps = Double(clip?.info.nominalFPS ?? 30)
        scrub(to: min(max(currentTime + Double(n) / fps, 0), duration), final: true)
    }

    func resetView() {
        yaw = 0; pitch = 0; hfov = 100
    }
}
