import Foundation

/// Cloud clips made workable locally without their originals (CLOUD.md):
///
///   ~/Library/Application Support/Reframe/Cloud/<mediaId>/
///     VID_….insv   only the original's tail (the trailer: metadata, calibration, gyro), ~5 MB.
///                  The trailer's offsets count from the end of the file, so `Clip(url:)` reads
///                  it as if it were the whole recording. Downloading the original replaces it
///                  in place, so the clip keeps its path (and its shots).
///     LRV_….lrv    the full low-res proxy (~6% of the original): playback, audio, analysis.
///     cloud.json   the record; written last, so its presence means the clip is ready.
public struct CloudRecord: Codable, Sendable, Equatable {
    public var mediaId: String
    /// The original's file name, e.g. VID_20261025_132131_00_070.insv.
    public var name: String
    /// The original's size in bytes; a smaller file at `masterURL` is the trailer stub.
    public var size: Int64
    public var lrvName: String?
    public var lrvSize: Int64?
    public var fetched = Date()
}

public enum CloudCache {
    public static var root: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Reframe/Cloud", isDirectory: true)
    }

    public static func dir(_ mediaId: String) -> URL { root.appendingPathComponent(mediaId, isDirectory: true) }

    /// Where a cloud recording's .insv lives (stub or full) — also its clip identity.
    public static func masterURL(mediaId: String, name: String) -> URL { dir(mediaId).appendingPathComponent(name) }

    public static func isCloud(_ clipURL: URL) -> Bool { clipURL.standardizedFileURL.path.hasPrefix(root.standardizedFileURL.path + "/") }

    public static func record(for clipURL: URL) -> CloudRecord? {
        guard isCloud(clipURL),
              let d = try? Data(contentsOf: clipURL.deletingLastPathComponent().appendingPathComponent("cloud.json")) else { return nil }
        return try? JSONDecoder().decode(CloudRecord.self, from: d)
    }

    /// True while the clip's original is still in the cloud (only its trailer is here).
    public static func isStub(_ clipURL: URL) -> Bool {
        guard let r = record(for: clipURL) else { return false }
        let size = (try? FileManager.default.attributesOfItem(atPath: clipURL.path)[.size] as? Int64) ?? 0
        return size < r.size
    }

    /// Cloud clips ready to open, as their .insv URLs.
    public static func readyClips() -> [URL] {
        let dirs = (try? FileManager.default.contentsOfDirectory(at: root, includingPropertiesForKeys: nil)) ?? []
        return dirs.compactMap { d in
            guard let data = try? Data(contentsOf: d.appendingPathComponent("cloud.json")),
                  let r = try? JSONDecoder().decode(CloudRecord.self, from: data) else { return nil }
            let u = d.appendingPathComponent(r.name)
            return FileManager.default.fileExists(atPath: u.path) ? u : nil
        }
    }

    public static func remove(_ mediaId: String) { try? FileManager.default.removeItem(at: dir(mediaId)) }

    /// Bytes on disk for one cloud clip.
    public static func diskUsage(_ mediaId: String) -> Int64 {
        let files = (try? FileManager.default.contentsOfDirectory(at: dir(mediaId), includingPropertiesForKeys: [.fileSizeKey])) ?? []
        return files.reduce(0) { $0 + Int64((try? $1.resourceValues(forKeys: [.fileSizeKey]).fileSize) ?? 0) }
    }

    public enum Stage: Sendable { case trailer, proxy(Double), original(Double), fromStudio }

    /// Make a cloud recording workable: its trailer stub and its LRV. Returns the .insv URL.
    public static func fetch(_ media: CloudMedia, client: CloudClient,
                             progress: @escaping @Sendable (Stage) -> Void) async throws -> URL {
        // Insta360 Studio may have downloaded it already: clone that rather than fetch.
        if let studio = StudioDownloads.files(mediaId: media.id), let original = studio.original {
            progress(.fromStudio)
            if let url = try? seed(media, fromLocal: original) { return url }
        }
        let files = try await client.files(for: media.id)
        guard let master = files.first(where: { $0.name.lowercased().hasSuffix(".insv") }) else {
            throw CloudError.badResponse("no .insv among \(files.map(\.name))")
        }
        let lrv = files.first { $0.name.lowercased().hasSuffix(".lrv") }
        let d = dir(media.id)
        try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true)

        // 1. The trailer: its length is in the last 72 bytes; keep a margin before it (the
        //    reader looks a few bytes ahead of the payload for the box header).
        progress(.trailer)
        let size = master.size > 0 ? master.size : try await contentLength(master.url, client: client)
        let end = try await client.range(master.url, from: size - 72, to: size - 1)
        guard end.status == 206, end.data.count == 72 else { throw CloudError.http(end.status, "trailer end") }
        let extra = Int64(end.data.le(32, as: UInt32.self))
        let keep = min(size, extra + 65_536)
        let tail = try await client.range(master.url, from: size - keep, to: size - 1)
        guard tail.status == 206, Int64(tail.data.count) == keep else { throw CloudError.http(tail.status, "trailer") }
        let stub = d.appendingPathComponent(master.name)
        let existing = (try? FileManager.default.attributesOfItem(atPath: stub.path)[.size] as? Int64) ?? 0
        if existing < size { try tail.data.write(to: stub, options: .atomic) }  // never clobber a full original
        _ = try InsvFile(url: stub)  // parses like the real file, or fails here

        // 2. The proxy.
        if let lrv {
            let dest = d.appendingPathComponent(lrv.name)
            let have = (try? FileManager.default.attributesOfItem(atPath: dest.path)[.size] as? Int64) ?? -1
            if have != lrv.size {
                try await download(lrv.url, to: dest) { progress(.proxy($0)) }
            }
        }

        // 3. Ready.
        let record = CloudRecord(mediaId: media.id, name: master.name, size: size, lrvName: lrv?.name, lrvSize: lrv?.size)
        try JSONEncoder().encode(record).write(to: d.appendingPathComponent("cloud.json"), options: .atomic)
        return stub
    }

    /// Make a cloud recording workable from a local copy of it instead of the network: its
    /// trailer stub cut from the local original, and its LRV cloned (APFS clones cost no space).
    /// The local copy must be the same file (same size as the cloud's original).
    public static func seed(_ media: CloudMedia, fromLocal original: URL) throws -> URL {
        let src = try BinaryFile(original)
        let size = Int64(src.size)
        if let expected = media.size, expected != size {
            throw CloudError.badResponse("\(original.lastPathComponent) is \(size) bytes here, \(expected) in the cloud")
        }
        let end = try src.read(at: src.size - 72, count: 72)
        guard end.count == 72 else { throw CloudError.badResponse("short file") }
        let extra = Int64(end.le(32, as: UInt32.self))
        let keep = min(size, extra + 65_536)
        let tail = try src.read(at: UInt64(size - keep), count: Int(keep))
        let d = dir(media.id)
        try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true)
        let stub = d.appendingPathComponent(original.lastPathComponent)
        let existing = (try? FileManager.default.attributesOfItem(atPath: stub.path)[.size] as? Int64) ?? 0
        if existing < size { try tail.write(to: stub, options: .atomic) }
        _ = try InsvFile(url: stub)

        var lrvName: String?, lrvSize: Int64?
        if let proxy = Clip.findProxy(for: original) {
            let dest = d.appendingPathComponent(proxy.lastPathComponent)
            if !FileManager.default.fileExists(atPath: dest.path) { try FileManager.default.copyItem(at: proxy, to: dest) }
            lrvName = proxy.lastPathComponent
            lrvSize = (try? FileManager.default.attributesOfItem(atPath: dest.path)[.size] as? Int64) ?? nil
        }
        let record = CloudRecord(mediaId: media.id, name: original.lastPathComponent, size: size, lrvName: lrvName, lrvSize: lrvSize)
        try JSONEncoder().encode(record).write(to: d.appendingPathComponent("cloud.json"), options: .atomic)
        return stub
    }

    /// Fill a stub with a local copy of its original (cloned; same file, so no download).
    public static func adoptOriginal(_ clipURL: URL, from local: URL) throws {
        guard let r = record(for: clipURL), isStub(clipURL) else { return }
        let size = (try? FileManager.default.attributesOfItem(atPath: local.path)[.size] as? Int64) ?? 0
        guard size == r.size else { throw CloudError.badResponse("\(local.lastPathComponent) doesn't match the cloud original") }
        let staged = clipURL.deletingLastPathComponent().appendingPathComponent(".\(clipURL.lastPathComponent).part")
        try? FileManager.default.removeItem(at: staged)
        try FileManager.default.copyItem(at: local, to: staged)
        _ = try FileManager.default.replaceItemAt(clipURL, withItemAt: staged)
    }

    /// Replace a clip's trailer stub with the full original (for export).
    public static func downloadOriginal(_ clipURL: URL, client: CloudClient,
                                        progress: @escaping @Sendable (Double) -> Void) async throws {
        guard let r = record(for: clipURL), isStub(clipURL) else { return }
        if let original = StudioDownloads.files(mediaId: r.mediaId)?.original,
           (try? adoptOriginal(clipURL, from: original)) != nil {
            progress(1)
            return
        }
        let files = try await client.files(for: r.mediaId)
        guard let master = files.first(where: { $0.name == r.name }) else { throw CloudError.badResponse("original not listed") }
        try await download(master.url, to: clipURL, progress: progress)
    }

    private static func contentLength(_ url: URL, client: CloudClient) async throws -> Int64 {
        let r = try await client.range(url, from: 0, to: 0)
        guard let total = r.contentRange?.split(separator: "/").last.flatMap({ Int64($0) }) else {
            throw CloudError.badResponse("no Content-Range")
        }
        return total
    }

    /// Download to a temporary file beside `dest`, then swap it in.
    static func download(_ url: URL, to dest: URL, progress: @escaping @Sendable (Double) -> Void) async throws {
        let tmp = try await Downloader().run(url, progress: progress)
        let staged = dest.deletingLastPathComponent().appendingPathComponent(".\(dest.lastPathComponent).part")
        try? FileManager.default.removeItem(at: staged)
        try FileManager.default.moveItem(at: tmp, to: staged)
        _ = try FileManager.default.replaceItemAt(dest, withItemAt: staged)
    }
}

