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
    var id: URL { clip.url }

    init(clip: Clip) { self.clip = clip }

    var shakeLabel: String? {
        guard let s = shake, !clip.info.isTimelapse else { return nil }
        return s.p95 < 30 ? "Calm" : s.p95 < 100 ? "Moving" : "Shaky"
    }
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

    @ObservationIgnored let renderer: ReframeRenderer
    @ObservationIgnored private var scanTask: Task<Void, Never>?

    private static let folderKey = "footageFolder"
    private static let lutKey = "lutFolder"

    init() {
        renderer = try! ReframeRenderer()
        let home = FileManager.default.homeDirectoryForCurrentUser
        let lutDefault = home.appendingPathComponent("Downloads/X6_I-Log_LUTs")
        if let path = UserDefaults.standard.string(forKey: Self.lutKey) {
            setLUTFolder(URL(fileURLWithPath: path))
        } else if FileManager.default.fileExists(atPath: lutDefault.path) {
            setLUTFolder(lutDefault)
        }
        let footageDefault = home.appendingPathComponent("Downloads/Insta360 Download")
        if let path = UserDefaults.standard.string(forKey: Self.folderKey) {
            open(folder: URL(fileURLWithPath: path))
        } else if FileManager.default.fileExists(atPath: footageDefault.path) {
            open(folder: footageDefault)
        }
    }

    var selectedItem: ClipItem? { items.first { $0.id == selection } }

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
        if panel.runModal() == .OK, let url = panel.url { setLUTFolder(url) }
    }

    func setLUTFolder(_ url: URL) {
        lutFolder = url
        UserDefaults.standard.set(url.path, forKey: Self.lutKey)
        lutURLs = CubeLUT.find(in: url)
        if let first = lutURLs.first { useLUT(first) }
    }

    func useLUT(_ url: URL) {
        activeLUT = url
        Task.detached(priority: .userInitiated) { [renderer] in
            if let lut = try? CubeLUT(url: url) { renderer.setLUT(lut) }
        }
    }

    func open(folder: URL) {
        self.folder = folder
        UserDefaults.standard.set(folder.path, forKey: Self.folderKey)
        items = []
        selection = nil
        loading = true
        scanTask?.cancel()
        scanTask = Task.detached(priority: .userInitiated) { [weak self] in
            for url in Clip.scan(folder: folder) {
                if Task.isCancelled { return }
                guard let clip = try? Clip(url: url) else { continue }
                let item = ClipItem(clip: clip)
                await MainActor.run {
                    guard let self else { return }
                    self.items.append(item)
                    if self.selection == nil { self.selection = item.id }
                }
                await self?.prepare(item)
            }
            await MainActor.run { self?.loading = false }
        }
    }

    /// Motion summary and a levelled 360 thumbnail from the proxy.
    private func prepare(_ item: ClipItem) async {
        let clip = item.clip
        let shake = clip.motion?.shakiness()
        var image: CGImage?
        if let reader = try? await FrameReader(url: clip.proxyURL ?? clip.url),
           let frames = try? reader.frame(at: min(clip.duration * 0.4, max(reader.duration - 0.2, 0))) {
            var view = ViewSettings()
            view.projection = .equirect
            view.applyLUT = clip.info.colorProfile == "I_Log"  // the I-Log LUT only suits I-Log footage
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
    }
}
