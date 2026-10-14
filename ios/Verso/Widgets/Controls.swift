import WidgetKit
import SwiftUI
import AppIntents
import VersoCore

/// §9.5 stretch: Control Center / Lock Screen / Action button controls.

/// Opens this week's Marginalia sheet (verso://marginalia).
struct MarginaliaControl: ControlWidget {
    static let kind = "VersoMarginaliaControl"

    var body: some ControlWidgetConfiguration {
        StaticControlConfiguration(kind: Self.kind) {
            ControlWidgetButton(action: OpenMarginaliaIntent()) {
                Label("Marginalia", systemImage: "pencil.line")
            }
        }
        .displayName("Write Marginalia")
        .description("Open this week's five-line check-in.")
    }
}

/// Which thread the Log control logs. nil = the most behind-pace active thread at the time of the tap.
struct SelectThreadControlIntent: ControlConfigurationIntent {
    static let title: LocalizedStringResource = "Choose Thread"
    static let description = IntentDescription("Which thread this control logs.")

    @Parameter(title: "Thread") var thread: ThreadEntity?

    init() {}
}

/// Logs a thread (+1, Done today, +20 min) without opening the app.
struct LogThreadControl: ControlWidget {
    static let kind = "VersoLogThreadControl"

    var body: some ControlWidgetConfiguration {
        AppIntentControlConfiguration(kind: Self.kind, provider: Provider()) { thread in
            // The name alone, so a side-mounted control (iPhone Duo) doesn't truncate it; the amount rides in
            // the action hint ("Log +20 min").
            ControlWidgetButton(action: LogThreadIntent(thread: thread, amount: nil)) {
                Label(thread.name, systemImage: "pencil.tip")
                    .controlWidgetActionHint(thread.logLabel.isEmpty ? "Log" : "Log \(thread.logLabel)")
            }
        }
        .displayName("Log a Thread")
        .description("Add a line to a Book Two thread.")
    }

    struct Provider: AppIntentControlValueProvider {
        func previewValue(configuration: SelectThreadControlIntent) -> ThreadEntity {
            configuration.thread ?? ThreadEntity(id: SampleData.serbianID, name: "Serbian", logLabel: "+20 min")
        }

        func currentValue(configuration: SelectThreadControlIntent) async throws -> ThreadEntity {
            let threads = (await ThreadProvider.summaries(at: [Date()])[0]) ?? []
            if let c = configuration.thread, let t = threads.first(where: { $0.id == c.id }) { return ThreadEntity(t) }
            if let t = ThreadEntry.order(threads, featuring: nil).first { return ThreadEntity(t) }
            return configuration.thread ?? ThreadEntity(id: UUID(), name: "No threads", logLabel: "")
        }
    }
}
