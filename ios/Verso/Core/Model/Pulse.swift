import Foundation

/// One weekly Marginalia record. Scales are 1...5.
public struct Pulse: Codable, Sendable, Identifiable, Hashable {
    public var id: UUID
    /// ISO-week Monday (start of day, local calendar).
    public var weekOf: Date
    public var energy: Int
    public var anchor: Int
    public var pull: Int
    public var restlessness: Int
    public var anchorReasons: [String]
    public var sentence: String?
    public var board: BoardSnapshot?

    public init(id: UUID = UUID(), weekOf: Date, energy: Int, anchor: Int, pull: Int, restlessness: Int,
                anchorReasons: [String] = [], sentence: String? = nil, board: BoardSnapshot? = nil) {
        self.id = id; self.weekOf = weekOf; self.energy = energy; self.anchor = anchor
        self.pull = pull; self.restlessness = restlessness; self.anchorReasons = anchorReasons
        self.sentence = sentence; self.board = board
    }

    public var toward: Double { Double(pull + restlessness) / 2 }
    public var away: Double { Double(energy + anchor) / 2 }
    /// −4 … +4
    public var water: Double { toward - away }

    public static let anchorChips = ["the team", "the money", "unfinished work", "timing", "fear", "nothing"]

    /// Word labels for each scale, 1 → 5.
    public static let energyWords = ["spent", "thin", "even", "full", "alive"]
    public static let anchorWords = ["loose", "light", "steady", "firm", "anchored"]
    public static let pullWords = ["faint", "murmur", "tug", "current", "magnetic"]
    public static let restlessnessWords = ["settled", "fidgety", "itchy", "pacing", "caged"]
}

/// The board as it stood when a Marginalia record was saved (drives the Drift view).
public struct BoardSnapshot: Codable, Sendable, Hashable {
    public var stage: Stage
    public var met: Int
    public var holding: Int
    public var total: Int
    public var dayRateAfter: Double?
    public var price: Double?
    public init(stage: Stage, met: Int, holding: Int, total: Int, dayRateAfter: Double? = nil, price: Double? = nil) {
        self.stage = stage; self.met = met; self.holding = holding; self.total = total
        self.dayRateAfter = dayRateAfter; self.price = price
    }
}
