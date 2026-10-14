import AppIntents
import Foundation
import VersoCore

/// A thread as Siri, Shortcuts and widget configuration see it.
struct ThreadEntity: AppEntity, Identifiable, Hashable, Sendable {
    static let typeDisplayRepresentation = TypeDisplayRepresentation(name: "Thread")
    static let defaultQuery = ThreadQuery()

    let id: UUID
    let name: String
    let logLabel: String

    var displayRepresentation: DisplayRepresentation {
        DisplayRepresentation(title: "\(name)", subtitle: "\(logLabel)")
    }

    init(id: UUID, name: String, logLabel: String = "+1") {
        self.id = id; self.name = name; self.logLabel = logLabel
    }

    init(_ summary: ThreadSummary) { self.init(id: summary.id, name: summary.name, logLabel: summary.logLabel) }
}

struct ThreadQuery: EntityQuery, EntityStringQuery {
    func entities(for identifiers: [UUID]) async throws -> [ThreadEntity] {
        try await all().filter { identifiers.contains($0.id) }
    }

    func suggestedEntities() async throws -> [ThreadEntity] { try await all() }

    func entities(matching string: String) async throws -> [ThreadEntity] {
        try await all().filter { $0.name.localizedCaseInsensitiveContains(string) }
    }

    func all() async throws -> [ThreadEntity] {
        let state = try await VersoStore.shared.load()
        let board = Board(state: state)
        return board.threads.map { ThreadEntity(id: $0.thread.id, name: $0.thread.name, logLabel: $0.progress.logLabel) }
    }
}
