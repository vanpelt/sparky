import AppKit
import ReframeKit
import SwiftUI

/// Searching footage by what's in view (SEMANTIC.md): the setting, the model download, the
/// background index of every clip, and the current query's results.
///
/// One search field (the window toolbar's) drives everything: Clips filters and ranks the clip
/// list, the editor swaps the heading lane's interest heat for the query's and offers shots aimed
/// at it, the inspector lists the matches, and Shots matches what each shot looks at.
@MainActor @Observable
final class FootageSearch {
    // MARK: Setting and models

    var isEnabled = UserDefaults.standard.bool(forKey: "search.enabled") {
        didSet {
            guard isEnabled != oldValue else { return }
            UserDefaults.standard.set(isEnabled, forKey: "search.enabled")
            if isEnabled {
                if modelState == .notInstalled || modelState.failure != nil { downloadModels() }
                startIndexing()
                scheduleQuery()
            } else {
                stopIndexing()
                clearResults()
            }
        }
    }

    enum ModelState: Equatable {
        case notInstalled, downloading(Double), ready, failed(String)
        var failure: String? { if case .failed(let s) = self { return s } else { return nil } }
    }
    private(set) var modelState: ModelState = SemanticModels.isInstalled ? .ready : .notInstalled
    var isReady: Bool { isEnabled && modelState == .ready }

    // MARK: Index

    /// Clips with a current index on disk.
    private(set) var indexed: Set<URL> = []
    /// The clip being indexed now, and how many are waiting (for Settings and the sidebar).
    private(set) var indexing: URL?
    private(set) var remaining = 0
    private var failedToIndex: Set<URL> = []
    private var loaded: [URL: SemanticIndex] = [:]

    // MARK: Query

    var text = "" { didSet { if text != oldValue { scheduleQuery() } } }
    /// Set to move keyboard focus to the search field (Find, ⌘F).
    var focusRequest = false
    var query: String { text.trimmingCharacters(in: .whitespacesAndNewlines) }
    var isActive: Bool { !query.isEmpty }
    /// The query `results` answer (lags `text` while typing).
    private(set) var resultsQuery = ""
    private(set) var results: [URL: SemanticMatch] = [:]
    private(set) var busy = false
    /// The query's heat per clip, for the heading lane: (time cells × 72 rows), white, alpha = match.
    private(set) var heat: [URL: CGImage] = [:]
    /// Shots aimed at the query, per clip (computed for the clips you open).
    private(set) var suggestions: [URL: [Suggestion]] = [:]
    /// How well each shot's line of sight matches (cosine), for the shots view.
    private(set) var shotScores: [Shot.ID: Float] = [:]

    weak var library: Library?
    private var indexTask: Task<Void, Never>?
    private var queryTask: Task<Void, Never>?
    private var downloadTask: Task<Void, Never>?
    private var embedding: [Float]?

    // MARK: Models

    /// At launch: with search on, finish (or redo) a model download that didn't complete.
    func start() {
        if isEnabled, modelState != .ready { downloadModels() }
    }

    func downloadModels() {
        guard downloadTask == nil else { return }
        modelState = .downloading(0)
        downloadTask = Task { [weak self] in
            do {
                try await Task.detached(priority: .userInitiated) {
                    try await SemanticModels.install { p in Task { @MainActor in
                        if case .downloading = self?.modelState { self?.modelState = .downloading(p) }
                    } }
                }.value
                self?.modelState = .ready
                self?.startIndexing()
                self?.scheduleQuery()
            } catch is CancellationError {
                self?.modelState = SemanticModels.isInstalled ? .ready : .notInstalled
            } catch {
                self?.modelState = .failed((error as? CustomStringConvertible)?.description ?? error.localizedDescription)
            }
            self?.downloadTask = nil
        }
    }

    func cancelDownload() { downloadTask?.cancel() }

    /// Delete the models; search turns off (the index is kept).
    func removeModels() {
        downloadTask?.cancel()
        isEnabled = false
        try? SemanticModels.remove()
        modelState = .notInstalled
    }

    // MARK: Indexing

