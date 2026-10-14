import Foundation
import UserNotifications
import VersoCore

/// The only two notifications Verso sends: the weekly Marginalia, and "A chapter ends tomorrow."
enum Notifications {
    static let marginaliaID = "verso.marginalia"
    static let vestPrefix = "verso.vest."

    static func requestAuthorization() async -> Bool {
        (try? await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge])) ?? false
    }

    /// Replace every pending Verso notification with the current schedule.
    @MainActor
    static func reschedule(for state: VersoState, calendar: Calendar = .current, now: Date = Date()) {
        let center = UNUserNotificationCenter.current()
        let settings = state.book.settings
        let grant = state.book.grant
        let recto = state.book.state.mode == .recto
        Task {
            let pending = await center.pendingNotificationRequests().map(\.identifier)
            center.removePendingNotificationRequests(withIdentifiers: pending.filter { $0.hasPrefix("verso.") })
            guard settings.notificationsOn else { return }
            let auth = await center.notificationSettings().authorizationStatus
            guard auth == .authorized || auth == .provisional else { return }

            // Marginalia: weekly, configurable.
            let m = UNMutableNotificationContent()
            m.title = "Marginalia"
            m.body = "Five lines for the week."
            m.userInfo = ["url": "verso://marginalia"]
            var dc = DateComponents(); dc.weekday = settings.marginaliaWeekday; dc.hour = settings.marginaliaHour; dc.minute = 0
            try? await center.add(UNNotificationRequest(identifier: marginaliaID, content: m,
                                                        trigger: UNCalendarNotificationTrigger(dateMatching: dc, repeats: true)))

            // The eve of each vest, six months ahead at most.
            guard !recto, let grant else { return }
            let horizon = Days.add(183, to: now, calendar: calendar)
            for (i, d) in Ledger.trancheDates(grant, calendar: calendar).enumerated() {
                let eve = Days.add(-1, to: d, calendar: calendar)
                guard eve > now, eve <= horizon,
                      var fire = calendar.date(bySettingHour: 9, minute: 0, second: 0, of: eve) as Date? else { continue }
                if fire < now { fire = now.addingTimeInterval(60) }
                let c = UNMutableNotificationContent()
                c.title = "Chapter \(Roman.chapter(i))"
                c.body = "A chapter ends tomorrow."
                c.userInfo = ["url": "verso://chapter/\(i)"]
                let comps = calendar.dateComponents([.year, .month, .day, .hour, .minute], from: fire)
                try? await center.add(UNNotificationRequest(identifier: vestPrefix + "\(i)", content: c,
                                                            trigger: UNCalendarNotificationTrigger(dateMatching: comps, repeats: false)))
            }
        }
    }
}

/// Routes a tapped notification to its verso:// link.
@MainActor
final class NotificationRouter: NSObject, UNUserNotificationCenterDelegate {
    static let shared = NotificationRouter()
    var onOpen: ((URL) -> Void)?

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        guard let s = response.notification.request.content.userInfo["url"] as? String else { return }
        await MainActor.run { if let url = URL(string: s) { onOpen?(url) } }
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner]
    }
}
