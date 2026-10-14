import Foundation

/// One vest tranche ("chapter"), valued at today's price.
public struct TrancheRow: Sendable, Hashable, Identifiable {
    public let index: Int
    public let date: Date
    public let fraction: Double
    public let isVested: Bool
    public let revestShares: Double
    public let rsuShares: Double
    public let revestPre: Double
    public let rsuPre: Double
    public let revestAfter: Double
    public let rsuAfter: Double

    public var id: Int { index }
    public var shares: Double { revestShares + rsuShares }
    public var pre: Double { revestPre + rsuPre }
    public var after: Double { revestAfter + rsuAfter }
    /// "I" … "XIII"
    public var numeral: String { Roman.chapter(index) }
}

/// The marginal view of one unvested chapter (§5.5).
public struct ChapterLine: Sendable, Hashable, Identifiable {
    public let index: Int
    public let date: Date
    /// Days of Book One this chapter costs: max(today, previous vest) → this vest.
    public let gap: Int
    /// after_j + wageRateAfter·gap
    public let value: Double
    public let rate: Double
    /// Rises 1/N → 1/1.
    public let shareOfRemaining: Double
    /// Σ after_i (i ≤ j) + rollover — everything Book One will have paid through this chapter.
    public let bankedThrough: Double
    /// after_j / bankedThrough — FALLS. The honest diminishing signal.
    public let ebbWeight: Double
    public var id: Int { index }
    public var numeral: String { Roman.chapter(index) }
}

/// The next vest (§5.4).
public struct NextVest: Sendable, Hashable {
    public let index: Int
    public let date: Date
    /// ≥ 1 (on vest day the tranche is already vested).
    public let daysToNext: Int
    public let prevDate: Date
    /// 0…1 across the current chapter.
    public let chapterProgress: Double
    public let after: Double
    public let pre: Double
    /// (after_n + wageRateAfter·daysToNext) / daysToNext
    public let rate: Double
    /// The honest exit day: vest date + 1.
    public let launchWindow: Date
    public var numeral: String { Roman.chapter(index) }
}

/// One choice on the Last Page scrubber (§5.6). `index == nil` means "now".
public struct LastPageChoice: Sendable, Hashable, Identifiable {
    public let index: Int?
    /// The day he would leave: today for "now", otherwise vest date + 1.
    public let leaveOn: Date
    public let kept: Double
    public let forfeit: Double
    public let daysBack: Int
    public let impliedRate: Double
    public var id: Int { index ?? -1 }
    public var label: String { index.map(Roman.chapter) ?? "Now" }
}

public enum Crossover: Sendable, Hashable {
    /// The CRWV price below which staying pays less than the Enough Line.
    case price(Double)
    /// Salary alone already clears the line.
    case payAloneClears
    /// Nothing left to vest.
    case none
}

public struct EnoughReading: Sendable, Hashable {
    public let line: Double
    /// dayRateAfter − E. Positive = above the line.
    public let margin: Double
    public let crossover: Crossover
    /// "$1 of CRWV = $X of remaining after-tax".
    public let priceSensitivity: Double
    public var isAbove: Bool { margin >= 0 }
}

/// Everything §5 derives. All money is at today's price; nothing is projected.
public struct LedgerResult: Sendable, Hashable {
    public let price: Double
    public let today: Date
    public let tranches: [TrancheRow]
    public let unvested: [TrancheRow]
    /// d_12 — the last tranche date.
    public let finalDate: Date
    /// D = max(1, days(today → F))
    public let daysToFinal: Int
    /// k = REV·(1−τR) + RSU·(1−τO)
    public let k: Double
    /// Σ_U f_i — the share of the grant still unread.
    public let fuel: Double

    public let remRevestShares: Double
    public let remRsuShares: Double
    public let remRevestPre: Double
    public let remRsuPre: Double
    public let remRevestAfter: Double
    public let remRsuAfter: Double

    public let wageRateAfter: Double
    public let wageRatePre: Double
    public let wagesAfter: Double
    public let wagesPre: Double
    /// HERO: the equivalent daily wage he walks away from (after tax, per calendar day).
    public let dayRateAfter: Double
    public let dayRatePre: Double

    public let rolloverShares: Double
    /// ROLL·px·(1−τO). Already his; never in any walking-away figure.
    public let rolloverValue: Double
    /// True while the rollover has not yet vested (dR > today, or dR unknown and it is still 2026).
    public let rolloverPending: Bool

    public let next: NextVest?
    public let chapters: [ChapterLine]
    /// "Now" first, then one choice per unvested chapter.
    public let lastPage: [LastPageChoice]
    public let enough: EnoughReading?

