import Foundation

/// The ordinal stage word. Never a score.
public enum Stage: String, Codable, Sendable, Hashable, CaseIterable {
    case exposition, risingAction, crisis, turningPoint, recto

    public var title: String {
        switch self {
        case .exposition: "Exposition"
        case .risingAction: "Rising Action"
        case .crisis: "The Crisis"
        case .turningPoint: "The Turning Point"
        case .recto: "Recto"
        }
    }

    /// E / R / C / T / V — for the accessoryCircular centre.
    public var initial: String {
        switch self {
        case .exposition: "E"
        case .risingAction: "R"
        case .crisis: "C"
        case .turningPoint: "T"
        case .recto: "V"
        }
    }

    public var ordinal: Int { Stage.allCases.firstIndex(of: self) ?? 0 }
}

public enum TickState: String, Codable, Sendable, Hashable {
    case unmet, met, holding
    /// ○ ● ❦
    public var mark: String {
        switch self { case .unmet: "○"; case .met: "●"; case .holding: "❦" }
    }
}

/// How dollars may appear outside the app.
public struct VeilPolicy: Codable, Sendable, Hashable {
    public var veilByDefault: Bool
    public var showFiguresOnLockScreen: Bool
    public init(veilByDefault: Bool = true, showFiguresOnLockScreen: Bool = false) {
        self.veilByDefault = veilByDefault; self.showFiguresOnLockScreen = showFiguresOnLockScreen
    }
}

/// A clause as a widget shows it.
public struct ClauseLine: Codable, Sendable, Identifiable, Hashable {
    public var id: UUID
    public var text: String
    public var role: ClauseRole
    public var state: TickState
    public var hint: String
    public init(id: UUID, text: String, role: ClauseRole, state: TickState, hint: String) {
        self.id = id; self.text = text; self.role = role; self.state = state; self.hint = hint
    }
}

public struct ThreadSummary: Codable, Sendable, Identifiable, Hashable {
    public var id: UUID
    public var name: String
    public var binding: Binding
    public var kindTag: String
    public var progress: Double
    /// "7 / 12"
    public var label: String
    /// "3/4 this week"
    public var periodLabel: String
    public var lastEntryAt: Date?
    public var status: MomentumStatus
    public var pace: Double
    /// "+1" | "Done today" | "+20 min" | "Next scene"
    public var logLabel: String
    public var defaultAmount: Double
    /// Monday-start dots for the current week (habit) — 7 entries.
    public var weekDots: [Bool]
    public var sortIndex: Int

    public init(id: UUID, name: String, binding: Binding, kindTag: String, progress: Double, label: String,
                periodLabel: String, lastEntryAt: Date?, status: MomentumStatus, pace: Double, logLabel: String,
                defaultAmount: Double, weekDots: [Bool], sortIndex: Int) {
        self.id = id; self.name = name; self.binding = binding; self.kindTag = kindTag; self.progress = progress
        self.label = label; self.periodLabel = periodLabel; self.lastEntryAt = lastEntryAt; self.status = status
        self.pace = pace; self.logLabel = logLabel; self.defaultAmount = defaultAmount; self.weekDots = weekDots
        self.sortIndex = sortIndex
    }
}

