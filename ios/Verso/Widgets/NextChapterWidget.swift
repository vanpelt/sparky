import WidgetKit
import SwiftUI
import VersoCore

/// §9.1 Next Chapter → verso://contents. The intended StandBy pick.
struct NextChapterWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: WidgetKinds.nextChapter, provider: LiveProvider(kind: WidgetKinds.nextChapter)) { entry in
            NextChapterWidgetView(entry: entry)
                .widgetURL(VersoWidgetURL.contents)
                .versoWidgetBackground()
        }
        .configurationDisplayName("Next Chapter")
        .description("Days until the next vest, and what it brings.")
        .supportedFamilies(VersoFamilies.supported(WidgetKinds.nextChapter))
    }
}

#Preview("Small", as: .systemSmall) { NextChapterWidget() } timeline: { VersoEntry.sample }
#Preview("Medium", as: .systemMedium) { NextChapterWidget() } timeline: { VersoEntry.sample }
#Preview("Large", as: .systemLarge) { NextChapterWidget() } timeline: { VersoEntry.sample }
@available(iOS 27.0, *)
#Preview("Extra Large", as: .systemExtraLargePortrait) { NextChapterWidget() } timeline: { VersoEntry.sample }
#Preview("Circular", as: .accessoryCircular) { NextChapterWidget() } timeline: { VersoEntry.sample }
#Preview("Rectangular", as: .accessoryRectangular) { NextChapterWidget() } timeline: { VersoEntry.sample }
#Preview("Inline", as: .accessoryInline) { NextChapterWidget() } timeline: { VersoEntry.sample }
