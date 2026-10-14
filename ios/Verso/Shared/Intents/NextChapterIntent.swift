import AppIntents
import Foundation
import VersoCore

/// "Chapter Six ends in 37 days."
struct NextChapterIntent: AppIntent {
    static let title: LocalizedStringResource = "When Does the Chapter End"
    static let description = IntentDescription("How many days until the next vest.")
    static let openAppWhenRun = false

    func perform() async throws -> some IntentResult & ProvidesDialog & ReturnsValue<Int> {
        // The same path the widgets take: the store's own files, the app's Board for now.
        let src = await WidgetData.source(store: VersoStore.shared)
        if case .unreadable = src {
            return .result(value: 0, dialog: "Open Verso to refresh, then ask again.")
        }
        guard let next = WidgetData.boards(src, at: [Date()])[0]?.next else {
            return .result(value: 0, dialog: src.hasGrant ? "Book One is fully read." : "Set your grant in Verso first.")
        }
        let n = Roman.spelled(next.index + 1)
        let days = next.daysToNext
        return .result(value: days, dialog: "Chapter \(n) ends in \(days) \(days == 1 ? "day" : "days").")
    }
}
