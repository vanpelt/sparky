import AppKit
import ReframeKit
import SwiftUI

struct ContentView: View {
    @Environment(Library.self) private var library
    @Environment(\.openSettings) private var openSettings
    @State private var viewer = ViewerModel()
    @State private var showInspector = true
    @State private var showExport = false
    @State private var keyMonitor: Any?
    @State private var clickMonitor: Any?
    @State private var shotSelection: Set<Shot.ID> = []
    @State private var columns: NavigationSplitViewVisibility = .all
    @State private var exportOnly: Set<Shot.ID>?

    var body: some View {
        NavigationSplitView(columnVisibility: $columns) {
            Sidebar()
                .navigationSplitViewColumnWidth(min: 230, ideal: 260, max: 360)
        } detail: {
            if library.browse == .shots {
                ShotsBrowser(selection: $shotSelection)
            } else if viewer.clip != nil {
                VStack(spacing: 0) {
                    ViewerPane(viewer: viewer)
                    Divider()
                    TimelinePane(viewer: viewer)
                }
            } else if let url = library.selection, library.selectedItem == nil, library.cloud.media(for: url) != nil {
                CloudFetchView(url: url)
            } else {
                emptyState
            }
        }
        .inspector(isPresented: Binding(get: { showInspector && library.browse == .clips },
                                        // Hiding it for the shots view mustn't count as closing it.
                                        set: { if library.browse == .clips { showInspector = $0 } })) {
            Inspector(item: library.selectedItem, viewer: viewer)
                .inspectorColumnWidth(min: 250, ideal: 280, max: 360)
        }
        .toolbar { toolbar }
        .navigationTitle(library.browse == .shots ? "Shots" : library.selectedItem.flatMap { $0.clip.captureDate }
            .map { $0.formatted(date: .abbreviated, time: .shortened) } ?? "Reframe")
        .navigationSubtitle(library.folder?.lastPathComponent ?? "")
        .tint(Theme.sightline)
        .sheet(isPresented: $showExport) { ExportSheet(only: exportOnly) }
        .onChange(of: library.browse) { _, b in
            // The shots view takes the whole window; the clip list comes back with the editor.
            withAnimation { columns = b == .shots ? .detailOnly : .all }
            if b == .shots { viewer.engine.stop(); viewer.isPlaying = false }
        }
        .onChange(of: library.shotToOpen) { _, _ in openPendingShot() }
        .onChange(of: library.preset) { _, _ in
            // Suggestions were re-ranked: a previewed one may no longer exist.
            if viewer.activeShotID == nil { viewer.deselectShot() }
            if let item = library.selectedItem { library.refreshSuggestionThumbnails(for: item) }
        }
        // Keyed on the item, not the selection: a cloud clip's item appears once it's fetched.
        .onChange(of: library.selectedItem?.id, initial: true) { _, _ in
            viewer.library = library
            viewer.load(library.selectedItem?.clip) { openPendingShot() }
            if let item = library.selectedItem { library.refreshSuggestionThumbnails(for: item) }
        }
        .onAppear { installKeys() }
        .onDisappear {
            if let keyMonitor { NSEvent.removeMonitor(keyMonitor) }
            if let clickMonitor { NSEvent.removeMonitor(clickMonitor) }
        }
        .task { await debugHooks() }
    }

    private var noFootage: Bool { library.items.isEmpty && library.cloud.media.isEmpty }

    private var emptyState: some View {
        ContentUnavailableView {
            Label(noFootage ? "No footage yet" : "Choose a clip", systemImage: "camera.metering.spot")
        } description: {
            Text(noFootage ? "Open a folder of Insta360 .insv recordings, or connect Insta360 Cloud, to start." : "Pick a clip from the sidebar.")
        } actions: {
            if library.items.isEmpty && library.cloud.media.isEmpty {
                Button("Open Footage Folder…") { library.chooseFolder() }.buttonStyle(.borderedProminent)
            }
        }
    }

    @ToolbarContentBuilder private var toolbar: some ToolbarContent {
        ToolbarItem(placement: .navigation) {
            Picker("Browse", selection: Binding(get: { library.browse }, set: { library.browse = $0 })) {
                Text("Clips").tag(Library.Browse.clips)
                Text(library.shots.isEmpty ? "Shots" : "Shots \(library.shots.count)").tag(Library.Browse.shots)
            }
            .pickerStyle(.segmented)
            .help("Clips: watch and mark. Shots: every shot you've made (⌘1 / ⌘2)")
        }
        ToolbarItemGroup(placement: .principal) {
            if viewer.clip != nil && library.browse == .clips {
                Picker("View", selection: $viewer.projection) {
                    Text("Frame").tag(Projection.flat)
                    Text("360°").tag(Projection.equirect)
                }
                .pickerStyle(.segmented)
                .help("The 16:9 frame you'll export, or the whole sphere (click it to aim)")

                Picker("Stabilization", selection: $viewer.stabilization) {
                    ForEach(Motion.Stabilization.allCases) { Text($0.label).tag($0) }
                }
                .pickerStyle(.segmented)
                .help("Off: moves with the camera. Follow: level, smoothed heading. Lock: level, fixed heading.")

                LookMenu(viewer: viewer)
            }
        }
        ToolbarItemGroup(placement: .primaryAction) {
            let selected = library.browse == .shots && !shotSelection.isEmpty ? shotSelection : nil
            Button {
                exportOnly = selected
                showExport = true
            } label: {
                Label(selected.map { "Export \($0.count) Selected" }
                      ?? (library.shots.isEmpty ? "Export" : "Export \(library.shots.count)"), systemImage: "square.and.arrow.up")
                    .labelStyle(.titleAndIcon)
            }
            .disabled(library.shots.isEmpty)
            .help("Render shots to ProRes with timelines for Resolve and Final Cut (⌘E)")
            .keyboardShortcut("e")
            Button { showInspector.toggle() } label: { Label("Inspector", systemImage: "sidebar.right") }
                .disabled(library.browse == .shots)
        }
    }

