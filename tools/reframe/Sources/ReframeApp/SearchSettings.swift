import ReframeKit
import SwiftUI

/// Reframe › Settings: a tab per area, the way Mac apps do it.
struct SettingsView: View {
    enum Tab: String { case search, storage }
    // Debug: REFRAME_SETTINGS_TAB=search|storage picks the tab (for snapshots).
    @AppStorage("settings.tab") private var tab: Tab =
        ProcessInfo.processInfo.environment["REFRAME_SETTINGS_TAB"].flatMap(Tab.init(rawValue:)) ?? .search

    var body: some View {
        TabView(selection: $tab) {
            SearchSettings()
                .tabItem { Label("Search", systemImage: "sparkle.magnifyingglass") }
                .tag(Tab.search)
            StorageSettings()
                .tabItem { Label("Storage", systemImage: "internaldrive") }
                .tag(Tab.storage)
        }
        .onAppear {
            if let t = ProcessInfo.processInfo.environment["REFRAME_SETTINGS_TAB"].flatMap(Tab.init(rawValue:)) { tab = t }
        }
    }
}

/// Settings › Search: turn searching by description on or off, the model it needs, and the index.
struct SearchSettings: View {
    @Environment(Library.self) private var library
    @State private var indexBytes: Int64 = 0
    @State private var confirmRemove = false

    var body: some View {
        @Bindable var search = library.search
        Form {
            Section {
                Toggle(isOn: $search.isEnabled) {
                    Text("Search what's in your footage")
                    Text("Describe a moment, like “a waterfall” or “riding bikes”, and Reframe finds when it happens "
                         + "and where it is in the 360° view. The same model helps aim suggested shots at what's worth "
                         + "looking at. Everything runs on this Mac; nothing is uploaded.")
                }
            }

            Section("Model") {
                LabeledContent {
                    modelStatus
                } label: {
                    Text(SemanticModels.name)
                    Text("Apple’s on-device image and text model, \(formatBytes(SemanticModels.downloadSize)). "
                         + "Runs on the Neural Engine.")
                }
                if let error = search.modelState.failure {
                    Label(error, systemImage: "exclamationmark.triangle").foregroundStyle(.red).font(.system(size: 12))
                }
                LabeledContent("License") {
                    Link("Apple Sample Code License", destination: SemanticModels.license)
                        .foregroundStyle(.secondary)
                }
            }

            Section("Index") {
                indexProgress
                cloudProgress
                LabeledContent("Size on disk") { Text(formatBytes(indexBytes)).font(Theme.numeric(13)) }
                Text(indexNote).caption().fixedSize(horizontal: false, vertical: true)
            }
        }
        .formStyle(.grouped)
        .frame(width: 560)
        .fixedSize(horizontal: false, vertical: true)
        .task(id: search.indexed.count) {
            indexBytes = await Task.detached { StorageReport.scan().bytes(.searchIndex) }.value
        }
        .confirmationDialog("Remove the search models?", isPresented: $confirmRemove) {
            Button("Remove Models", role: .destructive) { search.removeModels() }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Frees \(formatBytes(SemanticModels.downloadSize)) and turns search off. The index is kept.")
        }
    }

    @ViewBuilder private var modelStatus: some View {
        let search = library.search
        switch search.modelState {
        case .notInstalled, .failed:
            Button("Download") { search.downloadModels() }
        case .downloading(let p):
            HStack(spacing: 8) {
                ProgressView(value: p).frame(width: 120)
                Text(p.formatted(.percent.precision(.fractionLength(0)))).font(Theme.numeric(12)).foregroundStyle(.secondary)
                    .frame(width: 34, alignment: .trailing)
                Button("Cancel") { search.cancelDownload() }
            }
        case .ready:
            HStack(spacing: 10) {
                Label("Installed", systemImage: "checkmark.circle.fill").foregroundStyle(.secondary)
                Button("Remove…") { confirmRemove = true }
            }
        }
    }

    /// Indexed of indexable, what's being indexed, and roughly how long the rest takes.
    @ViewBuilder private var indexProgress: some View {
        let search = library.search
        let items = library.items
        let left = items.filter { !search.indexed.contains($0.id) }
        LabeledContent {
            VStack(alignment: .trailing, spacing: 4) {
                Text("\(search.indexed.count) of \(items.count) clips").font(Theme.numeric(13))
                if search.indexing != nil {
                    ProgressView(value: Double(search.indexed.count), total: Double(max(items.count, 1))).frame(width: 160)
                }
            }
        } label: {
            Text("Indexed")
            if let url = search.indexing {
                let footage = left.reduce(0) { $0 + $1.clip.duration }
                Text("Now: \(clipLabel(url)). \(left.count) left, about \(eta(footage * 0.03)).")
            } else if search.isReady, !left.isEmpty {
                Text("\(left.count) can't be indexed (no proxy or gyro).")
            }
        }
    }

    /// Clips still only in Insta360 Cloud, and Fetch All's progress.
    @ViewBuilder private var cloudProgress: some View {
        let cloud = library.cloud
        let local = library.items.filter(\.isCloud).count
        if cloud.isSignedIn, !cloud.media.isEmpty {
            LabeledContent {
                if let f = library.fetchAll {
                    HStack(spacing: 8) {
                        ProgressView(value: Double(f.done), total: Double(max(f.total, 1))).frame(width: 110)
                        Button("Stop") { library.stopFetchingAll() }
                    }
                } else if local < cloud.media.count {
                    Button("Fetch All") { library.fetchAllCloudClips() }
                } else {
                    Label("All fetched", systemImage: "checkmark.circle.fill").foregroundStyle(.secondary)
                }
            } label: {
                Text("Insta360 Cloud")
                if let f = library.fetchAll {
                    let stage = cloud.media.first { $0.name == f.current }.flatMap { cloud.fetching[$0.id] }
                    Text("Fetching \(f.done + 1) of \(f.total)" + (f.current.map { ": \(URL(fileURLWithPath: $0).deletingPathExtension().lastPathComponent)" } ?? "")
                         + (stage.map { ". \($0.label)" } ?? "."))
                } else {
                    Text("\(local) of \(cloud.media.count) recordings fetched. Clips are searchable once their low-res proxy is here (about 6% of the original).")
                }
            }
        }
    }

    private func clipLabel(_ url: URL) -> String {
        library.item(for: url.path)?.clip.captureDate?.formatted(date: .abbreviated, time: .shortened)
            ?? url.deletingPathExtension().lastPathComponent
    }

    private func eta(_ seconds: Double) -> String {
        seconds < 60 ? "\(max(Int(seconds.rounded()), 1)) s" : "\(Int((seconds / 60).rounded(.up))) min"
    }

    private var indexNote: String {
        let search = library.search
        if !search.isEnabled { return "While search is on, new clips are indexed as they're analyzed, adding about a third of a second per minute of footage." }
        if search.modelState != .ready { return "Indexing starts once the model is downloaded." }
        return "New clips are indexed as they're analyzed, adding about a third of a second per minute of footage. Clips analyzed before are indexed in the background."
    }
}
