import SwiftUI
import UIKit

/// Typography. All fonts ship with iOS; nothing is bundled.
public enum VersoFont {
    static let didotName: String? = ["Didot", "BodoniSvtyTwoITCTT-Book"].first { UIFont(name: $0, size: 12) != nil }

    /// Hero numerals: Didot (→ Bodoni 72 → New York). Use with `.monospacedDigit()` and `.contentTransition(.numericText())`.
    /// Scales with Dynamic Type; callers cap at 1.3× with `.dynamicTypeSize(...DynamicTypeSize.accessibility1)`.
    public static func hero(_ size: CGFloat, relativeTo style: Font.TextStyle = .largeTitle) -> Font {
        if let name = didotName { return .custom(name, size: size, relativeTo: style) }
        return .system(size: size, weight: .regular, design: .serif)
    }

    /// Display / headings: New York at a FIXED size. Only for glyphs drawn inside fixed geometry (a Lock
    /// Screen ring's centre letter); headings use `.displayFont(_:)`, which follows Dynamic Type.
    public static func display(_ size: CGFloat, weight: Font.Weight = .regular) -> Font {
        .system(size: size, weight: weight, design: .serif)
    }

    /// New York at a text style (scales with Dynamic Type).
    public static func serif(_ style: Font.TextStyle, weight: Font.Weight = .regular) -> Font {
        .system(style, design: .serif, weight: weight)
    }

    /// Body / marginalia: Iowan Old Style roman.
    public static func body(_ size: CGFloat = 17, relativeTo style: Font.TextStyle = .body) -> Font {
        .custom("IowanOldStyle-Roman", size: size, relativeTo: style)
    }

    /// Iowan italic: reserved for HIS words (epigraphs, Marginalia sentences, clause text, opening lines).
    public static func his(_ size: CGFloat = 17, relativeTo style: Font.TextStyle = .body) -> Font {
        .custom("IowanOldStyle-Italic", size: size, relativeTo: style)
    }

    /// Ledger figures: New York with tabular digits.
    public static func ledger(_ style: Font.TextStyle = .body) -> Font {
        .system(style, design: .serif).monospacedDigit()
    }

    /// Running heads at a FIXED size (fixed-geometry widget slots). Prefer `.runningHeadStyle(...)`,
    /// which scales with Dynamic Type.
    public static func runningHead(_ size: CGFloat = 11) -> Font {
        .system(size: size, weight: .regular, design: .serif).smallCaps()
    }

    /// The Colophon's typesetter slug.
    public static let slug = Font.system(size: 10, design: .monospaced)
}

public enum Glyph {
    public static let fleuron = "❦"
    public static let asterism = "⁂"
    public static let section = "§"
    public static let pilcrow = "¶"
    public static let pen = "✒︎"
}

/// New York at a designed size that scales with Dynamic Type like `relativeTo` would, capped at `maxScale`
/// so a 34pt heading doesn't become a 120pt one at AX5. `.system(size:)` alone never scales.
struct ScaledSerif: ViewModifier {
    @ScaledMetric private var scaled: CGFloat
    let base: CGFloat
    let weight: Font.Weight
    let smallCaps: Bool
    let maxScale: CGFloat

    init(size: CGFloat, weight: Font.Weight, relativeTo style: Font.TextStyle, smallCaps: Bool, maxScale: CGFloat) {
        _scaled = ScaledMetric(wrappedValue: size, relativeTo: style)
        self.base = size; self.weight = weight; self.smallCaps = smallCaps; self.maxScale = maxScale
    }

    func body(content: Content) -> some View {
        let f = Font.system(size: min(scaled, base * maxScale), weight: weight, design: .serif)
        content.font(smallCaps ? f.smallCaps() : f)
    }
}

public extension View {
    /// Display / headings: New York at `size`, following Dynamic Type (capped at 1.8× by default).
    func displayFont(_ size: CGFloat, weight: Font.Weight = .regular, relativeTo style: Font.TextStyle = .title,
                     maxScale: CGFloat = 1.8) -> some View {
        modifier(ScaledSerif(size: size, weight: weight, relativeTo: style, smallCaps: false, maxScale: maxScale))
    }

    /// A running head / small-caps label style. Scales with Dynamic Type (capped at 1.6×).
    func runningHeadStyle(_ color: Color = Palette.fadedInk, size: CGFloat = 11) -> some View {
        self.modifier(ScaledSerif(size: size, weight: .regular, relativeTo: .caption, smallCaps: true, maxScale: 1.6))
            .tracking(1.5).foregroundStyle(color)
    }

    /// Ink-bleed reveal: blur(6)+opacity 0 → sharp over 0.45s (skipped with Reduce Motion by the caller).
    func inkBleed(_ shown: Bool) -> some View {
        self.blur(radius: shown ? 0 : 6).opacity(shown ? 1 : 0).animation(.easeOut(duration: 0.45), value: shown)
    }
}
