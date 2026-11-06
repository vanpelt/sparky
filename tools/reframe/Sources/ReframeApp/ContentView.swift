import AppKit
import ReframeKit
import SwiftUI

struct ContentView: View {
    @Environment(Library.self) private var library
    @State private var viewer = ViewerModel()
    @State private var showInspector = true
    @State private var showExport = false
    @State private var keyMonitor: Any?

    var body: some View {
        NavigationSplitView {
            Sidebar()
                .navigationSplitViewColumnWidth(min: 230, ideal: 260, max: 360)
        } detail: {
            if viewer.clip != nil {
                VStack(spacing: 0) {
                    ViewerPane(viewer: viewer)
                    Divider()
                    TimelinePane(viewer: viewer)
                }
            } else {
                emptyState
            }
        }
        .inspector(isPresented: $showInspector) {
            Inspector(item: library.selectedItem, viewer: viewer)
                .inspectorColumnWidth(min: 250, ideal: 280, max: 360)
        }
        .toolbar { toolbar }
        .navigationTitle(library.selectedItem.flatMap { $0.clip.captureDate }
            .map { $0.formatted(date: .abbreviated, time: .shortened) } ?? "Reframe")
        .navigationSubtitle(library.folder?.lastPathComponent ?? "")
        .tint(Theme.sightline)
        .sheet(isPresented: $showExport) { ExportSheet() }
        .onChange(of: library.selection, initial: true) { _, _ in
            viewer.library = library
            viewer.load(library.selectedItem?.clip)
        }
        .onAppear { installKeys() }
        .onDisappear { if let keyMonitor { NSEvent.removeMonitor(keyMonitor) } }
        .task { await debugHooks() }
    }

    private var emptyState: some View {
        ContentUnavailableView {
            Label(library.items.isEmpty ? "No footage yet" : "Choose a clip", systemImage: "camera.metering.spot")
        } description: {
            Text(library.items.isEmpty ? "Open a folder of Insta360 .insv recordings to start." : "Pick a clip from the sidebar.")
        } actions: {
            if library.items.isEmpty {
                Button("Open Footage Folder…") { library.chooseFolder() }.buttonStyle(.borderedProminent)
            }
        }
    }

