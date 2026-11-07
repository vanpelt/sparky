import AppKit
import ImageIO
import Observation
import ReframeKit

/// The Insta360 Cloud side of the library (CLOUD.md): sign-in, the cloud listing, and fetching a
/// recording's trailer + LRV so it opens like a local clip. The `Library` turns fetched clips
/// into `ClipItem`s; this only tracks the cloud state.
@MainActor
@Observable
final class CloudLibrary {
    enum State: Equatable { case signedOut, loading, ready, failed(String) }

    var state: State = CloudSession.load() == nil ? .signedOut : .loading
    /// Videos in the cloud, oldest first.
    var media: [CloudMedia] = []
    var thumbnails: [String: CGImage] = [:]
    /// In-flight fetches (trailer + proxy) and original downloads, by media ID.
    var fetching: [String: CloudCache.Stage] = [:]
    var errors: [String: String] = [:]
    /// Storage used / plan size, GB.
    var quota: (used: Double, total: Double)?

    @ObservationIgnored private(set) var client: CloudClient?
    @ObservationIgnored private var signIn: CloudSignIn?
    @ObservationIgnored private var thumbTasks: Set<String> = []
    @ObservationIgnored private var fetchTasks: [String: Task<URL?, Never>] = [:]

    var isSignedIn: Bool { state != .signedOut }

    func start() {
        guard let session = CloudSession.load() else { state = .signedOut; return }
        if let e = session.expires, e < Date() { state = .failed("Your Insta360 sign-in expired."); return }
        client = CloudClient(session: session)
        Task { await refresh() }
    }

    func refresh() async {
        guard let client else { return }
        state = .loading
        do {
            // Sessions last ~30 days; renew in the last week so it never lapses while in use.
            if let e = client.session.expires, e.timeIntervalSinceNow < 7 * 86400 { try? await client.refresh() }
            let all = try await client.listAll()
            media = all.filter(\.isVideo).sorted { ($0.captureDate ?? .distantPast) < ($1.captureDate ?? .distantPast) }
            thumbnails = [:]; thumbTasks = []  // listing re-signs the cover URLs
            state = .ready
            if let info = try? await client.get(host: CloudClient.accountHost, path: "/cloud-subscription-service/user/v3/cloudBenefitInfo"),
               let used = info["baseInfo"]?["usage"]?.number, let total = info["baseInfo"]?["total"]?.number {
                quota = (used, total)
            }
        } catch {
            state = .failed("\(error)")
        }
    }

    func beginSignIn() {
        let s = CloudSignIn()
        signIn = s
        s.begin { [weak self] session in
            guard let self else { return }
            self.signIn = nil
            guard let session else { return }
            try? session.save()
            self.start()
        }
    }

    func signOut() {
        CloudSession.delete()
        client = nil
        media = []; thumbnails = [:]; quota = nil
        state = .signedOut
    }

    // MARK: Clips

    func url(for m: CloudMedia) -> URL { CloudCache.masterURL(mediaId: m.id, name: m.name) }
    func media(for url: URL) -> CloudMedia? { media.first { self.url(for: $0) == url } }

    /// Fetch the trailer and LRV (once); nil on failure (see `errors`).
    func fetch(_ m: CloudMedia) async -> URL? {
        if let t = fetchTasks[m.id] { return await t.value }
        guard let client else { return nil }
        errors[m.id] = nil
        fetching[m.id] = .trailer
        let task = Task<URL?, Never> { [weak self] in
            do {
                let url = try await CloudCache.fetch(m, client: client) { stage in
                    Task { @MainActor in if self?.fetching[m.id] != nil { self?.fetching[m.id] = stage } }
                }
                return url
            } catch {
                await MainActor.run { self?.errors[m.id] = Self.describe(error) }
                return nil
            }
        }
        fetchTasks[m.id] = task
        let url = await task.value
        fetchTasks[m.id] = nil
        fetching[m.id] = nil
        return url
    }

    /// Replace a clip's trailer stub with its full original.
    func downloadOriginal(_ clipURL: URL) async -> Bool {
        guard let client, let r = CloudCache.record(for: clipURL) else { return false }
        errors[r.mediaId] = nil
        fetching[r.mediaId] = .original(0)
        defer { fetching[r.mediaId] = nil }
        do {
            try await CloudCache.downloadOriginal(clipURL, client: client) { p in
                Task { @MainActor [weak self] in if self?.fetching[r.mediaId] != nil { self?.fetching[r.mediaId] = .original(p) } }
            }
            return true
        } catch {
            errors[r.mediaId] = Self.describe(error)
            return false
        }
    }

    func loadThumbnail(_ m: CloudMedia) {
        guard thumbnails[m.id] == nil, !thumbTasks.contains(m.id), let u = m.thumbnailURL, let client else { return }
        thumbTasks.insert(m.id)
        Task {
            guard let data = try? await client.fetch(u),
                  let src = CGImageSourceCreateWithData(data as CFData, nil),
                  let img = CGImageSourceCreateImageAtIndex(src, 0, nil) else { return }
            thumbnails[m.id] = img
        }
    }

    static func describe(_ e: Error) -> String {
        if let c = e as? CloudError { return c.description }
        return e.localizedDescription
    }
}

extension CloudCache.Stage {
    var label: String {
        switch self {
        case .trailer: return "Reading gyro and calibration…"
        case .proxy(let p): return "Fetching low-res proxy… \(Int(p * 100))%"
        case .original(let p): return "Downloading original… \(Int(p * 100))%"
        case .fromStudio: return "Using Insta360 Studio's download…"
        }
    }
    var fraction: Double? {
        switch self {
        case .trailer, .fromStudio: return nil
        case .proxy(let p), .original(let p): return p
        }
    }
}
