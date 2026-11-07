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
                if let s = viewer.selectedSuggestion { SuggestionSection(suggestion: s, viewer: viewer) }
                if library.search.isActive, library.search.isReady { MatchesSection(item: item, viewer: viewer) }
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
                SuggestionsSection()
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
    @FocusState private var nameFocused: Bool

    private func commitName() {
        viewer.renameActiveShot(name)
        name = viewer.activeShot?.name ?? shot.name
    }

    var body: some View {
        Section("Shot") {
            TextField("Name", text: $name, prompt: Text("Shot name"))
                .focused($nameFocused)
                .onAppear { name = shot.name }
                .onChange(of: shot.id) { _, _ in name = shot.name }
                .onChange(of: shot.name) { _, n in if !nameFocused { name = n } }
                // Saves on Return and when you click away; an empty name puts the old one back.
                // Return saves and hands the keyboard back to the editor (space plays again);
                // Esc puts the old name back.
                .onSubmit { commitName(); endEditing() }
                .onExitCommand { name = shot.name; endEditing() }
                .onChange(of: nameFocused) { _, focused in if !focused { commitName() } }
                .help("Rename this shot (also in the Shots view)")
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
                let current = viewer.currentKeyframe?.id == k.id
                HStack(spacing: 8) {
                    Image(systemName: current ? "diamond.fill" : "diamond").font(.system(size: 8)).foregroundStyle(Theme.sightline)
                    Button(formatTimecode(k.time, fps: viewer.fps)) { viewer.scrub(to: k.time, final: true) }
                        .buttonStyle(.plain)
                        .fontWeight(current ? .semibold : .regular)
                        .help("Select this keyframe: the playhead moves to it, and changing the view edits it")
                    Spacer()
                    Text(String(format: "%+.0f°  %@", k.framing.yaw, k.framing.preset?.label ?? String(format: "%.0f°", k.framing.hfov)))
                        .foregroundStyle(.secondary)
                    if shot.keyframes.count > 1 {
                        Button { viewer.removeKeyframe(k.id) } label: { Image(systemName: "minus.circle") }
                            .buttonStyle(.borderless)
                            .help("Remove keyframe")
                    }
                }
                .font(Theme.numeric(11))
                .opacity(shot.contains(k.time) ? 1 : 0.45)
                .help(shot.contains(k.time) ? "" : "Outside the shot after a trim: it still shapes the path, and returns if you extend the shot")
            }
            Button("Delete Shot", role: .destructive) { viewer.deleteActiveShot() }
        }
    }
}

/// Why a moment was suggested, and the two things you can do with it.
private struct SuggestionSection: View {
    let suggestion: Suggestion
    @Bindable var viewer: ViewerModel

    /// "Pans over 3 keyframes, Mega" or "Holds still, Dewarp".
    private var cameraSummary: String {
        let k = suggestion.keyframes.count
        let move = suggestion.stabilization == .follow ? "Looks ahead" : k <= 1 ? "Holds still" : "Pans over \(k) keyframes"
        return "\(move), \(suggestion.lens.label)"
    }

    var body: some View {
        Section("Suggested shot") {
            LabeledContent("In") {
                Button(formatTimecode(suggestion.inTime, fps: viewer.fps)) { viewer.scrub(to: suggestion.inTime, final: true) }
                    .buttonStyle(.plain).font(Theme.numeric(12)).help("Go to this point")
            }
            LabeledContent("Peak") {
                Button(formatTimecode(suggestion.peakTime, fps: viewer.fps)) { viewer.scrub(to: suggestion.peakTime, final: true) }
                    .buttonStyle(.plain).font(Theme.numeric(12)).help("Go to the most interesting moment")
            }
            LabeledContent("Length") { Text(formatDuration(suggestion.duration)).font(Theme.numeric(12)) }
            LabeledContent("Camera") { Text(cameraSummary).foregroundStyle(.secondary) }
            VStack(alignment: .leading, spacing: 5) {
                Text("Why")
                let rows = suggestion.contributions.sorted { abs($0.value) > abs($1.value) }.prefix(6)
                let top = max(rows.map { abs($0.value) }.max() ?? 1, 1e-3)
                ForEach(Array(rows), id: \.key) { signal, value in
                    HStack(spacing: 8) {
                        Text(signal.phrase.prefix(1).uppercased() + signal.phrase.dropFirst())
                            .font(.system(size: 11))
                            .foregroundStyle(value < 0 ? .tertiary : .secondary)
                            .frame(width: 92, alignment: .leading)
                        GeometryReader { geo in
                            RoundedRectangle(cornerRadius: 1.5)
                                .fill(Color.primary.opacity(value < 0 ? 0.15 : 0.5))
                                .frame(width: max(2, geo.size.width * abs(value) / top))
                        }
                        .frame(height: 5)
                    }
                    .help(value < 0 ? "Counts against this moment" : "Counts toward this moment")
                }
            }
            HStack {
                Button("Keep as Shot") { viewer.acceptSuggestion() }
                    .buttonStyle(.borderedProminent)
                    .tint(Theme.sightline)
                    .help("Turn it into a shot you can edit and export (A)")
                Button("Dismiss") { viewer.dismissSuggestion() }
                    .help("Hide this suggestion (X)")
            }
        }
    }
}

