import Foundation

/// book.json — everything small and slow-changing.
public struct BookFile: Codable, Sendable, Hashable {
    public var version: Int
    public var grant: Grant?
    public var settings: Settings
    public var chapters: [ChapterMeta]
    public var clauses: [Clause]
    public var stayCase: String?
    public var openingLines: [OpeningLine]
    public var letter: SealedLetter?
    public var state: BookState

    public static let currentVersion = 1

    public init(version: Int = BookFile.currentVersion, grant: Grant? = nil, settings: Settings = Settings(),
                chapters: [ChapterMeta] = [], clauses: [Clause] = [], stayCase: String? = nil,
                openingLines: [OpeningLine] = [], letter: SealedLetter? = nil,
                state: BookState = BookState(firstLaunchAt: Date())) {
        self.version = version; self.grant = grant; self.settings = settings; self.chapters = chapters
        self.clauses = clauses; self.stayCase = stayCase; self.openingLines = openingLines
        self.letter = letter; self.state = state
    }

    enum CodingKeys: String, CodingKey { case version, grant, settings, chapters, clauses, stayCase, openingLines, letter, state }
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        version = try c.decodeIfPresent(Int.self, forKey: .version) ?? 1
        grant = try c.decodeIfPresent(Grant.self, forKey: .grant)
        settings = try c.decodeIfPresent(Settings.self, forKey: .settings) ?? Settings()
        chapters = try c.decodeIfPresent([ChapterMeta].self, forKey: .chapters) ?? []
        clauses = try c.decodeIfPresent([Clause].self, forKey: .clauses) ?? []
        stayCase = try c.decodeIfPresent(String.self, forKey: .stayCase)
        openingLines = try c.decodeIfPresent([OpeningLine].self, forKey: .openingLines) ?? []
        letter = try c.decodeIfPresent(SealedLetter.self, forKey: .letter)
        state = try c.decodeIfPresent(BookState.self, forKey: .state) ?? BookState(firstLaunchAt: Date())
    }

    /// The chapter metadata for a tranche, creating an empty record if needed.
    public func chapter(_ index: Int) -> ChapterMeta {
        chapters.first { $0.trancheIndex == index } ?? ChapterMeta(trancheIndex: index)
    }

    public mutating func setChapter(_ meta: ChapterMeta) {
        if let i = chapters.firstIndex(where: { $0.trancheIndex == meta.trancheIndex }) { chapters[i] = meta }
        else { chapters.append(meta); chapters.sort { $0.trancheIndex < $1.trancheIndex } }
    }
}

public struct Settings: Codable, Sendable, Hashable {
    public var enoughLinePerDay: Double?
    public var veilByDefault: Bool
    public var showFiguresOnLockScreen: Bool
    /// Calendar weekday, 1 = Sunday.
    public var marginaliaWeekday: Int
    public var marginaliaHour: Int
    public var notificationsOn: Bool
    public var epigraph: String?
    public var ticker: String
    /// How the Ledger states the day rate, and so how the Day Rate widget states it too.
    public var dayRate: DayRateView = DayRateView()

    public init(enoughLinePerDay: Double? = nil, veilByDefault: Bool = true, showFiguresOnLockScreen: Bool = false,
                marginaliaWeekday: Int = 1, marginaliaHour: Int = 19, notificationsOn: Bool = true,
                epigraph: String? = nil, ticker: String = "CRWV") {
        self.enoughLinePerDay = enoughLinePerDay; self.veilByDefault = veilByDefault
        self.showFiguresOnLockScreen = showFiguresOnLockScreen; self.marginaliaWeekday = marginaliaWeekday
        self.marginaliaHour = marginaliaHour; self.notificationsOn = notificationsOn
        self.epigraph = epigraph; self.ticker = ticker
    }

    enum CodingKeys: String, CodingKey {
        case enoughLinePerDay, veilByDefault, showFiguresOnLockScreen, marginaliaWeekday, marginaliaHour, notificationsOn, epigraph, ticker
        case dayRate
    }
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = Settings()
        enoughLinePerDay = try c.decodeIfPresent(Double.self, forKey: .enoughLinePerDay)
        veilByDefault = try c.decodeIfPresent(Bool.self, forKey: .veilByDefault) ?? d.veilByDefault
        showFiguresOnLockScreen = try c.decodeIfPresent(Bool.self, forKey: .showFiguresOnLockScreen) ?? d.showFiguresOnLockScreen
        marginaliaWeekday = try c.decodeIfPresent(Int.self, forKey: .marginaliaWeekday) ?? d.marginaliaWeekday
        marginaliaHour = try c.decodeIfPresent(Int.self, forKey: .marginaliaHour) ?? d.marginaliaHour
        notificationsOn = try c.decodeIfPresent(Bool.self, forKey: .notificationsOn) ?? d.notificationsOn
        epigraph = try c.decodeIfPresent(String.self, forKey: .epigraph)
        ticker = try c.decodeIfPresent(String.self, forKey: .ticker) ?? d.ticker
        dayRate = try c.decodeIfPresent(DayRateView.self, forKey: .dayRate) ?? d.dayRate
    }
}

