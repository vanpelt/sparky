import AppIntents
import Foundation
import VersoCore

/// The one logging path shared by the widget +1 button, Siri and Shortcuts.
struct LogThreadIntent: AppIntent {
    static let title: LocalizedStringResource = "Log a Thread"
    static let description = IntentDescription("Add a line of progress to one of your Book Two threads.")
    static let openAppWhenRun = false

    @Parameter(title: "Thread") var thread: ThreadEntity
    @Parameter(title: "Amount") var amount: Double?

    init() {}
    init(thread: ThreadEntity, amount: Double? = nil) {
        self.thread = thread
        self.amount = amount
    }

    static var parameterSummary: some ParameterSummary { Summary("Log \(\.$thread)") { \.$amount } }

    func perform() async throws -> some IntentResult & ProvidesDialog {
        let entry = try await VersoStore.shared.log(threadID: thread.id, amount: amount)
        let state = try await VersoStore.shared.load()
        guard let t = state.threads.first(where: { $0.id == thread.id }) else {
            return .result(dialog: "That thread is gone.")
        }
        guard let entry else { return .result(dialog: "Already logged \(t.name) today.") }
        return .result(dialog: "\(Self.phrase(for: t, entry: entry))")
    }

    /// "Logged 20 min of Serbian." / "Logged 1 for Read 12 books." / "Marked Move done today."
    static func phrase(for t: PlotThread, entry: Entry) -> String {
        let amt = entry.amount.rounded() == entry.amount ? String(Int(entry.amount)) : String(entry.amount)
        switch t.kind {
        case .practice(let unit, _): return "Logged \(amt) \(unit.short) of \(t.name)."
        case .habit: return "Marked \(t.name) done today."
        case .count: return "Logged \(amt) for \(t.name)."
        case .project(let ms):
            let title = ms.first { $0.id == entry.milestoneID }?.title
            return title.map { "Finished “\($0)” for \(t.name)." } ?? "Logged a line for \(t.name)."
        }
    }
}
