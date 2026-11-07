import AVFoundation
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
    /// Audio-only player (the clip's sound track in a composition, so no video is decoded twice).
    /// While playing it is the master clock and video frames are paced to it.
    private var player: AVPlayer?
    var muted = false { didSet { lock.withLock { player }?.isMuted = muted } }

    var onTime: ((Double) -> Void)?
    var onEnd: (() -> Void)?

    var latest: LensFrames? { lock.withLock { _latest } }

    private func bump() -> Int { lock.withLock { generation += 1; return generation } }
    private func isCurrent(_ g: Int) -> Bool { lock.withLock { generation == g } }

    func load(clip: Clip, completion: @escaping @Sendable (Double) -> Void) {
        let g = bump()
        lock.withLock { _latest = nil; master = nil; proxy = nil; player?.pause(); player = nil }
        Task.detached(priority: .userInitiated) { [self] in
            let p: FrameReader? = if let u = clip.proxyURL { try? await FrameReader(url: u) } else { nil }
            // A cloud clip whose original isn't downloaded plays (and sounds) from its LRV.
            let m = CloudCache.isStub(clip.url) ? p : try? await FrameReader(url: clip.url)
            var audio: AVPlayer?
            if let m, let track = try? await m.asset.loadTracks(withMediaType: .audio).first,
               let duration = try? await m.asset.load(.duration) {
                let comp = AVMutableComposition()
                if let ct = comp.addMutableTrack(withMediaType: .audio, preferredTrackID: kCMPersistentTrackID_Invalid),
                   (try? ct.insertTimeRange(CMTimeRange(start: .zero, duration: duration), of: track, at: .zero)) != nil {
                    let player = AVPlayer(playerItem: AVPlayerItem(asset: comp))
                    player.automaticallyWaitsToMinimizeStalling = false
                    player.isMuted = muted
                    audio = player
                }
            }
            guard isCurrent(g) else { return }
            lock.withLock { master = m; proxy = p; player = audio }
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
            let audio = lock.withLock { player }
            if let audio {  // park the audio exactly at t before anything starts
                audio.pause()
                let ready = DispatchSemaphore(value: 0)
                audio.seek(to: CMTime(seconds: t, preferredTimescale: 48000), toleranceBefore: .zero,
                           toleranceAfter: .zero) { _ in ready.signal() }
                _ = ready.wait(timeout: .now() + 1)
            }
            var audioStarted = false
            var wallStart = 0.0, firstTime = 0.0, lastReport = 0.0
            var statsStart = CACurrentMediaTime(), statsFrames = 0, late = 0
            let debug = ProcessInfo.processInfo.environment["REFRAME_DEBUG"] != nil
            while isCurrent(g) {
                if debug {
                    statsFrames += 1
                    let now = CACurrentMediaTime()
                    if now - statsStart >= 2 {
                        let a = audio.map { String(format: " audio clock %.2fs rate %.1f", $0.currentTime().seconds, $0.rate) } ?? " no audio"
                        FileHandle.standardError.write(String(format: "playback: %.1f fps, %d late, video %.2fs%@\n",
                                                              Double(statsFrames) / (now - statsStart), late,
                                                              lock.withLock { _latest?.time ?? 0 }, a).data(using: .utf8)!)
                        statsStart = now; statsFrames = 0; late = 0
                    }
                }
                guard let f = seq.next() else {
                    audio?.pause()
                    DispatchQueue.main.async { self.onEnd?() }
                    return
                }
                let now = CACurrentMediaTime()
                if wallStart == 0 { wallStart = now; firstTime = f.time }
                if let audio {
                    // Audio is the clock: start it with the first frame, then hold each frame until
                    // its time comes up; drop frames if decoding ever falls behind.
                    if !audioStarted { audio.play(); audioStarted = true }
                    var clock = audio.currentTime().seconds
                    while clock < f.time - 0.002, isCurrent(g) {
                        Thread.sleep(forTimeInterval: min(f.time - clock, 0.004))
                        clock = audio.currentTime().seconds
                    }
                    if clock - f.time > 0.1 { late += 1; continue }
                } else {
                    let due = wallStart + (f.time - firstTime)
                    if due > now { Thread.sleep(forTimeInterval: due - now) } else if now - due > 0.05 { late += 1 }
                }
                guard isCurrent(g) else { return }
                lock.withLock { _latest = f }
                if now - lastReport > 0.05 {
                    lastReport = now
                    DispatchQueue.main.async { self.onTime?(f.time) }
                }
            }
        }
    }

    func stop() {
        _ = bump()
        lock.withLock { player }?.pause()
    }
}

