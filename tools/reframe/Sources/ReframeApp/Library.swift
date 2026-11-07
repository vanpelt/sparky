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
    /// Gyro speed per time bin across the clip, 0...1 (1 = 180°/s or more).
    var shakeProfile: [Double] = []
    /// Interest analysis (INTEREST_MAP.md), once it has run.
    var analysis: ClipAnalysis?
    /// Fused interest, 0...1, for the timeline and sidebar.
    var interest: [Float] = []
    /// The heading lane's heat: time × heading as white with alpha.
    var heat: CGImage?
    /// A recording from Insta360 Cloud (cached under CloudCache.root).
    let isCloud: Bool
    /// Its original is still only in the cloud (we hold the trailer and the LRV).
    var originalInCloud: Bool
    var id: URL { clip.url }

    init(clip: Clip) {
        self.clip = clip
        isCloud = CloudCache.isCloud(clip.url)
        originalInCloud = isCloud && CloudCache.isStub(clip.url)
    }

    var isLog: Bool { clip.info.colorProfile == "I_Log" }
}

@Observable
final class Library {
    var folder: URL?
    var items: [ClipItem] = []
    var selection: URL? { didSet { if selection != oldValue { fetchSelectedCloudClip() } } }
    /// Which browser fills the window: clips (the editor) or every shot as a contact sheet.
    enum Browse: String { case clips, shots }
    var browse: Browse = .clips
    /// A shot to select once its clip is in the viewer ("Open in Editor" from the shots view).
    var shotToOpen: Shot.ID?
    /// Cached cloud clips are still being added at launch.
    var loadingCloud = false
    /// Frames across each shot for hover-scrubbing in the shots view (rendered on first hover).
    var shotFrames: [Shot.ID: [CGImage]] = [:]
    @ObservationIgnored private var shotFrameSource: [Shot.ID: Shot] = [:]
    @ObservationIgnored private var shotFrameTasks: Set<Shot.ID> = []
    /// Insta360 Cloud: its recordings join the library once fetched (trailer + LRV).
    let cloud: CloudLibrary = MainActor.assumeIsolated { CloudLibrary() }
    var loading = false
    var lutFolder: URL?
    var lutURLs: [URL] = []
    var activeLUT: URL?
    var shots: [Shot] = [] { didSet { save() } }
    /// Suggestions the user dismissed (by ID).
    var dismissed: Set<String> = [] { didSet { save() } }
    var shotThumbnails: [Shot.ID: CGImage] = [:]
    var suggestionThumbnails: [String: CGImage] = [:]
    /// What suggestions favour (signal weights). Changing it re-ranks every clip from cached
    /// analysis, without re-analyzing.
    private(set) var preset = InterestWeights.Preset(rawValue: UserDefaults.standard.string(forKey: "interestPreset") ?? "")
        ?? .blend
    var weights: InterestWeights { .preset(preset) }

    /// Order clips within each day by how interesting they are, not when they were recorded.
    var sortByInterest = UserDefaults.standard.bool(forKey: "sortByInterest") {
        didSet { UserDefaults.standard.set(sortByInterest, forKey: "sortByInterest") }
    }
    let exports = ExportQueue()

    @ObservationIgnored let renderer: ReframeRenderer
    @ObservationIgnored private var scanTask: Task<Void, Never>?
    @ObservationIgnored private var thumbTasks: [Shot.ID: Task<Void, Never>] = [:]
    @ObservationIgnored private var loadingStore = false
    @ObservationIgnored private var saveWork: DispatchWorkItem?

    // Undo: snapshots of shots and dismissals. Rapid edits to one shot (a drag) coalesce.
    private struct Snapshot { var shots: [Shot]; var dismissed: Set<String> }
    @ObservationIgnored private var undoStack: [Snapshot] = []
    @ObservationIgnored private var redoStack: [Snapshot] = []
    @ObservationIgnored private var lastUndo: (key: Shot.ID?, at: Date)?
    var canUndo = false
    var canRedo = false

    private static let folderKey = "footageFolder"
    private static let lutKey = "lutFolder"

