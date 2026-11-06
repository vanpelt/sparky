import AppKit
import Observation
import ReframeKit
import simd

/// A clip in the library plus the things we derive for browsing it.
@Observable
final class ClipItem: Identifiable {
    let clip: Clip
    var thumbnail: CGImage?
    var shake: (median: Double, p95: Double)?
    var scan: HorizonScan?
    var id: URL { clip.url }

    init(clip: Clip) { self.clip = clip }

    var isLog: Bool { clip.info.colorProfile == "I_Log" }
}

@Observable
final class Library {
    var folder: URL?
    var items: [ClipItem] = []
    var selection: URL?
    var loading = false
    var lutFolder: URL?
    var lutURLs: [URL] = []
    var activeLUT: URL?
    var shots: [Shot] = [] {
        didSet { if let folder { var s = ShotStore(); s.shots = shots; s.save(folder: folder) } }
    }
    var shotThumbnails: [Shot.ID: CGImage] = [:]
    let exports = ExportQueue()

    @ObservationIgnored let renderer: ReframeRenderer
    @ObservationIgnored private var scanTask: Task<Void, Never>?
    @ObservationIgnored private var thumbTasks: [Shot.ID: Task<Void, Never>] = [:]

    private static let folderKey = "footageFolder"
    private static let lutKey = "lutFolder"

    init() {
        renderer = try! ReframeRenderer()
        let home = FileManager.default.homeDirectoryForCurrentUser
        let lutDefault = home.appendingPathComponent("Downloads/X6_I-Log_LUTs")
        if let path = UserDefaults.standard.string(forKey: Self.lutKey) {
            setLUTFolder(URL(fileURLWithPath: path), loadNow: true)
        } else if FileManager.default.fileExists(atPath: lutDefault.path) {
            setLUTFolder(lutDefault, loadNow: true)
        }
        let footageDefault = home.appendingPathComponent("Downloads/Insta360 Download")
        if let path = UserDefaults.standard.string(forKey: Self.folderKey) {
            open(folder: URL(fileURLWithPath: path))
        } else if FileManager.default.fileExists(atPath: footageDefault.path) {
            open(folder: footageDefault)
        }
    }

    var selectedItem: ClipItem? { items.first { $0.id == selection } }

    func item(for path: String) -> ClipItem? { items.first { $0.clip.url.path == path } }

    func shots(for clip: Clip?) -> [Shot] {
        guard let clip else { return [] }
        return shots.filter { $0.clipPath == clip.url.path }.sorted { $0.inTime < $1.inTime }
    }

    /// Clips grouped by recording day, oldest first.
    var days: [(day: Date, items: [ClipItem])] {
        let cal = Calendar.current
        let groups = Dictionary(grouping: items) { cal.startOfDay(for: $0.clip.captureDate ?? .distantPast) }
        return groups.keys.sorted().map { ($0, groups[$0]!) }
    }

    // MARK: Folders and LUTs

