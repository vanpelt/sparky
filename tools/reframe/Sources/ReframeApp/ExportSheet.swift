import AppKit
import ReframeKit
import SwiftUI

struct ExportSheet: View {
    @Environment(Library.self) private var library
    @Environment(\.dismiss) private var dismiss
    @AppStorage("export.size") private var size: ExportOptions.Size = .hd
    @AppStorage("export.codec") private var codec: ExportOptions.Codec = .proRes422HQ
    @AppStorage("export.bakeLook") private var bakeLook = false
    @AppStorage("export.resolve") private var resolve = true
    @AppStorage("export.finalCut") private var finalCut = true
    @AppStorage("export.destination") private var destinationPath = ""

    private var queue: ExportQueue { library.exports }
    private var destination: URL {
        if !destinationPath.isEmpty { return URL(fileURLWithPath: destinationPath) }
        let movies = FileManager.default.urls(for: .moviesDirectory, in: .userDomainMask)[0]
        return movies.appendingPathComponent("Reframe/\(library.folder?.lastPathComponent ?? "Selects")")
    }

    private var exportable: [(Shot, Clip)] {
        library.shots.compactMap { s in library.item(for: s.clipPath).map { (s, $0.clip) } }
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

    private var settings: some View {
        let shots = exportable
        let total = shots.reduce(0) { $0 + $1.0.duration }
        let clips = Set(shots.map(\.1.url)).count
        return VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 4) {
                Text("Export shots").font(.title3.weight(.semibold))
                Text("\(shots.count) shot\(shots.count == 1 ? "" : "s") from \(clips) clip\(clips == 1 ? "" : "s"), \(formatDuration(total)) in all")
                    .foregroundStyle(.secondary)
            }
            .padding(20)

            Form {
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
            }
            .formStyle(.grouped)
            .scrollDisabled(true)
            .frame(height: 400)

            HStack {
                Spacer()
                Button("Cancel") { dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button("Export \(shots.count) Shot\(shots.count == 1 ? "" : "s")") { start(shots) }
                    .keyboardShortcut(.defaultAction)
                    .buttonStyle(.borderedProminent)
                    .tint(Theme.sightline)
                    .disabled(shots.isEmpty)
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
