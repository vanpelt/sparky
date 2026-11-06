import ReframeKit
import SwiftUI

struct ContentView: View {
    @Environment(Library.self) private var library
    @State private var viewer = ViewerModel()
    @State private var showInspector = true

    var body: some View {
        @Bindable var library = library
        NavigationSplitView {
            Sidebar()
                .navigationSplitViewColumnWidth(min: 240, ideal: 280, max: 380)
        } detail: {
            if viewer.clip != nil {
                ViewerPane(viewer: viewer)
            } else {
                ContentUnavailableView {
                    Label(library.items.isEmpty ? "No Footage" : "Select a Clip", systemImage: "camera.metering.spot")
                } description: {
                    Text(library.items.isEmpty ? "Open a folder of Insta360 .insv files." : "")
                } actions: {
                    if library.items.isEmpty { Button("Open Footage Folder…") { library.chooseFolder() } }
                }
            }
        }
        .inspector(isPresented: $showInspector) {
            ClipInspector(item: library.selectedItem, viewer: viewer)
                .inspectorColumnWidth(min: 240, ideal: 270, max: 340)
        }
        .toolbar {
            ToolbarItemGroup(placement: .principal) {
                if viewer.clip != nil { ViewerToolbar(viewer: viewer) }
            }
            ToolbarItem(placement: .primaryAction) {
                Button { showInspector.toggle() } label: { Label("Inspector", systemImage: "sidebar.right") }
            }
        }
        .navigationTitle(library.selectedItem?.clip.name ?? "Reframe")
        .navigationSubtitle(library.folder?.lastPathComponent ?? "")
        .onChange(of: library.selection, initial: true) { _, _ in
            viewer.load(library.selectedItem?.clip)
        }
        .task { await debugHooks() }
    }

    /// Environment-driven hooks for headless checks:
    /// REFRAME_SELECT=<name substring>, REFRAME_PROJECTION=equirect, REFRAME_YAW=<deg>,
    /// REFRAME_AUTOPLAY=1, REFRAME_SNAPSHOT=<path.png> (also writes <path>-viewer.png).
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
        if env["REFRAME_PROJECTION"] == "equirect" { viewer.projection = .equirect }
        if let yaw = env["REFRAME_YAW"].flatMap(Double.init) { viewer.yaw = yaw }
        if env["REFRAME_AUTOPLAY"] != nil {
            try? await Task.sleep(for: .seconds(2))
            viewer.togglePlay()
        }
        if let snap = env["REFRAME_SNAPSHOT"] {
            try? await Task.sleep(for: .seconds(env["REFRAME_AUTOPLAY"] != nil ? 5.5 : 7.5))
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
            Section {
                ForEach(library.items) { item in
                    ClipRow(item: item).tag(item.id)
                }
            } header: {
                HStack {
                    Text("\(library.items.count) clips")
                    if library.loading { ProgressView().controlSize(.mini) }
                }
            }
        }
        .listStyle(.sidebar)
        .safeAreaInset(edge: .bottom) {
            Button { library.chooseFolder() } label: {
                Label("Open Folder…", systemImage: "folder")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.borderless)
            .padding(10)
        }
    }
}