    @ToolbarContentBuilder private var toolbar: some ToolbarContent {
        ToolbarItemGroup(placement: .principal) {
            if viewer.clip != nil {
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
            Button { showExport = true } label: {
                Label(library.shots.isEmpty ? "Export" : "Export \(library.shots.count)", systemImage: "square.and.arrow.up")
                    .labelStyle(.titleAndIcon)
            }
            .disabled(library.shots.isEmpty)
            .help("Render shots to ProRes with timelines for Resolve and Final Cut (⌘E)")
            .keyboardShortcut("e")
            Button { showInspector.toggle() } label: { Label("Inspector", systemImage: "sidebar.right") }
        }
    }

    /// Single-key shortcuts, ignored while typing in a text field.
    private func installKeys() {
        guard keyMonitor == nil else { return }
        keyMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { event in
            if NSApp.keyWindow?.firstResponder is NSText || event.modifierFlags.contains(.command) || showExport {
                return event
            }
            switch (event.keyCode, event.charactersIgnoringModifiers?.lowercased()) {
            case (49, _): viewer.togglePlay()
            case (123, _): viewer.step(frames: event.modifierFlags.contains(.shift) ? -10 : -1)
            case (124, _): viewer.step(frames: event.modifierFlags.contains(.shift) ? 10 : 1)
            case (_, "n"): viewer.newShot()
            case (_, "i"): viewer.markIn()
            case (_, "o"): viewer.markOut()
            case (_, "k"): viewer.addKeyframe()
            case (_, "f"): viewer.projection = viewer.projection == .flat ? .equirect : .flat
            case (53, _): viewer.deselectShot()  // esc
            default: return event
            }
            return nil
        }
    }

    /// Environment-driven hooks for headless checks:
    /// REFRAME_SELECT=<name substring>, REFRAME_PROJECTION=equirect, REFRAME_YAW=<deg>,
    /// REFRAME_AUTOPLAY=1, REFRAME_SEEK=<s>, REFRAME_DEMO_SHOT=1, REFRAME_EXPORT_SHEET=1,
    /// REFRAME_SNAPSHOT=<path.png> (also writes <path>-viewer.png).
    private func debugHooks() async {
        let env = ProcessInfo.processInfo.environment
        if let name = env["REFRAME_SELECT"] {
            for _ in 0..<100 {
                if let item = library.items.first(where: { $0.clip.name.contains(name) }) {
                    library.selection = item.id
                    break
                }
                try? await Task.sleep(for: .milliseconds(100))
            }
        }
        try? await Task.sleep(for: .seconds(1.5))
        if env["REFRAME_PROJECTION"] == "equirect" { viewer.projection = .equirect }
        if let yaw = env["REFRAME_YAW"].flatMap(Double.init) { viewer.manual.yaw = yaw }
        if env["REFRAME_DEMO_SHOT"] != nil, viewer.clip != nil {
            viewer.manual = Framing(yaw: env["REFRAME_YAW"].flatMap(Double.init) ?? 0, pitch: -8, hfov: 100)
            viewer.scrub(to: 12, final: true)
            if let shot = viewer.newShot(from: 12, to: 30) {
                var s = shot
                s.setKeyframe(at: 20, framing: Framing(yaw: s.keyframes[0].framing.yaw + 150, pitch: 0, hfov: 80))
                s.setKeyframe(at: 28, framing: Framing(yaw: s.keyframes[0].framing.yaw + 175, pitch: -10, hfov: 110))
                library.update(s)
                viewer.select(s)
            }
        }
        if let t = env["REFRAME_SEEK"].flatMap(Double.init) { viewer.scrub(to: t, final: true) }
        if env["REFRAME_AUTOPLAY"] != nil { viewer.togglePlay() }
        if env["REFRAME_EXPORT_SHEET"] != nil { showExport = true }
        if let snap = env["REFRAME_SNAPSHOT"] {
            try? await Task.sleep(for: .seconds(6))
            viewer.debugCapturePath = snap.replacingOccurrences(of: ".png", with: "-viewer.png")
        }
    }
}

// MARK: - Sidebar

struct Sidebar: View {
    @Environment(Library.self) private var library

    var body: some View {
        @Bindable var library = library
        List(selection: $library.selection) {
            ForEach(library.days, id: \.day) { day in
                Section(day.day == Calendar.current.startOfDay(for: .distantPast) ? "Undated"
                        : day.day.formatted(.dateTime.weekday(.wide).month(.wide).day())) {
                    ForEach(day.items) { item in
                        ClipRow(item: item, shotCount: library.shots(for: item.clip).count).tag(item.id)
                    }
                }
            }
        }
        .listStyle(.sidebar)
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
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
        }
    }
}

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

    private var subtitle: String {
        let info = item.clip.info
        if info.isTimelapse { return "Timelapse" }
        return "\(info.colorLabel), \(info.nominalFPS ?? 0)p"
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
            Text(String(format: "%.0f° fov", f.hfov)).help("Horizontal field of view")
            if viewer.activeShot != nil {
                Image(systemName: viewer.followPath ? "diamond.fill" : "hand.draw")
                    .foregroundStyle(viewer.followPath ? Theme.sightline : .white)
                    .help(viewer.followPath ? "Following the shot's keyframes" : "Steering by hand: press K to keyframe")
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
            let w = geo.size.width * f.hfov / 360
            RoundedRectangle(cornerRadius: 3)
                .strokeBorder(Theme.sightline, lineWidth: 1.5)
                .frame(width: w, height: w * 9 / 16)
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
