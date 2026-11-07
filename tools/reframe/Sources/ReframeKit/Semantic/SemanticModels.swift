import CoreML
import CryptoKit
import Foundation

/// The files visual search needs, pinned to Hugging Face commits and checked by SHA-256, and the
/// download that installs them into `CLIPEncoder.modelsDirectory`. That folder is in Application
/// Support (re-downloading 200 MB is not something the system should do behind our back, as it
/// may with Caches) and excluded from Time Machine (it can always be fetched again).
public enum SemanticModels {
    public struct File: Sendable {
        public var path: String     // relative to the models folder
        public var url: URL
        public var size: Int64
        public var sha256: String
    }

    public static let name = "MobileCLIP-S2"
    public static let license = URL(string: "https://github.com/apple/ml-mobileclip/blob/main/LICENSE_weights_data")!
    public static let source = URL(string: "https://huggingface.co/apple/coreml-mobileclip")!

    private static let mobileclip = "https://huggingface.co/apple/coreml-mobileclip/resolve/3e0a7bfb9fe83da8a3efaa3fd8f7df24214bb947/"
    private static let clip = "https://huggingface.co/openai/clip-vit-base-patch32/resolve/3d74acf9a28c67741b2f4f2ea7635f0aaf6f0268/"

    public static let files: [File] = {
        func mc(_ p: String, _ size: Int64, _ sha: String) -> File {
            File(path: p, url: URL(string: mobileclip + p)!, size: size, sha256: sha)
        }
        let core = "Data/com.apple.CoreML/"
        return [
            mc("mobileclip_s2_image.mlpackage/Manifest.json", 617, "6a1a3f93b8dca6c237dbb5dc7b19bb3c987042d14860288304986c099d8796b6"),
            mc("mobileclip_s2_image.mlpackage/\(core)model.mlmodel", 299_056, "2aeb3359f6cde65e9f9248ec2a742e9939bd4bbf48c2f55fcd255b4504d96a1b"),
            mc("mobileclip_s2_image.mlpackage/\(core)weights/weight.bin", 71_397_632, "6cbc7fb06b6072c1cae9c4496d67e0e6217adbf726dfeb82e44d4efe87c34c00"),
            mc("mobileclip_s2_text.mlpackage/Manifest.json", 617, "ea6a189d82fee2ee36eadb99022cda66a7f6b50a4bf9cc1d9541c7ea3b4242a0"),
            mc("mobileclip_s2_text.mlpackage/\(core)model.mlmodel", 128_127, "b8651b6d030bae419a9548b41c8fae11f96b59cfa21b6e532a4c4434522b4b80"),
            mc("mobileclip_s2_text.mlpackage/\(core)weights/weight.bin", 126_866_880, "8e8d5454f104b6cbb58d98bf11e038ff1f1943599efea111260a832f094cd0ce"),
            File(path: "clip-vocab.json", url: URL(string: clip + "vocab.json")!, size: 862_328,
                 sha256: "5047b556ce86ccaf6aa22b3ffccfc52d391ea4accdab9c2f2407da5b742d4363"),
            File(path: "clip-merges.txt", url: URL(string: clip + "merges.txt")!, size: 524_657,
                 sha256: "f526393189112391ce6f9795d4695f704121ce452c3aad1f5335cc41337eba85"),
        ]
    }()

    public static var downloadSize: Int64 { files.reduce(0) { $0 + $1.size } }

    /// Both models compiled (or still downloaded, to be compiled) and the tokenizer files present.
    public static var isInstalled: Bool { files.allSatisfy { !needsDownload($0) } }

    /// A model's package files aren't needed once it's compiled; the downloaded package is deleted then.
    public static func needsDownload(_ f: File) -> Bool {
        let dir = CLIPEncoder.modelsDirectory, fm = FileManager.default
        if let pkg = f.path.split(separator: "/").first, pkg.hasSuffix(".mlpackage") {
            let name = pkg.replacingOccurrences(of: ".mlpackage", with: "")
            if fm.fileExists(atPath: dir.appendingPathComponent("compiled/\(name).mlmodelc").path) { return false }
        }
        return (try? fm.attributesOfItem(atPath: dir.appendingPathComponent(f.path).path)[.size] as? Int64) != f.size
    }