    init() {
        renderer = try! ReframeRenderer()
        let home = FileManager.default.homeDirectoryForCurrentUser
        // ~/Movies, not ~/Downloads: macOS asks for permission to read Downloads after every
        // rebuild of an unsigned app, which blocks launch until someone clicks Allow.
        let lutDefault = home.appendingPathComponent("Movies/X6_I-Log_LUTs")
        if let path = UserDefaults.standard.string(forKey: Self.lutKey) {
            setLUTFolder(URL(fileURLWithPath: path), loadNow: true)
        } else if FileManager.default.fileExists(atPath: lutDefault.path) {
            setLUTFolder(lutDefault, loadNow: true)
        }
        // A folder opens only if you chose one (Insta360 Cloud needs none).
        if let path = UserDefaults.standard.string(forKey: Self.folderKey) {
            open(folder: URL(fileURLWithPath: path))
        } else {
            loadShots()
        }
        loadCloudClips()
        MainActor.assumeIsolated { cloud.start() }
    }

    var selectedItem: ClipItem? { items.first { $0.id == selection } }

    func item(for path: String) -> ClipItem? { items.first { $0.clip.url.path == path } }

    func shots(for clip: Clip?) -> [Shot] {
        guard let clip else { return [] }
        return shots.filter { $0.clipPath == clip.url.path }.sorted { $0.inTime < $1.inTime }
    }

