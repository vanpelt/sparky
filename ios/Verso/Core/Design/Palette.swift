import SwiftUI
import UIKit

/// Colour tokens: "Day Edition" (light) and "Night Reading" (dark). Dark is a lamp-lit page, never pure black.
public enum Palette {
    public static let paper = Color(light: 0xF2ECDF, dark: 0x141210)
    public static let deckle = Color(light: 0xE6DDCB, dark: 0x1F1C18)
    public static let rule = Color(light: 0xCDBFA8, dark: 0x3A322B)
    public static let ink = Color(light: 0x1C1A17, dark: 0xE8E0CF)
    public static let fadedInk = Color(light: 0x6E665A, dark: 0x8C8375)
    /// Book One accent: money, chapters, the bookmark ribbon.
    public static let oxblood = Color(light: 0x7B2121, dark: 0xD4673F)
    /// Rare: vest events, met clauses, the Turning Point. Never body text.
    public static let gilt = Color(light: 0xB08D3C, dark: 0xC9A55A)
    /// Book Two accent: threads, momentum.
    public static let verdigris = Color(light: 0x3F6B5F, dark: 0x6FA394)
    public static let rust = Color(light: 0xA3452F, dark: 0xD0694E)
    public static let dusk = Color(light: 0x4A3A55, dark: 0x8E7AA0)

    /// A thread's cloth: same hex both modes, drawn at 90% brightness on dark paper.
    public static func binding(_ b: Binding) -> Color {
        let hex = b.hex
        return Color(uiColor: UIColor { t in
            t.userInterfaceStyle == .dark ? UIColor(hex: hex).adjusted(brightness: 0.9) : UIColor(hex: hex)
        })
    }

    /// Ring stroke for a binding: +25% lightness on dark paper so it stays legible.
    public static func bindingStroke(_ b: Binding) -> Color {
        let hex = b.hex
        return Color(uiColor: UIColor { t in
            t.userInterfaceStyle == .dark ? UIColor(hex: hex).lightened(by: 0.25) : UIColor(hex: hex)
        })
    }

    public static func stage(_ s: Stage) -> Color {
        switch s {
        case .exposition: fadedInk
        case .risingAction: ink
        case .crisis: rust
        case .turningPoint, .recto: gilt
        }
    }
}

public extension Binding {
    var hex: UInt32 {
        switch self {
        case .oxblood: 0x7B2121
        case .navy: 0x24324A
        case .forest: 0x2F4A3A
        case .ochre: 0xA9782B
        case .plum: 0x4E2E48
        case .slate: 0x4A5056
        }
    }
    var displayName: String { rawValue.capitalized }
}

public extension Color {
    init(light: UInt32, dark: UInt32) {
        self.init(uiColor: UIColor { $0.userInterfaceStyle == .dark ? UIColor(hex: dark) : UIColor(hex: light) })
    }
    init(hex: UInt32) { self.init(uiColor: UIColor(hex: hex)) }
}

extension UIColor {
    convenience init(hex: UInt32) {
        self.init(red: CGFloat((hex >> 16) & 0xFF) / 255, green: CGFloat((hex >> 8) & 0xFF) / 255,
                  blue: CGFloat(hex & 0xFF) / 255, alpha: 1)
    }

    func adjusted(brightness factor: CGFloat) -> UIColor {
        var h: CGFloat = 0, s: CGFloat = 0, b: CGFloat = 0, a: CGFloat = 0
        getHue(&h, saturation: &s, brightness: &b, alpha: &a)
        return UIColor(hue: h, saturation: s, brightness: min(1, b * factor), alpha: a)
    }

    /// HSL lightness + `amount` (0…1).
    func lightened(by amount: CGFloat) -> UIColor {
        var h: CGFloat = 0, s: CGFloat = 0, v: CGFloat = 0, a: CGFloat = 0
        getHue(&h, saturation: &s, brightness: &v, alpha: &a)
        // HSV → HSL
        var l = v * (1 - s / 2)
        var sl: CGFloat = (l == 0 || l == 1) ? 0 : (v - l) / min(l, 1 - l)
        l = min(1, l + amount)
        // HSL → HSV
        let v2 = l + sl * min(l, 1 - l)
        sl = v2 == 0 ? 0 : 2 * (1 - l / v2)
        return UIColor(hue: h, saturation: sl, brightness: v2, alpha: a)
    }
}
