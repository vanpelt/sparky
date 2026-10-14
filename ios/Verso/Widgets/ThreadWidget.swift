import WidgetKit
import SwiftUI
import VersoCore

/// §9.4 Thread → verso://thread/<id>. Configurable (which thread) and interactive (log from the widget).
struct ThreadWidget: Widget {
    var body: some WidgetConfiguration {
        AppIntentConfiguration(kind: WidgetKinds.thread, intent: SelectThreadIntent.self, provider: ThreadProvider()) { entry in
            ThreadWidgetView(entry: entry)
                .widgetURL(entry.threads.first.map { VersoWidgetURL.thread($0.id) } ?? VersoWidgetURL.threads)
                .versoWidgetBackground()
        }
        .configurationDisplayName("Thread")
        .description("One Book Two thread, with a button to log it.")
        .supportedFamilies(VersoFamilies.supported(WidgetKinds.thread))
    }
}

#Preview("Small", as: .systemSmall) { ThreadWidget() } timeline: { ThreadEntry.sample }
#Preview("Medium", as: .systemMedium) { ThreadWidget() } timeline: { ThreadEntry.sample }
#Preview("Large", as: .systemLarge) { ThreadWidget() } timeline: { ThreadEntry.sample }
@available(iOS 27.0, *)
#Preview("Extra Large", as: .systemExtraLargePortrait) { ThreadWidget() } timeline: { ThreadEntry.sample }
#Preview("Rectangular", as: .accessoryRectangular) { ThreadWidget() } timeline: { ThreadEntry.sample }
