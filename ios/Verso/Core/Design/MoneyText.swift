import SwiftUI

/// Whether dollar figures are veiled in this view tree. The app sets it from AppModel (veilByDefault && !veilLifted);
/// accessory widgets set it from `showFiguresOnLockScreen`.
public extension EnvironmentValues {
    @Entry var versoVeiled: Bool = true
}

/// The ONLY way to render money: handles "≈", the veil (·····), privacySensitive and numericText.
/// Countdowns, fractions, rings and gauges are never veiled — don't use this for them.
public struct MoneyText: View {
    public enum Style: Sendable { case compact, full, short }
    let value: Double?
    let style: Style
    let afterTax: Bool
    var forceVisible: Bool
    @Environment(\.versoVeiled) private var veiled

    public init(_ value: Double?, style: Style = .compact, afterTax: Bool = true, forceVisible: Bool = false) {
        self.value = value; self.style = style; self.afterTax = afterTax; self.forceVisible = forceVisible
    }

    public var body: some View {
        let hide = veiled && !forceVisible
        Text(string(hidden: hide))
            .monospacedDigit()
            .contentTransition(.numericText(value: value ?? 0))
            .privacySensitive()
            .accessibilityLabel(hide ? "hidden amount" : string(hidden: false))
    }

    func string(hidden: Bool) -> String {
        let s: MoneyFormat.Style = switch style { case .compact: .compact; case .full: .full; case .short: .short }
        return MoneyFormat.string(value, style: s, afterTax: afterTax, veiled: hidden)
    }
}