    public var isComplete: Bool { unvested.isEmpty }
    public var remShares: Double { remRevestShares + remRsuShares }
    public var remPre: Double { remRevestPre + remRsuPre }
    public var remAfter: Double { remRevestAfter + remRsuAfter }
    public var equityPerDay: Double { remAfter / Double(daysToFinal) }
    public var payPerDay: Double { wageRateAfter }
    /// Everything he walks away from by leaving today (after tax).
    public var walkAwayAfter: Double { remAfter + wagesAfter }
    public var walkAwayPre: Double { remPre + wagesPre }
    public var chaptersLeft: Int { unvested.count }
    /// The current chapter = the next unvested tranche; the last one once complete.
    public var currentIndex: Int { next?.index ?? (tranches.count - 1) }
    public var vestedAfterTotal: Double { tranches.filter(\.isVested).reduce(0) { $0 + $1.after } }

    public static func perWorkingDay(_ x: Double) -> Double { x * 365 / 261 }

    /// The day rate and its parts as the reader chose to see them (the Ledger toggles, which the Day Rate
    /// widget follows). One place for the arithmetic so the app and the widget can't disagree.
    public func dayRate(_ v: DayRateView) -> Double { v.scale(v.afterTax ? dayRateAfter : dayRatePre) }
    public func equityRate(_ v: DayRateView) -> Double {
        v.scale(v.afterTax ? equityPerDay : (daysToFinal > 0 ? remPre / Double(daysToFinal) : 0))
    }
    public func payRate(_ v: DayRateView) -> Double { v.scale(v.afterTax ? payPerDay : wageRatePre) }
    public func walkAway(_ v: DayRateView) -> Double { v.afterTax ? walkAwayAfter : walkAwayPre }

    /// The chapter line for a tranche index, if it is unvested.
    public func chapter(_ index: Int) -> ChapterLine? { chapters.first { $0.index == index } }
}

public enum Ledger {
    /// The tranche dates for a grant (§5.2), day clamped to month length.
    public static func trancheDates(_ grant: Grant, calendar: Calendar = .current) -> [Date] {
        grant.schedule.map { Days.date(year: $0.year, month: $0.month, day: $0.day ?? grant.vestDayOfMonth, calendar: calendar) }
    }