/// Everything the viewer shows for the selected clip, and the shot-marking actions.
@Observable
final class ViewerModel {
    var clip: Clip?
    /// The framing being edited. Outside shots it is the free view; inside the active shot, every
    /// change to it is written to the keyframe at the playhead (added if there isn't one).
    var manual = LensPreset.dewarp.framing { didSet { writeEditToKeyframe() } }
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
    var muted = UserDefaults.standard.bool(forKey: "muted") {
        didSet { engine.muted = muted; UserDefaults.standard.set(muted, forKey: "muted") }
    }
    var currentTime = 0.0 { didSet { if currentTime != oldValue { editing = false } } }
    var duration = 0.0
    var isPlaying = false
    var lockTime = 0.0
    var activeShotID: Shot.ID? { didSet { editing = false } }
    /// The suggested (ghost) shot being previewed, if any, and its path as a shot.
    var selectedSuggestionID: String?
    var previewShot: Shot?

    @ObservationIgnored let engine = PlaybackEngine()
    let filmstrip = Filmstrip()
    @ObservationIgnored var debugCapturePath: String?
    @ObservationIgnored weak var library: Library?
    @ObservationIgnored private var applyingShot = false
    /// A view edit is in progress at the playhead inside the active shot (reset when either moves).
    @ObservationIgnored private var editing = false

    init() {
        engine.onTime = { [weak self] t in self?.currentTime = t }
        engine.onEnd = { [weak self] in self?.isPlaying = false }
        engine.muted = muted
    }

    var fps: Int { clip?.info.nominalFPS ?? 30 }

    var activeShot: Shot? {
        guard let id = activeShotID else { return nil }
        return library?.shots.first { $0.id == id }
    }

    /// The shot whose path drives the framing: the active shot, or the suggestion being previewed.
    var editingShot: Shot? { activeShot ?? previewShot }

    var suggestions: [Suggestion] { library?.suggestions(for: clip) ?? [] }

    var selectedSuggestion: Suggestion? {
        guard let id = selectedSuggestionID else { return nil }
        return suggestions.first { $0.id == id }
    }

    /// What A and X act on: the selected suggestion, or the one under the playhead.
    var targetSuggestion: Suggestion? {
        selectedSuggestion ?? suggestions.first { $0.inTime <= currentTime && currentTime <= $0.outTime }
    }

    func framing(at t: Double) -> Framing {
        if let s = editingShot, s.contains(t) { return s.framing(at: t) }
        return manual
    }

    var displayFraming: Framing { framing(at: currentTime) }

    /// Timeline scale, set by the heading lane (seconds per point), so keyframe spacing can be
    /// judged on screen.
    @ObservationIgnored var secondsPerPoint = 0.03

    /// Keyframes closer than this would draw on top of each other: about 8 points in the lane, and
    /// never less than a frame. Edits within it land on the existing keyframe.
    var keyframeSpacing: Double { max(1 / Double(max(fps, 1)), secondsPerPoint * 8) }

    /// The active shot's keyframe at the playhead (the nearest within `keyframeSpacing`): the
    /// selected keyframe. View edits change it.
    var currentKeyframe: Keyframe? {
        guard let s = activeShot else { return nil }
        return s.keyframes.filter { abs($0.time - currentTime) <= keyframeSpacing }
            .min { abs($0.time - currentTime) < abs($1.time - currentTime) }
    }

    /// Put the playhead exactly on the keyframe it is next to, so what you see is what you edit.
    private func snapToCurrentKeyframe() {
        if let k = currentKeyframe, k.time != currentTime { scrub(to: k.time, final: true) }
    }

    /// Changing the view here would edit (or add) a keyframe of the active shot.
    var editsKeyframe: Bool { (activeShot ?? previewShot)?.contains(currentTime) ?? false }

    /// Call before the user changes the framing. Inside a shot the edit lands on the keyframe at
    /// the playhead (a previewed suggestion is kept first); outside, it moves the free view.
    func beginManualEdit() {
        if activeShot == nil, let p = previewShot, p.contains(currentTime) { acceptSuggestion(seek: false) }
        guard activeShot?.contains(currentTime) == true else {
            if !editing { manual = displayFraming }
            return
        }
        if !editing { snapToCurrentKeyframe() }
        guard let s = activeShot else { return }
        if !editing {
            manual = s.framing(at: currentKeyframe?.time ?? currentTime)  // start from what's on screen
            editing = true
        }
    }

    private func writeEditToKeyframe() {
        guard editing, var s = activeShot, s.contains(currentTime) else { return }
        s.setKeyframe(at: currentKeyframe?.time ?? currentTime, framing: manual)
        library?.update(s)
    }

    // MARK: Clip and transport

    func load(_ clip: Clip?) {
        engine.stop()
        isPlaying = false
        self.clip = clip
        currentTime = 0
        duration = clip?.duration ?? 0
        activeShotID = nil
        selectedSuggestionID = nil
        previewShot = nil
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
        manual = LensPreset.dewarp.framing
    }

