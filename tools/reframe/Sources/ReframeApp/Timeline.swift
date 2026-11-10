import AppKit
import ReframeKit
import SwiftUI

/// Transport, filmstrip, heading lane and shots, stacked under the viewer.
/// The filmstrip shows *what* the camera sees over time; the heading lane shows *where* it looks.
struct TimelinePane: View {
    @Environment(Library.self) private var library
    @Bindable var viewer: ViewerModel

    var body: some View {
        let item = library.selectedItem
        let shots = library.shots(for: viewer.clip)
        let suggestions = viewer.suggestions
        VStack(spacing: 0) {
            TransportBar(viewer: viewer)
            VStack(spacing: 4) {
                FilmstripView(viewer: viewer, item: item, shots: shots)
                    .frame(height: 58)
                // While a search matches this clip, the lane shows where the query is instead.
                let searchHeat = item.flatMap { library.search.match(for: $0) != nil ? library.search.heat[$0.id] : nil }
                HeadingLane(viewer: viewer, shots: shots, suggestions: suggestions, heat: searchHeat ?? item?.heat,
                            analysis: item?.analysis,
                            heatLabel: searchHeat != nil ? "Showing “\(library.search.resultsQuery)”" : nil)
                    .frame(height: 76)
                MotionRibbon(profile: item?.shakeProfile ?? [], activeShot: viewer.editingShot, duration: viewer.duration)
                    .frame(height: 12)
            }
            .padding(.horizontal, 16)
            ShotStrip(viewer: viewer, shots: shots, suggestions: suggestions)
                .padding(.top, 10)
                .padding(.bottom, 12)
        }
        .background(.bar)
    }
}

// MARK: - Transport

struct TransportBar: View {
    @Bindable var viewer: ViewerModel

    var body: some View {
        HStack(spacing: 14) {
            Button { viewer.togglePlay() } label: {
                Image(systemName: viewer.isPlaying ? "pause.fill" : "play.fill")
                    .font(.system(size: 15, weight: .semibold))
                    .frame(width: 22, height: 22)
            }
            .buttonStyle(.borderless)
            .help("Play or pause (Space)")

            Button { viewer.muted.toggle() } label: {
                Image(systemName: viewer.muted ? "speaker.slash.fill" : "speaker.wave.2.fill")
                    .font(.system(size: 13))
                    .frame(width: 20, height: 22)
                    .foregroundStyle(viewer.muted ? .secondary : .primary)
            }
            .buttonStyle(.borderless)
            .help(viewer.muted ? "Unmute (M)" : "Mute (M)")

            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(formatTimecode(viewer.currentTime, fps: viewer.fps))
                    .font(Theme.numeric(17, weight: .semibold))
                Text(formatDuration(viewer.duration))
                    .font(Theme.numeric(12))
                    .foregroundStyle(.secondary)
            }

            Spacer(minLength: 12)

            HStack(spacing: 6) {
                markButton("New shot", "plus.rectangle.on.rectangle", key: "N") { viewer.newShot() }
                markButton("In", "arrow.right.to.line", key: "I") { viewer.markIn() }
                markButton("Out", "arrow.left.to.line", key: "O") { viewer.markOut() }
                markButton("Keyframe", "diamond", key: "K") { viewer.addKeyframe() }
            }
            .controlSize(.small)
        }
        .padding(.horizontal, 16)
        .padding(.top, 10)
        .padding(.bottom, 8)
    }

    private func markButton(_ title: String, _ icon: String, key: String, action: @escaping () -> Void) -> some View {
        Button(action: action) { Label(title, systemImage: icon) }
            .buttonStyle(.bordered)
            .help("\(title) (\(key))")
    }
}

// MARK: - Filmstrip

/// Frames across the clip, seen through the current framing (or the active shot's keyframed path),
/// so you can judge what an export will look like. Rendered from the proxy, refreshed after edits.
struct FilmstripView: View {
    @Environment(Library.self) private var library
    @Bindable var viewer: ViewerModel
    let item: ClipItem?
    let shots: [Shot]

