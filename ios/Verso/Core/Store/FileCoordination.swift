import Foundation

/// Coordinated, atomic JSON file IO shared by the app and widget processes.
enum FileIO {
    static func read<T: Decodable>(_ type: T.Type, at url: URL) throws -> T? {
        var coordError: NSError?
        var result: Result<T?, Error> = .success(nil)
        NSFileCoordinator(filePresenter: nil).coordinate(readingItemAt: url, options: [], error: &coordError) { u in
            guard FileManager.default.fileExists(atPath: u.path) else { result = .success(nil); return }
            result = Result { try VersoJSON.decoder().decode(T.self, from: Data(contentsOf: u)) }
        }
        if let coordError { throw coordError }
        return try result.get()
    }

    static func write<T: Encodable>(_ value: T, to url: URL) throws {
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        let data = try VersoJSON.encoder().encode(value)
        var coordError: NSError?
        var writeError: Error?
        NSFileCoordinator(filePresenter: nil).coordinate(writingItemAt: url, options: .forReplacing, error: &coordError) { u in
            do { try data.write(to: u, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication]) }
            catch { writeError = error }
        }
        if let coordError { throw coordError }
        if let writeError { throw writeError }
    }
}

/// One exclusive, cross-process lock over the store directory (`<dir>/.lock`, flock(2)), held for a whole
/// read-modify-write so the app and the widget extension can't interleave and drop each other's writes.
/// Held only for the synchronous commit (milliseconds, never across a network call), which also keeps iOS
/// from killing a suspended process for holding a lock in a shared container (0xdead10cc).
enum StoreLock {
    static func withLock<T>(in directory: URL, _ body: () throws -> T) throws -> T {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let path = directory.appendingPathComponent(".lock").path
        let fd = open(path, O_CREAT | O_RDWR | O_CLOEXEC, 0o644)
        guard fd >= 0 else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
        defer { close(fd) }
        while flock(fd, LOCK_EX) != 0 {
            guard errno == EINTR else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
        }
        defer { flock(fd, LOCK_UN) }
        return try body()
    }
}
