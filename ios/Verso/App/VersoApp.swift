import SwiftUI
import UserNotifications
import VersoCore

@main
struct VersoApp: App {
    @State private var model = AppModel.launch()
    @Environment(\.scenePhase) private var phase

    init() {
        UNUserNotificationCenter.current().delegate = NotificationRouter.shared
    }

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(model)
                .environment(\.versoVeiled, model.veiled)
                .tint(Palette.oxblood)
                .debugFrame()
                .onAppear { KeyboardDismissal.install() }
                .task {
                    NotificationRouter.shared.onOpen = { url in model.route = DeepLink(url: url) }
                    await model.start()
                }
                .onOpenURL { model.route = DeepLink(url: $0) }
                .onChange(of: phase) { _, p in
                    if p == .active { KeyboardDismissal.install() }
                    if p == .active, model.loaded { Task { await model.foreground() } }
                }
                // Midnight, a time-zone change or a clock change while the app stays open.
                .onReceive(NotificationCenter.default.publisher(for: UIApplication.significantTimeChangeNotification)) { _ in
                    if model.loaded { Task { await model.dayChanged() } }
                }
                // While active, once a minute: other processes' writes, the midnight roll, the 5-minute price TTL.
                .task(id: phase == .active) {
                    guard phase == .active else { return }
                    while !Task.isCancelled {
                        try? await Task.sleep(for: .seconds(60))
                        if Task.isCancelled { break }
                        if model.loaded { await model.tick() }
                    }
                }
        }
    }
}