    var body: some View {
        GeometryReader { geo in
            let count = max(4, Int(geo.size.width / 104))
            let strip = viewer.filmstrip
            ZStack(alignment: .topLeading) {
                HStack(spacing: 1) {
                    ForEach(0..<count, id: \.self) { i in
                        ZStack {
                            Rectangle().fill(Theme.surround)
                            if i < strip.tiles.count, let img = strip.tiles[i] {
                                Image(decorative: img, scale: 1).resizable().aspectRatio(16 / 9, contentMode: .fill)
                            }
                        }
                        .frame(width: max((geo.size.width - CGFloat(count - 1)) / CGFloat(count), 1), height: geo.size.height)
                        .clipped()
                    }
                }
                // Shot spans and the playhead, aligned with the heading lane below.
                Canvas { ctx, size in
                    guard viewer.duration > 0 else { return }
                    func x(_ t: Double) -> CGFloat { CGFloat(t / viewer.duration) * size.width }
                    for shot in shots where shot.id == viewer.activeShotID {
                        for edge in [x(shot.inTime), x(shot.outTime)] {
                            ctx.fill(Path(CGRect(x: edge - 1, y: 0, width: 2, height: size.height)), with: .color(Theme.sightline))
                        }
                    }
                    let px = x(viewer.currentTime)
                    ctx.fill(Path(CGRect(x: px - 0.75, y: 0, width: 1.5, height: size.height)), with: .color(.white.opacity(0.9)))
                }
                .allowsHitTesting(false)
            }
            .clipShape(RoundedRectangle(cornerRadius: 4))
            .contentShape(Rectangle())
            .gesture(DragGesture(minimumDistance: 0)
                .onChanged { v in viewer.scrub(to: time(v.location.x, geo.size.width), final: false) }
                .onEnded { v in viewer.scrub(to: time(v.location.x, geo.size.width), final: true) })
            .help("What the camera sees across the clip. Click or drag to scrub.")
            .task(id: refreshKey(count: count)) {
                guard let item else { return }
                await strip.render(clip: item.clip, isLog: item.isLog, renderer: library.renderer, count: count,
                                   duration: viewer.duration, framing: framingSnapshot())
            }
        }
    }

    private func time(_ x: CGFloat, _ w: CGFloat) -> Double {
        min(max(Double(x / max(w, 1)) * viewer.duration, 0), viewer.duration)
    }

    /// Everything that changes what the tiles show; coarse so dragging doesn't thrash.
    private func refreshKey(count: Int) -> String {
        let m = viewer.manual
        let shot = viewer.editingShot.map { "\($0.inTime)-\($0.outTime)-\($0.keyframes.map { "\($0.time)\($0.framing)" })" } ?? ""
        return "\(item?.id.path ?? "")|\(count)|\(viewer.duration)|\(Int(m.yaw / 3))|\(Int(m.pitch / 3))|\(m.hfov)|\(m.distortion)|"
            + "\(viewer.stabilization)|\(viewer.smoothing)|\(viewer.lockTime)|\(shot)"
    }

    private func framingSnapshot() -> Filmstrip.FramingRule {
        Filmstrip.FramingRule(shot: viewer.editingShot, manual: viewer.manual,
                              stabilization: viewer.stabilization, smoothing: viewer.smoothing, lockTime: viewer.lockTime)
    }
}

// MARK: - Heading lane

/// x = time, y = heading (relative to where the camera faced at the start; up is left).
/// The line is the camera's line of sight. Click to jump there and look there (inside a shot,
/// that keyframes it); drag to scrub. Drag a keyframe to move it, a shot's edge to trim it, and
/// Shift-drag to mark a new shot.
/// Interest is drawn as heat (luminance): bright where and when something is happening, with a
/// faint wash for time-only signals. Suggested shots are dashed spans.
struct HeadingLane: View {
    @Bindable var viewer: ViewerModel
    let shots: [Shot]
    var suggestions: [Suggestion] = []
    var heat: CGImage?
    var analysis: ClipAnalysis?
    /// Says what the heat is when it isn't interest (a search).
    var heatLabel: String?
    @State private var hover: CGPoint?
    @State private var mode: DragMode?
    @State private var newRange: ClosedRange<Double>?

