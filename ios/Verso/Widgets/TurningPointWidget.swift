import WidgetKit
import SwiftUI
import VersoCore

/// §9.3 Turning Point → verso://turning. His clauses and where the story stands; never a verdict.
struct TurningPointWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: WidgetKinds.turningPoint, provider: LiveProvider(kind: WidgetKinds.turningPoint)) { entry in
            TurningPointWidgetView(entry: entry)
                .widgetURL(VersoWidgetURL.turning)
                .versoWidgetBackground()
        }
        .configurationDisplayName("Turning Point")
        .description("Your clauses, and where the story stands.")
        .supportedFamilies(VersoFamilies.supported(WidgetKinds.turningPoint))
    }
}

#Preview("Small", as: .systemSmall) { TurningPointWidget() } timeline: { VersoEntry.sample }
#Preview("Medium", as: .systemMedium) { TurningPointWidget() } timeline: { VersoEntry.sample }
#Preview("Large", as: .systemLarge) { TurningPointWidget() } timeline: { VersoEntry.sample }
@available(iOS 27.0, *)
#Preview("Extra Large", as: .systemExtraLargePortrait) { TurningPointWidget() } timeline: { VersoEntry.sample }
#Preview("Circular", as: .accessoryCircular) { TurningPointWidget() } timeline: { VersoEntry.sample }
#Preview("Rectangular", as: .accessoryRectangular) { TurningPointWidget() } timeline: { VersoEntry.sample }