struct ClipRow: View {
    let item: ClipItem

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            ZStack {
                RoundedRectangle(cornerRadius: 6).fill(.quaternary)
                if let thumb = item.thumbnail {
                    Image(decorative: thumb, scale: 1)
                        .resizable()
                        .aspectRatio(2, contentMode: .fill)
                        .clipShape(RoundedRectangle(cornerRadius: 6))
                } else {
                    ProgressView().controlSize(.small)
                }
            }
            .aspectRatio(2, contentMode: .fit)
            .overlay(alignment: .bottomTrailing) {
                Text(formatDuration(item.clip.duration))
                    .font(.caption2.monospacedDigit().weight(.semibold))
                    .padding(.horizontal, 5).padding(.vertical, 2)
                    .background(.black.opacity(0.6), in: Capsule())
                    .foregroundStyle(.white)
                    .padding(5)
            }
            HStack(spacing: 4) {
                Text(item.clip.captureDate.map { $0.formatted(date: .abbreviated, time: .shortened) } ?? item.clip.name)
                    .font(.callout.weight(.medium))
                    .lineLimit(1)
                Spacer()
            }
            HStack(spacing: 4) {
                Badge(text: item.clip.info.colorLabel, color: badgeColor(item.clip.info.colorProfile))
                if item.clip.info.isTimelapse { Badge(text: "Timelapse", color: .purple) }
                Badge(text: "\(item.clip.info.nominalFPS ?? 0)p", color: .gray)
                if let s = item.shakeLabel { Badge(text: s, color: s == "Shaky" ? .red : s == "Moving" ? .yellow : .green) }
            }
        }
        .padding(.vertical, 4)
    }

    private func badgeColor(_ profile: String) -> Color {
        switch profile {
        case "I_Log": return .orange
        case "Dolby_Vision": return .indigo
        default: return .teal
        }
    }
}

struct Badge: View {
    let text: String
    let color: Color

    var body: some View {
        Text(text)
            .font(.caption2.weight(.semibold))
            .padding(.horizontal, 5).padding(.vertical, 1.5)
            .background(color.opacity(0.18), in: RoundedRectangle(cornerRadius: 4))
            .foregroundStyle(color)
    }
}

func formatDuration(_ s: Double) -> String {
    let t = Int(s.rounded())
    return t >= 3600 ? String(format: "%d:%02d:%02d", t / 3600, t / 60 % 60, t % 60) : String(format: "%d:%02d", t / 60, t % 60)
}

func formatTimecode(_ s: Double, fps: Int) -> String {
    let frames = Int((s * Double(max(fps, 1))).rounded())
    let f = max(fps, 1)
    return String(format: "%02d:%02d:%02d", frames / f / 60, frames / f % 60, frames % f)
}

// MARK: - Viewer

struct ViewerPane: View {
    @Environment(Library.self) private var library
    @Bindable var viewer: ViewerModel

    var body: some View {
        VStack(spacing: 0) {
            ZStack {
                Color.black
                MetalViewer(model: viewer, renderer: library.renderer)
                    .aspectRatio(viewer.projection == .flat ? 16.0 / 9.0 : 2, contentMode: .fit)
                    .overlay(alignment: .bottomLeading) { hud }
                    .overlay(alignment: .center) {
                        if viewer.projection == .equirect { aimMarker }
                    }
            }
            Divider()
            TransportBar(viewer: viewer)
        }
    }

    private var hud: some View {
        HStack(spacing: 10) {
            Label(String(format: "%.0f°", viewer.yaw), systemImage: "arrow.left.and.right")
            Label(String(format: "%.0f°", viewer.pitch), systemImage: "arrow.up.and.down")
            Label(String(format: "%.0f°", viewer.hfov), systemImage: "camera.aperture")
        }
        .font(.caption.monospacedDigit())
        .padding(.horizontal, 8).padding(.vertical, 4)
        .background(.ultraThinMaterial, in: Capsule())
        .padding(10)
    }

    /// In 360 mode, marks where the flat camera points (centre column, at its pitch).
    private var aimMarker: some View {
        GeometryReader { geo in
            let y = geo.size.height * (0.5 - viewer.pitch / 180)
            let w = geo.size.width * viewer.hfov / 360
            RoundedRectangle(cornerRadius: 3)
                .strokeBorder(.white.opacity(0.85), lineWidth: 1.5)
                .frame(width: w, height: w * 9 / 16)
                .position(x: geo.size.width / 2, y: y)
                .allowsHitTesting(false)
        }
    }
}

struct ViewerToolbar: View {
    @Environment(Library.self) private var library
    @Bindable var viewer: ViewerModel

