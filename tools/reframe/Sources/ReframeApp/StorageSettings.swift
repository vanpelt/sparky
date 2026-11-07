import AppKit
import ReframeKit
import SwiftUI

/// Settings › Storage: what Reframe keeps on disk as a two-ring chart (inner: kind of data, outer:
/// the clips inside it), sizes by kind, and ways to clear each, behind confirmations.
struct StorageSettings: View {
    @Environment(Library.self) private var library
    @State private var report: StorageReport?
    // Debug: REFRAME_STORAGE_HOVER=<segment or kind id> starts with that segment highlighted.
    @State private var hovered: String? = ProcessInfo.processInfo.environment["REFRAME_STORAGE_HOVER"]
    @State private var confirm: Action?

    enum Action: Identifiable {
        case analysis, originals, clips
        var id: Int { hashValue }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            if let report {
                HStack(alignment: .top, spacing: 28) {
                    StorageRing(report: report, hovered: $hovered)
                        .frame(width: 270, height: 270)
                    legend(report)
                }
                Divider()
                actions(report)
            } else {
                ProgressView().frame(maxWidth: .infinity, minHeight: 300)
            }
        }
        .padding(24)
        .frame(width: 680)
        .task { await rescan() }
        .confirmationDialog(title, isPresented: Binding(get: { confirm != nil }, set: { if !$0 { confirm = nil } }),
                            presenting: confirm) { action in
            buttons(action)
        } message: { action in
            Text(message(action))
        }
    }

    // MARK: Legend

    private func legend(_ r: StorageReport) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(alignment: .firstTextBaseline) {
                Text("Reframe").font(.system(size: 20, weight: .semibold))
                Spacer()
                Text(formatBytes(r.total)).font(Theme.numeric(20, weight: .semibold))
            }
            ForEach(StorageReport.Kind.allCases) { kind in
                let bytes = r.bytes(kind)
                if bytes > 0 {
                    HStack(alignment: .firstTextBaseline, spacing: 9) {
                        Circle().fill(StorageRing.color(kind)).frame(width: 9, height: 9)
                            .alignmentGuide(.firstTextBaseline) { $0[.bottom] - 1 }
                        VStack(alignment: .leading, spacing: 1) {
                            Text(kind.label).font(.system(size: 13))
                            Text(kind.detail).caption().lineLimit(2).fixedSize(horizontal: false, vertical: true)
                        }
                        Spacer(minLength: 8)
                        Text(formatBytes(bytes)).font(Theme.numeric(13))
                    }
                    .padding(.vertical, 2)
                    .padding(.horizontal, 6)
                    .background(RoundedRectangle(cornerRadius: 5).fill(.white.opacity(hovered == kind.rawValue ? 0.07 : 0)))
                    .onHover { hovered = $0 ? kind.rawValue : (hovered == kind.rawValue ? nil : hovered) }
                }
            }
            if r.shared > 0 {
                Text("\(formatBytes(r.shared)) of this is cloned from Insta360 Studio's downloads or your footage folder and shares disk space with those copies, so removing it frees less.")
                    .caption()
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.top, 4)
            }
        }
        .frame(maxWidth: .infinity)
    }

    // MARK: Actions

    private func actions(_ r: StorageReport) -> some View {
        let originals = r.entries(.originals).count, clips = Set(r.entries.compactMap(\.mediaId)).count
        return VStack(alignment: .leading, spacing: 10) {
            row("Analysis cache", "Recomputed when a clip is next opened, a few seconds each.",
                button: "Clear…", disabled: r.bytes(.analysis) == 0) { confirm = .analysis }
            row("Downloaded originals", originals == 0 ? "None downloaded."
                : "\(originals) original\(originals == 1 ? "" : "s"). Export downloads or clones them again when needed.",
                button: "Remove…", disabled: originals == 0) { confirm = .originals }
            row("Cloud clips", clips == 0 ? "None cached."
                : "\(clips) clip\(clips == 1 ? "" : "s") cached. Removed clips are fetched again when you open them; shots are kept.",
                button: "Remove…", disabled: clips == 0) { confirm = .clips }
        }
    }

    private func row(_ title: String, _ detail: String, button: String, disabled: Bool,
                     action: @escaping () -> Void) -> some View {
        HStack(alignment: .center) {
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.system(size: 13, weight: .medium))
                Text(detail).caption()
            }
            Spacer()
            Button(button, action: action).disabled(disabled)
        }
    }

    private var title: String {
        switch confirm {
        case .analysis: return "Clear the analysis cache?"
        case .originals: return "Remove downloaded originals?"
        case .clips: return "Remove cached cloud clips?"
        case nil: return ""
        }
    }

    private func message(_ a: Action) -> String {
        guard let r = report else { return "" }
        switch a {
        case .analysis:
            return "Frees \(formatBytes(r.bytes(.analysis))). Each clip is analyzed again the next time Reframe opens it."
        case .originals:
            return "Frees up to \(formatBytes(r.bytes(.originals))). The clips keep their proxies and shots; exporting them downloads (or clones from Insta360 Studio) the originals again."
        case .clips:
            return "Your shots are kept either way. A removed clip disappears from the shots view's pictures until you open it again, which fetches it (or clones it from Insta360 Studio)."
        }
    }

    @ViewBuilder private func buttons(_ a: Action) -> some View {
        switch a {
        case .analysis:
            Button("Clear Analysis Cache", role: .destructive) { library.clearAnalysisCache(); Task { await rescan() } }
        case .originals:
            Button("Remove Originals", role: .destructive) { library.removeDownloadedOriginals(); Task { await rescan() } }
        case .clips:
            let (unused, unusedBytes, all, allBytes) = clipCounts()
            if unused > 0 {
                Button("Remove \(unused) Clip\(unused == 1 ? "" : "s") Without Shots (\(formatBytes(unusedBytes)))", role: .destructive) {
                    library.removeCloudClips(keepingClipsWithShots: true); Task { await rescan() }
                }
            }
            Button("Remove All \(all) Clips (\(formatBytes(allBytes)))", role: .destructive) {
                library.removeCloudClips(keepingClipsWithShots: false); Task { await rescan() }
            }
        }
        Button("Cancel", role: .cancel) {}
    }

    /// (clips without shots, their bytes, all cached clips, their bytes)
    private func clipCounts() -> (Int, Int64, Int, Int64) {
        guard let r = report else { return (0, 0, 0, 0) }
        let withShots = Set(library.shots.compactMap { s -> String? in
            CloudCache.record(for: URL(fileURLWithPath: s.clipPath))?.mediaId
        })
        var byClip: [String: Int64] = [:]
        for e in r.entries { if let id = e.mediaId { byClip[id, default: 0] += e.bytes } }
        let unused = byClip.filter { !withShots.contains($0.key) }
        return (unused.count, unused.values.reduce(0, +), byClip.count, byClip.values.reduce(0, +))
    }

    private func rescan() async {
        let r = await Task.detached(priority: .userInitiated) { StorageReport.scan() }.value
        report = r
    }
}

