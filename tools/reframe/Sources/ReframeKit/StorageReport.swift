import Foundation

/// What Reframe keeps on disk, for the Storage settings: the cloud clip cache split into proxies,
/// downloaded originals and trailer stubs (gyro + metadata), the analysis cache, and the rest
/// (shot stores, sign-in, aliases). Sizes are allocated bytes on disk. Files cloned from another
/// copy (Insta360 Studio's downloads, a footage folder) share their blocks with it, so removing
/// them frees less than their size; `shared` counts those.
public struct StorageReport: Sendable {
    public enum Kind: String, CaseIterable, Sendable, Identifiable {
        case proxies, originals, gyro, analysis, other
        public var id: String { rawValue }
        public var label: String {
            switch self {
            case .proxies: return "Proxies"
            case .originals: return "Originals"
            case .gyro: return "Gyro & metadata"
            case .analysis: return "Analysis"
            case .other: return "Shots & settings"
            }
        }
        public var detail: String {
            switch self {
            case .proxies: return "Low-res LRV proxies of cloud clips, for browsing and analysis"
            case .originals: return "Full originals downloaded for export"
            case .gyro: return "Each cloud clip's trailer: gyro, calibration, metadata"
            case .analysis: return "Interest analysis results, reused until a clip's file changes"
            case .other: return "Your shots, sign-in, preferences and file aliases"
            }
        }
    }

    public struct Entry: Identifiable, Sendable {
        public var id: String
        public var kind: Kind
        /// A clip's name (VID_…), or a description for whole-category entries.
        public var name: String
        public var mediaId: String?
        public var bytes: Int64
        public var shared: Int64
    }

    public var entries: [Entry] = []

    public var total: Int64 { entries.reduce(0) { $0 + $1.bytes } }
    public var shared: Int64 { entries.reduce(0) { $0 + $1.shared } }
    public func bytes(_ kind: Kind) -> Int64 { entries.filter { $0.kind == kind }.reduce(0) { $0 + $1.bytes } }
    public func entries(_ kind: Kind) -> [Entry] { entries.filter { $0.kind == kind } }

    static var support: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("Reframe")
    }

    /// (allocated bytes, of which in files that may share their content with another file).
    static func size(_ url: URL) -> (Int64, Int64) {
        let keys: Set<URLResourceKey> = [.totalFileAllocatedSizeKey, .fileAllocatedSizeKey, .isDirectoryKey,
                                         .isSymbolicLinkKey, .mayShareFileContentKey]
        guard let v = try? url.resourceValues(forKeys: keys) else { return (0, 0) }
        if v.isDirectory == true {
            let files = (try? FileManager.default.contentsOfDirectory(at: url, includingPropertiesForKeys: Array(keys))) ?? []
            return files.reduce((0, 0)) { acc, f in let s = size(f); return (acc.0 + s.0, acc.1 + s.1) }
        }
        let bytes = Int64(v.totalFileAllocatedSize ?? v.fileAllocatedSize ?? 0)
        return (bytes, v.mayShareFileContent == true ? bytes : 0)
    }

    public static func scan() -> StorageReport {
        var r = StorageReport()
        let fm = FileManager.default
        // Cloud clips, one entry per clip per kind.
        let dirs = (try? fm.contentsOfDirectory(at: CloudCache.root, includingPropertiesForKeys: nil)) ?? []
        for d in dirs {
            let files = (try? fm.contentsOfDirectory(at: d, includingPropertiesForKeys: nil)) ?? []
            let record = (try? Data(contentsOf: d.appendingPathComponent("cloud.json")))
                .flatMap { try? JSONDecoder().decode(CloudRecord.self, from: $0) }
            let id = record?.mediaId ?? d.lastPathComponent
            let name = record.map { URL(fileURLWithPath: $0.name).deletingPathExtension().lastPathComponent } ?? d.lastPathComponent
            var by: [Kind: (Int64, Int64)] = [:]
            for f in files where !f.lastPathComponent.hasPrefix(".") {
                let s = size(f)
                let kind: Kind
                switch f.pathExtension.lowercased() {
                case "lrv": kind = .proxies
                case "insv":
                    let length = (try? fm.attributesOfItem(atPath: f.path)[.size] as? Int64) ?? 0
                    kind = record.map { length >= $0.size } == true ? .originals : .gyro
                default: kind = .gyro
                }
                by[kind, default: (0, 0)].0 += s.0
                by[kind, default: (0, 0)].1 += s.1
            }
            for (kind, s) in by where s.0 > 0 {
                r.entries.append(Entry(id: "\(kind.rawValue)-\(id)", kind: kind, name: name, mediaId: id, bytes: s.0, shared: s.1))
            }
        }
        let analysis = size(support.appendingPathComponent("analysis"))
        if analysis.0 > 0 {
            r.entries.append(Entry(id: "analysis", kind: .analysis, name: "Analysis cache", bytes: analysis.0, shared: analysis.1))
        }
        var other = size(support.appendingPathComponent("projects"))
        let session = size(CloudSession.fileURL)
        let aliases = size(FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Reframe/aliases"))
        other = (other.0 + session.0 + aliases.0, other.1 + session.1 + aliases.1)
        if other.0 > 0 { r.entries.append(Entry(id: "other", kind: .other, name: "Shots & settings", bytes: other.0, shared: other.1)) }
        r.entries.sort { $0.bytes > $1.bytes }
        return r
    }

    /// Delete the analysis cache (results are recomputed when a clip is next opened).
    @discardableResult
    public static func clearAnalysis() -> Int {
        let dir = support.appendingPathComponent("analysis")
        let files = (try? FileManager.default.contentsOfDirectory(at: dir, includingPropertiesForKeys: nil)) ?? []
        return files.filter { $0.pathExtension == "json" && (try? FileManager.default.removeItem(at: $0)) != nil }.count
    }
}

extension CloudCache {
    /// Turn a downloaded original back into its trailer stub (cut from the original itself), freeing
    /// the original's space. The clip keeps its path, shots and proxy.
    public static func revertToStub(_ clipURL: URL) throws {
        guard record(for: clipURL) != nil, !isStub(clipURL) else { return }
        let src = try BinaryFile(clipURL)
        let end = try src.read(at: src.size - 72, count: 72)
        guard end.count == 72 else { throw CloudError.badResponse("short file") }
        let keep = min(Int64(src.size), Int64(end.le(32, as: UInt32.self)) + 65_536)
        let tail = try src.read(at: src.size - UInt64(keep), count: Int(keep))
        try tail.write(to: clipURL, options: .atomic)
    }
}