    /// What a drag in the lane does, decided where it starts.
    private enum DragMode {
        case lane                                   // click to aim, drag to scrub
        case keyframe(Keyframe.ID)                  // move it in time
        case edge(Shot.ID, isIn: Bool)              // trim
        case create(from: Double)                   // Shift-drag: a new shot
    }

    private enum Hit { case keyframe(Keyframe.ID), edge(Shot.ID, isIn: Bool) }

    var body: some View {
        GeometryReader { geo in
            let size = geo.size
            Canvas { ctx, size in draw(&ctx, size) }
                .clipShape(RoundedRectangle(cornerRadius: 4))
                .contentShape(Rectangle())
                .gesture(
                    DragGesture(minimumDistance: 0)
                        .onChanged { v in dragChanged(v, size) }
                        .onEnded { v in dragEnded(v, size) }
                )
                .onContinuousHover { phase in
                    if case .active(let p) = phase {
                        hover = p
                        switch hit(p, size) {
                        case .edge: NSCursor.resizeLeftRight.set()
                        case .keyframe: NSCursor.pointingHand.set()
                        case nil: NSCursor.arrow.set()
                        }
                    } else {
                        hover = nil
                        NSCursor.arrow.set()
                    }
                }
                .help("Where the camera looks over time. Click to jump there and look that way. Drag a keyframe "
                      + "to move it or a shot's edge to trim it; Shift-drag to mark a new shot.")
                .onChange(of: size.width / max(viewer.duration, 0.001), initial: true) { _, pointsPerSecond in
                    viewer.secondsPerPoint = 1 / max(pointsPerSecond, 1e-6)
                }
        }
    }

    private func moved(_ v: DragGesture.Value) -> Bool { abs(v.translation.width) > 3 || abs(v.translation.height) > 3 }

    private func dragChanged(_ v: DragGesture.Value, _ size: CGSize) {
        if mode == nil {
            switch hit(v.startLocation, size) {
            case .keyframe(let id): mode = .keyframe(id)
            case .edge(let id, let isIn): mode = .edge(id, isIn: isIn)
            case nil: mode = NSEvent.modifierFlags.contains(.shift) ? .create(from: time(v.startLocation.x, size)) : .lane
            }
        }
        let t = time(v.location.x, size)
        switch mode {
        case .keyframe(let id): if moved(v) { viewer.moveKeyframe(id, to: t, final: false) }
        case .edge(let id, let isIn): viewer.moveEdge(of: id, isIn: isIn, to: t, final: false)
        case .create(let from): newRange = min(from, t)...max(from, t)
        case .lane: if moved(v) { viewer.scrub(to: t, final: false) }
        case nil: break
        }
    }

    private func dragEnded(_ v: DragGesture.Value, _ size: CGSize) {
        let t = time(v.location.x, size)
        switch mode {
        case .keyframe(let id):
            if moved(v) { viewer.moveKeyframe(id, to: t, final: true) }
            else if let k = viewer.activeShot?.keyframes.first(where: { $0.id == id }) { viewer.scrub(to: k.time, final: true) }
        case .edge(let id, let isIn): viewer.moveEdge(of: id, isIn: isIn, to: t, final: true)
        case .create:
            if let r = newRange, r.upperBound - r.lowerBound >= 0.5 {
                viewer.scrub(to: r.lowerBound, final: true)
                viewer.newShot(from: r.lowerBound, to: r.upperBound)
            }
        case .lane, nil:
            if moved(v) { viewer.scrub(to: t, final: true) }
            else { viewer.aim(heading: heading(rowFraction: v.location.y / size.height), at: t, final: true) }
        }
        mode = nil
        newRange = nil
    }