/// One URLSession download with progress callbacks.
private final class Downloader: NSObject, URLSessionDownloadDelegate, @unchecked Sendable {
    private var continuation: CheckedContinuation<URL, Error>?
    private var progress: (@Sendable (Double) -> Void)?
    private var lastReport = Date.distantPast

    func run(_ url: URL, progress: @escaping @Sendable (Double) -> Void) async throws -> URL {
        self.progress = progress
        let session = URLSession(configuration: .ephemeral, delegate: self, delegateQueue: nil)
        defer { session.finishTasksAndInvalidate() }
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { c in
                continuation = c
                session.downloadTask(with: url).resume()
            }
        } onCancel: {
            session.invalidateAndCancel()
        }
    }

    func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didWriteData _: Int64,
                    totalBytesWritten: Int64, totalBytesExpectedToWrite: Int64) {
        guard totalBytesExpectedToWrite > 0, Date().timeIntervalSince(lastReport) > 0.2 else { return }
        lastReport = Date()
        progress?(Double(totalBytesWritten) / Double(totalBytesExpectedToWrite))
    }

    func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didFinishDownloadingTo location: URL) {
        let status = (downloadTask.response as? HTTPURLResponse)?.statusCode ?? 0
        guard status == 200 else {
            continuation?.resume(throwing: CloudError.http(status, downloadTask.originalRequest?.url?.lastPathComponent ?? ""))
            continuation = nil
            return
        }
        // The file at `location` vanishes when this returns: move it somewhere we own.
        let keep = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        do {
            try FileManager.default.moveItem(at: location, to: keep)
            progress?(1)
            continuation?.resume(returning: keep)
        } catch { continuation?.resume(throwing: error) }
        continuation = nil
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        if let error { continuation?.resume(throwing: error); continuation = nil }
    }
}
