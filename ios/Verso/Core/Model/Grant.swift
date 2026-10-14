import Foundation

/// One row of the vest schedule. `day` overrides `Grant.vestDayOfMonth` for this tranche.
public struct Tranche: Codable, Sendable, Hashable {
    public var year: Int
    public var month: Int
    public var day: Int?
    public var fraction: Double

    public init(year: Int, month: Int, day: Int? = nil, fraction: Double) {
        self.year = year; self.month = month; self.day = day; self.fraction = fraction
    }

    /// The standard grant shape (not secret): 25% cliff at May 2026, then 6.25% quarterly through May 2029.
    public static let standard: [Tranche] = {
        var rows = [Tranche(year: 2026, month: 5, fraction: 0.25)]
        var y = 2026, m = 5
        for _ in 0..<12 {
            m += 3
            if m > 12 { m -= 12; y += 1 }
            rows.append(Tranche(year: y, month: m, fraction: 0.0625))
        }
        return rows
    }()
}

/// The equity grant plus pay. Every field except the share counts has a default, and decoding
/// tolerates missing keys so seed files and older book.json versions both load.
public struct Grant: Codable, Sendable, Hashable {
    public var revestShares: Double
    public var rsuShares: Double
    public var rolloverShares: Double
    public var rolloverVestDate: Date?
    public var schedule: [Tranche]
    public var vestDayOfMonth: Int
    public var bookOneStart: Date?
    public var salaryAnnual: Double
    public var bonusAnnual: Double
    public var taxRevest: Double
    public var revestSheltered: Bool
    public var taxOrdinary: Double
    public var isSample: Bool
    public var seededFrom: String?
    public var updatedAt: Date

    public init(
        revestShares: Double, rsuShares: Double, rolloverShares: Double = 0, rolloverVestDate: Date? = nil,
        schedule: [Tranche] = Tranche.standard, vestDayOfMonth: Int = 20, bookOneStart: Date? = nil,
        salaryAnnual: Double = 0, bonusAnnual: Double = 0,
        taxRevest: Double = 0.238, revestSheltered: Bool = false, taxOrdinary: Double = 0.503,
        isSample: Bool = false, seededFrom: String? = nil, updatedAt: Date = Date()
    ) {
        self.revestShares = revestShares; self.rsuShares = rsuShares
        self.rolloverShares = rolloverShares; self.rolloverVestDate = rolloverVestDate
        self.schedule = schedule; self.vestDayOfMonth = vestDayOfMonth; self.bookOneStart = bookOneStart
        self.salaryAnnual = salaryAnnual; self.bonusAnnual = bonusAnnual
        self.taxRevest = taxRevest; self.revestSheltered = revestSheltered; self.taxOrdinary = taxOrdinary
        self.isSample = isSample; self.seededFrom = seededFrom; self.updatedAt = updatedAt
    }

    /// τR: the effective revest rate (0 when sheltered by loss carryforward, matching futr's trancheCash).
    public var effectiveRevestRate: Double { revestSheltered ? 0 : taxRevest }

    enum CodingKeys: String, CodingKey {
        case revestShares, rsuShares, rolloverShares, rolloverVestDate, schedule, vestDayOfMonth, bookOneStart
        case salaryAnnual, bonusAnnual, taxRevest, revestSheltered, taxOrdinary, isSample, seededFrom, updatedAt
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        revestShares = try c.decodeIfPresent(Double.self, forKey: .revestShares) ?? 0
        rsuShares = try c.decodeIfPresent(Double.self, forKey: .rsuShares) ?? 0
        rolloverShares = try c.decodeIfPresent(Double.self, forKey: .rolloverShares) ?? 0
        rolloverVestDate = try c.decodeIfPresent(Date.self, forKey: .rolloverVestDate)
        schedule = try c.decodeIfPresent([Tranche].self, forKey: .schedule) ?? Tranche.standard
        vestDayOfMonth = try c.decodeIfPresent(Int.self, forKey: .vestDayOfMonth) ?? 20
        bookOneStart = try c.decodeIfPresent(Date.self, forKey: .bookOneStart)
        salaryAnnual = try c.decodeIfPresent(Double.self, forKey: .salaryAnnual) ?? 0
        bonusAnnual = try c.decodeIfPresent(Double.self, forKey: .bonusAnnual) ?? 0
        taxRevest = try c.decodeIfPresent(Double.self, forKey: .taxRevest) ?? 0.238
        revestSheltered = try c.decodeIfPresent(Bool.self, forKey: .revestSheltered) ?? false
        taxOrdinary = try c.decodeIfPresent(Double.self, forKey: .taxOrdinary) ?? 0.503
        isSample = try c.decodeIfPresent(Bool.self, forKey: .isSample) ?? false
        seededFrom = try c.decodeIfPresent(String.self, forKey: .seededFrom)
        updatedAt = try c.decodeIfPresent(Date.self, forKey: .updatedAt) ?? Date()
    }
}