    /// Select the shot "Open in Editor" asked for, once its clip is the one in the viewer.
    private func openPendingShot() {
        guard let id = library.shotToOpen, let shot = library.shots.first(where: { $0.id == id }) else { return }
        guard viewer.clip?.url.path == shot.clipPath, viewer.duration > 0 else { return }
        library.shotToOpen = nil
        viewer.select(shot)
    }

    /// Keys in the shots view: Return opens, Delete deletes, Esc clears, ⌘A selects all.
    private func shotsKey(_ event: NSEvent) -> Bool {
        switch event.keyCode {
        case 36, 76:  // return, enter
            guard let shot = library.shots.first(where: { shotSelection.contains($0.id) }) else { return false }
            library.openInEditor(shot)
        case 51, 117:  // delete, forward delete
            guard !shotSelection.isEmpty else { return false }
            library.delete(shotSelection)
            shotSelection = []
        case 53: shotSelection = []
        default:
            guard event.modifierFlags.contains(.command), event.charactersIgnoringModifiers == "a" else { return false }
            shotSelection = Set(library.shots.map(\.id))
        }
        return true
    }

    /// Single-key shortcuts, ignored while typing in a text field.
    private func installKeys() {
        guard keyMonitor == nil else { return }
        // Clicking anywhere outside the text field being edited (the viewer, the lanes, a card)
        // ends the edit: those views don't take keyboard focus themselves, so without this the
        // field keeps it and space types into the name instead of playing.
        clickMonitor = NSEvent.addLocalMonitorForEvents(matching: [.leftMouseDown, .rightMouseDown]) { event in
            if let window = event.window, let editor = window.firstResponder as? NSTextView, editor.isFieldEditor,
               let field = editor.delegate as? NSView {
                let p = field.convert(event.locationInWindow, from: nil)
                if !field.bounds.contains(p) { window.makeFirstResponder(nil) }
            }
            return event
        }
        keyMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { event in
            if NSApp.keyWindow?.firstResponder is NSText || showExport { return event }
            if library.browse == .shots { return shotsKey(event) ? nil : event }
            if event.modifierFlags.contains(.command) { return event }
            switch (event.keyCode, event.charactersIgnoringModifiers?.lowercased()) {
            case (49, _): viewer.togglePlay()
            case (123, _): viewer.step(frames: event.modifierFlags.contains(.shift) ? -10 : -1)
            case (124, _): viewer.step(frames: event.modifierFlags.contains(.shift) ? 10 : 1)
            case (_, "n"): viewer.newShot()
            case (_, "i"): viewer.markIn()
            case (_, "o"): viewer.markOut()
            case (_, "k"): viewer.addKeyframe()
            case (_, "a") where viewer.targetSuggestion != nil: viewer.acceptSuggestion()
            case (_, "x") where viewer.targetSuggestion != nil: viewer.dismissSuggestion()
            case (_, "f"): viewer.projection = viewer.projection == .flat ? .equirect : .flat
            case (_, "m"): viewer.muted.toggle()
            case (_, "1"): viewer.apply(.mega)
            case (_, "2"): viewer.apply(.ultra)
            case (_, "3"): viewer.apply(.dewarp)
            case (_, "4"): viewer.apply(.linear)
            case (51, _) where viewer.currentKeyframe != nil, (117, _) where viewer.currentKeyframe != nil:
                viewer.removeCurrentKeyframe()  // delete: the keyframe at the playhead
            case (53, _): viewer.deselectShot()  // esc
            default: return event
            }
            return nil
        }
    }