    public enum Failure: Error, CustomStringConvertible {
        case http(String, Int), checksum(String)
        public var description: String {
            switch self {
            case .http(let p, let code): return "Downloading \(p) failed (HTTP \(code))."
            case .checksum(let p): return "\(p) didn't match its checksum; it was discarded."
            }
        }
    }

    /// Downloads what's missing (verifying each file before it's moved into place), then compiles
    /// both models for this Mac. `progress` gets the overall fraction, 0…1, on an arbitrary queue.
    public static func install(progress: @escaping @Sendable (Double) -> Void) async throws {
        let dir = CLIPEncoder.modelsDirectory, fm = FileManager.default
        try fm.createDirectory(at: dir, withIntermediateDirectories: true)
        var excluded = URLResourceValues()
        excluded.isExcludedFromBackup = true
        var dirURL = dir
        try? dirURL.setResourceValues(excluded)

        let missing = files.filter(needsDownload)
        let total = Double(max(missing.reduce(0) { $0 + $1.size }, 1))
        var done: Int64 = 0
        for f in missing {
            try Task.checkCancellation()
            let base = done
            let tmp = try await download(f.url) { received in progress(min(Double(base + received) / total, 1) * 0.97) }
            guard try sha256(of: tmp) == f.sha256 else {
                try? fm.removeItem(at: tmp)
                throw Failure.checksum(f.path)
            }
            let dest = dir.appendingPathComponent(f.path)
            try fm.createDirectory(at: dest.deletingLastPathComponent(), withIntermediateDirectories: true)
            try? fm.removeItem(at: dest)
            try fm.moveItem(at: tmp, to: dest)
            done += f.size
        }
        // Compile now rather than on the first search (a few seconds), keeping only the compiled models.
        _ = try CLIPEncoder.shared(reload: true)
        progress(1)
    }

    /// Deletes the models (and their compiled forms); the search index is kept.
    public static func remove() throws {
        CLIPEncoder.unload()
        let dir = CLIPEncoder.modelsDirectory
        guard FileManager.default.fileExists(atPath: dir.path) else { return }
        try FileManager.default.removeItem(at: dir)
    }

    /// A download task with byte progress (the async `download(from:)` reports none) that stops
    /// when the calling task is cancelled.
    private static func download(_ url: URL, progress: @escaping @Sendable (Int64) -> Void) async throws -> URL {
        let d = Downloader(progress)
        let session = URLSession(configuration: .default, delegate: d, delegateQueue: nil)
        defer { session.finishTasksAndInvalidate() }
        let task = session.downloadTask(with: url)
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { cont in
                d.continuation = cont
                task.resume()
            }
        } onCancel: { task.cancel() }
    }

    static func sha256(of url: URL) throws -> String {
        let h = try FileHandle(forReadingFrom: url)
        defer { try? h.close() }
        var hasher = SHA256()
        while let chunk = try h.read(upToCount: 4 << 20), !chunk.isEmpty { hasher.update(data: chunk) }
        return hasher.finalize().map { String(format: "%02x", $0) }.joined()
    }

    private final class Downloader: NSObject, URLSessionDownloadDelegate, @unchecked Sendable {
        let report: @Sendable (Int64) -> Void
        var continuation: CheckedContinuation<URL, Error>?
        private let lock = NSLock()
        init(_ report: @escaping @Sendable (Int64) -> Void) { self.report = report }

        private func finish(_ r: Result<URL, Error>) {
            lock.withLock { continuation.take() }?.resume(with: r)
        }

        func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didWriteData bytesWritten: Int64,
                        totalBytesWritten: Int64, totalBytesExpectedToWrite: Int64) {
            report(totalBytesWritten)
        }

        func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didFinishDownloadingTo location: URL) {
            let code = (downloadTask.response as? HTTPURLResponse)?.statusCode ?? 0
            guard code == 200 else { return finish(.failure(Failure.http(downloadTask.originalRequest?.url?.lastPathComponent ?? "", code))) }
            // `location` is deleted when this returns: move it somewhere of ours.
            let keep = FileManager.default.temporaryDirectory.appendingPathComponent("reframe-model-\(UUID().uuidString)")
            do { try FileManager.default.moveItem(at: location, to: keep); finish(.success(keep)) }
            catch { finish(.failure(error)) }
        }

        func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
            if let error { finish(.failure((error as? URLError)?.code == .cancelled ? CancellationError() : error)) }
        }
    }
}
