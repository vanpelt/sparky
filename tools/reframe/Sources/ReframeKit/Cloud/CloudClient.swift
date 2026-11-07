import Foundation

// Insta360 Cloud (Insta360+) access through the private API Insta360 Studio uses. See CLOUD.md.
// No official API exists; this is personal-use access to the signed-in user's own media.

public enum CloudError: Error, CustomStringConvertible {
    case notSignedIn
    case expired
    case http(Int, String)
    case api(Int, String)
    case noHost([String])
    case badResponse(String)

    public var description: String {
        switch self {
        case .notSignedIn: return "not signed in (run: reframe-cli cloud login)"
        case .expired: return "session expired (run: reframe-cli cloud login)"
        case let .http(code, url): return "HTTP \(code) from \(url)"
        case let .api(code, msg): return "API error \(code): \(msg)"
        case let .noHost(tried): return "no media host answered; tried \(tried.joined(separator: ", "))"
        case let .badResponse(s): return "unexpected response: \(s)"
        }
    }
}

/// The signed-in session: a JWT (payload carries `openId` and `exp`) plus the regional media host
/// once discovered. Stored owner-only in Application Support; never printed.
public struct CloudSession: Codable, Sendable {
    public var token: String
    public var mediaHost: String?
    public var savedAt = Date()

    public init(token: String) { self.token = token }

    public var expires: Date? { CloudSession.claims(token)?["exp"].flatMap { ($0 as? NSNumber)?.doubleValue }.map(Date.init(timeIntervalSince1970:)) }

    public static var fileURL: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Reframe/cloud-session.json")
    }

    public static func load() -> CloudSession? {
        guard let d = try? Data(contentsOf: fileURL) else { return nil }
        return try? JSONDecoder().decode(CloudSession.self, from: d)
    }

    public func save() throws {
        let url = CloudSession.fileURL
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try JSONEncoder().encode(self).write(to: url, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }

    public static func delete() { try? FileManager.default.removeItem(at: fileURL) }

    /// JWT payload, unverified (only used to recognise the session token and read its expiry).
    public static func claims(_ jwt: String) -> [String: Any]? {
        let parts = jwt.split(separator: ".")
        guard parts.count >= 2 else { return nil }
        var b64 = parts[1].replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        while b64.count % 4 != 0 { b64 += "=" }
        guard let d = Data(base64Encoded: b64) else { return nil }
        return (try? JSONSerialization.jsonObject(with: d)) as? [String: Any]
    }

    /// The first JWT in `text` that looks like an Insta360 session token (payload has `openId`).
    public static func findToken(in text: String) -> String? {
        let allowed = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "-_."))
        var rest = text[...]
        while let r = rest.range(of: "eyJ") {
            var end = r.lowerBound
            while end < rest.endIndex, rest[end].unicodeScalars.allSatisfy(allowed.contains) { end = rest.index(after: end) }
            let candidate = String(rest[r.lowerBound..<end])
            if candidate.split(separator: ".").count >= 3, claims(candidate)?["openId"] != nil { return candidate }
            rest = rest[(end > r.lowerBound ? end : rest.index(after: r.lowerBound))...]
        }
        return nil
    }
}

/// One cloud recording (a logical media item; its files come from `files(for:)`).
public struct CloudMedia: Sendable {
    public var id: String
    public var name: String
    public var size: Int64?
    public var duration: Double?
    public var created: Date?
    /// `pano_video`, `pano_photo`, …
    public var kind: String?
    /// Signed 640×320 equirect cover (expires with the listing).
    public var thumbnailURL: URL?
    /// The raw item, for exploring fields the API returns.
    public var raw: [String: AnyCodableValue]

    /// Recording frame rate from the listing (`framerate`), e.g. 48 or 120.
    public var fps: Double? { raw["framerate"]?.number }

    public var isVideo: Bool { kind.map { $0.contains("video") } ?? name.lowercased().hasSuffix(".insv") }

    /// Recording time from the file name (VID_yyyyMMdd_HHmmss_…), in local time like
    /// `Clip.captureDate`, so cloud and local clips land on the same days.
    public var captureDate: Date? {
        let parts = name.split(separator: "_")
        guard parts.count >= 3 else { return created }
        let f = DateFormatter()
        f.dateFormat = "yyyyMMddHHmmss"
        f.timeZone = .current
        return f.date(from: String(parts[1] + parts[2])) ?? created
    }
}