    /// Environment-driven hooks for headless checks:
    /// REFRAME_SELECT=<name substring>, REFRAME_PROJECTION=equirect, REFRAME_YAW=<deg>,
    /// REFRAME_AUTOPLAY=1, REFRAME_SEEK=<s>, REFRAME_DEMO_SHOT=1, REFRAME_EXPORT_SHEET=1,
    /// REFRAME_SUGGESTION=1 (preview the best suggested shot; =last the latest one; =accept keeps the best),
    /// REFRAME_SELFTEST=1 (keyframe editing, trims and undo; results on stderr),
    /// REFRAME_PRESET=people|action|scenery (this launch only),
    /// REFRAME_SNAPSHOT=<path.png> (also writes <path>-viewer.png).
    private func debugHooks() async {
        let env = ProcessInfo.processInfo.environment
        if let name = env["REFRAME_SELECT"] {
            for _ in 0..<150 {
                if let item = library.items.first(where: { $0.clip.name.contains(name) }) {
                    library.selection = item.id
                    break
                }
                // A cloud recording not fetched yet: selecting it fetches it, then waits for it.
                if let m = library.cloud.media.first(where: { $0.name.contains(name) }) {
                    library.selection = library.cloud.url(for: m)
                    for _ in 0..<1200 where library.selectedItem == nil { try? await Task.sleep(for: .milliseconds(100)) }
                    if env["REFRAME_DEBUG"] != nil {
                        let stub = library.selectedItem.map { CloudCache.isStub($0.clip.url) } ?? false
                        FileHandle.standardError.write("cloud select: \(m.name) -> item \(library.selectedItem != nil), stub \(stub), error \(library.cloud.errors[m.id] ?? "none")\n".data(using: .utf8)!)
                    }
                    break
                }
                try? await Task.sleep(for: .milliseconds(100))
            }
        }
        try? await Task.sleep(for: .seconds(1.5))
        if env["REFRAME_PROJECTION"] == "equirect" { viewer.projection = .equirect }
        if let yaw = env["REFRAME_YAW"].flatMap(Double.init) { viewer.manual.yaw = yaw }
        if env["REFRAME_DEMO_SHOT"] != nil, viewer.clip != nil {
            viewer.manual = Framing(yaw: env["REFRAME_YAW"].flatMap(Double.init) ?? 0, pitch: -8, preset: .dewarp)
            viewer.scrub(to: 12, final: true)
            if let shot = viewer.newShot(from: 12, to: 30) {
                var s = shot
                s.setKeyframe(at: 20, framing: Framing(yaw: s.keyframes[0].framing.yaw + 150, pitch: 0, preset: .mega))
                s.setKeyframe(at: 28, framing: Framing(yaw: s.keyframes[0].framing.yaw + 175, pitch: -10, preset: .linear))
                library.update(s)
                viewer.select(s)
            }
        }
        if let p = env["REFRAME_PRESET"].flatMap(InterestWeights.Preset.init(rawValue:)) {
            library.setPreset(p, persist: false)
            try? await Task.sleep(for: .seconds(1))
        }
        if env["REFRAME_SUGGESTION"] != nil {
            for _ in 0..<100 where viewer.suggestions.isEmpty { try? await Task.sleep(for: .milliseconds(100)) }
            let pick = env["REFRAME_SUGGESTION"] == "last" ? viewer.suggestions.max(by: { $0.inTime < $1.inTime })
                : viewer.suggestions.max(by: { $0.score < $1.score })
            if let s = pick {
                viewer.select(s)
                if env["REFRAME_SUGGESTION"] == "accept" { viewer.acceptSuggestion() }
            }
            if env["REFRAME_DEBUG"] != nil {
                FileHandle.standardError.write("suggestion hook: \(viewer.suggestions.count) suggestions, picked \(pick?.id ?? "none"), active shot \(viewer.activeShot?.suggestionID ?? "none"), shots \(library.shots.count)\n".data(using: .utf8)!)
            }
        }
        if env["REFRAME_SELFTEST"] != nil { selfTest() }
        if let t = env["REFRAME_SEEK"].flatMap(Double.init) { viewer.scrub(to: t, final: true) }
        if env["REFRAME_AUTOPLAY"] != nil { viewer.togglePlay() }
        if env["REFRAME_EXPORT_SHEET"] != nil { showExport = true }
        if env["REFRAME_SETTINGS"] != nil { openSettings() }
        if env["REFRAME_BROWSE"] == "shots" {
            library.browse = .shots
            if let n = env["REFRAME_SHOTS_SELECT"].flatMap(Int.init) { shotSelection = Set(library.shots.prefix(n).map(\.id)) }
            if env["REFRAME_DEBUG"] != nil, let shot = library.shots.first(where: { library.item(for: $0.clipPath) != nil }) {
                library.loadShotFrames(shot)
                for _ in 0..<100 where library.shotFrames[shot.id] == nil { try? await Task.sleep(for: .milliseconds(100)) }
                FileHandle.standardError.write("shots hook: \(library.shots.count) shots, hover frames for \(shot.name) of \(URL(fileURLWithPath: shot.clipPath).lastPathComponent): \(library.shotFrames[shot.id]?.count ?? 0)\n".data(using: .utf8)!)
            }
        }
        // REFRAME_GESTURE_TEST=x,y;x,y (points from the window's top-left: the first card's name, then
        // its picture): double-click the name -> renaming, Esc; click the picture -> selected;
        // double-click the picture -> the editor on that shot.
        if let spec = env["REFRAME_GESTURE_TEST"], library.browse == .shots,
           let shot = library.shots.first(where: { $0.clipPath.contains("_001.insv") }) {
            func report(_ s: String) { FileHandle.standardError.write("gesture test: \(s)\n".data(using: .utf8)!) }
            let pts = spec.split(separator: ";").map { $0.split(separator: ",").compactMap { Double($0) } }
            for _ in 0..<100 where library.shotThumbnails[shot.id] == nil { try? await Task.sleep(for: .milliseconds(100)) }
            try? await Task.sleep(for: .seconds(1))
            guard pts.count == 2, let window = NSApp.windows.first(where: { $0.isVisible && $0.frame.width > 400 }) else { report("bad setup"); return }
            let h = window.contentView!.bounds.height
            func click(_ p: [Double], count: Int) {
                let loc = NSPoint(x: p[0], y: h - p[1])
                for n in 1...count {
                    for type in [NSEvent.EventType.leftMouseDown, .leftMouseUp] {
                        if let e = NSEvent.mouseEvent(with: type, location: loc, modifierFlags: [], timestamp: ProcessInfo.processInfo.systemUptime,
                                                      windowNumber: window.windowNumber, context: nil, eventNumber: 0, clickCount: n, pressure: 1) {
                            NSApp.postEvent(e, atStart: false)
                        }
                    }
                }
            }
            func editing() -> Bool { (window.firstResponder as? NSTextView)?.isFieldEditor == true }
            click(pts[0], count: 2)
            try? await Task.sleep(for: .milliseconds(800))
            report("double-click name -> editing \(editing()), selected \(shotSelection == [shot.id])")
            if let e = NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: [], timestamp: ProcessInfo.processInfo.systemUptime,
                                        windowNumber: window.windowNumber, context: nil, characters: "\u{1b}",
                                        charactersIgnoringModifiers: "\u{1b}", isARepeat: false, keyCode: 53) { NSApp.postEvent(e, atStart: false) }
            try? await Task.sleep(for: .milliseconds(500))
            report("esc -> editing \(editing()), name '\(library.shots.first { $0.id == shot.id }?.name ?? "?")'")
            shotSelection = []
            click(pts[1], count: 1)
            try? await Task.sleep(for: .milliseconds(800))
            report("click picture -> selected \(shotSelection == [shot.id]), still shots view \(library.browse == .shots)")
            click(pts[1], count: 2)
            for _ in 0..<80 where viewer.activeShotID != shot.id { try? await Task.sleep(for: .milliseconds(100)) }
            report("double-click picture -> browse \(library.browse.rawValue), active shot \(viewer.activeShotID == shot.id)")
        }
        // REFRAME_FOCUS_TEST=1: open the first shot, focus the inspector's Name field, then a
        // click on the viewer and a Return must each give the keyboard back to the editor.
        if env["REFRAME_FOCUS_TEST"] != nil, let target = library.shots.first(where: { library.item(for: $0.clipPath) != nil }) {
            library.openInEditor(target)
            for _ in 0..<80 where viewer.activeShotID != target.id { try? await Task.sleep(for: .milliseconds(100)) }
            try? await Task.sleep(for: .seconds(1))
            func report(_ s: String) { FileHandle.standardError.write("focus test: \(s)\n".data(using: .utf8)!) }
            func fields(_ v: NSView) -> [NSTextField] { (v as? NSTextField).map { [$0] } ?? [] + v.subviews.flatMap(fields) }
            guard let window = NSApp.windows.first(where: { $0.isVisible && $0.frame.width > 400 }),
                  let name = viewer.activeShot?.name,
                  let field = fields(window.contentView!).first(where: { $0.isEditable && $0.stringValue == name }) else {
                report("no editable name field"); return
            }
            func editing() -> Bool { (window.firstResponder as? NSTextView)?.isFieldEditor == true }
            func post(_ e: NSEvent?) { if let e { NSApp.postEvent(e, atStart: false) } }
            window.makeFirstResponder(field)
            report("focused: \(editing())")
            // A click in the middle of the viewer area (window coordinates, origin bottom-left).
            let p = NSPoint(x: window.frame.width * 0.45, y: window.frame.height * 0.7)
            for type in [NSEvent.EventType.leftMouseDown, .leftMouseUp] {
                post(NSEvent.mouseEvent(with: type, location: p, modifierFlags: [], timestamp: ProcessInfo.processInfo.systemUptime,
                                        windowNumber: window.windowNumber, context: nil, eventNumber: 0, clickCount: 1, pressure: 1))
            }
            try? await Task.sleep(for: .milliseconds(500))
            report("after clicking the viewer, still editing: \(editing())")
            window.makeFirstResponder(field)
            for type in [NSEvent.EventType.keyDown, .keyUp] {
                post(NSEvent.keyEvent(with: type, location: .zero, modifierFlags: [], timestamp: ProcessInfo.processInfo.systemUptime,
                                      windowNumber: window.windowNumber, context: nil, characters: "\r",
                                      charactersIgnoringModifiers: "\r", isARepeat: false, keyCode: 36))
            }
            try? await Task.sleep(for: .milliseconds(500))
            report("after Return, still editing: \(editing()), name '\(viewer.activeShot?.name ?? "?")'")
        }
        // REFRAME_RENAME_TEST=1: rename the first shot and back, reporting each step.
        if env["REFRAME_RENAME_TEST"] != nil, let shot = library.shots.first {
            let original = shot.name
            library.rename(shot.id, to: "  Rename test  ")
            let renamed = library.shots.first { $0.id == shot.id }?.name ?? "?"
            library.rename(shot.id, to: "   ")  // ignored
            let afterBlank = library.shots.first { $0.id == shot.id }?.name ?? "?"
            library.rename(shot.id, to: original)
            let back = library.shots.first { $0.id == shot.id }?.name ?? "?"
            FileHandle.standardError.write("rename test: '\(original)' -> '\(renamed)' -> blank keeps '\(afterBlank)' -> back '\(back)'\n".data(using: .utf8)!)
        }
        // REFRAME_OPEN_SHOT=<n>: "Open in Editor" on the n-th shot (0-based), then report where the viewer is.
        if let n = env["REFRAME_OPEN_SHOT"].flatMap(Int.init), n < library.shots.count {
            let shot = library.shots[n]
            library.openInEditor(shot)
            for _ in 0..<80 where viewer.activeShotID != shot.id { try? await Task.sleep(for: .milliseconds(100)) }
            try? await Task.sleep(for: .seconds(1))
            FileHandle.standardError.write(String(format: "open shot: %@ %@ browse=%@ active=%@ t=%.2f in=%.2f\n", shot.name,
                URL(fileURLWithPath: shot.clipPath).lastPathComponent, library.browse.rawValue,
                viewer.activeShotID == shot.id ? "yes" : "no", viewer.currentTime, shot.inTime).data(using: .utf8)!)
        }
        if let snap = env["REFRAME_SNAPSHOT"] {
            try? await Task.sleep(for: .seconds(6))
            viewer.debugCapturePath = snap.replacingOccurrences(of: ".png", with: "-viewer.png")
        }
    }
}