/// Two rings: the inner one by kind of data (fixed hue order), the outer one the clips within each
/// kind in that kind's hue, smallest merged. Hover a segment (or a legend row) to read it.
struct StorageRing: View {
    let report: StorageReport
    @Binding var hovered: String?

    /// Categorical slots 1–4 of the dark data-viz palette (validated on a dark surface, wrap-around
    /// pair included), in fixed order; "other" is neutral gray, not a hue.
    static func color(_ kind: StorageReport.Kind) -> Color {
        switch kind {
        case .proxies: return Color(red: 0x39 / 255, green: 0x87 / 255, blue: 0xe5 / 255)
        case .originals: return Color(red: 0xd9 / 255, green: 0x59 / 255, blue: 0x26 / 255)
        case .gyro: return Color(red: 0x19 / 255, green: 0x9e / 255, blue: 0x70 / 255)
        case .analysis: return Color(red: 0xc9 / 255, green: 0x85 / 255, blue: 0x00 / 255)
        case .other: return Color(white: 0.45)
        }
    }

    struct Segment: Identifiable {
        var id: String
        var ring: Int
        var start: Double, end: Double  // fractions of the circle, clockwise from 12 o'clock
        var kind: StorageReport.Kind
        var label: String
        var bytes: Int64
        var faint = false
    }

