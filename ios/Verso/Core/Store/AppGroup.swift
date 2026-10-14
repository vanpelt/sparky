import Foundation
import os

public enum AppGroup {
    /// Read from `VersoAppGroup` in the host bundle's Info.plist, which project.yml derives from `VERSO_ID`,
    /// so the id lives in exactly one place. The literal only covers hosts without the key (unit tests).
    public static let identifier = Bundle.main.object(forInfoDictionaryKey: "VersoAppGroup") as? String
        ?? "group.com.vanpelt.personal.verso"
    static let log = Logger(subsystem: "verso", category: "store")

    /// The shared App Group container, or nil when entitlements are absent (unsigned simulator builds).
    public static var sharedContainerURL: URL? {
        FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: identifier)
    }

    public static var isShared: Bool { sharedContainerURL != nil }

    /// `<group>/Verso/`, falling back to `Application Support/Verso/` (logged) when the group is unavailable.
    public static var storeDirectory: URL {
        if let group = sharedContainerURL { return group.appendingPathComponent("Verso", isDirectory: true) }
        log.notice("App Group \(identifier, privacy: .public) unavailable; falling back to Application Support/Verso")
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first
            ?? URL(fileURLWithPath: NSTemporaryDirectory())
        return base.appendingPathComponent("Verso", isDirectory: true)
    }
}
