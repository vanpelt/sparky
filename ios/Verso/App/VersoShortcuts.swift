import AppIntents

struct VersoShortcuts: AppShortcutsProvider {
    static var appShortcuts: [AppShortcut] {
        AppShortcut(intent: LogThreadIntent(), phrases: ["Log \(\.$thread) in \(.applicationName)"],
                    shortTitle: "Log Thread", systemImageName: "pencil.tip")
        AppShortcut(intent: OpenMarginaliaIntent(), phrases: ["Write marginalia in \(.applicationName)"],
                    shortTitle: "Marginalia", systemImageName: "scribble")
        AppShortcut(intent: NextChapterIntent(), phrases: ["When does the chapter end in \(.applicationName)"],
                    shortTitle: "Next Chapter", systemImageName: "book.closed")
    }
}