    /// Pure: §5 from a grant, a price and a day. `today` is normalised to the start of its day.
    public static func compute(grant: Grant, price px: Double, today now: Date, calendar: Calendar = .current,
                               enoughLine E: Double? = nil) -> LedgerResult {
        let today = calendar.startOfDay(for: now)
        let REV = grant.revestShares, RSU = grant.rsuShares
        let tR = grant.effectiveRevestRate, tO = grant.taxOrdinary
        let dates = trancheDates(grant, calendar: calendar)

        let rows: [TrancheRow] = grant.schedule.enumerated().map { i, t in
            let f = t.fraction
            return TrancheRow(index: i, date: dates[i], fraction: f, isVested: dates[i] <= today,
                              revestShares: REV * f, rsuShares: RSU * f,
                              revestPre: REV * f * px, rsuPre: RSU * f * px,
                              revestAfter: REV * f * px * (1 - tR), rsuAfter: RSU * f * px * (1 - tO))
        }
        let U = rows.filter { !$0.isVested }
        let finalDate = dates.last ?? today
        let D = max(1, Days.between(today, finalDate, calendar: calendar))
        let k = REV * (1 - tR) + RSU * (1 - tO)
        let fuel = U.reduce(0) { $0 + $1.fraction }

        let wageRateAfter = (grant.salaryAnnual + grant.bonusAnnual) * (1 - tO) / 365
        let wageRatePre = (grant.salaryAnnual + grant.bonusAnnual) / 365

        let complete = U.isEmpty
        let wagesAfter = complete ? 0 : wageRateAfter * Double(D)
        let wagesPre = complete ? 0 : wageRatePre * Double(D)

        let remRevSh = U.reduce(0) { $0 + $1.revestShares }, remRsuSh = U.reduce(0) { $0 + $1.rsuShares }
        let remRevPre = U.reduce(0) { $0 + $1.revestPre }, remRsuPre = U.reduce(0) { $0 + $1.rsuPre }
        let remRevAfter = U.reduce(0) { $0 + $1.revestAfter }, remRsuAfter = U.reduce(0) { $0 + $1.rsuAfter }
        let remAfter = remRevAfter + remRsuAfter, remPre = remRevPre + remRsuPre

        let dayRateAfter = complete ? 0 : (remAfter + wagesAfter) / Double(D)
        let dayRatePre = complete ? 0 : (remPre + wagesPre) / Double(D)

        // Rollover: already his.
        let rollValue = grant.rolloverShares * px * (1 - tO)
        let rollPending: Bool = {
            guard grant.rolloverShares > 0 else { return false }
            if let dR = grant.rolloverVestDate { return calendar.startOfDay(for: dR) > today }
            return calendar.component(.year, from: today) == 2026
        }()

        // Next vest (§5.4)
        var next: NextVest?
        if let n = U.first {
            let daysToNext = max(1, Days.between(today, n.date, calendar: calendar))
            let prevDate: Date = n.index > 0 ? dates[n.index - 1]
                : (grant.bookOneStart.map { calendar.startOfDay(for: $0) } ?? Days.add(-91, to: n.date, calendar: calendar))
            let span = max(1, Days.between(prevDate, n.date, calendar: calendar))
            let progress = min(1, max(0, Double(Days.between(prevDate, today, calendar: calendar)) / Double(span)))
            next = NextVest(index: n.index, date: n.date, daysToNext: daysToNext, prevDate: prevDate,
                            chapterProgress: progress, after: n.after, pre: n.pre,
                            rate: (n.after + wageRateAfter * Double(daysToNext)) / Double(daysToNext),
                            launchWindow: Days.add(1, to: n.date, calendar: calendar))
        }

        // Marginal chapters and Ebb (§5.5)
        var chapters: [ChapterLine] = []
        var values: [Double] = []
        var gaps: [Int] = []
        for j in U {
            let prev = j.index > 0 ? max(today, dates[j.index - 1]) : today
            let gap = max(1, Days.between(prev, j.date, calendar: calendar))
            gaps.append(gap)
            values.append(j.after + wageRateAfter * Double(gap))
        }
        for (pos, j) in U.enumerated() {
            let tail = values[pos...].reduce(0, +)
            let banked = rows.prefix(through: j.index).reduce(0) { $0 + $1.after } + rollValue
            chapters.append(ChapterLine(index: j.index, date: j.date, gap: gaps[pos], value: values[pos],
                                        rate: values[pos] / Double(gaps[pos]),
                                        shareOfRemaining: tail > 0 ? values[pos] / tail : 0,
                                        bankedThrough: banked, ebbWeight: banked > 0 ? j.after / banked : 0))
        }

        // The Last Page (§5.6)
        var lastPage = [LastPageChoice(index: nil, leaveOn: today, kept: 0, forfeit: remAfter + wagesAfter,
                                       daysBack: complete ? 0 : D,
                                       impliedRate: complete ? 0 : (remAfter + wagesAfter) / Double(D))]
        var cumulative = 0.0
        for j in U {
            cumulative += j.after
            let kept = cumulative + wageRateAfter * Double(max(0, Days.between(today, j.date, calendar: calendar)))
            let forfeit = max(0, (remAfter + wagesAfter) - kept)
            let back = max(0, Days.between(j.date, finalDate, calendar: calendar))
            lastPage.append(LastPageChoice(index: j.index, leaveOn: Days.add(1, to: j.date, calendar: calendar),
                                           kept: kept, forfeit: forfeit, daysBack: back,
                                           impliedRate: back > 0 ? forfeit / Double(back) : 0))
        }

        // Enough Line (§5.7)
        var enough: EnoughReading?
        if let E {
            let sens = k * fuel
            let cross: Crossover
            if E <= wageRateAfter { cross = .payAloneClears }
            else if fuel == 0 { cross = .none }
            else { cross = .price((E - wageRateAfter) * Double(D) / sens) }
            enough = EnoughReading(line: E, margin: dayRateAfter - E, crossover: cross, priceSensitivity: sens)
        }

        return LedgerResult(
            price: px, today: today, tranches: rows, unvested: U, finalDate: finalDate, daysToFinal: D, k: k, fuel: fuel,
            remRevestShares: remRevSh, remRsuShares: remRsuSh, remRevestPre: remRevPre, remRsuPre: remRsuPre,
            remRevestAfter: remRevAfter, remRsuAfter: remRsuAfter,
            wageRateAfter: wageRateAfter, wageRatePre: wageRatePre, wagesAfter: wagesAfter, wagesPre: wagesPre,
            dayRateAfter: dayRateAfter, dayRatePre: dayRatePre,
            rolloverShares: grant.rolloverShares, rolloverValue: rollValue, rolloverPending: rollPending,
            next: next, chapters: chapters, lastPage: lastPage, enough: enough)
    }
}

/// How a day rate is stated: after or before tax, per calendar day or per working day (261 a year).
public struct DayRateView: Codable, Sendable, Hashable {
    public var afterTax: Bool
    public var perWorkingDay: Bool
    public init(afterTax: Bool = true, perWorkingDay: Bool = false) {
        self.afterTax = afterTax; self.perWorkingDay = perWorkingDay
    }
    public func scale(_ x: Double) -> Double { perWorkingDay ? LedgerResult.perWorkingDay(x) : x }
    /// "day" or "working day".
    public var unit: String { perWorkingDay ? "working day" : "day" }
    /// "/day" or "/workday", for tight widget lines.
    public var shortUnit: String { perWorkingDay ? "/workday" : "/day" }
    /// "PER DAY, AFTER TAX" and friends.
    public var caption: String { "PER \(unit.uppercased()), \(afterTax ? "AFTER TAX" : "PRE-TAX")" }
}
