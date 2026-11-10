import AppKit
import ReframeKit
import SwiftUI

/// Every shot in the library as a contact sheet: grouped by recording day, hover to scrub through
/// a shot, double-click (or Return) to open it in the editor.
struct ShotsBrowser: View {
    @Environment(Library.self) private var library
    @Binding var selection: Set<Shot.ID>
    @AppStorage("shots.sort") private var sort: Sort = .recorded
    /// Anchor for shift-click ranges.
    @State private var anchor: Shot.ID?
    /// The shot whose name is being edited in place.
    @State private var renaming: Shot.ID?

    enum Sort: String, CaseIterable, Identifiable {
        case recorded, created, longest
        var id: String { rawValue }
        var label: String {
            switch self {
            case .recorded: return "Recording time"
            case .created: return "Newest first"
            case .longest: return "Longest first"
            }
        }
    }

    struct Group: Identifiable {
        var id: String
        var title: String
        var shots: [Shot]
    }

    /// The shots in view, filtered and grouped (by recording day, or one group when sorted otherwise).
    var groups: [Group] {
        // The window's search field: a shot's name or clip, or (with search on) what it looks at.
        let q = library.search.query.lowercased()
        let scores = library.search.shotScores
        let shots = library.shots.filter { s in
            q.isEmpty || s.name.lowercased().contains(q)
                || URL(fileURLWithPath: s.clipPath).lastPathComponent.lowercased().contains(q)
                || (scores[s.id] ?? -1) > SemanticIndex.matchFloor
        }
        switch sort {
        case .recorded:
            let cal = Calendar.current
            let dated = shots.map { ($0, library.recordingDate(of: $0)) }
            let byDay = Dictionary(grouping: dated) { cal.startOfDay(for: $0.1 ?? .distantPast) }
            return byDay.keys.sorted().map { day in
                let g = byDay[day]!.sorted {
                    ($0.1 ?? .distantPast, $0.0.inTime) < ($1.1 ?? .distantPast, $1.0.inTime)
                }.map(\.0)
                let title = day == cal.startOfDay(for: .distantPast) ? "Undated"
                    : day.formatted(.dateTime.weekday(.wide).month(.wide).day())
                return Group(id: title, title: title, shots: g)
            }
        case .created:
            return [Group(id: "all", title: "", shots: shots.sorted { $0.created > $1.created })]
        case .longest:
            return [Group(id: "all", title: "", shots: shots.sorted { $0.duration > $1.duration })]
        }
    }

    var ordered: [Shot] { groups.flatMap(\.shots) }

