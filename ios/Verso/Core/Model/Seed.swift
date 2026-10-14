import Foundation

/// The shape of Seed/grant.local.json (gitignored, real) and Seed/grant.example.json (committed, fake).
/// Every field except the share counts has a default; missing keys are tolerated.
public struct SeedFile: Codable, Sendable, Hashable {
    public var version: Int
    public var grant: Grant
    public var enoughLinePerDay: Double?

    enum CodingKeys: String, CodingKey { case version, grant, enoughLinePerDay }
    public init(version: Int = 1, grant: Grant, enoughLinePerDay: Double? = nil) {
        self.version = version; self.grant = grant; self.enoughLinePerDay = enoughLinePerDay
    }
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        version = try c.decodeIfPresent(Int.self, forKey: .version) ?? 1
        grant = try c.decode(Grant.self, forKey: .grant)
        enoughLinePerDay = try c.decodeIfPresent(Double.self, forKey: .enoughLinePerDay)
    }

    public static let resourceName = "grant.local"

    /// Decode a seed from raw JSON.
    public static func decode(_ data: Data) throws -> SeedFile {
        try VersoJSON.decoder().decode(SeedFile.self, from: data)
    }

    /// The bundled seed, if the "Copy local seed (if present)" build phase found one.
    public static func bundled(in bundle: Bundle = .main) -> SeedFile? {
        guard let url = bundle.url(forResource: resourceName, withExtension: "json"),
              let data = try? Data(contentsOf: url) else { return nil }
        return try? decode(data)
    }

    public static func isBundled(in bundle: Bundle = .main) -> Bool {
        bundle.url(forResource: resourceName, withExtension: "json") != nil
    }
}

/// Shared JSON coding: ISO-8601 dates (fractional seconds tolerated, and plain yyyy-MM-dd dates in seeds).
public enum VersoJSON {
    public static func encoder() -> JSONEncoder {
        let e = JSONEncoder()
        e.outputFormatting = [.prettyPrinted, .sortedKeys]
        e.dateEncodingStrategy = .custom { date, enc in
            var c = enc.singleValueContainer()
            try c.encode(date.formatted(.iso8601))
        }
        return e
    }

    public static func decoder() -> JSONDecoder {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { dec in
            let c = try dec.singleValueContainer()
            if let n = try? c.decode(Double.self) { return Date(timeIntervalSince1970: n) }
            let s = try c.decode(String.self)
            if let date = parseDate(s) { return date }
            throw DecodingError.dataCorruptedError(in: c, debugDescription: "Unrecognised date: \(s)")
        }
        return d
    }

    static func parseDate(_ s: String) -> Date? {
        if let d = try? Date(s, strategy: .iso8601) { return d }
        if let d = try? Date(s, strategy: Date.ISO8601FormatStyle(includingFractionalSeconds: true)) { return d }
        // Plain calendar date: interpret at local midnight.
        let parts = s.split(separator: "-").compactMap { Int($0) }
        if parts.count == 3 {
            return Calendar.current.date(from: DateComponents(year: parts[0], month: parts[1], day: parts[2]))
        }
        return nil
    }
}