/// snapshot.json — derived, for widgets. Never the source of truth.
///
/// Widgets recompute every date-derived value (countdown, chapter, rates, date clauses) from
/// `grant` + `price` per timeline entry, so it stays right even if the app hasn't run for weeks.
public struct VersoSnapshot: Codable, Sendable, Hashable {
    public var version: Int = 1
    public var generatedAt: Date
    public var grant: Grant?
    public var price: PriceCache
    public var stage: Stage
    public var clausesMet: Int
    public var clausesHolding: Int
    public var clausesTotal: Int
    public var tickStates: [TickState]
    /// Up to 2 unmet clauses, closest first.
    public var nearestClauses: [ClauseLine]
    public var pulseState: PulseState
    /// Consecutive recorded weeks the displayed pulse state has held.
    public var pulseStateWeeks: Int
    public var divergenceWeeks: Int
    public var latestSentence: String?
    public var lastMarginaliaWeekOf: Date?
    public var marginaliaWeekday: Int
    public var marginaliaHour: Int
    public var threads: [ThreadSummary]
    public var enoughLine: Double?
    public var veil: VeilPolicy
    public var mode: BookState.Mode
    /// Clauses with their stored state, so widgets can re-evaluate date/money kinds live.
    public var clauses: [Clause]
    /// Met-ness of non-date clauses (threads, pulse, manual) as of `generatedAt`.
    public var storedMet: [UUID: Bool]
    public var chapterTitles: [Int: String]
    /// When Book One was closed (Recto mode). Additive; older snapshots decode it as nil.
    public var closedAt: Date? = nil
    /// Settings.ticker, for widget price lines. Additive; nil in older snapshots (read as "CRWV").
    public var ticker: String? = nil
    /// PulseAnalysis.headline as of `generatedAt` (honest about a lean the hysteresis hasn't called).
    /// Additive; nil in older snapshots (widgets fall back to the state copy).
    public var pulseLine: String? = nil
    /// Settings.dayRate, so the Day Rate widget states the rate the way the Ledger does. Additive; older
    /// snapshots decode the default (after tax, per calendar day).
    public var dayRate: DayRateView = DayRateView()

    public init(generatedAt: Date, grant: Grant?, price: PriceCache, stage: Stage, clausesMet: Int, clausesHolding: Int,
                clausesTotal: Int, tickStates: [TickState], nearestClauses: [ClauseLine], pulseState: PulseState,
                pulseStateWeeks: Int, divergenceWeeks: Int, latestSentence: String?, lastMarginaliaWeekOf: Date?,
                marginaliaWeekday: Int, marginaliaHour: Int, threads: [ThreadSummary], enoughLine: Double?,
                veil: VeilPolicy, mode: BookState.Mode, clauses: [Clause], storedMet: [UUID: Bool],
                chapterTitles: [Int: String]) {
        self.generatedAt = generatedAt; self.grant = grant; self.price = price; self.stage = stage
        self.clausesMet = clausesMet; self.clausesHolding = clausesHolding; self.clausesTotal = clausesTotal
        self.tickStates = tickStates; self.nearestClauses = nearestClauses; self.pulseState = pulseState
        self.pulseStateWeeks = pulseStateWeeks; self.divergenceWeeks = divergenceWeeks
        self.latestSentence = latestSentence; self.lastMarginaliaWeekOf = lastMarginaliaWeekOf
        self.marginaliaWeekday = marginaliaWeekday; self.marginaliaHour = marginaliaHour; self.threads = threads
        self.enoughLine = enoughLine; self.veil = veil; self.mode = mode; self.clauses = clauses
        self.storedMet = storedMet; self.chapterTitles = chapterTitles
    }

    /// An empty snapshot (no grant, no price) for placeholders.
    public static let empty = VersoSnapshot(
        generatedAt: Date(timeIntervalSince1970: 0), grant: nil, price: PriceCache(), stage: .exposition,
        clausesMet: 0, clausesHolding: 0, clausesTotal: 0, tickStates: [], nearestClauses: [],
        pulseState: .unread, pulseStateWeeks: 0, divergenceWeeks: 0, latestSentence: nil, lastMarginaliaWeekOf: nil,
        marginaliaWeekday: 1, marginaliaHour: 19, threads: [], enoughLine: nil, veil: VeilPolicy(), mode: .bookOne,
        clauses: [], storedMet: [:], chapterTitles: [:])
}

// MARK: - Tolerant decoding
//
// A Debug install that adds a field must not blank every widget until the app rewrites snapshot.json:
// every field decodes if present and otherwise takes the default `.empty` (or the type) would give.