public struct CloudFile: Sendable {
    public var name: String
    public var size: Int64
    /// Signed CDN URL (expires; resolve again on 403).
    public var url: URL
}

/// A JSON leaf/tree kept Sendable so raw responses can be carried around.
public indirect enum AnyCodableValue: Sendable, CustomStringConvertible {
    case string(String), number(Double), bool(Bool), null
    case array([AnyCodableValue]), object([String: AnyCodableValue])

    init(_ any: Any) {
        switch any {
        case let s as String: self = .string(s)
        case let n as NSNumber:
            self = CFGetTypeID(n) == CFBooleanGetTypeID() ? .bool(n.boolValue) : .number(n.doubleValue)
        case let a as [Any]: self = .array(a.map(AnyCodableValue.init))
        case let o as [String: Any]: self = .object(o.mapValues(AnyCodableValue.init))
        default: self = .null
        }
    }

    public var string: String? {
        switch self {
        case let .string(s): return s
        case let .number(n): return n == n.rounded() && abs(n) < 1e15 ? String(Int64(n)) : String(n)
        default: return nil
        }
    }
    public var number: Double? {
        switch self {
        case let .number(n): return n
        case let .string(s): return Double(s)
        default: return nil
        }
    }
    public subscript(_ key: String) -> AnyCodableValue? { if case let .object(o) = self { return o[key] } else { return nil } }
    public var array: [AnyCodableValue]? { if case let .array(a) = self { return a } else { return nil } }
    public var object: [String: AnyCodableValue]? { if case let .object(o) = self { return o } else { return nil } }

    /// Short, credential-safe rendering: URL queries and long opaque strings are elided.
    public var description: String {
        switch self {
        case let .string(s): return "\"\(CloudClient.redact(s))\""
        case let .number(n): return n == n.rounded() && abs(n) < 1e15 ? String(Int64(n)) : String(n)
        case let .bool(b): return String(b)
        case .null: return "null"
        case let .array(a): return "[\(a.count) items]"
        case let .object(o): return "{\(o.count) keys}"
        }
    }
}

public final class CloudClient: @unchecked Sendable {
    public static let accountHost = "openapi-g.insta360.com"
    /// Regional media hosts seen in Studio and the community clients; probed in order when the
    /// account doesn't say which one it lives on.
    public static let candidateHosts = ["service-g.insta360.com", "service-fra.insta360.com", "service-sg.insta360.com",
                                        "service-va.insta360.com"]
    public static let loginURL = URL(string: "https://cloud.insta360.com/account/studio_login/?v=2&studio_language=English")!

    public private(set) var session: CloudSession
    private let http: URLSession
    private let equipment: String

    public init(session: CloudSession) {
        self.session = session
        let cfg = URLSessionConfiguration.ephemeral
        cfg.httpAdditionalHeaders = ["User-Agent": "insta360/studio"]
        http = URLSession(configuration: cfg)
        // Stable per-install id, like Studio's X-Equipment-Code.
        let key = "cloudEquipmentCode"
        if let e = UserDefaults.standard.string(forKey: key) { equipment = e } else {
            equipment = UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased()
            UserDefaults.standard.set(equipment, forKey: key)
        }
    }

    public static func signedIn() throws -> CloudClient {
        guard let s = CloudSession.load() else { throw CloudError.notSignedIn }
        if let e = s.expires, e < Date() { throw CloudError.expired }
        return CloudClient(session: s)
    }

    // MARK: Requests

    private func headers() -> [String: String] {
        let trace = (0..<2).map { _ in String(format: "%016llx", UInt64.random(in: 0...UInt64.max)) }.joined()
        return [
            "Authentication": session.token,
            "X-User-Token": session.token,
            "x-insta360-platform": "pc/mac",
            "app_version": "6.0.8",
            "app_traffic": "release",
            "X-Equipment-Code": equipment,
            "X-Language": "en",
            "X-MainLand": "0",
            "X-Insta360-Trace-Id": "pc" + trace,
            "Referer": "https://cloud.insta360.com/",
        ]
    }