    /// Keyframes of the active shot first (they sit on top), then shot edges.
    private func hit(_ p: CGPoint, _ size: CGSize) -> Hit? {
        guard viewer.duration > 0 else { return nil }
        for (id, c) in keyframePoints(size) where hypot(p.x - c.x, p.y - c.y) <= 7 { return .keyframe(id) }
        let ordered = shots.sorted { a, _ in a.id == viewer.activeShotID }
        for shot in ordered {
            for (isIn, t) in [(true, shot.inTime), (false, shot.outTime)] {
                if abs(p.x - CGFloat(t / viewer.duration) * size.width) <= 5 { return .edge(shot.id, isIn: isIn) }
            }
        }
        return nil
    }

    /// Where the active shot's keyframes are drawn.
    private func keyframePoints(_ size: CGSize) -> [(Keyframe.ID, CGPoint)] {
        guard let shot = viewer.activeShot, let motion = viewer.clip?.motion, viewer.duration > 0 else { return [] }
        return shot.keyframes.map { k in
            let hd = motion.baseHeading(at: k.time, mode: shot.stabilization, smoothing: shot.smoothing,
                                        lockTime: shot.lockTime) + k.framing.yaw
            return (k.id, CGPoint(x: CGFloat(k.time / viewer.duration) * size.width,
                                  y: CGFloat(rowFraction(heading: hd)) * size.height))
        }
    }

    private var origin: Double {
        viewer.clip?.motion?.baseHeading(at: 0, mode: .lock, smoothing: 0, lockTime: 0) ?? 0
    }

    private func rowFraction(heading: Double) -> Double {
        var f = 0.5 - (heading - origin) / 360
        f -= floor(f)
        return f
    }

    private func heading(rowFraction f: Double) -> Double { origin + (0.5 - f) * 360 }

    private func time(_ x: CGFloat, _ size: CGSize) -> Double {
        min(max(Double(x / max(size.width, 1)) * viewer.duration, 0), viewer.duration)
    }

