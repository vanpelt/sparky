import CoreGraphics

/// How a window is set: one column, or an open book of two pages. Pure geometry, so it is decided (and tested)
/// in Core; the app reads it as `\.pageLayout`.
public struct PageLayout: Equatable, Sendable {
    /// Two pages side by side (the open iPhone Duo, a landscape iPad).
    public let isSpread: Bool
    /// The width of one text column: the single column, or each page of the spread.
    public let columnWidth: CGFloat
    /// The outer side margin. On a spread the two inner margins at the spine are half this each, so the
    /// gutter between the columns matches the outer ones.
    public let gutter: CGFloat
    /// A short window (under 700pt tall): the closed Duo, a Split View half, each page of an open spread. Heroes
    /// set for a phone's 874pt take a step down there so the page's first lines still show above the foot. An
    /// iPhone SE (375×667) is short too but keeps the phone layout it was drawn with: only windows from 380pt
    /// wide count.
    public let isShort: Bool
    /// The window is a phone column, as Verso was drawn for: one column, at most 440pt wide (a Pro Max), and
    /// not short. Everything Verso does differently elsewhere (the pen in the running foot, controls inline in
    /// the running head) keys off this being false, so a phone never moves.
    public let isPhone: Bool

    public init(isSpread: Bool, columnWidth: CGFloat, gutter: CGFloat, isShort: Bool = false, isPhone: Bool = false) {
        self.isSpread = isSpread; self.columnWidth = columnWidth; self.gutter = gutter
        self.isShort = isShort; self.isPhone = isPhone
    }
}

/// The reading measure. Every phone the app was drawn for is narrower than the measure, so there nothing changes:
/// 24pt gutters, 28pt on the widest phones. Wider windows hold the column to the measure and widen the margins,
/// and a wide landscape window opens into a spread.
public enum Measure {
    /// The widest a text column runs: about 65–70 characters of Iowan 17.
    public static let text: CGFloat = 560
    /// The narrowest window that opens into two pages: each page then still holds about 400pt of text.
    public static let spreadMinWidth: CGFloat = 860
    /// Below this height a window is short (`PageLayout.isShort`).
    public static let shortHeight: CGFloat = 700

    /// The phone gutter: 24pt, 28pt from 430pt (the Pro Max phones, the closed Duo).
    static func phoneGutter(_ width: CGFloat) -> CGFloat { width >= 430 ? 28 : 24 }

    /// Spread iff the window is at least `spreadMinWidth` wide AND landscape: the inner display turned upright is
    /// regular×regular too, but two 300pt pages there would be cramped, so it gets one capped column.
    public static func layout(for size: CGSize) -> PageLayout {
        let w = max(0, size.width)
        let short = size.height > 0 && size.height < shortHeight && w >= 380
        if w >= spreadMinWidth && w > size.height {
            let g = phoneGutter(w)
            // outer + two half-margins at the spine + outer = three gutters across the window.
            return PageLayout(isSpread: true, columnWidth: min(text, (w - 3 * g) / 2), gutter: g, isShort: short)
        }
        let g = max(phoneGutter(w), (w - text) / 2)
        return PageLayout(isSpread: false, columnWidth: max(0, w - 2 * g), gutter: g, isShort: short,
                          isPhone: !short && w <= 440)
    }
}