    func chooseFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.prompt = "Open Footage"
        if panel.runModal() == .OK, let url = panel.url { open(folder: url) }
    }

    func chooseLUTFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.prompt = "Use LUTs"
        if panel.runModal() == .OK, let url = panel.url { setLUTFolder(url, loadNow: false) }
    }

    func setLUTFolder(_ url: URL, loadNow: Bool) {
        lutFolder = url
        UserDefaults.standard.set(url.path, forKey: Self.lutKey)
        lutURLs = CubeLUT.find(in: url)
        if let first = lutURLs.first { useLUT(first, synchronously: loadNow) }
    }

    func useLUT(_ url: URL, synchronously: Bool = false) {
        activeLUT = url
        if synchronously {
            if let lut = try? CubeLUT(url: url) { renderer.setLUT(lut) }
            return
        }
        Task.detached(priority: .userInitiated) { [renderer] in
            if let lut = try? CubeLUT(url: url) { renderer.setLUT(lut) }
        }
    }

    func open(folder: URL) {
        self.folder = folder
        UserDefaults.standard.set(folder.path, forKey: Self.folderKey)
        items = []
        selection = nil
        shots = ShotStore.load(folder: folder).shots
        shotThumbnails = [:]
        loading = true
        scanTask?.cancel()
        scanTask = Task.detached(priority: .userInitiated) { [weak self] in
            var loaded: [ClipItem] = []
            for url in Clip.scan(folder: folder) {
                if Task.isCancelled { return }
                guard let clip = try? Clip(url: url) else { continue }
                let item = ClipItem(clip: clip)
                loaded.append(item)
                await MainActor.run {
                    guard let self else { return }
                    self.items.append(item)
                    if self.selection == nil { self.selection = item.id }
                }
            }
            for item in loaded {
                if Task.isCancelled { return }
                await self?.prepare(item)
            }
            await MainActor.run {
                guard let self else { return }
                self.loading = false
                for shot in self.shots { self.refreshThumbnail(for: shot) }
            }
        }
    }

    /// Motion summary, a levelled 360° thumbnail and the horizon scan, from the proxy.
    private func prepare(_ item: ClipItem) async {
        let clip = item.clip
        let shake = clip.motion?.shakiness()
        var image: CGImage?
        if let reader = try? await FrameReader(url: clip.proxyURL ?? clip.url),
           let frames = try? reader.frame(at: min(clip.duration * 0.4, max(reader.duration - 0.2, 0))) {
            var view = ViewSettings()
            view.projection = .equirect
            view.applyLUT = item.isLog
            if let m = clip.motion {
                view.bodyFromCamera = m.bodyFromCamera(at: frames.time, mode: .follow, smoothing: 0, lockTime: 0,
                                                       yaw: 0, pitch: 0, roll: 0)
            }
            image = renderer.renderImage(frames: frames, rig: clip.rig, view: view, width: 640, height: 320,
                                         sixteenBit: false)
        }
        await MainActor.run {
            item.shake = shake
            item.thumbnail = image
        }
        let scan = await HorizonScan.make(clip: clip, renderer: renderer, applyLUT: item.isLog)
        await MainActor.run { item.scan = scan }
    }

    // MARK: Shots

    func add(_ shot: Shot) {
        shots.append(shot)
        refreshThumbnail(for: shot)
    }

    func update(_ shot: Shot) {
        guard let i = shots.firstIndex(where: { $0.id == shot.id }) else { return }
        let old = shots[i]
        shots[i] = shot
        if old.inTime != shot.inTime || old.keyframes.first != shot.keyframes.first
            || old.stabilization != shot.stabilization { refreshThumbnail(for: shot) }
    }

    func delete(_ id: Shot.ID) {
        shots.removeAll { $0.id == id }
        shotThumbnails[id] = nil
    }

    func nextShotName(for clip: Clip) -> String {
        let taken = Set(shots(for: clip).map(\.name))
        var n = shots(for: clip).count + 1
        while taken.contains("Shot \(n)") { n += 1 }
        return "Shot \(n)"
    }

    /// Frame at the shot's in-point, framed as the shot, from the proxy.
    func refreshThumbnail(for shot: Shot) {
        guard let item = item(for: shot.clipPath) else { return }
        thumbTasks[shot.id]?.cancel()
        thumbTasks[shot.id] = Task.detached(priority: .utility) { [weak self, renderer] in
            try? await Task.sleep(for: .milliseconds(250))  // coalesce rapid edits
            if Task.isCancelled { return }
            let clip = item.clip
            guard let reader = try? await FrameReader(url: clip.proxyURL ?? clip.url),
                  let frames = try? reader.frame(at: shot.inTime), let motion = clip.motion else { return }
            let f = shot.framing(at: frames.time)
            var view = ViewSettings()
            view.hfov = f.hfov
            view.applyLUT = item.isLog
            view.bodyFromCamera = motion.bodyFromCamera(at: frames.time, mode: shot.stabilization, smoothing: shot.smoothing,
                                                        lockTime: shot.lockTime, yaw: f.yaw, pitch: f.pitch, roll: 0)
            let image = renderer.renderImage(frames: frames, rig: clip.rig, view: view, width: 320, height: 180,
                                             sixteenBit: false)
            await MainActor.run { self?.shotThumbnails[shot.id] = image }
        }
    }
}

// MARK: - Export queue

@Observable
final class ExportQueue {
    enum Status: Equatable {
        case queued, running(Double), done(URL), failed(String), cancelled
    }