    /// Call when the library's clips change: picks up indexes already on disk and queues the rest.
    func itemsChanged() {
        guard let library else { return }
        let items = library.items
        Task.detached(priority: .utility) {
            let have = Set(items.filter { SemanticIndexer.isIndexed($0.clip) }.map(\.id))
            await MainActor.run { [weak self] in
                guard let self else { return }
                let added = have.subtracting(self.indexed)
                self.indexed = have
                if !added.isEmpty, self.isActive { self.scheduleQuery(debounce: false) }
                self.startIndexing()
            }
        }
    }

    private func nextToIndex() -> ClipItem? {
        guard let library else { return nil }
        let todo = library.items.filter {
            !indexed.contains($0.id) && !failedToIndex.contains($0.id) && $0.clip.proxyURL != nil && $0.clip.motion != nil
        }
        remaining = todo.count
        // The clip you're looking at first, then newest first (what you just shot).
        return todo.first { $0.id == library.selection }
            ?? todo.max { ($0.clip.captureDate ?? .distantPast) < ($1.clip.captureDate ?? .distantPast) }
    }

    func startIndexing() {
        guard isReady, indexTask == nil else { return }
        indexTask = Task { [weak self] in
            while let self, !Task.isCancelled, self.isReady, let item = self.nextToIndex() {
                self.indexing = item.id
                let clip = item.clip
                let index = await Task.detached(priority: .utility) { try? await SemanticIndexer.index(clip) }.value
                if let index {
                    self.loaded[item.id] = index
                    self.indexed.insert(item.id)
                    if self.isActive, let q = self.embedding { self.add(item, index: index, query: q) }
                } else {
                    self.failedToIndex.insert(item.id)
                }
            }
            self?.indexing = nil
            self?.remaining = 0
            self?.indexTask = nil
        }
    }

    private func stopIndexing() {
        indexTask?.cancel()
        indexTask = nil
        indexing = nil
    }

    func clearIndex() {
        stopIndexing()
        SemanticIndexer.clear()
        indexed = []
        loaded = [:]
        failedToIndex = []
        clearResults()
        startIndexing()
    }

    // MARK: Querying

    func scheduleQuery(debounce: Bool = true) {
        queryTask?.cancel()
        let q = query
        guard !q.isEmpty, isReady else {
            if q.isEmpty || !isEnabled { clearResults() }
            return
        }
        busy = true
        queryTask = Task { [weak self] in
            if debounce { try? await Task.sleep(for: .milliseconds(220)) }
            guard !Task.isCancelled, let self else { return }
            let v = await Task.detached(priority: .userInitiated) { try? CLIPEncoder.shared().embed(text: q) }.value
            guard !Task.isCancelled, let v else { self.busy = false; return }
            await self.score(query: q, embedding: v)
        }
    }

    private func clearResults() {
        queryTask?.cancel()
        results = [:]; heat = [:]; suggestions = [:]; shotScores = [:]
        resultsQuery = ""; embedding = nil; busy = false
    }

    /// Score every indexed clip (loading indexes as needed, memory-mapped) off the main thread.
    private func score(query q: String, embedding v: [Float]) async {
        guard let library else { return }
        let items = library.items.filter { indexed.contains($0.id) }
        let have = loaded
        let shots = library.shots
        let motions = Dictionary(items.map { ($0.clip.url.path, $0.clip.motion) }, uniquingKeysWith: { a, _ in a })
        struct Out: Sendable {
            var loaded: [URL: SemanticIndex] = [:]
            var results: [URL: SemanticMatch] = [:]
            var heat: [URL: CGImage] = [:]
            var shotScores: [Shot.ID: Float] = [:]
        }
        let out: Out = await Task.detached(priority: .userInitiated) {
            var o = Out()
            for item in items {
                guard let index = have[item.id] ?? SemanticIndexer.cached(item.clip) else { continue }
                o.loaded[item.id] = index
                let m = index.match(v)
                o.results[item.id] = m
                if m.isMatch, let img = FootageSearch.heatImage(index, match: m) { o.heat[item.id] = img }
            }
            for s in shots {
                guard let url = Optional(URL(fileURLWithPath: s.clipPath)), let index = o.loaded[url],
                      let motion = motions[s.clipPath] ?? nil, let m = o.results[url] else { continue }
                o.shotScores[s.id] = index.along(s, motion: motion, values: m.similarity).value
            }
            return o
        }.value
        guard !Task.isCancelled, q == query else { return }
        loaded.merge(out.loaded) { a, _ in a }
        results = out.results
        heat = out.heat
        shotScores = out.shotScores
        suggestions = [:]
        embedding = v
        resultsQuery = q
        busy = false
        if let item = library.selectedItem { prepareSuggestions(for: item) }
    }