private struct FramingSection: View {
    @Bindable var viewer: ViewerModel

    var body: some View {
        Section("Framing") {
            row("Yaw", value: \.yaw, range: -180...180)
            row("Pitch", value: \.pitch, range: -89...89)
            VStack(alignment: .leading, spacing: 8) {
                Text("Lens")
                LensPresetPicker(current: viewer.displayFraming) { viewer.apply($0) }
                Text(lensSummary).caption()
            }
            HStack {
                Button("Reset") { viewer.resetFraming() }
                Spacer()
                if viewer.currentKeyframe != nil {
                    Text("Editing this keyframe").caption()
                } else if viewer.editsKeyframe {
                    Text("Changes add a keyframe here").caption()
                }
            }
        }
    }

    private var lensSummary: String {
        let f = viewer.displayFraming
        let name = f.preset?.label ?? "Custom"
        return String(format: "%@: %g° field of view, distortion %.2f, covers %.0f° across", name,
                      (f.hfov * 10).rounded() / 10, f.distortion, f.coverage)
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

/// What suggestions favour, for every clip. Re-ranks instantly from cached analysis.
/// While searching: where the query shows up in this clip. Click one to jump there and look at it.
private struct MatchesSection: View {
    @Environment(Library.self) private var library
    let item: ClipItem
    @Bindable var viewer: ViewerModel

    var body: some View {
        let search = library.search
        Section {
            if let m = search.match(for: item) {
                ForEach(m.hits.sorted { $0.time < $1.time }, id: \.time) { hit in
                    Button { search.show(hit, in: viewer) } label: {
                        HStack(spacing: 10) {
                            Text(formatDuration(hit.time)).font(Theme.numeric(12)).frame(width: 40, alignment: .leading)
                            Text(directionLabel(hit.yaw).capitalizedFirst).font(.system(size: 12))
                            Spacer()
                            StrengthBars(value: Double((hit.score - SemanticIndex.matchFloor) / SemanticIndex.matchSpread))
                        }
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .help("Jump to \(formatDuration(hit.time)) and look \(directionLabel(hit.yaw))")
                }
                Text("⌘G and ⇧⌘G step through them. Suggested shots below are aimed at the search.").caption()
            } else {
                Text(search.indexed.contains(item.id) ? "Nothing in this clip looks like “\(search.resultsQuery)”."
                     : "This clip isn't indexed yet.").caption()
            }
        } header: {
            Text("Matches for “\(search.resultsQuery.isEmpty ? search.query : search.resultsQuery)”")
        }
    }
}

/// Three bars for how clearly something matches (a value, not decoration).
private struct StrengthBars: View {
    let value: Double

    var body: some View {
        HStack(alignment: .bottom, spacing: 2) {
            ForEach(0..<3) { i in
                RoundedRectangle(cornerRadius: 1)
                    .fill(.primary.opacity(value > Double(i) / 3 ? 0.75 : 0.18))
                    .frame(width: 3, height: 5 + CGFloat(i) * 3)
            }
        }
        .accessibilityLabel(value > 0.66 ? "Strong match" : value > 0.33 ? "Good match" : "Weak match")
    }
}

extension String {
    var capitalizedFirst: String { prefix(1).uppercased() + dropFirst() }
}

private struct SuggestionsSection: View {
    @Environment(Library.self) private var library

    var body: some View {
        Section("Suggestions") {
            Picker("Favor", selection: Binding(get: { library.preset }, set: { library.setPreset($0) })) {
                ForEach(InterestWeights.Preset.allCases, id: \.self) { Text($0.label).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            Text(library.preset.summary).caption()
        }
    }
}

extension InterestWeights.Preset {
    var label: String {
        switch self {
        case .blend: return "Blend"
        case .people: return "People"
        case .action: return "Action"
        case .scenery: return "Scenery"
        }
    }

    var summary: String {
        switch self {
        case .blend: return "A mix of people, action and scenery."
        case .people: return "Faces, voices and laughter come first."
        case .action: return "Movement, jumps, impacts and loud moments come first."
        case .scenery: return "Views that stand out and new places, with the Linear lens when nobody is in frame."
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