public struct ChapterMeta: Codable, Sendable, Hashable {
    public var trancheIndex: Int
    public var title: String?
    public var epigraph: String?
    public var notes: String?
    public var priceAtVest: Double?
    public init(trancheIndex: Int, title: String? = nil, epigraph: String? = nil, notes: String? = nil, priceAtVest: Double? = nil) {
        self.trancheIndex = trancheIndex; self.title = title; self.epigraph = epigraph
        self.notes = notes; self.priceAtVest = priceAtVest
    }
}

public struct OpeningLine: Codable, Sendable, Identifiable, Hashable {
    public var id: UUID
    public var date: Date
    public var text: String
    public init(id: UUID = UUID(), date: Date = Date(), text: String) { self.id = id; self.date = date; self.text = text }
}

public struct SealedLetter: Codable, Sendable, Hashable {
    public var body: String
    public var sealedAt: Date
    public var opensOn: Date?
    public var openedAt: Date?
    public init(body: String, sealedAt: Date = Date(), opensOn: Date? = nil, openedAt: Date? = nil) {
        self.body = body; self.sealedAt = sealedAt; self.opensOn = opensOn; self.openedAt = openedAt
    }
    public var isOpen: Bool { openedAt != nil }
}

public struct BookState: Codable, Sendable, Hashable {
    public enum Mode: String, Codable, Sendable, Hashable { case bookOne, recto }
    public var mode: Mode
    public var closedAt: Date?
    public var closingSentence: String?
    public var firstLaunchAt: Date
    public var lastTitleCardVestIndex: Int?
    public var seedImportedAt: Date?
    /// When the stage first became .turningPoint in the current run (Reading aftercare). nil otherwise.
    public var turningPointSince: Date?
    /// Set once the first-run cards have been dismissed.
    public var onboardedAt: Date?

    public init(mode: Mode = .bookOne, closedAt: Date? = nil, closingSentence: String? = nil, firstLaunchAt: Date,
                lastTitleCardVestIndex: Int? = nil, seedImportedAt: Date? = nil, turningPointSince: Date? = nil,
                onboardedAt: Date? = nil) {
        self.mode = mode; self.closedAt = closedAt; self.closingSentence = closingSentence
        self.firstLaunchAt = firstLaunchAt; self.lastTitleCardVestIndex = lastTitleCardVestIndex
        self.seedImportedAt = seedImportedAt; self.turningPointSince = turningPointSince; self.onboardedAt = onboardedAt
    }

    enum CodingKeys: String, CodingKey { case mode, closedAt, closingSentence, firstLaunchAt, lastTitleCardVestIndex, seedImportedAt, turningPointSince, onboardedAt }
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        mode = try c.decodeIfPresent(Mode.self, forKey: .mode) ?? .bookOne
        closedAt = try c.decodeIfPresent(Date.self, forKey: .closedAt)
        closingSentence = try c.decodeIfPresent(String.self, forKey: .closingSentence)
        firstLaunchAt = try c.decodeIfPresent(Date.self, forKey: .firstLaunchAt) ?? Date()
        lastTitleCardVestIndex = try c.decodeIfPresent(Int.self, forKey: .lastTitleCardVestIndex)
        seedImportedAt = try c.decodeIfPresent(Date.self, forKey: .seedImportedAt)
        turningPointSince = try c.decodeIfPresent(Date.self, forKey: .turningPointSince)
        onboardedAt = try c.decodeIfPresent(Date.self, forKey: .onboardedAt)
    }
}

// MARK: - The other files on disk

public struct ThreadsFile: Codable, Sendable, Hashable {
    public var version: Int = 1
    public var threads: [PlotThread] = []
    public init(threads: [PlotThread] = []) { self.threads = threads }
}

public struct EntriesFile: Codable, Sendable, Hashable {
    public var version: Int = 1
    public var entries: [Entry] = []
    public init(entries: [Entry] = []) { self.entries = entries }
}

public struct PulsesFile: Codable, Sendable, Hashable {
    public var version: Int = 1
    public var pulses: [Pulse] = []
    public init(pulses: [Pulse] = []) { self.pulses = pulses }
}

/// Everything the store holds, loaded together. The UI's AppModel mirrors this.
public struct VersoState: Sendable, Hashable {
    public var book: BookFile
    public var threads: [PlotThread]
    public var entries: [Entry]
    public var pulses: [Pulse]
    public var price: PriceCache

    public init(book: BookFile, threads: [PlotThread] = [], entries: [Entry] = [], pulses: [Pulse] = [], price: PriceCache = PriceCache()) {
        self.book = book; self.threads = threads; self.entries = entries; self.pulses = pulses; self.price = price
    }

    public var activeThreads: [PlotThread] { threads.filter(\.isActive).sorted { $0.sortIndex < $1.sortIndex } }
    public func entries(for threadID: UUID) -> [Entry] { entries.filter { $0.threadID == threadID } }
}

/// The combined export document (Colophon › Export JSON).
public struct VersoExport: Codable, Sendable {
    public var version: Int = 1
    public var exportedAt: Date
    public var book: BookFile
    public var threads: [PlotThread]
    public var entries: [Entry]
    public var pulses: [Pulse]
    public init(exportedAt: Date = Date(), book: BookFile, threads: [PlotThread], entries: [Entry], pulses: [Pulse]) {
        self.exportedAt = exportedAt; self.book = book; self.threads = threads; self.entries = entries; self.pulses = pulses
    }
}
