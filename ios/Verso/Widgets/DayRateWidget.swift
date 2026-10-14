import WidgetKit
import SwiftUI
import VersoCore

/// §9.2 Day Rate → verso://ledger. The equivalent daily wage he walks away from, at today's price.
struct DayRateWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: WidgetKinds.dayRate, provider: LiveProvider(kind: WidgetKinds.dayRate, priceAgeEntries: true)) { entry in
            DayRateWidgetView(entry: entry)
                .widgetURL(VersoWidgetURL.ledger)
                .versoWidgetBackground()
        }
        .configurationDisplayName("Day Rate")
        .description("What Book One pays you per day to stay, after tax.")
        .supportedFamilies(VersoFamilies.supported(WidgetKinds.dayRate))
    }
}

#Preview("Small", as: .systemSmall) { DayRateWidget() } timeline: { VersoEntry.sample }
#Preview("Medium", as: .systemMedium) { DayRateWidget() } timeline: { VersoEntry.sample }
#Preview("Large", as: .systemLarge) { DayRateWidget() } timeline: { VersoEntry.sample }
@available(iOS 27.0, *)
#Preview("Extra Large", as: .systemExtraLargePortrait) { DayRateWidget() } timeline: { VersoEntry.sample }
#Preview("Rectangular", as: .accessoryRectangular) { DayRateWidget() } timeline: { VersoEntry.sample }
