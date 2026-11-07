import Foundation
import SQLite3

/// Cloud recordings Insta360 Studio has already downloaded, so Reframe can clone them instead of
/// fetching them again (CLOUD.md). Studio records its downloads per account in
/// `~/Library/Application Support/Insta360/Insta360 Studio/user_data/<account>/download.db`:
/// `downloaded_list` holds finished downloads (TASK_ID = "<mediaId>##0##<path>##1") and
/// `download_file_list` each file of a task (the .insv and its .lrv) with its size and path.
/// Read-only; Studio's records are never written.
public enum StudioDownloads {
    public struct Files: Sendable {
        public var original: URL?
        public var proxy: URL?
    }

    static var userData: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Insta360/Insta360 Studio/user_data", isDirectory: true)
    }

    static func databases() -> [URL] {
        let accounts = (try? FileManager.default.contentsOfDirectory(at: userData, includingPropertiesForKeys: nil)) ?? []
        return accounts.map { $0.appendingPathComponent("download.db") }
            .filter { FileManager.default.fileExists(atPath: $0.path) }
    }

    /// Every finished download Studio knows of, by media ID: (file name, size, path) per file.
    /// Empty unless Studio is installed (its leftover records alone aren't trusted).
    public static func all() -> [String: [(name: String, size: Int64, path: String)]] {
        guard StudioProject.isStudioInstalled else { return [:] }
        var out: [String: [(String, Int64, String)]] = [:]
        for db in databases() {
            var h: OpaquePointer?
            // Read-only, and never create or migrate anything while Studio may have it open.
            guard sqlite3_open_v2(db.absoluteString + "?mode=ro", &h, SQLITE_OPEN_READONLY | SQLITE_OPEN_URI, nil) == SQLITE_OK else {
                sqlite3_close(h); continue
            }
            defer { sqlite3_close(h) }
            sqlite3_busy_timeout(h, 500)
            let sql = """
                SELECT f.TASK_ID, f.FILE_NAME, f.FILE_SIZE, f.FILE_PATH
                FROM download_file_list f JOIN downloaded_list d ON d.TASK_ID = f.TASK_ID
                """
            var st: OpaquePointer?
            guard sqlite3_prepare_v2(h, sql, -1, &st, nil) == SQLITE_OK else { continue }
            defer { sqlite3_finalize(st) }
            while sqlite3_step(st) == SQLITE_ROW {
                func text(_ i: Int32) -> String { sqlite3_column_text(st, i).map { String(cString: $0) } ?? "" }
                let task = text(0)
                guard let id = task.components(separatedBy: "##").first, !id.isEmpty else { continue }
                out[id, default: []].append((text(1), sqlite3_column_int64(st, 2), text(3)))
            }
        }
        return out
    }

    /// The files Studio downloaded for a recording that are still on disk at their full size.
    /// (Checking them touches Studio's download folder: in ~/Downloads that means macOS asks.)
    public static func files(mediaId: String) -> Files? {
        guard let rows = all()[mediaId] else { return nil }
        func present(_ r: (name: String, size: Int64, path: String)) -> URL? {
            let size = (try? FileManager.default.attributesOfItem(atPath: r.path)[.size] as? Int64) ?? -1
            return size == r.size ? URL(fileURLWithPath: r.path) : nil
        }
        let original = rows.first { $0.name.lowercased().hasSuffix(".insv") }.flatMap(present)
        let proxy = rows.first { $0.name.lowercased().hasSuffix(".lrv") }.flatMap(present)
        return original == nil && proxy == nil ? nil : Files(original: original, proxy: proxy)
    }
}
