import Foundation

public enum MoneyFormat {
    /// The veil glyph used wherever a dollar figure is hidden.
    public static let veil = "·····"

    /// Compact, as futr: ≥1e9 "$1.23B", ≥1e6 "$1.2M", ≥1e3 "$787k", otherwise "$950".
    public static func compact(_ v: Double) -> String {
        guard v.isFinite else { return "—" }
        let s = v < 0 ? "−" : "", a = abs(v)
        if a >= 1e9 { return s + "$" + String(format: "%.2f", a / 1e9) + "B" }
        if a >= 1e6 { return s + "$" + String(format: "%.1f", a / 1e6) + "M" }
        if a >= 1e3 { return s + "$" + String(Int((a / 1e3).rounded())) + "k" }
        return s + "$" + String(Int(a.rounded()))
    }

    /// Short widget form with one decimal in thousands: 9,461 → "$9.5k".
    public static func short(_ v: Double) -> String {
        guard v.isFinite else { return "—" }
        let a = abs(v), s = v < 0 ? "−" : ""
        if a >= 1e6 { return s + "$" + String(format: "%.1f", a / 1e6) + "M" }
        if a >= 1e4 { return s + "$" + String(Int((a / 1e3).rounded())) + "k" }
        if a >= 1e3 { return s + "$" + String(format: "%.1f", a / 1e3) + "k" }
        return s + "$" + String(Int(a.rounded()))
    }

    /// Whole dollars with grouping: "$9,461".
    public static func full(_ v: Double) -> String {
        guard v.isFinite else { return "—" }
        let s = v < 0 ? "−" : ""
        return s + "$" + Int(abs(v).rounded()).formatted(.number.grouping(.automatic))
    }

    /// A share price: "$100.00".
    public static func price(_ v: Double?) -> String {
        guard let v, v.isFinite else { return "$—.—" }
        return "$" + v.formatted(.number.precision(.fractionLength(2)).grouping(.automatic))
    }

    public static func shares(_ v: Double) -> String {
        Int(v.rounded()).formatted(.number.grouping(.automatic))
    }

    public enum Style: Sendable { case compact, full, short }

    /// Formats with the "≈" prefix for after-tax figures, or the veil when hidden.
    public static func string(_ v: Double?, style: Style = .compact, afterTax: Bool = true, veiled: Bool = false) -> String {
        if veiled { return veil }
        guard let v else { return "—" }
        let body: String = switch style {
        case .compact: compact(v)
        case .full: full(v)
        case .short: short(v)
        }
        return afterTax ? "≈ " + body : body
    }

    public static let footnote = "Not tax advice. Today's price, your rates."
}