    private func draw(_ ctx: inout GraphicsContext, _ size: CGSize) {
        let full = CGRect(origin: .zero, size: size)
        ctx.fill(Path(full), with: .color(Theme.surround))
        guard viewer.duration > 0, let motion = viewer.clip?.motion else { return }
        let w = size.width, h = size.height, dur = viewer.duration
        func x(_ t: Double) -> CGFloat { CGFloat(t / dur) * w }

        // Interest heat (pre-rendered: time × heading, +180° at the top as the rows are).
        if let heat {
            ctx.draw(Image(decorative: heat, scale: 1).interpolation(.medium), in: full)
        }

        // Direction guides relative to the start heading.
        for (f, label) in [(0.25, "Left"), (0.5, "Ahead"), (0.75, "Right")] {
            let y = CGFloat(f) * h
            var line = Path()
            line.move(to: CGPoint(x: 0, y: y)); line.addLine(to: CGPoint(x: w, y: y))
            ctx.stroke(line, with: .color(.white.opacity(f == 0.5 ? 0.16 : 0.08)), lineWidth: 0.5)
            ctx.draw(Text(label).font(.system(size: 9, weight: .medium)).foregroundStyle(.white.opacity(0.4)),
                     at: CGPoint(x: 6, y: y - 1.5), anchor: .bottomLeading)
        }
        ctx.draw(Text("Behind").font(.system(size: 9, weight: .medium)).foregroundStyle(.white.opacity(0.3)),
                 at: CGPoint(x: 6, y: h - 2), anchor: .bottomLeading)
        if let heatLabel {
            ctx.draw(Text(Image(systemName: "magnifyingglass")).font(.system(size: 8, weight: .semibold))
                     + Text(" " + heatLabel).font(.system(size: 9, weight: .medium)),
                     at: CGPoint(x: w - 6, y: 3), anchor: .topTrailing)
        }

        // Shot spans.
        for shot in shots {
            let r = CGRect(x: x(shot.inTime), y: 0, width: max(x(shot.outTime) - x(shot.inTime), 1), height: h)
            let active = shot.id == viewer.activeShotID
            ctx.fill(Path(r), with: .color(active ? Theme.sightline.opacity(0.10) : Theme.range))
            for edge in [r.minX, r.maxX] {
                ctx.fill(Path(CGRect(x: edge - 0.75, y: 0, width: 1.5, height: h)),
                         with: .color(active ? Theme.sightline : .white.opacity(0.3)))
            }
        }

        // Shift-drag: the shot about to be made.
        if let r = newRange {
            let rect = CGRect(x: x(r.lowerBound), y: 0, width: max(x(r.upperBound) - x(r.lowerBound), 1), height: h)
            ctx.fill(Path(rect), with: .color(Theme.sightline.opacity(0.12)))
            ctx.stroke(Path(rect), with: .color(Theme.sightline), lineWidth: 1.5)
        }

        // Suggested shots: dashed spans with their reasons.
        for sg in suggestions {
            let selected = sg.id == viewer.selectedSuggestionID
            let r = CGRect(x: x(sg.inTime), y: 0.75, width: max(x(sg.outTime) - x(sg.inTime), 2), height: h - 1.5)
            ctx.fill(Path(r), with: .color(.white.opacity(selected ? 0.10 : 0.04)))
            ctx.stroke(Path(roundedRect: r, cornerRadius: 2), with: .color(.white.opacity(selected ? 0.85 : 0.45)),
                       style: StrokeStyle(lineWidth: selected ? 1.5 : 1, dash: [3, 3]))
            if r.width > 60 {
                ctx.draw(Text(r.width > 150 ? sg.summary : sg.title).font(.system(size: 9, weight: .medium)).foregroundStyle(.white.opacity(selected ? 0.9 : 0.6)),
                         at: CGPoint(x: r.minX + 5, y: h - 4), anchor: .bottomLeading)
            }
        }

        // Line of sight over time.
        let active = viewer.editingShot
        let previewing = viewer.activeShot == nil && active != nil
        let n = max(Int(w / 2), 2)
        var segments: [(Path, Bool)] = []
        var path = Path(), prev: CGPoint?, prevInside = false
        for i in 0...n {
            let t = Double(i) / Double(n) * dur
            let f = viewer.framing(at: t)
            let hd = motion.baseHeading(at: t, mode: viewer.stabilization, smoothing: viewer.smoothing,
                                        lockTime: viewer.lockTime) + f.yaw
            let p = CGPoint(x: x(t), y: CGFloat(rowFraction(heading: hd)) * h)
            let inside = active?.contains(t) ?? false
            if let q = prev, abs(p.y - q.y) < h / 2, inside == prevInside {
                path.addLine(to: p)
            } else {
                if prev != nil { segments.append((path, prevInside)) }
                path = Path(); path.move(to: p)
            }
            prev = p; prevInside = inside
        }
        segments.append((path, prevInside))
        for (p, inside) in segments {
            ctx.stroke(p, with: .color(inside ? Theme.sightline : Theme.sightlineDim),
                       style: StrokeStyle(lineWidth: inside ? 2 : 1.5, lineCap: .round, lineJoin: .round))
        }
        if let active {
            for k in active.keyframes {
                // Keyframes left outside a trimmed shot still shape its path; draw them faintly.
                var ctx = ctx
                if !active.contains(k.time) { ctx.opacity = 0.35 }
                let hd = motion.baseHeading(at: k.time, mode: active.stabilization, smoothing: active.smoothing,
                                            lockTime: active.lockTime) + k.framing.yaw
                let c = CGPoint(x: x(k.time), y: CGFloat(rowFraction(heading: hd)) * h)
                var d = Path()
                d.move(to: CGPoint(x: c.x, y: c.y - 5)); d.addLine(to: CGPoint(x: c.x + 5, y: c.y))
                d.addLine(to: CGPoint(x: c.x, y: c.y + 5)); d.addLine(to: CGPoint(x: c.x - 5, y: c.y)); d.closeSubpath()
                if previewing {  // a suggestion's keyframes: outlined until it's kept
                    ctx.fill(d, with: .color(Theme.surround))
                    ctx.stroke(d, with: .color(Theme.sightline), lineWidth: 1.5)
                } else if k.id == viewer.currentKeyframe?.id {  // selected: under the playhead
                    ctx.fill(d, with: .color(Theme.sightline))
                    ctx.stroke(d, with: .color(.white), lineWidth: 2)
                } else {
                    ctx.fill(d, with: .color(Theme.sightline))
                    ctx.stroke(d, with: .color(.black.opacity(0.6)), lineWidth: 1)
                }
            }
        }

        // Playhead with the current view: a bracket as tall as the frame's horizontal coverage.
        let px = x(viewer.currentTime)
        ctx.fill(Path(CGRect(x: px - 0.75, y: 0, width: 1.5, height: h)), with: .color(.white.opacity(0.9)))
        let f = viewer.displayFraming
        let hd = motion.baseHeading(at: viewer.currentTime, mode: viewer.stabilization, smoothing: viewer.smoothing,
                                    lockTime: viewer.lockTime) + f.yaw
        let dot = CGPoint(x: px, y: CGFloat(rowFraction(heading: hd)) * h)
        let span = CGFloat(f.coverage / 360) * h
        ctx.fill(Path(roundedRect: CGRect(x: px - 2.5, y: dot.y - span / 2, width: 5, height: span), cornerRadius: 2.5),
                 with: .color(Theme.sightline.opacity(0.45)))
        ctx.fill(Path(ellipseIn: CGRect(x: dot.x - 4.5, y: dot.y - 4.5, width: 9, height: 9)), with: .color(.white))
        ctx.fill(Path(ellipseIn: CGRect(x: dot.x - 3, y: dot.y - 3, width: 6, height: 6)), with: .color(Theme.sightline))

        if let hover {
            var v = Path()
            v.move(to: CGPoint(x: hover.x, y: 0)); v.addLine(to: CGPoint(x: hover.x, y: h))
            ctx.stroke(v, with: .color(.white.opacity(0.35)), style: StrokeStyle(lineWidth: 1, dash: [2, 3]))
            let t = Double(hover.x / w) * dur
            var label = Text(formatTimecode(t, fps: viewer.fps)).font(Theme.numeric(10))
            let yaw = heading(rowFraction: hover.y / h) - origin
            var why = analysis?.explain(at: t, heading: yaw).filter { $0.value > 0 }.map(\.signal) ?? []
            let faces = analysis?.faceCount(at: t, heading: yaw) ?? 0
            if faces > 0, !why.contains(.faces) { why.insert(.faces, at: 0) }
            let words = why.map { $0 == .faces && faces > 0 ? (faces == 1 ? "a face" : "\(faces) faces") : $0.phrase }
            if !words.isEmpty { label = label + Text("  " + words.joined(separator: ", ")).font(.system(size: 10)) }
            let resolved = ctx.resolve(label.foregroundStyle(.white.opacity(0.85)))
            let tw = resolved.measure(in: size).width
            let lx = hover.x + 5 + tw > w ? hover.x - 5 - tw : hover.x + 5
            ctx.draw(resolved, at: CGPoint(x: lx, y: 3), anchor: .topLeading)
        }
    }
}

