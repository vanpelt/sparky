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
                         + "and where it is in the 360° view. Everything runs on this Mac; nothing is uploaded.")
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
                LabeledContent("Clips indexed") {
                    Text("\(search.indexed.count) of \(library.items.count)").font(Theme.numeric(13))
                }
                if let url = search.indexing {
                    LabeledContent {
                        HStack(spacing: 8) {
                            ProgressView().controlSize(.small)
                            Text("\(search.remaining) to go").font(Theme.numeric(12)).foregroundStyle(.secondary)
                        }
                    } label: {
                        Text("Indexing")
                        Text(library.item(for: url.path)?.clip.captureDate?.formatted(date: .abbreviated, time: .shortened)
                             ?? url.deletingPathExtension().lastPathComponent)
                    }
                }
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

    private var indexNote: String {
        let search = library.search
        if !search.isEnabled { return "While search is on, new clips are indexed in the background, about 2 seconds per minute of footage." }
        if search.modelState != .ready { return "Indexing starts once the model is downloaded." }
        let cloudOnly = library.cloud.media.count - library.items.filter(\.isCloud).count
        return "New clips are indexed in the background, about 2 seconds per minute of footage."
            + (cloudOnly > 0 ? " \(cloudOnly) clip\(cloudOnly == 1 ? " is" : "s are") still only in Insta360 Cloud; they're indexed once fetched." : "")
    }
}
