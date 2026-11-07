import AppKit
import ReframeKit
import SwiftUI

struct ExportSheet: View {
    @Environment(Library.self) private var library
    @Environment(\.dismiss) private var dismiss
    /// Export only these shots (the shots view's selection); nil exports all.
    var only: Set<Shot.ID>? = nil
    @AppStorage("export.size") private var size: ExportOptions.Size = .hd
    @AppStorage("export.codec") private var codec: ExportOptions.Codec = .proRes422HQ
    @AppStorage("export.bakeLook") private var bakeLook = false
    @AppStorage("export.resolve") private var resolve = true
    @AppStorage("export.finalCut") private var finalCut = true
    @AppStorage("export.destination") private var destinationPath = ""
    @AppStorage("export.studioFPS") private var studioFPS = 24
    @State private var studio: StudioSend = .idle
    @State private var confirmQuitStudio = false
    @State private var studioName = ""

    enum StudioSend: Equatable {
        case idle, working(String), done(String), failed(String)
    }

    private var queue: ExportQueue { library.exports }
    private var destination: URL {
        if !destinationPath.isEmpty { return URL(fileURLWithPath: destinationPath) }
        let movies = FileManager.default.urls(for: .moviesDirectory, in: .userDomainMask)[0]
        return movies.appendingPathComponent("Reframe/\(library.folder?.lastPathComponent ?? "Selects")")
    }