    /// GET a JSON API path; returns the envelope's `data` (code 0), throws otherwise.
    public func get(host: String, path: String, query: [String: String] = [:]) async throws -> AnyCodableValue {
        var c = URLComponents()
        c.scheme = "https"; c.host = host; c.path = path
        if !query.isEmpty { c.queryItems = query.sorted { $0.key < $1.key }.map { URLQueryItem(name: $0.key, value: $0.value) } }
        var req = URLRequest(url: c.url!)
        for (k, v) in headers() { req.setValue(v, forHTTPHeaderField: k) }
        req.timeoutInterval = 30
        let (data, resp) = try await http.data(for: req)
        let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard status == 200 else { throw CloudError.http(status, "\(host)\(path)") }
        guard let obj = try? JSONSerialization.jsonObject(with: data) else {
            throw CloudError.badResponse(String(decoding: data.prefix(160), as: UTF8.self))
        }
        let v = AnyCodableValue(obj)
        let code = Int(v["code"]?.number ?? v["errCode"]?.number ?? 0)
        guard code == 0 else {
            throw CloudError.api(code, v["msg"]?.string ?? v["errInfo"]?.string ?? v["message"]?.string ?? "")
        }
        return v["data"] ?? .null
    }

    // MARK: Account

    public func profile() async throws -> AnyCodableValue {
        do { return try await get(host: Self.accountHost, path: "/account/v1/getProfileByToken") } catch {
            return try await get(host: Self.accountHost, path: "/account/v2/getProfile")
        }
    }

    public func regionalDomain() async throws -> AnyCodableValue {
        try await get(host: Self.accountHost, path: "/account/service/getRegionalDomain")
    }

    /// Mint a fresh token from the current one (sessions last ~30 days).
    public func refresh() async throws {
        let d = try await get(host: Self.accountHost, path: "/account/v2/refreshToken")
        guard let t = d["token"]?.string, CloudSession.claims(t) != nil else { throw CloudError.badResponse("refresh had no token") }
        session.token = t
        session.savedAt = Date()
        try session.save()
    }

    // MARK: Media

    /// The media host for this account: remembered, else the first candidate that lists media.
    public func mediaHost(log: ((String) -> Void)? = nil) async throws -> String {
        if let h = session.mediaHost { return h }
        var tried: [String] = []
        var hosts = Self.candidateHosts
        // The subscription info names the account's media endpoint (baseInfo.endpoint,
        // e.g. https://service-va.insta360.com for US accounts).
        if let d = try? await get(host: Self.accountHost, path: "/cloud-subscription-service/user/v3/cloudBenefitInfo"),
           let named = d["baseInfo"]?["endpoint"]?.string.flatMap({ URL(string: $0)?.host }) ?? Self.hosts(in: d).first(where: { $0.hasPrefix("service") }) {
            hosts.insert(named, at: 0)
        }
        for h in hosts where !tried.contains(h) {
            tried.append(h)
            do {
                _ = try await get(host: h, path: "/cloud/service/media/view/list", query: ["pageNumber": "1", "pageSize": "1"])
                log?("media host: \(h)")
                session.mediaHost = h
                try? session.save()
                return h
            } catch { log?("  \(h): \(error)") }
        }
        throw CloudError.noHost(tried)
    }

    /// Every recording in the cloud, newest first as the API returns them.
    public func listAll(pageSize: Int = 100, log: ((String) -> Void)? = nil) async throws -> [CloudMedia] {
        let host = try await mediaHost(log: log)
        var out: [CloudMedia] = []
        var page = 1
        while true {
            let d = try await get(host: host, path: "/cloud/service/media/view/list",
                                  query: ["pageNumber": String(page), "pageSize": String(pageSize)])
            let items = Self.items(in: d)
            out += items.compactMap(Self.media)
            let total = Int(d["totalCount"]?.number ?? d["total"]?.number ?? -1)
            if items.count < pageSize || (total >= 0 && out.count >= total) || page > 100 { break }
            page += 1
        }
        return out
    }

    public func detail(_ id: String) async throws -> AnyCodableValue {
        try await get(host: try await mediaHost(), path: "/cloud/service/media/view/detail", query: ["mediaId": id])
    }

    public func downloadInfo(_ id: String) async throws -> AnyCodableValue {
        try await get(host: try await mediaHost(), path: "/cloud/service/media/download", query: ["mediaId": id])
    }

