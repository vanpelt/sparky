import SwiftUI

/// Design tokens (see DESIGN.md). Color is reserved for the sightline: where the camera looks.
enum Theme {
    /// Optical cyan: look path, playhead, keyframes, primary action.
    static let sightline = Color(red: 0.20, green: 0.84, blue: 0.77)
    static let sightlineDim = sightline.opacity(0.35)
    /// Shot spans on the horizon scan.
    static let range = Color.white.opacity(0.12)
    /// Neutral surround behind the frame (not tinted, so color judgement isn't biased).
    static let surround = Color(white: 0.085)

    /// Instrument-like numerals: timecode, durations, degrees.
    static func numeric(_ size: CGFloat, weight: Font.Weight = .medium) -> Font {
        .system(size: size, weight: weight, design: .rounded).monospacedDigit()
    }

    static let title = Font.system(size: 13, weight: .semibold, design: .rounded)
}

extension View {
    /// Small secondary caption in the interface's plain voice.
    func caption() -> some View { font(.system(size: 11)).foregroundStyle(.secondary) }
}

func formatDuration(_ s: Double) -> String {
    let t = Int(s.rounded())
    return t >= 3600 ? String(format: "%d:%02d:%02d", t / 3600, t / 60 % 60, t % 60)
        : String(format: "%d:%02d", t / 60, t % 60)
}

/// mm:ss:ff (or h:mm:ss:ff past an hour), frames at the clip's rate.
func formatTimecode(_ s: Double, fps: Int) -> String {
    let f = max(fps, 1)
    let frames = Int((max(s, 0) * Double(f)).rounded())
    let secs = frames / f
    if secs >= 3600 { return String(format: "%d:%02d:%02d:%02d", secs / 3600, secs / 60 % 60, secs % 60, frames % f) }
    return String(format: "%02d:%02d:%02d", secs / 60, secs % 60, frames % f)
}

/// Five bars encoding how much the camera rotated (p95 deg/s): a value, not a label.
struct ShakeMeter: View {
    let p95: Double

    var level: Int {
        switch p95 {
        case ..<15: return 1
        case ..<40: return 2
        case ..<80: return 3
        case ..<140: return 4
        default: return 5
        }
    }

    var body: some View {
        HStack(alignment: .bottom, spacing: 1.5) {
            ForEach(0..<5) { i in
                RoundedRectangle(cornerRadius: 0.75)
                    .fill(i < level ? Color.primary.opacity(0.75) : Color.primary.opacity(0.15))
                    .frame(width: 2.5, height: 4 + CGFloat(i) * 2)
            }
        }
        .accessibilityElement()
        .accessibilityLabel("Camera motion \(level) of 5")
        .help(String(format: "Camera rotation: %.0f°/s at the 95th percentile", p95))
    }
}
