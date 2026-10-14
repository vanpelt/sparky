import WidgetKit
import SwiftUI

@main
struct VersoWidgetsBundle: WidgetBundle {
    var body: some Widget {
        NextChapterWidget()
        DayRateWidget()
        TurningPointWidget()
        ThreadWidget()
        MarginaliaControl()
        LogThreadControl()
    }
}