extension ContentView {
    /// REFRAME_SELFTEST=1: exercises keyframe editing, trimming and undo through the same calls the
    /// mouse and keys make, prints PASS/FAIL lines to stderr, then undoes everything.
    func selfTest() {
        func check(_ name: String, _ ok: Bool) {
            FileHandle.standardError.write("selftest \(ok ? "PASS" : "FAIL"): \(name)\n".data(using: .utf8)!)
        }
        guard viewer.clip != nil else { check("clip loaded", false); return }
        let before = library.shots
        viewer.scrub(to: 10, final: true)
        guard let shot = viewer.newShot(from: 10, to: 15) else { check("new shot", false); return }
        let base = shot.keyframes[0].framing.yaw
        check("new shot has one keyframe at its in point", viewer.activeShot?.keyframes.count == 1)

        // Edit between keyframes: adds one at the playhead, leaves the first alone.
        viewer.scrub(to: 13, final: true)
        viewer.beginManualEdit(); viewer.manual.yaw = base + 40
        var s = viewer.activeShot!
        check("editing between keyframes adds a keyframe there", s.keyframes.count == 2 && abs(s.keyframes[1].time - 13) < 0.01)
        check("the first keyframe is unchanged", abs(s.keyframes[0].framing.yaw - base) < 0.01)
        check("view shows the edit", abs(viewer.displayFraming.yaw - (base + 40)) < 0.5)

        // Near an existing keyframe (inside the on-screen spacing): edit it, don't stack another.
        let near = 13 + viewer.keyframeSpacing * 0.6
        viewer.scrub(to: near, final: true)
        check("a keyframe a few points away counts as selected", viewer.currentKeyframe?.time == 13)
        viewer.beginManualEdit(); viewer.manual.yaw = base + 50
        s = viewer.activeShot!
        check("editing near a keyframe edits it (no stacked keyframe)", s.keyframes.count == 2 && abs(s.keyframes[1].framing.yaw - (base + 50)) < 0.01)
        check("the playhead snaps onto that keyframe", abs(viewer.currentTime - 13) < 1e-6)
        viewer.scrub(to: near, final: true)
        viewer.addKeyframe()
        check("K near a keyframe selects it instead of adding one", viewer.activeShot!.keyframes.count == 2 && abs(viewer.currentTime - 13) < 1e-6)
        viewer.moveKeyframe(viewer.activeShot!.keyframes[1].id, to: 10.01, final: true)
        check("a dragged keyframe stops short of its neighbour", viewer.activeShot!.keyframes[1].time >= 10 + viewer.keyframeSpacing - 1e-6)
        viewer.moveKeyframe(viewer.activeShot!.keyframes[1].id, to: 13, final: true)

        // Select the first keyframe (playhead on it) and edit: only it changes.
        viewer.scrub(to: 10, final: true)
        check("keyframe at playhead is selected", viewer.currentKeyframe?.id == s.keyframes[0].id)
        viewer.beginManualEdit(); viewer.manual.yaw = base - 30; viewer.apply(.mega)
        s = viewer.activeShot!
        check("editing a selected keyframe changes it", abs(s.keyframes[0].framing.yaw - (base - 30)) < 0.01 && s.keyframes[0].framing.preset == .mega)
        check("other keyframes keep their framing", abs(s.keyframes[1].framing.yaw - (base + 50)) < 0.01 && s.keyframes[1].framing.preset != .mega)
        check("no extra keyframes", s.keyframes.count == 2)

        // Retime, trim, delete.
        viewer.moveKeyframe(s.keyframes[1].id, to: 14, final: true)
        check("keyframe moved", abs(viewer.activeShot!.keyframes[1].time - 14) < 0.01 && abs(viewer.currentTime - 14) < 0.01)
        viewer.moveEdge(of: shot.id, isIn: false, to: 18, final: true)
        check("out point dragged", abs(viewer.activeShot!.outTime - 18) < 0.01)
        viewer.moveEdge(of: shot.id, isIn: true, to: 17.9, final: true)
        check("in point clamps to keep 0.5 s", abs(viewer.activeShot!.inTime - 17.5) < 0.01)
        viewer.scrub(to: 14, final: true)
        check("delete removes the keyframe at the playhead", viewer.removeCurrentKeyframe() && viewer.activeShot!.keyframes.count == 1)

        // Outside any shot the view is free and nothing is written.
        viewer.scrub(to: 30, final: true)
        let snapshot = library.shots
        viewer.beginManualEdit(); viewer.manual.yaw += 25
        check("editing outside a shot writes nothing", library.shots == snapshot)

        // Undo all of it.
        for _ in 0..<40 where library.shots != before { library.undo() }
        check("undo restores the shots", library.shots == before)
        library.redo()
        check("redo works", library.shots != before)
        library.undo()
        viewer.deselectShot()
        check("back to the original shots", library.shots == before)
    }
}