    /// Jump to `t` and point the camera at a world heading (from the heading lane).
    /// Inside a shot this keyframes the new aim at `t`.
    func aim(heading: Double, at t: Double, final: Bool) {
        guard let motion = clip?.motion else { return }
        scrub(to: t, final: final)
        beginManualEdit()
        let base = motion.baseHeading(at: currentTime, mode: stabilization, smoothing: smoothing, lockTime: lockTime)
        var yaw = (heading - base).truncatingRemainder(dividingBy: 360)
        if yaw > 180 { yaw -= 360 } else if yaw < -180 { yaw += 360 }
        var f = manual
        f.yaw = yaw
        f.pitch = 0
        manual = f
    }

    /// Switch lens preset, keeping where the camera points.
    func apply(_ preset: LensPreset) {
        beginManualEdit()
        manual.apply(preset)
    }

    // MARK: Shots

    func select(_ shot: Shot, seek: Bool = true) {
        selectedSuggestionID = nil
        previewShot = nil
        applyingShot = true
        activeShotID = shot.id
        stabilization = shot.stabilization
        smoothing = shot.smoothing
        lockTime = shot.lockTime
        applyingShot = false
        manual = shot.framing(at: seek ? shot.inTime : currentTime)
        if seek { scrub(to: shot.inTime, final: true) }
    }

    func deselectShot() {
        if activeShotID != nil { manual = displayFraming }
        activeShotID = nil
        selectedSuggestionID = nil
        previewShot = nil
    }

    // MARK: Suggestions

    /// Preview a suggestion: jump to it and look along its suggested camera path.
    func select(_ s: Suggestion) {
        guard let clip else { return }
        deselectShot()
        let shot = s.shot(clipPath: clip.url.path, name: s.title)
        selectedSuggestionID = s.id
        previewShot = shot
        applyingShot = true
        stabilization = shot.stabilization
        smoothing = shot.smoothing
        lockTime = shot.lockTime
        applyingShot = false
        manual = shot.framing(at: s.inTime)
        scrub(to: s.inTime, final: true)
    }

    /// Keep the targeted suggestion (A): it becomes a normal shot, selected for editing.
    func acceptSuggestion(seek: Bool = true) {
        guard let s = targetSuggestion, let clip, let library else { return }
        let shot = library.accept(s, clip: clip)
        select(shot, seek: seek)
    }

    /// Dismiss the targeted suggestion (X); it won't come back for this folder.
    func dismissSuggestion() {
        guard let s = targetSuggestion else { return }
        library?.dismiss(s.id)
        if selectedSuggestionID == s.id { selectedSuggestionID = nil; previewShot = nil }
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
        selectedSuggestionID = nil
        previewShot = nil
        activeShotID = shot.id
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

    /// K: pin the current view as a keyframe at the playhead (keeping a previewed suggestion
    /// first). Outside any shot, starts one.
    func addKeyframe() {
        if activeShot == nil, let p = previewShot, p.contains(currentTime) { acceptSuggestion(seek: false) }
        guard var s = activeShot, s.contains(currentTime) else { newShot(); return }
        if currentKeyframe != nil {
            snapToCurrentKeyframe()  // one is already here: select it rather than stack another
        } else {
            s.setKeyframe(at: currentTime, framing: displayFraming)
            library?.update(s)
        }
    }

    func removeKeyframe(_ id: Keyframe.ID) {
        guard var s = activeShot else { return }
        s.removeKeyframe(id)
        library?.update(s)
    }

    /// Delete: remove the keyframe at the playhead (a shot keeps at least one).
    @discardableResult
    func removeCurrentKeyframe() -> Bool {
        guard let k = currentKeyframe, (activeShot?.keyframes.count ?? 0) > 1 else { return false }
        removeKeyframe(k.id)
        return true
    }

    /// Move a keyframe in time (between its neighbours, inside the shot); the playhead follows it.
    func moveKeyframe(_ id: Keyframe.ID, to t: Double, final: Bool) {
        guard var s = activeShot, let i = s.keyframes.firstIndex(where: { $0.id == id }) else { return }
        let gap = keyframeSpacing
        let lo = i > 0 ? s.keyframes[i - 1].time + gap : s.inTime
        let hi = i < s.keyframes.count - 1 ? s.keyframes[i + 1].time - gap : s.outTime
        guard lo <= hi else { return }
        s.keyframes[i].time = min(max(t, lo), hi)
        library?.update(s)
        scrub(to: s.keyframes[i].time, final: final)
    }

    /// Drag a shot's in or out point. Keyframes stay where they are in the clip.
    func moveEdge(of id: Shot.ID, isIn: Bool, to t: Double, final: Bool) {
        guard var s = library?.shots.first(where: { $0.id == id }) else { return }
        if activeShotID != id { select(s, seek: false) }
        let t = min(max(t, 0), duration)
        if isIn { s.inTime = min(t, s.outTime - 0.5) } else { s.outTime = max(t, s.inTime + 0.5) }
        library?.update(s)
        scrub(to: isIn ? s.inTime : s.outTime, final: final)
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
    }

    private func pushSettingsToShot() {
        guard !applyingShot, var s = activeShot else { return }
        s.stabilization = stabilization
        s.smoothing = smoothing
        s.lockTime = lockTime
        library?.update(s)
    }
}
