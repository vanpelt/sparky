import Foundation

/// The cloth colour of a thread's spine.
public enum Binding: String, Codable, Sendable, CaseIterable, Hashable {
    case oxblood, navy, forest, ochre, plum, slate
}

public enum PracticeUnit: String, Codable, Sendable, Hashable {
    case minutes, sessions
    public var short: String { self == .minutes ? "min" : "sessions" }
}

public struct Milestone: Codable, Sendable, Identifiable, Hashable {
    public var id: UUID
    public var title: String
    public var doneAt: Date?
    public init(id: UUID = UUID(), title: String, doneAt: Date? = nil) {
        self.id = id; self.title = title; self.doneAt = doneAt
    }

    public static func filmDefaults() -> [Milestone] {
        ["Logline", "Script draft", "Shot list", "Shoot day", "Edit", "Screening"].map { Milestone(title: $0) }
    }
}

public enum ThreadKind: Codable, Sendable, Hashable {
    case count(target: Int, deadline: Date?)
    case habit(perWeek: Int)
    case project(milestones: [Milestone])
    case practice(unit: PracticeUnit, weeklyTarget: Double)

    /// Small-caps glyph: COUNT / HABIT / PROJECT / PRACTICE.
    public var tag: String {
        switch self {
        case .count: "COUNT"
        case .habit: "HABIT"
        case .project: "PROJECT"
        case .practice: "PRACTICE"
        }
    }
}

/// One interest or goal of Book Two. (Named PlotThread to avoid clashing with Foundation.Thread.)
public struct PlotThread: Codable, Sendable, Identifiable, Hashable {
    public var id: UUID
    public var name: String
    public var subtitle: String?
    public var logline: String?
    public var kind: ThreadKind
    public var binding: Binding
    public var defaultAmount: Double
    public var createdAt: Date
    public var archivedAt: Date?
    public var sortIndex: Int

    public init(id: UUID = UUID(), name: String, subtitle: String? = nil, logline: String? = nil,
                kind: ThreadKind, binding: Binding, defaultAmount: Double = 1,
                createdAt: Date = Date(), archivedAt: Date? = nil, sortIndex: Int = 0) {
        self.id = id; self.name = name; self.subtitle = subtitle; self.logline = logline
        self.kind = kind; self.binding = binding; self.defaultAmount = defaultAmount
        self.createdAt = createdAt; self.archivedAt = archivedAt; self.sortIndex = sortIndex
    }

    public var isActive: Bool { archivedAt == nil }

    public var milestones: [Milestone] {
        if case .project(let m) = kind { return m }
        return []
    }

    /// The four first-run presets.
    public static func presets(now: Date = Date()) -> [PlotThread] {
        [
            PlotThread(name: "Move", logline: "A body that can carry Book Two.",
                       kind: .habit(perWeek: 4), binding: .forest, createdAt: now, sortIndex: 0),
            PlotThread(name: "Read 12 books", kind: .count(target: 12, deadline: nil),
                       binding: .navy, createdAt: now, sortIndex: 1),
            PlotThread(name: "Make a short film", kind: .project(milestones: Milestone.filmDefaults()),
                       binding: .oxblood, createdAt: now, sortIndex: 2),
            PlotThread(name: "Learn Serbian", subtitle: "Српски",
                       kind: .practice(unit: .minutes, weeklyTarget: 120), binding: .plum,
                       defaultAmount: 20, createdAt: now, sortIndex: 3),
        ]
    }
}