// MARK: - Sidebar

struct Sidebar: View {
    @Environment(Library.self) private var library

    var body: some View {
        @Bindable var library = library
        ScrollViewReader { proxy in
        List(selection: $library.selection) {
            ForEach(library.days, id: \.day) { day in
                Section(day.day == Calendar.current.startOfDay(for: .distantPast) ? "Undated"
                        : day.day.formatted(.dateTime.weekday(.wide).month(.wide).day())) {
                    ForEach(day.rows) { row in
                        Group {
                            if let item = row.item {
                                ClipRow(item: item, shotCount: library.shots(for: item.clip).count)
                            } else if let m = row.media {
                                CloudClipRow(media: m)
                            }
                        }
                        .tag(row.id)
                        .id(row.id)
                        .contextMenu { if row.media != nil || row.item?.isCloud == true { cloudMenu(row) } }
                    }
                }
            }
        }
        .listStyle(.sidebar)
        .onChange(of: library.selection) { _, sel in
            if let sel { withAnimation { proxy.scrollTo(sel) } }
        }
        .onChange(of: library.selectedItem?.id) { _, sel in
            // Fetched cloud clips and late-loading folders shift the rows: scroll again.
            if let sel { DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) { withAnimation { proxy.scrollTo(sel, anchor: .center) } } }
        }
        }
        .overlay {
            if library.items.isEmpty && library.loading { ProgressView() }
        }
        .safeAreaInset(edge: .bottom) {
            HStack {
                Button { library.chooseFolder() } label: {
                    Label(library.folder?.lastPathComponent ?? "Open Folder…", systemImage: "folder")
                        .lineLimit(1)
                }
                .buttonStyle(.borderless)
                .help("Open a different footage folder (⌘O)")
                Spacer()
                if library.loading { ProgressView().controlSize(.mini) }
                CloudButton()
                Menu {
                    Picker("Sort clips", selection: $library.sortByInterest) {
                        Text("By recording time").tag(false)
                        Text("Best moments first").tag(true)
                    }
                    .pickerStyle(.inline)
                } label: {
                    Image(systemName: "arrow.up.arrow.down")
                }
                .menuStyle(.borderlessButton)
                .menuIndicator(.hidden)
                .fixedSize()
                .help(library.sortByInterest ? "Best moments first within each day" : "By recording time within each day")
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
        }
    }
}