    struct Job: Identifiable {
        let id: Shot.ID
        let name: String
        var status: Status = .queued
    }

    var jobs: [Job] = []
    var running = false
    var timelineURLs: [URL] = []
    var destination: URL?

    @ObservationIgnored private var task: Task<Void, Never>?
    @ObservationIgnored private let cancelFlag = CancelFlag()

    var finished: Int { jobs.filter { if case .done = $0.status { return true }; return false }.count }

    var overallProgress: Double {
        guard !jobs.isEmpty else { return 0 }
        let sum = jobs.reduce(0.0) { acc, j in
            switch j.status {
            case .running(let p): return acc + p
            case .done, .failed, .cancelled: return acc + 1
            case .queued: return acc
            }
        }
        return sum / Double(jobs.count)
    }

    struct Request {
        var shots: [(Shot, Clip)]
        var options: ExportOptions
        var lut: CubeLUT?
        var resolveTimeline: Bool
        var finalCutTimeline: Bool
        var title: String
    }

    /// Export names: recording time plus shot name, unique within the batch
    /// ("2026-10-23 0941 Shot 1"), so files from different clips never collide.
    static func exportNames(_ shots: [(Shot, Clip)]) -> [String] {
        let f = DateFormatter()
        f.dateFormat = "yyyy-MM-dd HHmm"
        var used: [String: Int] = [:]
        return shots.map { shot, clip in
            let base = "\(clip.captureDate.map(f.string(from:)) ?? clip.name) \(shot.name)"
            used[base, default: 0] += 1
            return used[base]! == 1 ? base : "\(base) (\(used[base]!))"
        }
    }

    func start(_ request: Request) {
        guard !running else { return }
        var request = request
        let names = Self.exportNames(request.shots)
        request.shots = zip(request.shots, names).map { pair, name in
            var s = pair.0
            s.name = name
            return (s, pair.1)
        }
        jobs = request.shots.map { Job(id: $0.0.id, name: $0.0.name) }
        timelineURLs = []
        destination = request.options.destination
        running = true
        cancelFlag.value = false
        let flag = cancelFlag
        task = Task.detached(priority: .userInitiated) { [weak self] in
            var results: [ExportedShot] = []
            guard let exporter = try? ShotExporter(lut: request.lut) else { return }
            for (i, (shot, clip)) in request.shots.enumerated() {
                if flag.value { await self?.set(i, .cancelled); continue }
                await self?.set(i, .running(0))
                do {
                    let r = try await exporter.export(shot, clip: clip, options: request.options, progress: { p in
                        Task { @MainActor in self?.set(i, .running(p)) }
                    }, isCancelled: { flag.value })
                    results.append(r)
                    await self?.set(i, .done(r.url))
                } catch is CancellationError {
                    await self?.set(i, .cancelled)
                } catch {
                    await self?.set(i, .failed(error.localizedDescription))
                }
            }
            var written: [URL] = []
            if !results.isEmpty {
                let dir = request.options.destination
                if request.resolveTimeline, let data = try? Interchange.otio(results, title: request.title) {
                    let u = dir.appendingPathComponent("\(request.title).otio")
                    if (try? data.write(to: u)) != nil { written.append(u) }
                }
                if request.finalCutTimeline {
                    let u = dir.appendingPathComponent("\(request.title).fcpxml")
                    if (try? Interchange.fcpxml(results, title: request.title).write(to: u, atomically: true, encoding: .utf8)) != nil {
                        written.append(u)
                    }
                }
            }
            await MainActor.run {
                self?.timelineURLs = written
                self?.running = false
            }
        }
    }

    @MainActor private func set(_ i: Int, _ s: Status) {
        guard jobs.indices.contains(i) else { return }
        if case .running = jobs[i].status, case .running = s {} else if jobs[i].status == s { return }
        jobs[i].status = s
    }

    func cancel() { cancelFlag.value = true }

    func reset() {
        guard !running else { return }
        jobs = []
        timelineURLs = []
    }
}

final class CancelFlag: @unchecked Sendable {
    private let lock = NSLock()
    private var _value = false
    var value: Bool {
        get { lock.withLock { _value } }
        set { lock.withLock { _value = newValue } }
    }
}