/// Gyro speed over time: where the camera was steady or thrashing.
struct MotionRibbon: View {
    let profile: [Double]
    let activeShot: Shot?
    let duration: Double

    var body: some View {
        Canvas { ctx, size in
            guard !profile.isEmpty, duration > 0 else { return }
            let bw = size.width / CGFloat(profile.count)
            for (i, v) in profile.enumerated() {
                let t = (Double(i) + 0.5) / Double(profile.count) * duration
                let inside = activeShot?.contains(t) ?? false
                let bh = max(1, CGFloat(v) * size.height)
                ctx.fill(Path(CGRect(x: CGFloat(i) * bw, y: size.height - bh, width: max(bw - 0.5, 0.5), height: bh)),
                         with: .color(.primary.opacity(inside ? 0.6 : 0.28)))
            }
        }
        .accessibilityLabel("Camera motion over time")
        .help("Camera rotation speed over time. Low bars are steady footage.")
    }
}

// MARK: - Shots

struct ShotStrip: View {
    @Environment(Library.self) private var library
    @Bindable var viewer: ViewerModel
    let shots: [Shot]
    var suggestions: [Suggestion] = []

    private enum Entry: Identifiable {
        case shot(Shot), suggestion(Suggestion)
        var id: String {
            switch self {
            case .shot(let s): return s.id.uuidString
            case .suggestion(let s): return s.id
            }
        }
        var time: Double {
            switch self {
            case .shot(let s): return s.inTime
            case .suggestion(let s): return s.inTime
            }
        }
    }

