import Foundation

public enum ClauseRole: String, Codable, Sendable, Hashable, CaseIterable {
    case required, ordinary, regardless
}

public enum PulseMetric: String, Codable, Sendable, Hashable {
    case pulls, divergence
}

public enum ClauseKind: Codable, Sendable, Hashable {
    case date(after: Date)
    case tranche(index: Int)
    case remainingBelow(Double)
    case dayRateBelowEnough
    case priceBelow(Double)
    case priceAbove(Double)
    case threadTarget(threadID: UUID)
    case milestone(threadID: UUID, milestoneID: UUID)
    case pulse(metric: PulseMetric, weeks: Int, ofLast: Int)
    case manual(checked: Bool)

    public var isManual: Bool { if case .manual = self { return true }; return false }

    /// True when `self` is a numeric loosening of `before` (easier to meet). Drives the
    /// "Past-you set this on …. What changed?" prompt.
    public func loosens(_ before: ClauseKind) -> Bool {
        switch (before, self) {
        case let (.date(a), .date(b)): return b < a
        case let (.tranche(a), .tranche(b)): return b < a
        case let (.remainingBelow(a), .remainingBelow(b)): return b > a
        case let (.priceBelow(a), .priceBelow(b)): return b > a
        case let (.priceAbove(a), .priceAbove(b)): return b < a
        case let (.pulse(m1, w1, n1), .pulse(m2, w2, n2)) where m1 == m2:
            return (w2 < w1 && n2 >= n1) || (w2 <= w1 && n2 > n1)
        default: return false
        }
    }
}

/// One "I go when…" condition.
public struct Clause: Codable, Sendable, Identifiable, Hashable {
    public var id: UUID
    public var text: String
    public var kind: ClauseKind
    public var role: ClauseRole
    public var createdAt: Date
    public var metSince: Date?
    public var revisions: [ClauseRevision]

    public init(id: UUID = UUID(), text: String, kind: ClauseKind, role: ClauseRole = .ordinary,
                createdAt: Date = Date(), metSince: Date? = nil, revisions: [ClauseRevision] = []) {
        self.id = id; self.text = text; self.kind = kind; self.role = role
        self.createdAt = createdAt; self.metSince = metSince; self.revisions = revisions
    }
}

/// A tracked change: what the clause was before an edit.
public struct ClauseRevision: Codable, Sendable, Hashable {
    public var at: Date
    public var beforeText: String
    public var beforeKind: ClauseKind
    public var beforeRole: ClauseRole
    public var reason: String?
    public init(at: Date, beforeText: String, beforeKind: ClauseKind, beforeRole: ClauseRole, reason: String? = nil) {
        self.at = at; self.beforeText = beforeText; self.beforeKind = beforeKind
        self.beforeRole = beforeRole; self.reason = reason
    }
}
