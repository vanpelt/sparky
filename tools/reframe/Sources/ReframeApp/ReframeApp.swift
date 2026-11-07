import AppKit
import ReframeKit
import SwiftUI

@main
struct ReframeApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @State private var library = Library()

    var body: some Scene {
        WindowGroup("Reframe") {
            ContentView()
                .environment(library)
                .preferredColorScheme(.dark)  // neutral dark surround for judging footage
                .frame(minWidth: 1100, minHeight: 650)
        }
        .windowToolbarStyle(.unified)
        .restorationBehavior(.disabled)  // always open the main window, even after a forced quit
        .defaultLaunchBehavior(.presented)
        .commands {
            CommandGroup(replacing: .undoRedo) {
                // Text fields keep their own undo; everywhere else undo edits to shots.
                Button("Undo") {
                    if NSApp.keyWindow?.firstResponder is NSText { NSApp.sendAction(Selector(("undo:")), to: nil, from: nil) }
                    else { library.undo() }
                }
                .keyboardShortcut("z")
                Button("Redo") {
                    if NSApp.keyWindow?.firstResponder is NSText { NSApp.sendAction(Selector(("redo:")), to: nil, from: nil) }
                    else { library.redo() }
                }
                .keyboardShortcut("z", modifiers: [.command, .shift])
            }
            CommandGroup(before: .sidebar) {
                Button("Clips") { library.browse = .clips }.keyboardShortcut("1")
                Button("Shots") { library.browse = .shots }.keyboardShortcut("2")
                Divider()
            }
            CommandGroup(after: .newItem) {
                Button("Open Footage Folder…") { library.chooseFolder() }
                    .keyboardShortcut("o")
                Button("Close Footage Folder") { library.closeFolder() }
                    .disabled(library.folder == nil)
                Button("Choose LUT Folder…") { library.chooseLUTFolder() }
            }
        }

        // Reframe › Settings… (⌘,): storage.
        Settings {
            StorageSettings()
                .environment(library)
                .preferredColorScheme(.dark)
        }
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        // Needed when launched as a bare executable (swift run / Xcode) rather than a bundle.
        NSApp.setActivationPolicy(.regular)
        NSApp.activate(ignoringOtherApps: true)
        // Dark everywhere (a neutral surround for judging footage): preferredColorScheme reaches
        // the main window but not Settings, sheets' hosts or alerts.
        NSApp.appearance = NSAppearance(named: .darkAqua)
        // Launched outside LaunchServices (e.g. a bare executable), SwiftUI may not open the
        // initial window. Ask for one if nothing appeared.
        DispatchQueue.main.asyncAfter(deadline: .now() + 1) {
            if NSApp.windows.filter(\.isVisible).isEmpty {
                NSApp.sendAction(#selector(NSResponder.newWindowForTab(_:)), to: nil, from: nil)
            }
        }
        // Debug: REFRAME_SNAPSHOT=/path.png writes the window's own rendering after a few seconds.
        if ProcessInfo.processInfo.environment["REFRAME_DEBUG"] != nil {
            for s in [0.5, 2, 5, 8] {
                DispatchQueue.main.asyncAfter(deadline: .now() + s) {
                    FileHandle.standardError.write("t+\(s)s windows=\(NSApp.windows.count) visible=\(NSApp.windows.filter(\.isVisible).count) active=\(NSApp.isActive)\n".data(using: .utf8)!)
                }
            }
        }
        // Debug: REFRAME_SETTINGS=1 opens Settings, and the snapshot then captures that window.
        let settings = ProcessInfo.processInfo.environment["REFRAME_SETTINGS"] != nil
        if let path = ProcessInfo.processInfo.environment["REFRAME_SNAPSHOT"] {
            DispatchQueue.main.asyncAfter(deadline: .now() + 9) {
                var windows = NSApp.windows.filter { $0.isVisible && $0.contentView != nil && $0.frame.width > 400 }
                if settings { windows = windows.filter { $0.frame.width < 900 } }
                guard let view = windows.max(by: { $0.frame.width < $1.frame.width })?.contentView,
                      let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) else {
                    FileHandle.standardError.write("snapshot: no window (\(NSApp.windows.count) total)\n".data(using: .utf8)!)
                    return
                }
                view.cacheDisplay(in: view.bounds, to: rep)
                try? rep.representation(using: .png, properties: [:])?.write(to: URL(fileURLWithPath: path))
                if let sheet = view.window?.attachedSheet?.contentView,
                   let srep = sheet.bitmapImageRepForCachingDisplay(in: sheet.bounds) {
                    sheet.cacheDisplay(in: sheet.bounds, to: srep)
                    try? srep.representation(using: .png, properties: [:])?
                        .write(to: URL(fileURLWithPath: path.replacingOccurrences(of: ".png", with: "-sheet.png")))
                }
                FileHandle.standardError.write("snapshot: wrote \(path)\n".data(using: .utf8)!)
            }
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }
}