    var body: some View {
        let groups = groups
        VStack(spacing: 0) {
            header(groups.flatMap(\.shots))
            Divider()
            if library.shots.isEmpty {
                ContentUnavailableView {
                    Label("No shots yet", systemImage: "rectangle.on.rectangle.angled")
                } description: {
                    Text("Mark one while watching a clip (N, then I and O), or keep a suggested shot with A.")
                } actions: {
                    Button("Browse Clips") { library.browse = .clips }.buttonStyle(.borderedProminent)
                }
            } else if groups.allSatisfy({ $0.shots.isEmpty }) {
                ContentUnavailableView.search(text: library.search.query)
            } else {
                ScrollView {
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 250, maximum: 360), spacing: 18, alignment: .top)],
                              alignment: .leading, spacing: 22, pinnedViews: [.sectionHeaders]) {
                        ForEach(groups) { g in
                            Section {
                                ForEach(g.shots) { shot in
                                    ShotCard(shot: shot, selected: selection.contains(shot.id), renaming: $renaming)
                                        // Selection on every click; the card's frame (open) and name
                                        // (rename) handle double-clicks themselves.
                                        .simultaneousGesture(TapGesture().onEnded { click(shot) })
                                        .contextMenu { menu(for: shot) }
                                }
                            } header: {
                                if !g.title.isEmpty {
                                    Text(g.title)
                                        .font(.system(size: 13, weight: .semibold))
                                        .frame(maxWidth: .infinity, alignment: .leading)
                                        .padding(.vertical, 6)
                                        .background(.bar)
                                }
                            }
                        }
                    }
                    .padding(.horizontal, 22)
                    .padding(.bottom, 24)
                    // Clicking between cards clears the selection (behind the cards, not around them,
                    // so a click on a card never reaches it).
                    .background { Color.clear.contentShape(Rectangle()).onTapGesture { selection = [] } }
                    .frame(maxWidth: .infinity, minHeight: 0, alignment: .topLeading)
                }
                .background(Color(nsColor: .windowBackgroundColor))
            }
        }
    }

    private func header(_ shown: [Shot]) -> some View {
        let total = shown.reduce(0) { $0 + $1.duration }
        let clips = Set(shown.map(\.clipPath)).count
        return HStack(spacing: 14) {
            VStack(alignment: .leading, spacing: 2) {
                Text("\(shown.count) shot\(shown.count == 1 ? "" : "s")")
                    .font(.system(size: 15, weight: .semibold))
                Text("\(formatDuration(total)) from \(clips) clip\(clips == 1 ? "" : "s")"
                     + (selection.isEmpty ? "" : " · \(selection.count) selected"))
                    .caption()
            }
            Spacer()
            Picker("Sort", selection: $sort) {
                ForEach(Sort.allCases) { Text($0.label).tag($0) }
            }
            .labelsHidden()
            .fixedSize()
        }
        .padding(.horizontal, 22)
        .padding(.vertical, 12)
        .background(.bar)
    }

    /// Click selects one; ⌘-click toggles; shift-click extends from the last click.
    private func click(_ shot: Shot) {
        let mods = NSEvent.modifierFlags
        if mods.contains(.command) {
            if selection.contains(shot.id) { selection.remove(shot.id) } else { selection.insert(shot.id) }
            anchor = shot.id
        } else if mods.contains(.shift), let a = anchor, let i = ordered.firstIndex(where: { $0.id == a }),
                  let j = ordered.firstIndex(where: { $0.id == shot.id }) {
            selection.formUnion(ordered[min(i, j)...max(i, j)].map(\.id))
        } else {
            selection = [shot.id]
            anchor = shot.id
        }
    }

    @ViewBuilder private func menu(for shot: Shot) -> some View {
        let targets = selection.contains(shot.id) ? selection : [shot.id]
        Button("Open in Editor") { library.openInEditor(shot) }
            .disabled(library.item(for: shot.clipPath) == nil)
        Button("Rename") {
            selection = [shot.id]
            renaming = shot.id
        }
        Divider()
        Button(targets.count > 1 ? "Delete \(targets.count) Shots" : "Delete Shot", role: .destructive) {
            library.delete(targets)
            selection.subtract(targets)
        }
    }
}

/// One shot: its frame (hover to scrub through the shot), length, lens and where it came from.
struct ShotCard: View {
    @Environment(Library.self) private var library
    let shot: Shot
    let selected: Bool
    @Binding var renaming: Shot.ID?
    @State private var hover: Double?
    @State private var draft = ""
    @FocusState private var nameFocused: Bool