extension Sidebar {
    @ViewBuilder func cloudMenu(_ row: Library.Row) -> some View {
        if let item = row.item {
            if item.originalInCloud, let r = CloudCache.record(for: item.clip.url) {
                Button("Download Original (\(formatBytes(r.size)))") {
                    Task { await library.downloadOriginals([item.clip]) }
                }
            }
            Button("Remove Downloaded Files") { library.removeCloudCache(item) }
        } else {
            Button("Fetch from Cloud") { library.selection = row.id }
        }
        Divider()
        Button("Fetch All Cloud Clips") { library.fetchAllCloudClips() }
    }
}

/// A recording that is only in Insta360 Cloud so far: its cloud cover, length and size.
struct CloudClipRow: View {
    @Environment(Library.self) private var library
    let media: CloudMedia

    var body: some View {
        let stage = library.cloud.fetching[media.id]
        VStack(alignment: .leading, spacing: 6) {
            ZStack {
                Rectangle().fill(Theme.surround)
                if let thumb = library.cloud.thumbnails[media.id] {
                    Image(decorative: thumb, scale: 1).resizable().aspectRatio(2, contentMode: .fill).opacity(0.7)
                }
                if stage != nil { ProgressView().controlSize(.small) }
            }
            .aspectRatio(2, contentMode: .fit)
            .clipShape(RoundedRectangle(cornerRadius: 5))
            .overlay(alignment: .bottomTrailing) {
                if let d = media.duration {
                    Text(formatDuration(d))
                        .font(Theme.numeric(10, weight: .semibold))
                        .foregroundStyle(.white)
                        .padding(.horizontal, 5).padding(.vertical, 1.5)
                        .background(.black.opacity(0.55), in: RoundedRectangle(cornerRadius: 3))
                        .padding(5)
                }
            }
            .overlay(alignment: .topTrailing) { CloudBadge(originalInCloud: true).padding(5) }
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(media.captureDate.map { $0.formatted(date: .omitted, time: .shortened) } ?? media.name)
                    .font(Theme.title)
                Text(stage?.label ?? library.cloud.errors[media.id].map { _ in "Couldn't fetch" }
                     ?? ([media.fps.map { "\(Int($0.rounded()))p" }, media.size.map(formatBytes)].compactMap { $0 }
                            .joined(separator: ", ")))
                    .font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(1)
                Spacer(minLength: 0)
            }
        }
        .padding(.vertical, 5)
        .task(id: media.thumbnailURL) { library.cloud.loadThumbnail(media) }
    }
}

