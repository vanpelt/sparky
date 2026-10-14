import AppIntents
import Foundation

/// Opens the weekly Marginalia sheet (verso://marginalia).
struct OpenMarginaliaIntent: AppIntent {
    static let title: LocalizedStringResource = "Write Marginalia"
    static let description = IntentDescription("Open this week's five-line check-in.")
    static let openAppWhenRun = true

    static let url = URL(string: "verso://marginalia")!

    func perform() async throws -> some IntentResult & OpensIntent {
        .result(opensIntent: OpenURLIntent(Self.url))
    }
}
