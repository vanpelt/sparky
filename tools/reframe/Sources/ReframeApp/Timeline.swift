import ReframeKit
import SwiftUI

/// Transport, horizon scan and shots, stacked under the viewer.
struct TimelinePane: View {
    @Environment(Library.self) private var library
    @Bindable var viewer: ViewerModel

    var body: some View {
        let item = library.selectedItem
        let shots = library.shots(for: viewer.clip)
        VStack(spacing: 0) {
            TransportBar(viewer: viewer)
            HorizonScanView(viewer: viewer, scan: item?.scan, shots: shots)
                .frame(height: 132)
                .padding(.horizontal, 16)
            ShakeRibbon(scan: item?.scan, activeShot: viewer.activeShot, duration: viewer.duration)
                .frame(height: 12)
                .padding(.horizontal, 16)
                .padding(.top, 3)
            ShotStrip(viewer: viewer, shots: shots)
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

            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(formatTimecode(viewer.currentTime, fps: viewer.fps))
                    .font(Theme.numeric(17, weight: .semibold))
                Text(formatDuration(viewer.duration))
                    .font(Theme.numeric(12))
                    .foregroundStyle(.secondary)
            }

            Spacer(minLength: 12)

            if viewer.hasUnkeyedFraming {
                Button { viewer.addKeyframe() } label: {
                    Label("Keyframe this framing", systemImage: "diamond.fill")
                }
                .buttonStyle(.borderedProminent)
                .tint(Theme.sightline)
                .controlSize(.small)
                .help("Add a keyframe here so the shot keeps this framing (K)")
            }

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

// MARK: - Horizon scan

/// x = time, y = heading. Click to jump there and look there; drag to scrub.
struct HorizonScanView: View {
    @Bindable var viewer: ViewerModel
    let scan: HorizonScan?
    let shots: [Shot]
    @State private var hover: CGPoint?

    var body: some View {
        GeometryReader { geo in
            let size = geo.size
            ZStack(alignment: .topLeading) {
                Canvas { ctx, size in draw(&ctx, size) }
                if scan == nil {
                    Text(viewer.clip == nil ? "" : "Scanning the horizon…")
                        .caption()
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
            .clipShape(RoundedRectangle(cornerRadius: 5))
            .contentShape(Rectangle())
            .gesture(
                DragGesture(minimumDistance: 0)
                    .onChanged { v in
                        if abs(v.translation.width) > 3 || abs(v.translation.height) > 3 {
                            viewer.scrub(to: time(v.location.x, size), final: false)
                        }
                    }
                    .onEnded { v in
                        let t = time(v.location.x, size)
                        if abs(v.translation.width) <= 3 && abs(v.translation.height) <= 3, let scan {
                            viewer.aim(heading: scan.heading(rowFraction: v.location.y / size.height), at: t, final: true)
                        } else {
                            viewer.scrub(to: t, final: true)
                        }
                    }
            )
            .onContinuousHover { phase in
                if case .active(let p) = phase { hover = p } else { hover = nil }
            }
            .help("Click to jump there and look there. Drag to scrub.")
        }
    }

    private func time(_ x: CGFloat, _ size: CGSize) -> Double {
        min(max(Double(x / max(size.width, 1)) * viewer.duration, 0), viewer.duration)
    }

    private func draw(_ ctx: inout GraphicsContext, _ size: CGSize) {
        let full = CGRect(origin: .zero, size: size)
        ctx.fill(Path(full), with: .color(Theme.surround))
        guard viewer.duration > 0 else { return }
        let w = size.width, h = size.height, dur = viewer.duration
        func x(_ t: Double) -> CGFloat { CGFloat(t / dur) * w }

        if let scan {
            ctx.draw(Image(decorative: scan.image, scale: 1), in: full)
            ctx.fill(Path(full), with: .color(.black.opacity(0.16)))
            // Where the camera faced at the start, and directly behind it.
            for (f, label) in [(0.5, "Start"), (0.0, "Behind"), (0.25, "Left"), (0.75, "Right")] {
                let y = CGFloat(f) * h
                var line = Path()
                line.move(to: CGPoint(x: 0, y: y)); line.addLine(to: CGPoint(x: w, y: y))
                ctx.stroke(line, with: .color(.white.opacity(f == 0.5 ? 0.22 : 0.1)), lineWidth: 0.5)
                if f > 0 {
                    ctx.draw(Text(label).font(.system(size: 9, weight: .medium)).foregroundStyle(.white.opacity(0.55)),
                             at: CGPoint(x: 6, y: y - 2), anchor: .bottomLeading)
                }
            }
        }

        // Shot spans.
        for shot in shots {
            let r = CGRect(x: x(shot.inTime), y: 0, width: max(x(shot.outTime) - x(shot.inTime), 1), height: h)
            let active = shot.id == viewer.activeShotID
            ctx.fill(Path(r), with: .color(active ? Theme.sightline.opacity(0.10) : Theme.range))
            for edge in [r.minX, r.maxX] {
                ctx.fill(Path(CGRect(x: edge - 0.75, y: 0, width: 1.5, height: h)),
                         with: .color(active ? Theme.sightline : .white.opacity(0.35)))
            }
        }

        // Line of sight: where the camera looks, over time.
        if let scan, let motion = viewer.clip?.motion {
            let active = viewer.activeShot
            let n = max(Int(w / 2), 2)
            var segments: [(Path, Bool)] = []
            var path = Path(), prev: CGPoint?, prevInside = false
            for i in 0...n {
                let t = Double(i) / Double(n) * dur
                let f = viewer.framing(at: t)
                let heading = motion.baseHeading(at: t, mode: viewer.stabilization, smoothing: viewer.smoothing,
                                                 lockTime: viewer.lockTime) + f.yaw
                let p = CGPoint(x: x(t), y: CGFloat(scan.rowFraction(heading: heading)) * h)
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
                ctx.stroke(p, with: .color(.black.opacity(0.5)), style: StrokeStyle(lineWidth: inside ? 4 : 3, lineCap: .round))
                ctx.stroke(p, with: .color(inside ? Theme.sightline : Theme.sightlineDim),
                           style: StrokeStyle(lineWidth: inside ? 2 : 1.5, lineCap: .round, lineJoin: .round))
            }

            // Keyframes on the active shot.
            if let active {
                for k in active.keyframes {
                    let heading = motion.baseHeading(at: k.time, mode: active.stabilization, smoothing: active.smoothing,
                                                     lockTime: active.lockTime) + k.framing.yaw
                    let c = CGPoint(x: x(k.time), y: CGFloat(scan.rowFraction(heading: heading)) * h)
                    var d = Path()
                    d.move(to: CGPoint(x: c.x, y: c.y - 5)); d.addLine(to: CGPoint(x: c.x + 5, y: c.y))
                    d.addLine(to: CGPoint(x: c.x, y: c.y + 5)); d.addLine(to: CGPoint(x: c.x - 5, y: c.y)); d.closeSubpath()
                    ctx.fill(d, with: .color(Theme.sightline))
                    ctx.stroke(d, with: .color(.black.opacity(0.6)), lineWidth: 1)
                }
            }

            // Playhead, with the current line of sight marked on it.
            let px = x(viewer.currentTime)
            ctx.fill(Path(CGRect(x: px - 0.75, y: 0, width: 1.5, height: h)), with: .color(.white.opacity(0.9)))
            let f = viewer.displayFraming
            let heading = motion.baseHeading(at: viewer.currentTime, mode: viewer.stabilization,
                                             smoothing: viewer.smoothing, lockTime: viewer.lockTime) + f.yaw
            let dot = CGPoint(x: px, y: CGFloat(scan.rowFraction(heading: heading)) * h)
            let span = CGFloat(f.hfov / 360) * h  // the frame's horizontal field of view, as a bracket
            ctx.fill(Path(roundedRect: CGRect(x: px - 2.5, y: dot.y - span / 2, width: 5, height: span), cornerRadius: 2.5),
                     with: .color(Theme.sightline.opacity(0.45)))
            ctx.fill(Path(ellipseIn: CGRect(x: dot.x - 4.5, y: dot.y - 4.5, width: 9, height: 9)), with: .color(.white))
            ctx.fill(Path(ellipseIn: CGRect(x: dot.x - 3, y: dot.y - 3, width: 6, height: 6)), with: .color(Theme.sightline))
        }

        // Hover crosshair.
        if let hover {
            var v = Path()
            v.move(to: CGPoint(x: hover.x, y: 0)); v.addLine(to: CGPoint(x: hover.x, y: h))
            ctx.stroke(v, with: .color(.white.opacity(0.35)), style: StrokeStyle(lineWidth: 1, dash: [2, 3]))
            ctx.draw(Text(formatTimecode(Double(hover.x / w) * dur, fps: viewer.fps))
                        .font(Theme.numeric(10)).foregroundStyle(.white.opacity(0.85)),
                     at: CGPoint(x: min(hover.x + 5, w - 50), y: h - 4), anchor: .bottomLeading)
        }
    }
}

/// Gyro speed over time: where the camera was calm or thrashing.
struct ShakeRibbon: View {
    let scan: HorizonScan?
    let activeShot: Shot?
    let duration: Double

    var body: some View {
        Canvas { ctx, size in
            guard let shake = scan?.shake, !shake.isEmpty, duration > 0 else { return }
            let bw = size.width / CGFloat(shake.count)
            for (i, v) in shake.enumerated() {
                let t = (Double(i) + 0.5) / Double(shake.count) * duration
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

    var body: some View {
        if shots.isEmpty {
            HStack {
                Text("Press N to start a shot at the playhead, or I and O to mark its ends.")
                    .caption()
                Spacer()
            }
            .frame(height: 108)
            .padding(.horizontal, 16)
        } else {
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(alignment: .top, spacing: 14) {
                    ForEach(shots) { shot in
                        ShotTile(shot: shot, thumbnail: library.shotThumbnails[shot.id],
                                 selected: shot.id == viewer.activeShotID, fps: viewer.fps)
                            .onTapGesture { viewer.select(shot) }
                            .contextMenu {
                                Button("Delete Shot", role: .destructive) {
                                    if viewer.activeShotID == shot.id { viewer.deleteActiveShot() } else { library.delete(shot.id) }
                                }
                            }
                    }
                }
                .padding(.horizontal, 16)
            }
            .frame(height: 108)
        }
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
                if shot.keyframes.count > 1 {
                    Label("\(shot.keyframes.count)", systemImage: "diamond.fill")
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
