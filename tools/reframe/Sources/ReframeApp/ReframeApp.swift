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
            CommandGroup(after: .newItem) {
                Button("Open Footage Folder…") { library.chooseFolder() }
                    .keyboardShortcut("o")
                Button("Choose LUT Folder…") { library.chooseLUTFolder() }
            }
        }
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        // Needed when launched as a bare executable (swift run / Xcode) rather than a bundle.
        NSApp.setActivationPolicy(.regular)
        NSApp.activate(ignoringOtherApps: true)
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
        if let path = ProcessInfo.processInfo.environment["REFRAME_SNAPSHOT"] {
            DispatchQueue.main.asyncAfter(deadline: .now() + 9) {
                let windows = NSApp.windows.filter { $0.isVisible && $0.contentView != nil && $0.frame.width > 400 }
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
