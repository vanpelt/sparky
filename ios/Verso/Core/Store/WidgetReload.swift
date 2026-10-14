import Foundation
import WidgetKit

/// Widget kinds (shared by the widget extension and the store's reload hooks).
public enum WidgetKinds {
    public static let nextChapter = "VersoNextChapter"
    public static let dayRate = "VersoDayRate"
    public static let turningPoint = "VersoTurningPoint"
    public static let thread = "VersoThread"
    public static let all = [nextChapter, dayRate, turningPoint, thread]
}

/// What a mutation touched, which decides the widget kinds to reload.
public struct ChangeSet: OptionSet, Sendable, Hashable {
    public let rawValue: Int
    public init(rawValue: Int) { self.rawValue = rawValue }
    public static let grant = ChangeSet(rawValue: 1 << 0)
    public static let price = ChangeSet(rawValue: 1 << 1)
    public static let threads = ChangeSet(rawValue: 1 << 2)
    public static let clauses = ChangeSet(rawValue: 1 << 3)
    public static let pulses = ChangeSet(rawValue: 1 << 4)
    public static let settings = ChangeSet(rawValue: 1 << 5)
    public static let book = ChangeSet(rawValue: 1 << 6)
    public static let all: ChangeSet = [.grant, .price, .threads, .clauses, .pulses, .settings, .book]

    public var widgetKinds: [String] {
        var k = Set<String>()
        if !isDisjoint(with: [.grant, .price, .settings, .book]) { k.formUnion([WidgetKinds.nextChapter, WidgetKinds.dayRate]) }
        if !isDisjoint(with: [.grant, .price, .threads, .clauses, .pulses, .settings, .book]) { k.insert(WidgetKinds.turningPoint) }
        if !isDisjoint(with: [.threads, .settings]) { k.insert(WidgetKinds.thread) }
        return k.sorted()
    }
}

public protocol WidgetReloading: Sendable {
    func reload(kinds: [String])
}

public struct WidgetCenterReloader: WidgetReloading {
    public init() {}
    public func reload(kinds: [String]) {
        for k in kinds { WidgetCenter.shared.reloadTimelines(ofKind: k) }
    }
}

public struct NoWidgetReloader: WidgetReloading {
    public init() {}
    public func reload(kinds: [String]) {}
}