struct CloudBadge: View {
    let originalInCloud: Bool

    var body: some View {
        Image(systemName: originalInCloud ? "cloud" : "checkmark.icloud")
            .font(.system(size: 10, weight: .semibold))
            .foregroundStyle(.white)
            .padding(3)
            .background(.black.opacity(0.55), in: RoundedRectangle(cornerRadius: 3))
            .help(originalInCloud ? "In Insta360 Cloud: browsing from its low-res proxy; the original downloads when you export."
                  : "From Insta360 Cloud; the original is downloaded.")
    }
}

/// Sidebar footer control: connect, refresh, fetch all, sign out.
struct CloudButton: View {
    @Environment(Library.self) private var library

    var body: some View {
        let cloud = library.cloud
        Group {
            if cloud.isSignedIn {
                Menu {
                    if let q = cloud.quota {
                        Text(String(format: "%.0f of %.0f GB used", q.used, q.total))
                    }
                    if case .failed(let m) = cloud.state { Text(m) }
                    Button("Refresh") { Task { await cloud.refresh() } }
                    Button("Fetch All Cloud Clips") { library.fetchAllCloudClips() }
                    SettingsLink { Text("Manage Storage…") }
                    Divider()
                    Button("Sign Out of Insta360 Cloud") { cloud.signOut() }
                } label: {
                    if cloud.state == .loading || !cloud.fetching.isEmpty {
                        ProgressView().controlSize(.mini)
                    } else {
                        Image(systemName: { if case .failed = cloud.state { return "exclamationmark.icloud" } else { return "icloud" } }())
                    }
                }
                .menuStyle(.borderlessButton)
                .menuIndicator(.hidden)
                .fixedSize()
                .help("Insta360 Cloud: \(cloud.media.count) recordings")
            } else {
                Button { cloud.beginSignIn() } label: { Image(systemName: "icloud.slash") }
                    .buttonStyle(.borderless)
                    .help("Connect Insta360 Cloud to browse recordings without downloading them")
            }
        }
    }
}

/// The main pane while a cloud recording is fetched (or failed to).
struct CloudFetchView: View {
    @Environment(Library.self) private var library
    let url: URL

    var body: some View {
        let cloud = library.cloud
        let m = cloud.media(for: url)
        let id = m?.id ?? ""
        ContentUnavailableView {
            Label(m?.name ?? url.lastPathComponent, systemImage: "icloud.and.arrow.down")
        } description: {
            if let stage = cloud.fetching[id] {
                VStack(spacing: 8) {
                    Text(stage.label)
                    if let f = stage.fraction { ProgressView(value: f).frame(width: 240) } else { ProgressView().controlSize(.small) }
                    Text("Only the gyro and the low-res proxy come down; the original stays in the cloud until you export.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            } else if let err = cloud.errors[id] {
                Text(err)
            } else {
                Text("Fetching from Insta360 Cloud…")
            }
        } actions: {
            if cloud.errors[id] != nil {
                Button("Try Again") { library.retryCloudFetch() }.buttonStyle(.borderedProminent)
            }
        }
    }
}

func formatBytes(_ b: Int64) -> String { ByteCountFormatter.string(fromByteCount: b, countStyle: .file) }

struct ClipRow: View {
    let item: ClipItem
    let shotCount: Int

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            ZStack {
                Rectangle().fill(Theme.surround)
                if let thumb = item.thumbnail {
                    Image(decorative: thumb, scale: 1).resizable().aspectRatio(2, contentMode: .fill)
                } else {
                    ProgressView().controlSize(.small)
                }
            }
            .aspectRatio(2, contentMode: .fit)
            .clipShape(RoundedRectangle(cornerRadius: 5))
            .overlay(alignment: .bottomTrailing) {
                Text(formatDuration(item.clip.duration))
                    .font(Theme.numeric(10, weight: .semibold))
                    .foregroundStyle(.white)
                    .padding(.horizontal, 5).padding(.vertical, 1.5)
                    .background(.black.opacity(0.55), in: RoundedRectangle(cornerRadius: 3))
                    .padding(5)
            }
            .overlay(alignment: .topTrailing) {
                if item.isCloud { CloudBadge(originalInCloud: item.originalInCloud).padding(5) }
            }
            .overlay(alignment: .bottomLeading) {
                if shotCount > 0 {
                    HStack(spacing: 3) {
                        Circle().fill(Theme.sightline).frame(width: 6, height: 6)
                        Text("\(shotCount)").font(Theme.numeric(10, weight: .semibold))
                    }
                    .foregroundStyle(.white)
                    .padding(.horizontal, 5).padding(.vertical, 1.5)
                    .background(.black.opacity(0.55), in: RoundedRectangle(cornerRadius: 3))
                    .padding(5)
                    .help("\(shotCount) shot\(shotCount == 1 ? "" : "s") marked")
                }
            }
            if !item.interest.isEmpty {
                InterestSparkline(values: item.interest)
                    .frame(height: 10)
                    .help(sparklineHelp)
            }
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(item.clip.captureDate.map { $0.formatted(date: .omitted, time: .shortened) } ?? item.clip.name)
                    .font(Theme.title)
                Text(subtitle).font(.system(size: 11)).foregroundStyle(.secondary)
                Spacer(minLength: 0)
                if let s = item.shake, !item.clip.info.isTimelapse { ShakeMeter(p95: s.p95) }
            }
        }
        .padding(.vertical, 5)
    }

    private var sparklineHelp: String {
        let n = item.analysis?.suggestions.count ?? 0
        let best = item.analysis?.suggestions.first.map { " The best is \($0.title.lowercased()) at \(formatDuration($0.peakTime))." } ?? ""
        return "Interest over the clip: \(n) suggested shot\(n == 1 ? "" : "s").\(best)"
    }

    private var subtitle: String {
        let info = item.clip.info
        if info.isTimelapse { return "Timelapse" }
        return "\(info.colorLabel), \(info.nominalFPS ?? 0)p"
    }
}

