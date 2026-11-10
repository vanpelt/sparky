import Foundation

/// One recording: the .insv master, its .lrv proxy, metadata, calibration and motion.
public final class Clip: Identifiable, @unchecked Sendable {
    public let url: URL
    public let proxyURL: URL?
    public let file: InsvFile
    public let rig: Rig
    public var id: URL { url }
    public var info: InsvInfo { file.info }
    public var name: String { url.deletingPathExtension().lastPathComponent }

    private var _motion: Motion??
    private let lock = NSLock()

    public init(url: URL) throws {
        self.url = url
        file = try InsvFile(url: url)
        rig = try Rig(info: file.info)
        proxyURL = Clip.findProxy(for: url)
    }

    /// IMU orientation (computed once, on first use).
    public var motion: Motion? {
        lock.lock(); defer { lock.unlock() }
        if let m = _motion { return m }
        let m = Motion(file: file)
        _motion = .some(m)
        return m
    }

    public var duration: Double {
        if let ms = info.durationMs { return Double(ms) / 1000 }
        if let n = info.frameCount, let fps = info.nominalFPS, fps > 0 { return Double(n) / Double(fps) }
        return 0
    }

    public var captureDate: Date? {
        let f = DateFormatter()
        f.dateFormat = "yyyyMMddHHmmss"
        f.timeZone = .current
        return f.date(from: info.captureTime)
    }

    static func findProxy(for url: URL) -> URL? {
        let dir = url.deletingLastPathComponent()
        let guess = url.lastPathComponent
            .replacingOccurrences(of: "VID_", with: "LRV_")
            .replacingOccurrences(of: "_00_", with: "_01_")
            .replacingOccurrences(of: ".insv", with: ".lrv")
        let candidate = dir.appendingPathComponent(guess)
        if FileManager.default.fileExists(atPath: candidate.path) { return candidate }
        let stamp = url.lastPathComponent.split(separator: "_").dropFirst().prefix(2).joined(separator: "_")
        let files = (try? FileManager.default.contentsOfDirectory(at: dir, includingPropertiesForKeys: nil)) ?? []
        return files.first { $0.pathExtension.lowercased() == "lrv" && $0.lastPathComponent.contains(stamp) }
    }

    /// All .insv recordings under a folder, oldest first.
    public static func scan(folder: URL) -> [URL] {
        guard let e = FileManager.default.enumerator(at: folder, includingPropertiesForKeys: nil,
                                                     options: [.skipsHiddenFiles]) else { return [] }
        return e.compactMap { $0 as? URL }
            .filter { $0.pathExtension.lowercased() == "insv" && !$0.lastPathComponent.contains("_10_") }
            .sorted { $0.lastPathComponent < $1.lastPathComponent }
    }
}
