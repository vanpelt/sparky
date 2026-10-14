import AppIntents
import WidgetKit
import VersoCore

/// Configuration for the Thread widget. nil = the most behind-pace active thread.
struct SelectThreadIntent: WidgetConfigurationIntent {
    static let title: LocalizedStringResource = "Choose Thread"
    static let description = IntentDescription("Which thread this widget shows.")

    @Parameter(title: "Thread") var thread: ThreadEntity?

    init() {}
    init(thread: ThreadEntity?) { self.thread = thread }
}