    /// Saves shortly after the last change, so a drag that edits a keyframe every mouse event
    /// writes the file once.
    private func save() {
        guard !loadingStore else { return }
        saveWork?.cancel()
        let folder = folder
        let work = DispatchWorkItem { [weak self] in
            guard let self else { return }
            // Cloud clips' shots live in their own store, so they follow you across folders.
            // Dismissals are few and keyed by clip name: both stores keep them all.
            let cloudShots = self.shots.filter { CloudCache.isCloud(URL(fileURLWithPath: $0.clipPath)) }
            var c = ShotStore()
            c.shots = cloudShots
            c.dismissed = self.dismissed
            c.save(folder: CloudCache.root)
            if let folder {
                var s = ShotStore()
                s.shots = self.shots.filter { !CloudCache.isCloud(URL(fileURLWithPath: $0.clipPath)) }
                s.dismissed = self.dismissed
                s.save(folder: folder)
            }
        }
        saveWork = work
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.4, execute: work)
    }

    // MARK: Undo

    /// Call before changing shots. Edits with the same key within a second form one undo step.
    private func recordUndo(key: Shot.ID? = nil) {
        let now = Date()
        if let key, let last = lastUndo, last.key == key, now.timeIntervalSince(last.at) < 1 {
            lastUndo = (key, now)
            return
        }
        undoStack.append(Snapshot(shots: shots, dismissed: dismissed))
        if undoStack.count > 200 { undoStack.removeFirst() }
        redoStack = []
        lastUndo = (key, now)
        canUndo = true; canRedo = false
    }

    func undo() {
        guard let snap = undoStack.popLast() else { return }
        redoStack.append(Snapshot(shots: shots, dismissed: dismissed))
        restore(snap)
    }

    func redo() {
        guard let snap = redoStack.popLast() else { return }
        undoStack.append(Snapshot(shots: shots, dismissed: dismissed))
        restore(snap)
    }

    private func restore(_ snap: Snapshot) {
        let changed = Set(snap.shots.filter { s in !shots.contains(s) }.map(\.id))
        shots = snap.shots
        dismissed = snap.dismissed
        lastUndo = nil
        canUndo = !undoStack.isEmpty
        canRedo = !redoStack.isEmpty
        for shot in shots where changed.contains(shot.id) { refreshThumbnail(for: shot) }
    }

    // MARK: Folders and LUTs

    func chooseFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.prompt = "Open Footage"
        if panel.runModal() == .OK, let url = panel.url { open(folder: url) }
    }

    /// Back to no folder: only cloud recordings (and their shots) stay in the library.
    func closeFolder() {
        guard folder != nil else { return }
        scanTask?.cancel()
        folder = nil
        UserDefaults.standard.removeObject(forKey: Self.folderKey)
        if let s = selection, item(for: s.path)?.isCloud != true { selection = nil }
        items = items.filter(\.isCloud)
        loading = false
        loadShots()
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
        items = items.filter(\.isCloud)
        selection = nil
        loadShots()
        shotThumbnails = [:]
        suggestionThumbnails = [:]
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
            await self?.pruneAnalysisCache()
            await MainActor.run {
                guard let self else { return }
                self.loading = false
                for shot in self.shots { self.refreshThumbnail(for: shot) }
            }
            // Interest analysis for clips not analyzed yet (cached ones were picked up above).
            for item in loaded where await MainActor.run(body: { item.analysis == nil }) {
                if Task.isCancelled { return }
                await self?.analyze(item)
            }
        }
    }

    /// Drop cached analyses of older versions of the loaded clips' files.
    private func pruneAnalysisCache() async {
        let clips = await MainActor.run { items.map(\.clip) }
        let r = Analyzer.pruneCache(keeping: clips)
        if r.files > 0, ProcessInfo.processInfo.environment["REFRAME_DEBUG"] != nil {
            FileHandle.standardError.write("pruned \(r.files) stale analyses (\(r.bytes / 1024) KB)\n".data(using: .utf8)!)
        }
    }

    /// Shots for the open folder plus the cloud clips' shots.
    private func loadShots() {
        let store = folder.map { ShotStore.load(folder: $0) } ?? ShotStore()
        let cloudStore = ShotStore.load(folder: CloudCache.root)
        undoStack = []; redoStack = []; canUndo = false; canRedo = false
        loadingStore = true
        let loaded = store.shots + cloudStore.shots
        // Keyframes stacked closer than 0.1 s (an earlier bug) draw as one: merge them.
        shots = loaded.map { var s = $0; s.mergeCloseKeyframes(within: 0.1); return s }
        dismissed = store.dismissed.union(cloudStore.dismissed)
        loadingStore = false
        if shots != loaded { save() }
    }

    // MARK: Cloud

    /// Cloud clips fetched earlier open straight from the cache (offline too): all of them join
    /// the library first, then get thumbnails, then analysis (which can take a while).
    private func loadCloudClips() {
        loadingCloud = true
        Task.detached(priority: .userInitiated) { [weak self] in
            var added: [ClipItem] = []
            for url in CloudCache.readyClips() {
                guard let clip = try? Clip(url: url) else { continue }
                let item = ClipItem(clip: clip)
                added.append(item)
                await MainActor.run {
                    guard let self, !self.items.contains(where: { $0.id == url }) else { return }
                    self.items.append(item)
                }
            }
            await MainActor.run { self?.loadingCloud = false }
            await self?.pruneAnalysisCache()
            for item in added {
                await self?.prepare(item)
                await MainActor.run {
                    guard let self else { return }
                    for shot in self.shots where shot.clipPath == item.id.path { self.refreshThumbnail(for: shot) }
                }
            }
            for item in added where await MainActor.run(body: { item.analysis == nil }) {
                await self?.analyze(item)
            }
        }
    }

    /// Turn a fetched cloud recording into a library item and prepare it like a local one.
    @discardableResult
    func addCloudClip(_ url: URL, select: Bool) async -> ClipItem? {
        if let existing = await MainActor.run(body: { items.first { $0.id == url } }) { return existing }
        guard let clip = try? Clip(url: url) else { return nil }
        let item = ClipItem(clip: clip)
        await MainActor.run {
            guard !items.contains(where: { $0.id == url }) else { return }
            items.append(item)
            if select { selection = item.id }
        }
        await prepare(item)
        await MainActor.run { for shot in shots where shot.clipPath == url.path { refreshThumbnail(for: shot) } }
        await analyze(item)
        return item
    }

    func retryCloudFetch() { fetchSelectedCloudClip() }

    /// Selecting a cloud recording that isn't here yet fetches it (trailer, then LRV).
    private func fetchSelectedCloudClip() {
        guard let url = selection, item(for: url.path) == nil else { return }
        MainActor.assumeIsolated {
            guard let m = cloud.media(for: url) else { return }
            Task { [weak self] in
                guard let self, let fetched = await self.makeAvailable(m) else { return }
                await self.addCloudClip(fetched, select: false)
            }
        }
    }

    /// A cloud recording's trailer and LRV, from a local copy in the open folder when there is one
    /// (no download), else from the cloud.
    @MainActor private func makeAvailable(_ m: CloudMedia) async -> URL? {
        if let local = items.first(where: { !$0.isCloud && $0.clip.url.lastPathComponent == m.name }),
           let url = try? CloudCache.seed(m, fromLocal: local.clip.url) {
            return url
        }
        return await cloud.fetch(m)
    }

    /// Fetch every cloud recording not here yet, one at a time.
    func fetchAllCloudClips() {
        MainActor.assumeIsolated {
            let pending = days.flatMap(\.rows).compactMap { $0.item == nil ? $0.media : nil }
            Task { [weak self] in
                for m in pending {
                    guard let self, let url = await self.makeAvailable(m) else { continue }
                    await self.addCloudClip(url, select: false)
                }
            }
        }
    }

    /// Download the originals of these cloud clips (export needs full resolution).
    func downloadOriginals(_ clips: [Clip]) async {
        for clip in clips where CloudCache.isStub(clip.url) {
            // The open folder may hold the same recording: clone it rather than download it.
            let name = clip.url.lastPathComponent
            if let local = items.first(where: { !$0.isCloud && $0.clip.url.lastPathComponent == name }),
               (try? CloudCache.adoptOriginal(clip.url, from: local.clip.url)) != nil {
                item(for: clip.url.path)?.originalInCloud = false
                continue
            }
            let ok = await cloud.downloadOriginal(clip.url)
            if ok, let item = item(for: clip.url.path) { item.originalInCloud = false }
        }
    }

    // MARK: Storage

    /// Delete the analysis cache; clips are analyzed again when next opened.
    @MainActor @discardableResult func clearAnalysisCache() -> Int { StorageReport.clearAnalysis() }

    /// Turn every downloaded original back into its trailer stub (proxies and shots stay).
    @MainActor @discardableResult func removeDownloadedOriginals() -> Int {
        var n = 0
        for url in CloudCache.readyClips() where !CloudCache.isStub(url) {
            guard (try? CloudCache.revertToStub(url)) != nil else { continue }
            item(for: url.path)?.originalInCloud = true
            n += 1
        }
        return n
    }

    /// Remove cached cloud clips (all, or only those without shots). Shots are never deleted; a
    /// removed clip is fetched again (or cloned from Studio) when you open it.
    @MainActor @discardableResult func removeCloudClips(keepingClipsWithShots: Bool) -> Int {
        let withShots = Set(shots.map(\.clipPath))
        var n = 0
        for url in CloudCache.readyClips() where !(keepingClipsWithShots && withShots.contains(url.path)) {
            guard let r = CloudCache.record(for: url) else { continue }
            if selection == url { selection = nil }
            items.removeAll { $0.id == url }
            CloudCache.remove(r.mediaId)
            n += 1
        }
        return n
    }

    /// Remove a cloud clip's cached files (its shots stay; fetching it again brings them back).
    func removeCloudCache(_ item: ClipItem) {
        guard item.isCloud, let r = CloudCache.record(for: item.clip.url) else { return }
        if selection == item.id { selection = nil }
        items.removeAll { $0.id == item.id }
        CloudCache.remove(r.mediaId)
    }

    /// A sidebar entry: a clip in the library, or a cloud recording not fetched yet.
    struct Row: Identifiable {
        var id: URL
        var media: CloudMedia?
        var item: ClipItem?
        var date: Date? { item?.clip.captureDate ?? media?.captureDate }
    }

    /// Everything, local and cloud, by recording day (oldest first); within a day by time or by
    /// interest. Same file name means the same recording, and the cloud's entry wins, so shots
    /// always reference the cloud (a local copy only saves downloading the original).
    @MainActor var days: [(day: Date, rows: [Row])] {
        let inCloud = Set(cloud.media.map(\.name)).union(items.filter(\.isCloud).map { $0.clip.url.lastPathComponent })
        var rows: [URL: Row] = [:]
        for m in cloud.media {
            let u = cloud.url(for: m)
            rows[u] = Row(id: u, media: m)
        }
        for item in items where item.isCloud || !inCloud.contains(item.clip.url.lastPathComponent) {
            rows[item.id, default: Row(id: item.id)].item = item
        }
        let cal = Calendar.current
        let groups = Dictionary(grouping: rows.values) { cal.startOfDay(for: $0.date ?? .distantPast) }
        return groups.keys.sorted().map { day in
            let g = groups[day]!.sorted { ($0.date ?? .distantPast) < ($1.date ?? .distantPast) }
            guard sortByInterest else { return (day, g) }
            return (day, g.sorted { ($0.item?.analysis?.score ?? -1) > ($1.item?.analysis?.score ?? -1) })
        }
    }

    /// Run (or load) the interest analysis, then move the thumbnail to the best moment.
    private func analyze(_ item: ClipItem) async {
        guard let a = try? await Analyzer.analyze(item.clip, weights: weights) else { return }
        await MainActor.run { self.apply(a, to: item) }
        if let image = await renderClipThumbnail(item, at: a.bestMoment) {
            await MainActor.run { item.thumbnail = image }
        }
    }

    @MainActor private func apply(_ a: ClipAnalysis, to item: ClipItem) {
        // Results can arrive after the preset changed (loading races the re-rank): match it here.
        let a = a.weights == weights ? a : a.reweighted(weights)
        item.analysis = a
        let fused = a.fused()
        let top = max(fused.max() ?? 1, 0.6)  // a dull clip stays dull rather than stretching to full scale
        item.interest = fused.map { clamp(Double($0 / top)) }.map(Float.init)
        item.heat = Library.heatImage(a)
        if item.id == selection { refreshSuggestionThumbnails(for: item) }
    }

    private func clamp(_ x: Double) -> Double { min(max(x, 0), 1) }

    /// Directional interest as a (time cells × 72 rows) image, row 0 = +180°: white, alpha = heat.
    /// Time-only interest adds a faint wash across all headings.
    static func heatImage(_ a: ClipAnalysis) -> CGImage? {
        let time = a.signals.fused(a.weights)
        let n = time.count, B = HeadingGrid.bins
        guard n > 0 else { return nil }
        let dir = a.headingFused() ?? []
        let dirTop = max(dir.max() ?? 0, 0.6), timeTop = max(time.max() ?? 0, 0.6)
        var px = [UInt8](repeating: 0, count: n * B * 4)
        for r in 0..<B {
            let bin = HeadingGrid.bin(yaw: 180 - (Double(r) + 0.5) * HeadingGrid.binWidth)
            for t in 0..<n {
                let d = dir.isEmpty ? 0 : max(dir[t * B + bin], 0) / dirTop
                let v = min(0.42 * min(d, 1) + 0.10 * min(max(time[t], 0) / timeTop, 1), 1)
                let o = (r * n + t) * 4
                let byte = UInt8(v * 255)
                px[o] = byte; px[o + 1] = byte; px[o + 2] = byte; px[o + 3] = byte  // premultiplied white
            }
        }
        guard let provider = CGDataProvider(data: Data(px) as CFData) else { return nil }
        return CGImage(width: n, height: B, bitsPerComponent: 8, bitsPerPixel: 32, bytesPerRow: n * 4,
                       space: CGColorSpaceCreateDeviceRGB(),
                       bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.premultipliedLast.rawValue),
                       provider: provider, decode: nil, shouldInterpolate: true, intent: .defaultIntent)
    }

    /// Motion summary and a levelled 360° thumbnail (at the best moment, if a cached analysis
    /// knows it), from the proxy.
    private func prepare(_ item: ClipItem) async {
        let clip = item.clip
        let shake = clip.motion?.shakiness()
        let cached = Analyzer.cached(clip, weights: weights)
        let image = await renderClipThumbnail(item, at: cached?.bestMoment ?? clip.duration * 0.4)
        await MainActor.run {
            item.shake = shake
            item.thumbnail = image
            if let cached { apply(cached, to: item) }
        }
        let profile = clip.motion?.speedProfile(bins: 240, duration: clip.duration).map { min($0 / 180, 1) } ?? []
        await MainActor.run { item.shakeProfile = profile }
    }

    private func renderClipThumbnail(_ item: ClipItem, at t: Double) async -> CGImage? {
        let clip = item.clip
        guard let reader = try? await FrameReader(url: clip.proxyURL ?? clip.url),
              let frames = try? reader.frame(at: min(max(t, 0), max(reader.duration - 0.2, 0))) else { return nil }
        var view = ViewSettings()
        view.projection = .equirect
        view.applyLUT = item.isLog
        if let m = clip.motion {
            view.bodyFromCamera = m.bodyFromCamera(at: frames.time, mode: .follow, smoothing: 0, lockTime: 0,
                                                   yaw: 0, pitch: 0, roll: 0)
        }
        return renderer.renderImage(frames: frames, rig: clip.rig, view: view, width: 640, height: 320, sixteenBit: false)
    }

    // MARK: Suggestions

    func setPreset(_ p: InterestWeights.Preset, persist: Bool = true) {
        guard p != preset else { return }
        preset = p
        if persist { UserDefaults.standard.set(p.rawValue, forKey: "interestPreset") }
        let w = weights
        let analyzed = items.compactMap { item in item.analysis.map { (item, $0) } }
        suggestionThumbnails = [:]
        Task.detached(priority: .userInitiated) { [weak self] in
            for (item, a) in analyzed {
                let r = a.reweighted(w)
                await MainActor.run {
                    guard let self, self.preset == p else { return }
                    self.apply(r, to: item)
                }
            }
        }
    }

    /// Suggested shots still on offer: not dismissed and not already covered by a shot.
    func suggestions(for clip: Clip?) -> [Suggestion] {
        guard let clip, let a = item(for: clip.url.path)?.analysis else { return [] }
        let taken = shots(for: clip)
        return a.suggestions.filter { s in
            !dismissed.contains(s.id) && !taken.contains { shot in
                min(shot.outTime, s.outTime) - max(shot.inTime, s.inTime) > 0.5 * s.duration
            }
        }
        .sorted { $0.inTime < $1.inTime }
    }

    /// Keep a suggestion: it becomes an ordinary, editable shot.
    func accept(_ s: Suggestion, clip: Clip) -> Shot {
        let shot = s.shot(clipPath: clip.url.path, name: nextShotName(for: clip))
        add(shot)
        return shot
    }

    func dismiss(_ id: String) {
        recordUndo()
        dismissed.insert(id)
    }

    /// The peak frame of each suggestion, framed as it would be kept.
    func refreshSuggestionThumbnails(for item: ClipItem) {
        for s in suggestions(for: item.clip) where suggestionThumbnails[s.id] == nil {
            let shot = s.shot(clipPath: item.clip.url.path, name: "")
            Task.detached(priority: .utility) { [weak self, renderer] in
                let image = await Library.renderShotFrame(shot, item: item, at: s.peakTime, renderer: renderer)
                await MainActor.run { self?.suggestionThumbnails[s.id] = image }
            }
        }
    }

    // MARK: Shots

    func add(_ shot: Shot) {
        recordUndo()
        shots.append(shot)
        refreshThumbnail(for: shot)
    }

    func update(_ shot: Shot) {
        guard let i = shots.firstIndex(where: { $0.id == shot.id }), shots[i] != shot else { return }
        recordUndo(key: shot.id)
        let old = shots[i]
        shots[i] = shot
        if old.inTime != shot.inTime || old.keyframes.first != shot.keyframes.first
            || old.stabilization != shot.stabilization { refreshThumbnail(for: shot) }
    }

    func delete(_ id: Shot.ID) { delete([id]) }

    /// Delete several shots as one undo step.
    func delete(_ ids: Set<Shot.ID>) {
        guard shots.contains(where: { ids.contains($0.id) }) else { return }
        recordUndo()
        shots.removeAll { ids.contains($0.id) }
        for id in ids { shotThumbnails[id] = nil; shotFrames[id] = nil }
    }

    /// Rename a shot (empty or unchanged names are ignored). Undoable.
    func rename(_ id: Shot.ID, to name: String) {
        let n = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !n.isEmpty, var s = shots.first(where: { $0.id == id }), s.name != n else { return }
        s.name = n
        update(s)
    }

    // MARK: Shots view

    /// Switch to the editor on the shot's clip, with the shot selected at its in point.
    func openInEditor(_ shot: Shot) {
        guard let item = item(for: shot.clipPath) else { return }
        shotToOpen = shot.id
        browse = .clips
        selection = item.id
    }

    /// When the shot was recorded: its clip's capture time, else the camera file name's stamp.
    func recordingDate(of shot: Shot) -> Date? {
        if let d = item(for: shot.clipPath)?.clip.captureDate { return d }
        let parts = URL(fileURLWithPath: shot.clipPath).lastPathComponent.split(separator: "_")
        guard parts.count >= 3 else { return nil }
        let f = DateFormatter()
        f.dateFormat = "yyyyMMddHHmmss"
        f.timeZone = .current
        return f.date(from: String(parts[1] + parts[2]))
    }

    /// Render `count` frames spread across the shot (once per edit of the shot).
    @MainActor func loadShotFrames(_ shot: Shot, count: Int = 10) {
        // A rename doesn't change the pictures.
        if var had = shotFrameSource[shot.id], shotFrames[shot.id] != nil {
            had.name = shot.name
            if had == shot { return }
        }
        guard !shotFrameTasks.contains(shot.id), let item = item(for: shot.clipPath) else { return }
        shotFrameTasks.insert(shot.id)
        Task.detached(priority: .userInitiated) { [weak self, renderer] in
            let times = (0..<count).map { shot.inTime + (Double($0) + 0.5) / Double(count) * shot.duration }
            let frames = await Library.renderShotFrames(shot, item: item, at: times, renderer: renderer)
            await MainActor.run {
                guard let self else { return }
                self.shotFrameTasks.remove(shot.id)
                guard !frames.isEmpty else { return }
                self.shotFrames[shot.id] = frames
                self.shotFrameSource[shot.id] = shot
            }
        }
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
            guard let image = await Library.renderShotFrame(shot, item: item, at: shot.inTime, renderer: renderer) else { return }
            await MainActor.run { self?.shotThumbnails[shot.id] = image }
        }
    }

    /// One proxy frame seen through a shot's framing.
    static func renderShotFrame(_ shot: Shot, item: ClipItem, at t: Double, renderer: ReframeRenderer) async -> CGImage? {
        await renderShotFrames(shot, item: item, at: [t], renderer: renderer).first
    }

    /// Proxy frames seen through a shot's framing, from one reader.
    static func renderShotFrames(_ shot: Shot, item: ClipItem, at times: [Double], renderer: ReframeRenderer) async -> [CGImage] {
        let clip = item.clip
        guard let reader = try? await FrameReader(url: clip.proxyURL ?? clip.url), let motion = clip.motion else { return [] }
        return times.compactMap { t in
            guard let frames = try? reader.frame(at: t) else { return nil }
            return shotImage(shot, item: item, frames: frames, motion: motion, renderer: renderer)
        }
    }

    private static func shotImage(_ shot: Shot, item: ClipItem, frames: LensFrames, motion: Motion,
                                  renderer: ReframeRenderer) -> CGImage? {
        let clip = item.clip
        let f = shot.framing(at: frames.time)
        var view = ViewSettings()
        view.hfov = f.hfov
        view.distortion = f.distortion
        view.applyLUT = item.isLog
        view.bodyFromCamera = motion.bodyFromCamera(at: frames.time, mode: shot.stabilization, smoothing: shot.smoothing,
                                                    lockTime: shot.lockTime, yaw: f.yaw, pitch: f.pitch, roll: 0)
        return renderer.renderImage(frames: frames, rig: clip.rig, view: view, width: 320, height: 180, sixteenBit: false)
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
