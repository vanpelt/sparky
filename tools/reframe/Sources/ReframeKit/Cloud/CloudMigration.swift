import Foundation

/// Moves shots that point at local camera files over to the same recordings in Insta360 Cloud, so
/// every shot references the cloud (CLOUD.md). For each shot in a folder's store whose file name
/// matches a cloud recording:
///   - the cloud clip is made ready in the cache, from the local copy when it is still there (trailer
///     cut from it, LRV cloned: no network, no extra space), else fetched from the cloud;
///   - the shot's clip path becomes the cloud clip's, and it moves to the cloud store.
/// Shots without a cloud match stay where they are. Each store written is backed up first
/// (`<store>.json.bak-<timestamp>`).
public enum CloudMigration {
    public struct Move: Sendable {
        public var store: URL
        public var shot: String
        public var from: String
        public var mediaId: String
        /// The local copy is there to seed from (else the cloud clip is fetched).
        public var localCopy: Bool
        /// The cloud clip is already in the cache.
        public var cached: Bool
    }

    public struct Plan: Sendable {
        public var moves: [Move] = []
        /// Shots with no cloud recording of the same name (they stay local).
        public var unmatched: [(store: URL, shot: String, file: String)] = []
        /// Stores (other than the cloud's) that will be left without shots.
        public var emptied: [URL] = []
    }

    static var cloudStoreURL: URL { ShotStore.url(for: CloudCache.root) }

    /// Every folder store in Application Support (not the cloud's, not backups).
    static func folderStores() -> [URL] {
        let files = (try? FileManager.default.contentsOfDirectory(at: ShotStore.directory, includingPropertiesForKeys: nil)) ?? []
        return files.filter { $0.pathExtension == "json" && $0.standardizedFileURL != cloudStoreURL.standardizedFileURL }
            .sorted { $0.lastPathComponent < $1.lastPathComponent }
    }

    public static func plan(media: [CloudMedia]) throws -> Plan {
        let byName = Dictionary(media.map { ($0.name, $0) }, uniquingKeysWith: { a, _ in a })
        var plan = Plan()
        for url in folderStores() {
            let store = try ShotStore.load(url: url)
            var left = 0
            for s in store.shots where !CloudCache.isCloud(URL(fileURLWithPath: s.clipPath)) {
                let file = URL(fileURLWithPath: s.clipPath).lastPathComponent
                guard let m = byName[file] else {
                    plan.unmatched.append((url, s.name, file)); left += 1; continue
                }
                let cached = FileManager.default.fileExists(atPath: CloudCache.dir(m.id).appendingPathComponent("cloud.json").path)
                plan.moves.append(Move(store: url, shot: s.name, from: s.clipPath, mediaId: m.id,
                                       localCopy: FileManager.default.fileExists(atPath: s.clipPath), cached: cached))
            }
            if left == 0 && !store.shots.isEmpty && plan.moves.contains(where: { $0.store == url }) { plan.emptied.append(url) }
        }
        return plan
    }

    /// Carry out a plan. Returns the backups written.
    @discardableResult
    public static func run(media: [CloudMedia], client: CloudClient?, log: (String) -> Void) async throws -> [URL] {
        let plan = try plan(media: media)
        guard !plan.moves.isEmpty else { log("nothing to move"); return [] }
        let byId = Dictionary(media.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })

        // 1. Every cloud clip the moved shots need, ready in the cache.
        var ready: [String: URL] = [:]
        for mv in plan.moves where ready[mv.mediaId] == nil {
            guard let m = byId[mv.mediaId] else { continue }
            let existing = CloudCache.masterURL(mediaId: m.id, name: m.name)
            if mv.cached, FileManager.default.fileExists(atPath: existing.path) {
                ready[m.id] = existing
            } else if mv.localCopy, let url = try? CloudCache.seed(m, fromLocal: URL(fileURLWithPath: mv.from)) {
                log("  \(m.name): seeded from the local copy")
                ready[m.id] = url
            } else if let client {
                log("  \(m.name): fetching from the cloud…")
                ready[m.id] = try await CloudCache.fetch(m, client: client) { _ in }
            } else {
                throw CloudError.notSignedIn
            }
        }

        // 2. Back up, then rewrite the stores.
        let stamp = ISO8601DateFormatter().string(from: Date()).replacingOccurrences(of: ":", with: "")
        var backups: [URL] = []
        func backup(_ url: URL) throws {
            guard FileManager.default.fileExists(atPath: url.path) else { return }
            let b = url.deletingLastPathComponent().appendingPathComponent(url.lastPathComponent + ".bak-" + stamp)
            try FileManager.default.copyItem(at: url, to: b)
            backups.append(b)
        }
        var cloud = (try? ShotStore.load(url: cloudStoreURL)) ?? ShotStore()
        try backup(cloudStoreURL)
        for url in Set(plan.moves.map(\.store)).sorted(by: { $0.path < $1.path }) {
            var store = try ShotStore.load(url: url)
            try backup(url)
            var kept: [Shot] = []
            for var s in store.shots {
                let file = URL(fileURLWithPath: s.clipPath).lastPathComponent
                if let mv = plan.moves.first(where: { $0.store == url && $0.from == s.clipPath }), let target = ready[mv.mediaId] {
                    s.clipPath = target.path
                    cloud.shots.append(s)
                    log("  moved \(s.name) of \(file)")
                } else {
                    kept.append(s)
                }
            }
            store.shots = kept
            cloud.dismissed.formUnion(store.dismissed)
            try store.save(to: url)
        }
        try cloud.save(to: cloudStoreURL)
        return backups
    }
}
