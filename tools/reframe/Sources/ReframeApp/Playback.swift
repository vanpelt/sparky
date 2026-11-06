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

/// Everything the viewer shows for the selected clip, and the shot-marking actions.
@Observable
final class ViewerModel {
    var clip: Clip?
    /// Framing the user is steering by hand.
    var manual = Framing(yaw: 0, pitch: 0, hfov: 100)
    var projection: Projection = .flat
    var stabilization: Motion.Stabilization = .follow {
        didSet {
            if stabilization == .lock && oldValue != .lock { lockTime = currentTime }
            pushSettingsToShot()
        }
    }
    var smoothing = 0.8 { didSet { pushSettingsToShot() } }
    var rollingShutter = true
    var showLUT = true
    var currentTime = 0.0
    var duration = 0.0
    var isPlaying = false
    var lockTime = 0.0
    var activeShotID: Shot.ID?
    /// While true, the active shot's keyframe path drives the framing inside its range.
    var followPath = true

    @ObservationIgnored let engine = PlaybackEngine()
    @ObservationIgnored var debugCapturePath: String?
    @ObservationIgnored weak var library: Library?
    @ObservationIgnored private var applyingShot = false

    init() {
        engine.onTime = { [weak self] t in self?.currentTime = t }
        engine.onEnd = { [weak self] in self?.isPlaying = false }
    }

    var fps: Int { clip?.info.nominalFPS ?? 30 }

    var activeShot: Shot? {
        guard let id = activeShotID else { return nil }
        return library?.shots.first { $0.id == id }
    }

    func framing(at t: Double) -> Framing {
        if followPath, let s = activeShot, s.contains(t) { return s.framing(at: t) }
        return manual
    }

    var displayFraming: Framing { framing(at: currentTime) }

    /// The hand-steered framing differs from the shot's path here: offer to keyframe it.
    var hasUnkeyedFraming: Bool {
        guard !followPath, let s = activeShot, s.contains(currentTime) else { return false }
        let p = s.framing(at: currentTime)
        return abs(p.yaw - manual.yaw) > 0.5 || abs(p.pitch - manual.pitch) > 0.5 || abs(p.hfov - manual.hfov) > 0.5
    }

    /// Call before the user changes the framing by hand: start from what's on screen.
    func beginManualEdit() {
        if followPath { manual = displayFraming }
        followPath = false
    }

    // MARK: Clip and transport

    func load(_ clip: Clip?) {
        engine.stop()
        isPlaying = false
        self.clip = clip
        currentTime = 0
        duration = clip?.duration ?? 0
        activeShotID = nil
        followPath = true
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
        currentTime = min(max(t, 0), duration)
        engine.seek(to: currentTime, preview: !final)
    }

    func step(frames n: Int) {
        scrub(to: currentTime + Double(n) / Double(fps), final: true)
    }

    func resetFraming() {
        beginManualEdit()
        manual = Framing(yaw: 0, pitch: 0, hfov: 100)
    }

    /// Jump to `t` and point the camera at a world heading (from the horizon scan).
    func aim(heading: Double, at t: Double, final: Bool) {
        guard let motion = clip?.motion else { return }
        beginManualEdit()
        let base = motion.baseHeading(at: t, mode: stabilization, smoothing: smoothing, lockTime: lockTime)
        var yaw = (heading - base).truncatingRemainder(dividingBy: 360)
        if yaw > 180 { yaw -= 360 } else if yaw < -180 { yaw += 360 }
        manual.yaw = yaw
        manual.pitch = 0
        scrub(to: t, final: final)
    }

    // MARK: Shots

    func select(_ shot: Shot) {
        applyingShot = true
        activeShotID = shot.id
        followPath = true
        stabilization = shot.stabilization
        smoothing = shot.smoothing
        lockTime = shot.lockTime
        applyingShot = false
        manual = shot.framing(at: shot.inTime)
        scrub(to: shot.inTime, final: true)
    }

    func deselectShot() {
        if activeShotID != nil { manual = displayFraming }
        activeShotID = nil
        followPath = true
    }

    @discardableResult
    func newShot(from tIn: Double? = nil, to tOut: Double? = nil) -> Shot? {
        guard let clip, let library else { return nil }
        let start = tIn ?? currentTime
        let end = min(tOut ?? start + 5, duration)
        guard end - start > 0.1 else { return nil }
        let shot = Shot(clipPath: clip.url.path, name: library.nextShotName(for: clip), inTime: start, outTime: end,
                        framing: displayFraming, stabilization: stabilization, smoothing: smoothing,
                        lockTime: stabilization == .lock ? lockTime : start)
        library.add(shot)
        activeShotID = shot.id
        followPath = true
        return shot
    }

    func markIn() {
        guard var s = activeShot, currentTime < s.outTime - 0.1 else { newShot(); return }
        s.inTime = currentTime
        library?.update(s)
    }

    func markOut() {
        guard var s = activeShot, currentTime > s.inTime + 0.1 else {
            newShot(from: max(0, currentTime - 5), to: currentTime)
            return
        }
        s.outTime = currentTime
        library?.update(s)
    }

    func addKeyframe() {
        guard var s = activeShot, s.contains(currentTime) else { newShot(); return }
        s.setKeyframe(at: currentTime, framing: followPath ? displayFraming : manual)
        library?.update(s)
        followPath = true
    }

    func removeKeyframe(_ id: Keyframe.ID) {
        guard var s = activeShot else { return }
        s.removeKeyframe(id)
        library?.update(s)
    }

    func renameActiveShot(_ name: String) {
        guard var s = activeShot else { return }
        s.name = name
        library?.update(s)
    }

    func deleteActiveShot() {
        guard let id = activeShotID else { return }
        manual = displayFraming
        library?.delete(id)
        activeShotID = nil
        followPath = true
    }

    private func pushSettingsToShot() {
        guard !applyingShot, var s = activeShot else { return }
        s.stabilization = stabilization
        s.smoothing = smoothing
        s.lockTime = lockTime
        library?.update(s)
    }
}