    var body: some View {
        let item = library.item(for: shot.clipPath)
        VStack(alignment: .leading, spacing: 7) {
            frame(item)
            VStack(alignment: .leading, spacing: 2) {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    name
                    if shot.suggestionID != nil {
                        Image(systemName: "sparkles").font(.system(size: 10)).foregroundStyle(.secondary)
                            .help("Kept from a suggested shot")
                    }
                    Spacer(minLength: 0)
                    Text(lensLabel).font(.system(size: 11)).foregroundStyle(.secondary)
                }
                Text(subtitle(item)).caption().lineLimit(1)
            }
            .padding(.horizontal, 2)
        }
        .padding(6)
        .background(RoundedRectangle(cornerRadius: 8).fill(selected ? Theme.sightline.opacity(0.12) : .clear))
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(selected ? [.isSelected, .isButton] : .isButton)
    }

    private func frame(_ item: ClipItem?) -> some View {
        let frames = library.shotFrames[shot.id]
        let image: CGImage? = {
            if let hover, let frames, !frames.isEmpty {
                return frames[min(Int(hover * Double(frames.count)), frames.count - 1)]
            }
            return library.shotThumbnails[shot.id]
        }()
        return ZStack {
            Rectangle().fill(Theme.surround)
            if let image {
                Image(decorative: image, scale: 1).resizable().aspectRatio(16 / 9, contentMode: .fill)
            } else if item == nil && !library.loadingCloud && !library.loading {
                VStack(spacing: 4) {
                    Image(systemName: "questionmark.folder").font(.system(size: 18))
                    Text("Clip not loaded").font(.system(size: 11))
                }
                .foregroundStyle(.secondary)
            } else {
                ProgressView().controlSize(.small)
            }
        }
        .aspectRatio(16 / 9, contentMode: .fit)
        .clipShape(RoundedRectangle(cornerRadius: 5))
        .overlay {
            RoundedRectangle(cornerRadius: 5)
                .strokeBorder(selected ? Theme.sightline : .white.opacity(0.08), lineWidth: selected ? 2 : 1)
        }
        .overlay(alignment: .bottom) { track }
        .overlay(alignment: .bottomTrailing) {
            Text(formatDuration(shot.duration))
                .font(Theme.numeric(10, weight: .semibold))
                .foregroundStyle(.white)
                .padding(.horizontal, 5).padding(.vertical, 1.5)
                .background(.black.opacity(0.55), in: RoundedRectangle(cornerRadius: 3))
                .padding(.horizontal, 6).padding(.bottom, 10)
        }
        .overlay(alignment: .topTrailing) {
            if let item, item.isCloud { CloudBadge(originalInCloud: item.originalInCloud).padding(6) }
        }
        .overlay {
            // Hover anywhere across the frame to scrub through the shot; double-click to open it.
            GeometryReader { g in
                Color.clear
                    .contentShape(Rectangle())
                    .onTapGesture(count: 2) { library.openInEditor(shot) }
                    .onContinuousHover { phase in
                        switch phase {
                        case .active(let p):
                            library.loadShotFrames(shot)
                            hover = g.size.width > 0 ? min(max(p.x / g.size.width, 0), 1) : nil
                        case .ended:
                            hover = nil
                        }
                    }
            }
        }
    }

    /// The shot's name; click it on a selected card (or choose Rename) to edit it in place.
    @ViewBuilder private var name: some View {
        if renaming == shot.id {
            TextField("Name", text: $draft)
                .textFieldStyle(.plain)
                .font(Theme.title)
                .focused($nameFocused)
                .onAppear { draft = shot.name; nameFocused = true }
                .onSubmit { commit(); endEditing() }
                .onExitCommand { renaming = nil; endEditing() }
                .onChange(of: nameFocused) { _, focused in if !focused && renaming == shot.id { commit() } }
                .padding(.horizontal, 3)
                .background(RoundedRectangle(cornerRadius: 3).fill(Color(nsColor: .textBackgroundColor)))
                .overlay(RoundedRectangle(cornerRadius: 3).strokeBorder(Theme.sightline, lineWidth: 1))
        } else {
            Text(shot.name)
                .font(Theme.title)
                .lineLimit(1)
                .onTapGesture(count: 2) { renaming = shot.id }
                .help("Double-click to rename")
        }
    }

    private func commit() {
        library.rename(shot.id, to: draft)
        renaming = nil
    }

    /// Keyframe ticks along the bottom, and the scrub position while hovering.
    private var track: some View {
        Canvas { ctx, size in
            let y = size.height - 3
            ctx.fill(Path(CGRect(x: 0, y: y - 1, width: size.width, height: 2)), with: .color(.black.opacity(0.35)))
            for k in shot.keyframesInRange where shot.duration > 0 {
                let x = (k.time - shot.inTime) / shot.duration * size.width
                var p = Path()
                p.move(to: CGPoint(x: x, y: y - 3)); p.addLine(to: CGPoint(x: x + 3, y: y))
                p.addLine(to: CGPoint(x: x, y: y + 3)); p.addLine(to: CGPoint(x: x - 3, y: y)); p.closeSubpath()
                ctx.fill(p, with: .color(.white.opacity(0.85)))
            }
            if let hover {
                ctx.fill(Path(CGRect(x: 0, y: y - 1, width: hover * size.width, height: 2)), with: .color(Theme.sightline))
            }
        }
        .frame(height: 8)
        .padding(.horizontal, 1)
        .allowsHitTesting(false)
    }

    private var lensLabel: String {
        let presets = Set(shot.keyframesInRange.map { $0.framing.preset?.label ?? "Custom" })
        if presets.isEmpty { return shot.framing(at: shot.inTime).preset?.label ?? "Custom" }
        return presets.count == 1 ? presets.first! : "Mixed lens"
    }

    private func subtitle(_ item: ClipItem?) -> String {
        let when = library.recordingDate(of: shot).map { $0.formatted(date: .omitted, time: .shortened) }
        let file = URL(fileURLWithPath: shot.clipPath).deletingPathExtension().lastPathComponent
        let short = file.split(separator: "_").last.map { "VID_\($0)" } ?? file
        let range = "\(formatDuration(shot.inTime))–\(formatDuration(shot.outTime))"
        return [when, short, range, shot.stabilization.label].compactMap { $0 }.joined(separator: " · ")
    }
}