    /// A clip indexed while a search is showing joins the results.
    private func add(_ item: ClipItem, index: SemanticIndex, query v: [Float]) {
        let m = index.match(v)
        results[item.id] = m
        if m.isMatch, let img = FootageSearch.heatImage(index, match: m) { heat[item.id] = img }
    }

    /// Shots aimed at the query for one clip: its interest analysis with the query's heading
    /// channel added and dominant (ClipAnalysis.searched).
    func prepareSuggestions(for item: ClipItem) {
        guard isActive, let m = results[item.id], m.isMatch, suggestions[item.id] == nil,
              let a = item.analysis, let index = loaded[item.id] else { return }
        let q = resultsQuery
        Task { [weak self] in
            let s = await Task.detached(priority: .userInitiated) {
                let channel = index.headingChannel(m.relevance, rate: a.signals.rate, count: a.signals.count)
                return a.searched(channel, id: "\(a.clipName)~\(q)").suggestions
            }.value
            guard let self, self.resultsQuery == q else { return }
            self.suggestions[item.id] = s
            self.library?.refreshSuggestionThumbnails(for: item)
        }
    }

    // MARK: Reading results

    /// Matching clips, best first.
    func ranked(_ items: [ClipItem]) -> [(ClipItem, SemanticMatch)] {
        items.compactMap { i in results[i.id].flatMap { $0.isMatch ? (i, $0) : nil } }.sorted { $0.1.best > $1.1.best }
    }

    func match(for item: ClipItem?) -> SemanticMatch? {
        guard let item, isActive, let m = results[item.id], m.isMatch else { return nil }
        return m
    }

    /// Clips that can't be searched yet: being indexed, or only in Insta360 Cloud.
    var unindexedCount: Int {
        guard let library else { return 0 }
        return library.items.filter { !indexed.contains($0.id) }.count
    }

    /// Jump the editor to the next (or previous) match in the open clip and look at it.
    func jump(_ viewer: ViewerModel, forward: Bool) {
        guard let item = library?.selectedItem, let m = match(for: item) else { return }
        let hits = m.hits.sorted { $0.time < $1.time }
        let now = viewer.currentTime
        let hit = forward ? (hits.first { $0.time > now + 0.5 } ?? hits.first)
                          : (hits.last { $0.time < now - 0.5 } ?? hits.last)
        if let hit { show(hit, in: viewer) }
    }

    func show(_ hit: SemanticHit, in viewer: ViewerModel) {
        guard let motion = viewer.clip?.motion else { return }
        viewer.deselectShot()  // aiming inside a shot would keyframe it
        let origin = motion.baseHeading(at: 0, mode: .lock, smoothing: 0, lockTime: 0)
        viewer.aim(heading: origin + hit.yaw, at: hit.time, final: true)
    }

    /// The query's relevance as heat: same layout as Library.heatImage (row 0 = +180°).
    nonisolated static func heatImage(_ index: SemanticIndex, match m: SemanticMatch) -> CGImage? {
        let rate = 4.0, n = max(Int((index.duration * rate).rounded(.up)), 1), B = HeadingGrid.bins
        let grid = index.headingChannel(m.relevance, rate: rate, count: n)
        var px = [UInt8](repeating: 0, count: n * B * 4)
        for r in 0..<B {
            let bin = HeadingGrid.bin(yaw: 180 - (Double(r) + 0.5) * HeadingGrid.binWidth)
            for t in 0..<n {
                let byte = UInt8(min(max(grid[t * B + bin], 0), 1) * 0.85 * 255)
                let o = (r * n + t) * 4
                px[o] = byte; px[o + 1] = byte; px[o + 2] = byte; px[o + 3] = byte
            }
        }
        guard let provider = CGDataProvider(data: Data(px) as CFData) else { return nil }
        return CGImage(width: n, height: B, bitsPerComponent: 8, bitsPerPixel: 32, bytesPerRow: n * 4,
                       space: CGColorSpaceCreateDeviceRGB(),
                       bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.premultipliedLast.rawValue),
                       provider: provider, decode: nil, shouldInterpolate: true, intent: .defaultIntent)
    }
}