    var body: some View {
        Picker("Projection", selection: $viewer.projection) {
            Text("Flat").tag(Projection.flat)
            Text("360°").tag(Projection.equirect)
        }
        .pickerStyle(.segmented)
        .help("Flat 16:9 reframe, or the whole sphere (click to aim)")

        Picker("Stabilization", selection: $viewer.stabilization) {
            ForEach(Motion.Stabilization.allCases) { Text($0.label).tag($0) }
        }
        .pickerStyle(.segmented)
        .help("Off: camera-fixed. Follow: level horizon, smoothed heading. Lock: fixed heading.")

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
            Label(viewer.showLUT ? (library.activeLUT.map(CubeLUT.shortName) ?? "LUT") : "Log",
                  systemImage: "camera.filters")
        }
        .help("Preview look only — exports stay in the camera's log encoding")
    }
}

struct TransportBar: View {
    @Bindable var viewer: ViewerModel
    @State private var scrubbing = false

    var body: some View {
        HStack(spacing: 12) {
            Button { viewer.step(frames: -1) } label: { Image(systemName: "backward.frame.fill") }
                .buttonStyle(.borderless)
            Button { viewer.togglePlay() } label: {
                Image(systemName: viewer.isPlaying ? "pause.fill" : "play.fill").frame(width: 18)
            }
            .buttonStyle(.borderless)
            .keyboardShortcut(.space, modifiers: [])
            Button { viewer.step(frames: 1) } label: { Image(systemName: "forward.frame.fill") }
                .buttonStyle(.borderless)

            Text(formatTimecode(viewer.currentTime, fps: viewer.clip?.info.nominalFPS ?? 30))
                .font(.callout.monospacedDigit())
                .foregroundStyle(.secondary)

            Slider(value: Binding(get: { viewer.currentTime },
                                  set: { viewer.scrub(to: $0, final: !scrubbing) }),
                   in: 0...max(viewer.duration, 0.01)) { editing in
                scrubbing = editing
                if !editing { viewer.scrub(to: viewer.currentTime, final: true) }
            }

            Text(formatDuration(viewer.duration))
                .font(.callout.monospacedDigit())
                .foregroundStyle(.secondary)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .background(.bar)
    }
}

// MARK: - Inspector

struct ClipInspector: View {
    let item: ClipItem?
    @Bindable var viewer: ViewerModel

    var body: some View {
        if let item {
            let info = item.clip.info
            Form {
                Section("Framing") {
                    LabeledContent("Yaw") { Slider(value: $viewer.yaw, in: -180...180) }
                    LabeledContent("Pitch") { Slider(value: $viewer.pitch, in: -89...89) }
                    LabeledContent("Field of view") { Slider(value: $viewer.hfov, in: 20...160) }
                    Button("Reset framing") { viewer.resetView() }
                }
                Section("Stabilization") {
                    LabeledContent("Follow smoothing") {
                        Slider(value: $viewer.smoothing, in: 0.1...3) {
                            Text("")
                        } minimumValueLabel: { Text("Tight").font(.caption2) } maximumValueLabel: { Text("Loose").font(.caption2) }
                    }
                    Toggle("Rolling-shutter correction", isOn: $viewer.rollingShutter)
                }
                Section("Clip") {
                    LabeledContent("Camera", value: "\(info.camera) · \(info.firmware)")
                    LabeledContent("Color", value: info.colorLabel)
                    LabeledContent("Frame rate", value: "\(info.nominalFPS ?? 0) fps")
                    LabeledContent("Duration", value: formatDuration(item.clip.duration))
                    if let iv = info.timelapseIntervalMs { LabeledContent("Interval", value: "\(Double(iv) / 1000)s") }
                    LabeledContent("Lens streams", value: "2 × 3840² HEVC 10-bit")
                    LabeledContent("Readout", value: String(format: "%.1f ms", info.rollingShutterMs ?? 0))
                    if let s = item.shake {
                        LabeledContent("Rotation", value: String(format: "%.0f°/s median · %.0f°/s p95", s.median, s.p95))
                    }
                    LabeledContent("Proxy", value: item.clip.proxyURL == nil ? "None" : "LRV")
                }
                Section {
                    Text(item.clip.url.path)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                }
            }
            .formStyle(.grouped)
        } else {
            ContentUnavailableView("No Selection", systemImage: "info.circle")
        }
    }
}