/// Interest across a clip as a quiet filled curve (plain ink: color belongs to the sightline).
struct InterestSparkline: View {
    let values: [Float]

    var body: some View {
        Canvas { ctx, size in
            guard values.count > 1 else { return }
            let cols = max(Int(size.width / 2), 2)
            var p = Path()
            p.move(to: CGPoint(x: 0, y: size.height))
            for c in 0..<cols {
                let a = c * values.count / cols, b = max(a + 1, (c + 1) * values.count / cols)
                let v = CGFloat(values[a..<min(b, values.count)].max() ?? 0)
                p.addLine(to: CGPoint(x: CGFloat(c) / CGFloat(cols - 1) * size.width, y: size.height * (1 - v)))
            }
            p.addLine(to: CGPoint(x: size.width, y: size.height))
            p.closeSubpath()
            ctx.fill(p, with: .color(.primary.opacity(0.35)))
        }
        .accessibilityLabel("Interest over the clip")
    }
}

// MARK: - Viewer

struct ViewerPane: View {
    @Environment(Library.self) private var library
    @Bindable var viewer: ViewerModel

    var body: some View {
        ZStack {
            Theme.surround
            MetalViewer(model: viewer, renderer: library.renderer)
                .aspectRatio(viewer.projection == .flat ? 16.0 / 9.0 : 2, contentMode: .fit)
                .overlay(alignment: .bottomLeading) { hud }
                .overlay(alignment: .bottomTrailing) {
                    LensPresetPicker(current: viewer.displayFraming, compact: true) { viewer.apply($0) }
                        .padding(3)
                        .background(.black.opacity(0.45), in: Capsule())
                        .padding(10)
                }
                .overlay {
                    if viewer.projection == .equirect { aimMarker }
                }
                .padding(12)
        }
        .frame(minHeight: 260)
    }

    private var hud: some View {
        let f = viewer.displayFraming
        return HStack(spacing: 12) {
            Text(String(format: "%+.0f°", f.yaw)).help("Yaw: left is positive")
            Text(String(format: "%+.0f°", f.pitch)).help("Pitch")
            Text(f.preset?.label ?? String(format: "%.0f°", f.hfov)).help(String(format: "Covers %.0f° across", f.coverage))
            if viewer.editsKeyframe {
                let on = viewer.currentKeyframe != nil
                Image(systemName: on ? "diamond.fill" : "diamond")
                    .foregroundStyle(Theme.sightline)
                    .help(on ? "On a keyframe: changing the view edits it" : "Between keyframes: changing the view adds one here")
            }
        }
        .font(Theme.numeric(11))
        .foregroundStyle(.white.opacity(0.9))
        .padding(.horizontal, 9).padding(.vertical, 4)
        .background(.black.opacity(0.45), in: Capsule())
        .padding(10)
    }

    /// In 360° mode, outlines the frame that would be exported (centred, at its pitch).
    private var aimMarker: some View {
        GeometryReader { geo in
            let f = viewer.displayFraming
            let w = geo.size.width * f.coverage / 360
            let h = geo.size.height * f.verticalCoverage(aspect: 9 / 16) / 180
            RoundedRectangle(cornerRadius: 3)
                .strokeBorder(Theme.sightline, lineWidth: 1.5)
                .frame(width: w, height: h)
                .position(x: geo.size.width / 2, y: geo.size.height * (0.5 - f.pitch / 180))
                .allowsHitTesting(false)
        }
    }
}

struct LookMenu: View {
    @Environment(Library.self) private var library
    @Bindable var viewer: ViewerModel

    var body: some View {
        Menu {
            Toggle("Preview with LUT", isOn: $viewer.showLUT)
            Divider()
            ForEach(library.lutURLs, id: \.self) { url in
                Button {
                    library.useLUT(url)
                    viewer.showLUT = true
                } label: {
                    if library.activeLUT == url { Label(CubeLUT.shortName(url), systemImage: "checkmark") }
                    else { Text(CubeLUT.shortName(url)) }
                }
            }
            Divider()
            Button("Choose LUT Folder…") { library.chooseLUTFolder() }
        } label: {
            Label(lookLabel, systemImage: "camera.filters")
                .labelStyle(.titleAndIcon)
        }
        .help("Preview look for I-Log clips. Exports stay in I-Log unless you bake the look in.")
    }

    private var lookLabel: String {
        guard viewer.clip?.info.colorProfile == "I_Log" else { return viewer.clip?.info.colorLabel ?? "Look" }
        return viewer.showLUT ? (library.activeLUT.map(CubeLUT.shortName) ?? "LUT") : "I-Log"
    }
}
