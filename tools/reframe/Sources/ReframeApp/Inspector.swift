import ReframeKit
import SwiftUI

struct Inspector: View {
    @Environment(Library.self) private var library
    let item: ClipItem?
    @Bindable var viewer: ViewerModel

    var body: some View {
        if let item {
            Form {
                if let shot = viewer.activeShot { ShotSection(shot: shot, viewer: viewer) }
                FramingSection(viewer: viewer)
                Section("Stabilization") {
                    Picker("Mode", selection: $viewer.stabilization) {
                        ForEach(Motion.Stabilization.allCases) { Text($0.label).tag($0) }
                    }
                    .pickerStyle(.segmented)
                    if viewer.stabilization == .follow {
                        LabeledContent("Smoothing") {
                            Slider(value: $viewer.smoothing, in: 0.1...3)
                        }
                    }
                    Toggle("Correct rolling shutter", isOn: $viewer.rollingShutter)
                }
                ClipSection(item: item)
            }
            .formStyle(.grouped)
        } else {
            ContentUnavailableView("No clip selected", systemImage: "camera.metering.unknown")
        }
    }
}

private struct ShotSection: View {
    let shot: Shot
    @Bindable var viewer: ViewerModel
    @State private var name = ""

    var body: some View {
        Section("Shot") {
            TextField("Name", text: $name)
                .onAppear { name = shot.name }
                .onChange(of: shot.id) { _, _ in name = shot.name }
                .onSubmit { viewer.renameActiveShot(name) }
            LabeledContent("In") {
                Button(formatTimecode(shot.inTime, fps: viewer.fps)) { viewer.scrub(to: shot.inTime, final: true) }
                    .buttonStyle(.plain).font(Theme.numeric(12)).help("Go to this point")
            }
            LabeledContent("Out") {
                Button(formatTimecode(shot.outTime, fps: viewer.fps)) { viewer.scrub(to: shot.outTime, final: true) }
                    .buttonStyle(.plain).font(Theme.numeric(12)).help("Go to this point")
            }
            LabeledContent("Length") { Text(formatDuration(shot.duration)).font(Theme.numeric(12)) }
            ForEach(shot.keyframes) { k in
                HStack(spacing: 8) {
                    Image(systemName: "diamond.fill").font(.system(size: 8)).foregroundStyle(Theme.sightline)
                    Button(formatTimecode(k.time, fps: viewer.fps)) { viewer.scrub(to: k.time, final: true) }
                        .buttonStyle(.plain)
                        .help("Go to this keyframe")
                    Spacer()
                    Text(String(format: "%+.0f°  %.0f° fov", k.framing.yaw, k.framing.hfov))
                        .foregroundStyle(.secondary)
                    if shot.keyframes.count > 1 {
                        Button { viewer.removeKeyframe(k.id) } label: { Image(systemName: "minus.circle") }
                            .buttonStyle(.borderless)
                            .help("Remove keyframe")
                    }
                }
                .font(Theme.numeric(11))
            }
            Button("Delete Shot", role: .destructive) { viewer.deleteActiveShot() }
        }
    }
}

private struct FramingSection: View {
    @Bindable var viewer: ViewerModel

    var body: some View {
        Section("Framing") {
            row("Yaw", value: \.yaw, range: -180...180)
            row("Pitch", value: \.pitch, range: -89...89)
            row("Field of view", value: \.hfov, range: 20...160)
            HStack {
                Button("Reset") { viewer.resetFraming() }
                Spacer()
                if viewer.activeShot != nil {
                    Text(viewer.followPath ? "Following keyframes" : "Steering by hand").caption()
                }
            }
        }
    }

    private func row(_ title: String, value: WritableKeyPath<Framing, Double>, range: ClosedRange<Double>) -> some View {
        LabeledContent(title) {
            HStack(spacing: 8) {
                Slider(value: Binding(get: { viewer.displayFraming[keyPath: value] },
                                      set: { v in viewer.beginManualEdit(); viewer.manual[keyPath: value] = v }),
                       in: range)
                Text(String(format: "%.0f°", viewer.displayFraming[keyPath: value]))
                    .font(Theme.numeric(11))
                    .foregroundStyle(.secondary)
                    .frame(width: 34, alignment: .trailing)
            }
        }
    }
}

private struct ClipSection: View {
    let item: ClipItem

    var body: some View {
        let info = item.clip.info
        Section("Clip") {
            LabeledContent("Camera", value: "\(info.camera), \(info.firmware)")
            LabeledContent("Color", value: info.colorLabel)
            LabeledContent("Frame rate", value: "\(info.nominalFPS ?? 0) fps")
            LabeledContent("Length", value: formatDuration(item.clip.duration))
            if let iv = info.timelapseIntervalMs {
                LabeledContent("Interval", value: String(format: "%g s", Double(iv) / 1000))
            }
            if let s = item.shake {
                LabeledContent("Camera motion") {
                    HStack(spacing: 6) {
                        ShakeMeter(p95: s.p95)
                        Text(String(format: "%.0f°/s", s.p95)).font(Theme.numeric(11)).foregroundStyle(.secondary)
                    }
                }
            }
            LabeledContent("Rolling shutter", value: String(format: "%.1f ms", info.rollingShutterMs ?? 0))
            LabeledContent("Proxy", value: item.clip.proxyURL == nil ? "None" : "Low-res LRV")
            Text(item.clip.url.path)
                .font(.system(size: 10))
                .foregroundStyle(.tertiary)
                .textSelection(.enabled)
        }
    }
}