    var body: some View {
        let entries = (shots.map(Entry.shot) + suggestions.map(Entry.suggestion)).sorted { $0.time < $1.time }
        if entries.isEmpty {
            HStack {
                Text("Press N to start a shot at the playhead, or Shift-drag across the heading lane to mark one.")
                    .caption()
                Spacer()
            }
            .frame(height: 120)
            .padding(.horizontal, 16)
        } else {
            ScrollViewReader { proxy in
                ScrollView(.horizontal) {
                    HStack(alignment: .top, spacing: 14) {
                        ForEach(entries) { entry in
                            tile(entry).id(entry.id)
                        }
                    }
                    .padding(.horizontal, 16)
                    .padding(.bottom, 12)  // room for the scroller when one is shown
                }
                .scrollIndicators(.automatic)
                .scrollPosition($position)
                .onScrollGeometryChange(for: ScrollMetrics.self) { g in
                    ScrollMetrics(offset: g.contentOffset.x, maxOffset: max(g.contentSize.width - g.containerSize.width, 0))
                } action: { _, m in wheel.metrics = m }
                .onHover { wheel.hovering = $0 }
                .onAppear { installWheel() }
                .onDisappear { removeWheel() }
                // Keep whatever you select (here, in the lane or by keys) in view.
                .onChange(of: selectedEntryID) { _, id in
                    guard let id else { return }
                    withAnimation(.easeInOut(duration: 0.25)) { proxy.scrollTo(id, anchor: .center) }
                }
            }
            .frame(height: 120)
        }
    }

    private var selectedEntryID: String? {
        viewer.activeShotID?.uuidString ?? viewer.selectedSuggestionID
    }

    @ViewBuilder private func tile(_ entry: Entry) -> some View {
        switch entry {
        case .shot(let shot):
            ShotTile(shot: shot, thumbnail: library.shotThumbnails[shot.id],
                     selected: shot.id == viewer.activeShotID, fps: viewer.fps)
                .onTapGesture { viewer.select(shot) }
                .contextMenu {
                    Button("Delete Shot", role: .destructive) {
                        if viewer.activeShotID == shot.id { viewer.deleteActiveShot() } else { library.delete(shot.id) }
                    }
                }
        case .suggestion(let sg):
            SuggestionTile(suggestion: sg, thumbnail: library.suggestionThumbnails[sg.id],
                           selected: sg.id == viewer.selectedSuggestionID)
                .onTapGesture { viewer.select(sg) }
                .contextMenu {
                    Button("Keep as Shot") { viewer.select(sg); viewer.acceptSuggestion() }
                    Button("Dismiss") { viewer.select(sg); viewer.dismissSuggestion() }
                }
        }
    }

    // MARK: Vertical wheel -> horizontal scroll

    // A horizontal ScrollView on macOS only follows sideways swipes. A mouse wheel (or an
    // ordinary vertical two-finger scroll) over the strip should move it too.
    struct ScrollMetrics: Equatable { var offset: CGFloat = 0; var maxOffset: CGFloat = 0 }

    final class WheelState {
        var hovering = false
        var metrics = ScrollMetrics()
        var monitor: Any?
    }

    @State private var position = ScrollPosition(edge: .leading)
    @State private var wheel = WheelState()

    private func installWheel() {
        guard wheel.monitor == nil else { return }
        let wheel = wheel, position = $position
        wheel.monitor = NSEvent.addLocalMonitorForEvents(matching: .scrollWheel) { event in
            guard wheel.hovering, abs(event.scrollingDeltaY) > abs(event.scrollingDeltaX) else { return event }
            let step = event.hasPreciseScrollingDeltas ? event.scrollingDeltaY : event.scrollingDeltaY * 12
            // The extent is only known after the first scroll; until then the scroll view clamps.
            var x = max(wheel.metrics.offset - step, 0)
            if wheel.metrics.maxOffset > 0 { x = min(x, wheel.metrics.maxOffset) }
            position.wrappedValue.scrollTo(x: x)
            wheel.metrics.offset = x
            return nil
        }
    }