    /// Signed URLs for the recording's files (original .insv parts and the .lrv proxy).
    public func files(for id: String) async throws -> [CloudFile] {
        let d = try await downloadInfo(id)
        let list = d["paths"]?.array ?? d["files"]?.array ?? d["fileList"]?.array ?? d.array ?? []
        return list.compactMap { it in
            guard let s = (it["url"] ?? it["downloadUrl"] ?? it["download_url"])?.string, let u = URL(string: s) else { return nil }
            let name = (it["name"] ?? it["fileName"])?.string ?? u.lastPathComponent
            return CloudFile(name: name, size: Int64((it["size"] ?? it["fileSize"])?.number ?? 0), url: u)
        }
    }

    /// GET a CDN asset that needs the session headers (cover images do; signed file URLs don't).
    public func fetch(_ url: URL) async throws -> Data {
        var req = URLRequest(url: url)
        for (k, v) in headers() { req.setValue(v, forHTTPHeaderField: k) }
        req.timeoutInterval = 30
        let (data, resp) = try await http.data(for: req)
        let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard status / 100 == 2 else { throw CloudError.http(status, url.lastPathComponent) }
        return data
    }

    // MARK: Byte ranges

    public struct RangeResult: Sendable {
        public var status: Int
        public var contentRange: String?
        public var acceptRanges: String?
        public var data: Data
        public var seconds: Double
    }

    /// GET bytes [from, to] (inclusive) of a CDN URL. Signed file URLs carry their own auth; the
    /// API headers are sent too, which DASH proxy URLs may need.
    public func range(_ url: URL, from: Int64, to: Int64, apiHeaders: Bool = false) async throws -> RangeResult {
        var req = URLRequest(url: url)
        req.setValue("bytes=\(from)-\(to)", forHTTPHeaderField: "Range")
        if apiHeaders { for (k, v) in headers() { req.setValue(v, forHTTPHeaderField: k) } }
        req.timeoutInterval = 60
        let t0 = Date()
        let (data, resp) = try await http.data(for: req)
        let h = resp as? HTTPURLResponse
        return RangeResult(status: h?.statusCode ?? 0, contentRange: h?.value(forHTTPHeaderField: "Content-Range"),
                           acceptRanges: h?.value(forHTTPHeaderField: "Accept-Ranges"), data: data,
                           seconds: Date().timeIntervalSince(t0))
    }

    // MARK: Parsing helpers

    static func items(in d: AnyCodableValue) -> [AnyCodableValue] {
        if let a = d.array { return a }
        for k in ["list", "mediaList", "records", "items", "data", "medias"] {
            if let a = d[k]?.array { return a }
        }
        return []
    }

    static func media(_ v: AnyCodableValue) -> CloudMedia? {
        guard let o = v.object, let id = (v["mediaId"] ?? v["id"] ?? v["uuid"])?.string else { return nil }
        let name = (v["mediaName"] ?? v["originName"] ?? v["fileName"] ?? v["name"])?.string ?? id
        let size = (v["downOriginalSize"] ?? v["mediaSize"] ?? v["fileSize"] ?? v["size"])?.number.map { Int64($0) }
        var dur = (v["duration"] ?? v["durations"] ?? v["videoDuration"])?.number
        if let d = dur, d > 10_000 { dur = d / 1000 }  // ms
        let t = (v["mediaTime"] ?? v["shootTime"] ?? v["createTime"] ?? v["uploadTime"])?.number
        let created = t.map { Date(timeIntervalSince1970: $0 > 1e11 ? $0 / 1000 : $0) }
        let thumb = (v["thumbPath"] ?? v["coverUrl"] ?? v["planeThumbPath"])?.string.flatMap(URL.init(string:))
        return CloudMedia(id: id, name: name, size: size, duration: dur, created: created,
                          kind: v["mediaType"]?.string, thumbnailURL: thumb, raw: o)
    }

    static func hosts(in v: AnyCodableValue) -> [String] {
        switch v {
        case let .string(s):
            let h = URL(string: s)?.host ?? s
            return h.hasSuffix(".insta360.com") ? [h] : []
        case let .array(a): return a.flatMap(hosts)
        case let .object(o): return o.values.flatMap(hosts)
        default: return []
        }
    }

    /// Strip query strings and elide long opaque values so output never shows credentials.
    public static func redact(_ s: String) -> String {
        var t = s
        if let q = t.firstIndex(of: "?"), t.hasPrefix("http") { t = String(t[..<q]) + "?…" }
        if t.hasPrefix("eyJ") || (t.count > 40 && !t.contains("/") && !t.contains(" ")) { return "<\(t.count) chars>" }
        return t.count > 140 ? String(t.prefix(140)) + "…" : t
    }
}