/// Where a heading-grid yaw is relative to where the camera faced at the start, in the heading
/// lane's words.
func directionLabel(_ yaw: Double) -> String {
    var y = yaw.truncatingRemainder(dividingBy: 360)
    if y > 180 { y -= 360 } else if y < -180 { y += 360 }
    switch abs(y) {
    case ..<30: return "ahead"
    case 150...: return "behind"
    default: return y > 0 ? (abs(y) < 60 ? "ahead left" : abs(y) > 120 ? "behind left" : "left")
                          : (abs(y) < 60 ? "ahead right" : abs(y) > 120 ? "behind right" : "right")
    }
}

/// The open editor, for menu commands (Find Next Match).
struct ViewerKey: FocusedValueKey { typealias Value = ViewerModel }
extension FocusedValues {
    var viewer: ViewerModel? {
        get { self[ViewerKey.self] }
        set { self[ViewerKey.self] = newValue }
    }
}

/// Edit › Find: focus the search field, and step through matches in the open clip.
struct SearchCommands: Commands {
    let library: Library
    @FocusedValue(\.viewer) private var viewer

    var body: some Commands {
        CommandGroup(after: .textEditing) {
            Divider()
            Button("Search Footage…") { library.search.focusRequest = true }
                .keyboardShortcut("f")
            Button("Next Match") { if let viewer { library.search.jump(viewer, forward: true) } }
                .keyboardShortcut("g")
                .disabled(viewer == nil || library.search.match(for: library.selectedItem) == nil)
            Button("Previous Match") { if let viewer { library.search.jump(viewer, forward: false) } }
                .keyboardShortcut("g", modifiers: [.command, .shift])
                .disabled(viewer == nil || library.search.match(for: library.selectedItem) == nil)
        }
    }
}

/// In the sidebar while searching with search off (or its model not downloaded yet).
struct SearchOffRow: View {
    @Environment(Library.self) private var library
    @Environment(\.openSettings) private var openSettings

    var body: some View {
        let search = library.search
        VStack(alignment: .leading, spacing: 7) {
            Text("Search what's in your footage").font(.system(size: 12, weight: .semibold))
            switch (search.isEnabled, search.modelState) {
            case (_, .downloading(let p)):
                ProgressView(value: p) {
                    Text("Downloading the search model… \(p.formatted(.percent.precision(.fractionLength(0))))").caption()
                }
            case (_, .failed(let error)):
                Text(error).caption().lineLimit(6).fixedSize(horizontal: false, vertical: true)
                Button("Try Again") { search.downloadModels() }
            default:
                Text("Describe a moment and find it in every clip, and where it is in the 360° view. "
                     + "Needs a \(formatBytes(SemanticModels.downloadSize)) on-device model.")
                    .caption().lineLimit(6).fixedSize(horizontal: false, vertical: true)
                HStack {
                    Button("Turn On Search") { search.isEnabled = true }
                    Button("Settings…") { openSettings() }.buttonStyle(.link).foregroundStyle(.secondary)
                }
                .controlSize(.small)
            }
        }
        .padding(.vertical, 4)
    }
}

/// Below search results: what isn't searched yet.
struct SearchStatusRow: View {
    @Environment(Library.self) private var library

    var body: some View {
        let search = library.search
        let cloudOnly = max(library.cloud.media.count - library.items.filter(\.isCloud).count, 0)
        if search.indexing != nil || cloudOnly > 0 {
            Section {
                if search.indexing != nil {
                    HStack(spacing: 7) {
                        ProgressView().controlSize(.mini)
                        Text("Indexing \(search.remaining) more clip\(search.remaining == 1 ? "" : "s")…").caption()
                    }
                }
                if cloudOnly > 0 {
                    VStack(alignment: .leading, spacing: 5) {
                        Text("\(cloudOnly) clip\(cloudOnly == 1 ? " is" : "s are") only in Insta360 Cloud, so not searched yet.")
                            .caption().lineLimit(6).fixedSize(horizontal: false, vertical: true)
                        Button("Fetch All Cloud Clips") { library.fetchAllCloudClips() }.controlSize(.small)
                    }
                }
            }
        }
    }
}