extension VersoSnapshot {
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let e = VersoSnapshot.empty
        version = try c.decodeIfPresent(Int.self, forKey: .version) ?? 1
        generatedAt = try c.decodeIfPresent(Date.self, forKey: .generatedAt) ?? e.generatedAt
        grant = try c.decodeIfPresent(Grant.self, forKey: .grant)
        price = try c.decodeIfPresent(PriceCache.self, forKey: .price) ?? PriceCache()
        stage = try c.decodeIfPresent(Stage.self, forKey: .stage) ?? e.stage
        clausesMet = try c.decodeIfPresent(Int.self, forKey: .clausesMet) ?? 0
        clausesHolding = try c.decodeIfPresent(Int.self, forKey: .clausesHolding) ?? 0
        clausesTotal = try c.decodeIfPresent(Int.self, forKey: .clausesTotal) ?? 0
        tickStates = try c.decodeIfPresent([TickState].self, forKey: .tickStates) ?? []
        nearestClauses = try c.decodeIfPresent([ClauseLine].self, forKey: .nearestClauses) ?? []
        pulseState = try c.decodeIfPresent(PulseState.self, forKey: .pulseState) ?? e.pulseState
        pulseStateWeeks = try c.decodeIfPresent(Int.self, forKey: .pulseStateWeeks) ?? 0
        divergenceWeeks = try c.decodeIfPresent(Int.self, forKey: .divergenceWeeks) ?? 0
        latestSentence = try c.decodeIfPresent(String.self, forKey: .latestSentence)
        lastMarginaliaWeekOf = try c.decodeIfPresent(Date.self, forKey: .lastMarginaliaWeekOf)
        marginaliaWeekday = try c.decodeIfPresent(Int.self, forKey: .marginaliaWeekday) ?? e.marginaliaWeekday
        marginaliaHour = try c.decodeIfPresent(Int.self, forKey: .marginaliaHour) ?? e.marginaliaHour
        threads = try c.decodeIfPresent([ThreadSummary].self, forKey: .threads) ?? []
        enoughLine = try c.decodeIfPresent(Double.self, forKey: .enoughLine)
        veil = try c.decodeIfPresent(VeilPolicy.self, forKey: .veil) ?? VeilPolicy()
        mode = try c.decodeIfPresent(BookState.Mode.self, forKey: .mode) ?? .bookOne
        clauses = try c.decodeIfPresent([Clause].self, forKey: .clauses) ?? []
        storedMet = try c.decodeIfPresent([UUID: Bool].self, forKey: .storedMet) ?? [:]
        chapterTitles = try c.decodeIfPresent([Int: String].self, forKey: .chapterTitles) ?? [:]
        closedAt = try c.decodeIfPresent(Date.self, forKey: .closedAt)
        ticker = try c.decodeIfPresent(String.self, forKey: .ticker)
        pulseLine = try c.decodeIfPresent(String.self, forKey: .pulseLine)
        dayRate = try c.decodeIfPresent(DayRateView.self, forKey: .dayRate) ?? DayRateView()
    }
}

extension ThreadSummary {
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(UUID.self, forKey: .id)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        binding = try c.decode(Binding.self, forKey: .binding)
        kindTag = try c.decodeIfPresent(String.self, forKey: .kindTag) ?? ""
        progress = try c.decodeIfPresent(Double.self, forKey: .progress) ?? 0
        label = try c.decodeIfPresent(String.self, forKey: .label) ?? ""
        periodLabel = try c.decodeIfPresent(String.self, forKey: .periodLabel) ?? ""
        lastEntryAt = try c.decodeIfPresent(Date.self, forKey: .lastEntryAt)
        status = try c.decode(MomentumStatus.self, forKey: .status)
        pace = try c.decodeIfPresent(Double.self, forKey: .pace) ?? 0
        logLabel = try c.decodeIfPresent(String.self, forKey: .logLabel) ?? "+1"
        defaultAmount = try c.decodeIfPresent(Double.self, forKey: .defaultAmount) ?? 1
        weekDots = try c.decodeIfPresent([Bool].self, forKey: .weekDots) ?? []
        sortIndex = try c.decodeIfPresent(Int.self, forKey: .sortIndex) ?? 0
    }
}