    private func removeWheel() {
        if let m = wheel.monitor { NSEvent.removeMonitor(m) }
        wheel.monitor = nil
    }
}

/// A suggested shot: a pale, dashed tile with its reasons. A keeps it, X dismisses it.
struct SuggestionTile: View {
    let suggestion: Suggestion
    let thumbnail: CGImage?
    let selected: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 5) {
            ZStack {
                Rectangle().fill(Theme.surround)
                if let thumbnail {
                    Image(decorative: thumbnail, scale: 1).resizable().aspectRatio(16 / 9, contentMode: .fill)
                        .opacity(selected ? 0.9 : 0.5)
                }
            }
            .frame(width: 136, height: 76.5)
            .clipShape(RoundedRectangle(cornerRadius: 4))
            .overlay {
                RoundedRectangle(cornerRadius: 4)
                    .strokeBorder(.white.opacity(selected ? 0.85 : 0.35), style: StrokeStyle(lineWidth: selected ? 1.5 : 1, dash: [4, 3]))
            }
            .overlay(alignment: .bottomLeading) {
                if selected {
                    Text("A keeps it, X dismisses")
                        .font(.system(size: 9, weight: .medium))
                        .foregroundStyle(.white)
                        .padding(.horizontal, 5).padding(.vertical, 1.5)
                        .background(.black.opacity(0.55), in: Capsule())
                        .padding(4)
                }
            }
            HStack(spacing: 6) {
                Text(suggestion.title)
                    .font(.system(size: 11, weight: selected ? .semibold : .regular))
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                Spacer(minLength: 0)
                Text(formatDuration(suggestion.duration))
                    .font(Theme.numeric(10))
                    .foregroundStyle(.tertiary)
            }
            .frame(width: 136)
        }
        .contentShape(Rectangle())
        .help("Suggested: \(suggestion.reasons.map(\.phrase).joined(separator: ", ")). Press A to keep it as a shot, X to dismiss.")
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Suggested shot, \(suggestion.title)")
        .accessibilityAddTraits(selected ? [.isSelected, .isButton] : .isButton)
    }
}

struct ShotTile: View {
    let shot: Shot
    let thumbnail: CGImage?
    let selected: Bool
    let fps: Int

    var body: some View {
        VStack(alignment: .leading, spacing: 5) {
            ZStack {
                Rectangle().fill(Theme.surround)
                if let thumbnail {
                    Image(decorative: thumbnail, scale: 1).resizable().aspectRatio(16 / 9, contentMode: .fill)
                }
            }
            .frame(width: 136, height: 76.5)
            .clipShape(RoundedRectangle(cornerRadius: 4))
            .overlay {
                RoundedRectangle(cornerRadius: 4)
                    .strokeBorder(selected ? Theme.sightline : .white.opacity(0.08), lineWidth: selected ? 2 : 1)
            }
            .overlay(alignment: .bottomTrailing) {
                if shot.keyframesInRange.count > 1 {
                    Label("\(shot.keyframesInRange.count)", systemImage: "diamond.fill")
                        .labelStyle(.titleAndIcon)
                        .font(Theme.numeric(9, weight: .semibold))
                        .foregroundStyle(.white)
                        .padding(.horizontal, 4).padding(.vertical, 1)
                        .background(.black.opacity(0.55), in: Capsule())
                        .padding(4)
                }
            }
            HStack(spacing: 6) {
                Text(shot.name)
                    .font(.system(size: 11, weight: selected ? .semibold : .regular))
                    .lineLimit(1)
                Spacer(minLength: 0)
                Text(formatDuration(shot.duration))
                    .font(Theme.numeric(10))
                    .foregroundStyle(.secondary)
            }
            .frame(width: 136)
        }
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(selected ? [.isSelected, .isButton] : .isButton)
    }
}