    var segments: [Segment] {
        let total = Double(max(report.total, 1))
        var out: [Segment] = []
        var a = 0.0
        for kind in StorageReport.Kind.allCases {
            let bytes = report.bytes(kind)
            guard bytes > 0 else { continue }
            let span = Double(bytes) / total
            out.append(Segment(id: kind.rawValue, ring: 0, start: a, end: a + span, kind: kind, label: kind.label, bytes: bytes))
            // Outer ring: this kind's clips, biggest first; slivers under ~2° merge into one.
            if kind != .analysis && kind != .other {
                var b = a, small: Int64 = 0, smallCount = 0
                for e in report.entries(kind).sorted(by: { $0.bytes > $1.bytes }) {
                    let s = Double(e.bytes) / total
                    if s < 0.0055 { small += e.bytes; smallCount += 1; continue }
                    out.append(Segment(id: e.id, ring: 1, start: b, end: b + s, kind: kind, label: e.name, bytes: e.bytes))
                    b += s
                }
                if small > 0 {
                    out.append(Segment(id: "small-\(kind.rawValue)", ring: 1, start: b, end: a + span, kind: kind,
                                       label: "\(smallCount) smaller clip\(smallCount == 1 ? "" : "s")", bytes: small, faint: true))
                }
            }
            a += span
        }
        return out
    }

    private func radii(_ ring: Int, _ R: Double) -> (Double, Double) {
        ring == 0 ? (R * 0.36, R * 0.70) : (R * 0.72, R * 0.98)
    }

    var body: some View {
        let segs = segments
        let focus = segs.first { $0.id == hovered }
        GeometryReader { g in
            let size = min(g.size.width, g.size.height), R = size / 2
            let c = CGPoint(x: g.size.width / 2, y: g.size.height / 2)
            ZStack {
                Canvas { ctx, _ in
                    for s in segs {
                        let (r0, r1) = radii(s.ring, R)
                        let p = Self.sector(c, r0, r1, s.start, s.end)
                        let lit = hovered == nil || hovered == s.id || hovered == s.kind.rawValue
                        let base = Self.color(s.kind).opacity(s.ring == 0 ? 1 : (s.faint ? 0.38 : 0.72))
                        ctx.fill(p, with: .color(lit ? base : base.opacity(0.28)))
                        // 2 px surface gap between neighbours
                        ctx.stroke(p, with: .color(Color(nsColor: .windowBackgroundColor)), lineWidth: 2)
                    }
                }
                VStack(spacing: 2) {
                    if let focus {
                        Text(focus.label).font(.system(size: 12, weight: .medium)).multilineTextAlignment(.center)
                            .lineLimit(2).frame(maxWidth: R * 0.62)
                        Text(formatBytes(focus.bytes)).font(Theme.numeric(15, weight: .semibold))
                    } else {
                        Text(formatBytes(report.total)).font(Theme.numeric(17, weight: .semibold))
                        Text("on disk").caption()
                    }
                }
                .allowsHitTesting(false)
            }
            .contentShape(Rectangle())
            .onContinuousHover { phase in
                guard case .active(let p) = phase else { hovered = nil; return }
                let dx = p.x - c.x, dy = p.y - c.y, r = (dx * dx + dy * dy).squareRoot()
                var f = (atan2(dy, dx) + .pi / 2) / (2 * .pi)
                if f < 0 { f += 1 }
                hovered = segs.first { s in
                    let (r0, r1) = radii(s.ring, R)
                    return r >= r0 && r <= r1 && f >= s.start && f < s.end
                }?.id
            }
        }
        .accessibilityElement()
        .accessibilityLabel("Storage: \(formatBytes(report.total)) in all")
    }

    /// Annular sector from fraction a0 to a1, clockwise from 12 o'clock.
    static func sector(_ c: CGPoint, _ r0: Double, _ r1: Double, _ a0: Double, _ a1: Double) -> Path {
        let s = Angle(radians: a0 * 2 * .pi - .pi / 2), e = Angle(radians: a1 * 2 * .pi - .pi / 2)
        var p = Path()
        p.addArc(center: c, radius: r1, startAngle: s, endAngle: e, clockwise: false)
        p.addArc(center: c, radius: r0, startAngle: e, endAngle: s, clockwise: true)
        p.closeSubpath()
        return p
    }
}