    private var exportable: [(Shot, Clip)] {
        library.shots.filter { only?.contains($0.id) ?? true }
            .compactMap { s in library.item(for: s.clipPath).map { (s, $0.clip) } }
            .sorted { ($0.1.captureDate ?? .distantPast, $0.0.inTime) < ($1.1.captureDate ?? .distantPast, $1.0.inTime) }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            if queue.jobs.isEmpty { settings } else { progress }
        }
        .frame(width: 520)
        .background(Color(nsColor: .windowBackgroundColor))
        .tint(Theme.sightline)
        .onAppear {
            // Debug: REFRAME_EXPORT_AUTOSTART=1 starts the export as soon as the sheet opens.
            if ProcessInfo.processInfo.environment["REFRAME_EXPORT_AUTOSTART"] != nil, queue.jobs.isEmpty {
                start(exportable)
            }
        }
    }

    // MARK: Settings

    @ViewBuilder private func cloudSection(_ clips: [Clip]) -> some View {
        let cloud = library.cloud
        let records = clips.compactMap { CloudCache.record(for: $0.url) }
        let total = records.reduce(Int64(0)) { $0 + $1.size }
        let active = records.compactMap { cloud.fetching[$0.mediaId] }
        Section {
            LabeledContent {
                if active.isEmpty {
                    Button("Download Originals") { Task { await library.downloadOriginals(clips) } }
                        .disabled(!cloud.isSignedIn)
                } else {
                    ProgressView().controlSize(.small)
                }
            } label: {
                Text("\(clips.count) clip\(clips.count == 1 ? "'s" : "s'") original\(clips.count == 1 ? " is" : "s are") in Insta360 Cloud")
                Text(active.first.map { "\($0.label) (\(records.count) left)" }
                     ?? "Exporting files renders from the originals: \(formatBytes(total)) to download. Send to Studio doesn't need them: Studio streams cloud clips.")
                let errs = records.compactMap { cloud.errors[$0.mediaId] }
                if let e = errs.first { Text(e).foregroundStyle(.red) }
            }
        }
    }

    private var settings: some View {
        let shots = exportable
        let total = shots.reduce(0) { $0 + $1.0.duration }
        let clips = Set(shots.map(\.1.url)).count
        // Clips still in Insta360 Cloud: export renders the original, so it has to come down first.
        var seen = Set<URL>()
        let inCloud = shots.map(\.1).filter { seen.insert($0.url).inserted && CloudCache.isStub($0.url) }
        return VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 4) {
                Text("Export shots").font(.title3.weight(.semibold))
                Text("\(shots.count) shot\(shots.count == 1 ? "" : "s") from \(clips) clip\(clips == 1 ? "" : "s"), \(formatDuration(total)) in all")
                    .foregroundStyle(.secondary)
            }
            .padding(20)

            Form {
                if !inCloud.isEmpty { cloudSection(inCloud) }
                Section {
                    LabeledContent("Save to") {
                        HStack {
                            Text(destination.path(percentEncoded: false).replacingOccurrences(of: NSHomeDirectory(), with: "~"))
                                .lineLimit(1).truncationMode(.middle)
                                .foregroundStyle(.secondary)
                            Button("Choose…") { chooseDestination() }
                        }
                    }
                    Picker("Size", selection: $size) {
                        ForEach(ExportOptions.Size.allCases) { Text($0.label).tag($0) }
                    }
                    Picker("Codec", selection: $codec) {
                        ForEach(ExportOptions.Codec.allCases) { Text($0.label).tag($0) }
                    }
                }
                Section {
                    Toggle(isOn: $bakeLook) {
                        Text("Bake the preview look into the pixels")
                        Text(bakeLook ? "I-Log shots will be exported with the \(library.activeLUT.map(CubeLUT.shortName) ?? "selected") LUT applied."
                                      : "Shots stay in I-Log, ready to grade. Apply the X6 LUT or a color space transform in your editor.")
                    }
                }
                Section("Timelines") {
                    Toggle(isOn: $resolve) {
                        Text("DaVinci Resolve")
                        Text("OpenTimelineIO (.otio). In Resolve, choose File › Import › Timeline.")
                    }
                    Toggle(isOn: $finalCut) {
                        Text("Final Cut Pro")
                        Text("FCPXML with an event of selects and a project in shot order.")
                    }
                }
                Section("Insta360 Studio") {
                    TextField("Project name", text: $studioName, prompt: Text(studioBaseName))
                    Picker("Frame rate", selection: $studioFPS) {
                        ForEach([24, 25, 30, 60], id: \.self) { Text("\($0) fps").tag($0) }
                    }
                    LabeledContent {
                        if case .working = studio {
                            ProgressView().controlSize(.small)
                        } else {
                            Button("Send to Studio") {
                                if StudioProject.isStudioRunning { confirmQuitStudio = true } else { sendToStudio(shots) }
                            }
                            .disabled(shots.isEmpty || !StudioProject.isStudioInstalled)
                        }
                    } label: {
                        Text("Timeline project")
                        studioCaption
                    }
                }
            }
            .formStyle(.grouped)
            .scrollDisabled(true)
            .frame(height: inCloud.isEmpty ? 570 : 660)
            .onAppear { if studioName.isEmpty { studioName = StudioProject.availableName(studioBaseName) } }
            .alert("Quit Insta360 Studio?", isPresented: $confirmQuitStudio) {
                Button("Quit and Send") { sendToStudio(shots) }
                Button("Cancel", role: .cancel) {}
            } message: {
                Text("Studio has to be closed while Reframe adds the project. It may ask you to save first; Reframe reopens it afterwards.")
            }

            HStack {
                Spacer()
                Button("Cancel") { dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button("Export \(shots.count) Shot\(shots.count == 1 ? "" : "s")") { start(shots) }
                    .keyboardShortcut(.defaultAction)
                    .buttonStyle(.borderedProminent)
                    .tint(Theme.sightline)
                    .disabled(shots.isEmpty || !inCloud.isEmpty)
            }
            .padding(20)
        }
    }

    private func start(_ shots: [(Shot, Clip)]) {
        var options = ExportOptions(destination: destination)
        options.size = size
        options.codec = codec
        options.bakeLook = bakeLook
        let lut = bakeLook ? library.activeLUT.flatMap { try? CubeLUT(url: $0) } : nil
        let title = "Reframe \(library.folder?.lastPathComponent ?? "selects")"
        queue.start(.init(shots: shots, options: options, lut: lut, resolveTimeline: resolve,
                          finalCutTimeline: finalCut, title: title))
    }

    @ViewBuilder private var studioCaption: some View {
        switch studio {
        case .idle:
            let requested = studioRequestedName, name = StudioProject.availableName(requested)
            if !StudioProject.isStudioInstalled {
                Text("Insta360 Studio isn't installed.")
            } else if name != requested {
                Text("Studio already has “\(requested)”, so this one will be “\(name)”.")
            } else {
                Text("The shots on a new 16:9 timeline over the original .insv files, with your framing as keyframes. Nothing is rendered.")
            }
        case .working(let m): Text(m)
        case .done(let name): Text("Added “\(name)” to Studio's projects.").foregroundStyle(Theme.sightline)
        case .failed(let m): Text(m).foregroundStyle(.red)
        }
    }

    private var studioBaseName: String { "Reframe \(library.folder?.lastPathComponent ?? "selects")" }

    /// The typed name, made safe as a folder name (falls back to the default).
    private var studioRequestedName: String {
        let n = studioName.replacingOccurrences(of: "/", with: "-").replacingOccurrences(of: ":", with: "-")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return n.isEmpty || n.hasPrefix(".") ? studioBaseName : n
    }

    /// Writes the shots as a Studio timeline project (quitting Studio first if needed), then opens Studio.
    /// A taken name gets " 2", " 3"... so an existing project is never overwritten.
    private func sendToStudio(_ shots: [(Shot, Clip)]) {
        let fps = Double(studioFPS)
        let base = studioRequestedName
        studio = .working("Preparing \(shots.count) shot\(shots.count == 1 ? "" : "s")…")
        Task {
            if StudioProject.isStudioRunning {
                studio = .working("Waiting for Studio to quit…")
                guard await StudioProject.quitStudio() else {
                    studio = .failed("Studio is still open. Quit it and try again.")
                    return
                }
            }
            studio = .working("Writing the project…")
            let result = await Task.detached { () -> Result<String, Error> in
                let items = shots.compactMap { StudioProject.Item(shot: $0.0, clip: $0.1) }
                let name = StudioProject.availableName(base)
                return Result { try StudioProject.write(name: name, items: items, fps: fps, register: true); return name }
            }.value
            switch result {
            case .success(let name):
                StudioProject.openStudio()
                studio = .done(name)
                studioName = StudioProject.availableName(base)
            case .failure(let e):
                studio = .failed((e as? StudioProject.Failure)?.description ?? e.localizedDescription)
            }
        }
    }

    private func chooseDestination() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.prompt = "Save Here"
        if panel.runModal() == .OK, let url = panel.url { destinationPath = url.path }
    }

    // MARK: Progress

    private var progress: some View {
        VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 8) {
                Text(queue.running ? "Exporting…" : "Export finished").font(.title3.weight(.semibold))
                ProgressView(value: queue.overallProgress)
                    .tint(Theme.sightline)
                Text("\(queue.finished) of \(queue.jobs.count) shots written")
                    .foregroundStyle(.secondary)
            }
            .padding(20)

            List(queue.jobs) { job in
                HStack(spacing: 10) {
                    status(job.status)
                        .frame(width: 18)
                    Text(job.name).lineLimit(1)
                    Spacer()
                    detail(job.status)
                }
                .padding(.vertical, 2)
            }
            .frame(height: min(CGFloat(queue.jobs.count) * 30 + 16, 240))
            .scrollContentBackground(.hidden)

            if !queue.running && !queue.timelineURLs.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    ForEach(queue.timelineURLs, id: \.self) { u in
                        Label(u.lastPathComponent, systemImage: u.pathExtension == "otio" ? "film.stack" : "film")
                            .font(.callout)
                    }
                }
                .padding(.horizontal, 20)
                .padding(.top, 8)
            }

            HStack {
                if queue.running {
                    Spacer()
                    Button("Stop") { queue.cancel() }
                } else {
                    Button("Show in Finder") {
                        if let d = queue.destination { NSWorkspace.shared.activateFileViewerSelecting([d]) }
                    }
                    Spacer()
                    Button("Done") { queue.reset(); dismiss() }
                        .keyboardShortcut(.defaultAction)
                        .buttonStyle(.borderedProminent)
                        .tint(Theme.sightline)
                }
            }
            .padding(20)
        }
    }

    @ViewBuilder private func status(_ s: ExportQueue.Status) -> some View {
        switch s {
        case .queued: Image(systemName: "circle.dashed").foregroundStyle(.tertiary)
        case .running: ProgressView().controlSize(.small)
        case .done: Image(systemName: "checkmark.circle.fill").foregroundStyle(Theme.sightline)
        case .failed: Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.red)
        case .cancelled: Image(systemName: "stop.circle").foregroundStyle(.secondary)
        }
    }

    @ViewBuilder private func detail(_ s: ExportQueue.Status) -> some View {
        switch s {
        case .running(let p): Text("\(Int(p * 100))%").font(Theme.numeric(11)).foregroundStyle(.secondary)
        case .failed(let m): Text(m).font(.caption).foregroundStyle(.red).lineLimit(1).help(m)
        case .cancelled: Text("Stopped").caption()
        case .done(let u):
            Button { NSWorkspace.shared.activateFileViewerSelecting([u]) } label: { Image(systemName: "magnifyingglass") }
                .buttonStyle(.borderless).help("Show in Finder")
        case .queued: EmptyView()
        }
    }
}
