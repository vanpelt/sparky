import CoreGraphics
import Testing
@testable import VersoCore

/// The reading measure across every window Verso can find itself in: today's phones must not move a point,
/// and the Duo's inner display opens into a spread only when it is wide AND landscape.
struct LayoutTests {
    struct Case: Sendable, CustomTestStringConvertible {
        let name: String
        let size: CGSize
        let spread: Bool
        let gutter: CGFloat
        let column: CGFloat
        var short = false
        var phone = false
        var testDescription: String { name }
    }

    static let cases: [Case] = [
        Case(name: "iPhone 17 Pro", size: CGSize(width: 402, height: 874), spread: false, gutter: 24, column: 354, phone: true),
        Case(name: "iPhone 17 Pro Max", size: CGSize(width: 440, height: 956), spread: false, gutter: 28, column: 384, phone: true),
        Case(name: "Duo closed", size: CGSize(width: 466, height: 678), spread: false, gutter: 28, column: 410, short: true),
        Case(name: "Duo closed, 27.0 build", size: CGSize(width: 386, height: 678), spread: false, gutter: 24, column: 338, short: true),
        Case(name: "Duo open", size: CGSize(width: 951, height: 669), spread: true, gutter: 28, column: 433.5, short: true),
        Case(name: "Duo open, 27.0 build", size: CGSize(width: 871, height: 669), spread: true, gutter: 28, column: 393.5, short: true),
        Case(name: "Duo inner, portrait", size: CGSize(width: 669, height: 951), spread: false, gutter: 54.5, column: 560),
        Case(name: "Split View half", size: CGSize(width: 475, height: 669), spread: false, gutter: 28, column: 419, short: true),
        Case(name: "iPad mini, landscape", size: CGSize(width: 1133, height: 744), spread: true, gutter: 28, column: 524.5),
        Case(name: "iPad mini, portrait", size: CGSize(width: 744, height: 1133), spread: false, gutter: 92, column: 560),
        Case(name: "iPad Pro 13, landscape", size: CGSize(width: 1376, height: 1032), spread: true, gutter: 28, column: 560),
    ]

    @Test(arguments: cases) func table(_ c: Case) {
        let l = Measure.layout(for: c.size)
        #expect(l.isSpread == c.spread)
        #expect(l.gutter == c.gutter)
        #expect(l.columnWidth == c.column)
        #expect(l.isShort == c.short)
        #expect(l.isPhone == c.phone)
    }

    /// Every phone Verso was drawn for is a phone column, an iPhone SE included (short, but under 380 wide).
    @Test func phonesStayPhones() {
        for size in [CGSize(width: 375, height: 667), CGSize(width: 375, height: 812), CGSize(width: 393, height: 852),
                     CGSize(width: 402, height: 874), CGSize(width: 430, height: 932), CGSize(width: 440, height: 956)] {
            let l = Measure.layout(for: size)
            #expect(l.isPhone, "\(size)")
            #expect(!l.isShort, "\(size)")
        }
    }

    /// Every width the old `Gutter` covered (up to where the measure starts to bind) keeps its old margin.
    @Test func phonesUnchanged() {
        for w in stride(from: CGFloat(320), through: 616, by: 1) {
            let old: CGFloat = w >= 430 ? 28 : 24
            let l = Measure.layout(for: CGSize(width: w, height: 874))
            #expect(!l.isSpread)
            #expect(l.gutter == old, "width \(w)")
            #expect(l.columnWidth == w - 2 * old, "width \(w)")
        }
    }

    @Test func columnNeverPassesTheMeasure() {
        for w in stride(from: CGFloat(320), through: 2000, by: 7) {
            for h in [CGFloat(600), 1400] {
                #expect(Measure.layout(for: CGSize(width: w, height: h)).columnWidth <= Measure.text)
            }
        }
    }

    @Test func spreadThreshold() {
        #expect(Measure.layout(for: CGSize(width: 860, height: 700)).isSpread)
        #expect(!Measure.layout(for: CGSize(width: 859, height: 700)).isSpread)
        // Square is not landscape: one column.
        #expect(!Measure.layout(for: CGSize(width: 900, height: 900)).isSpread)
    }

    /// Before the first layout pass the window can be zero: nothing negative comes back.
    @Test func degenerateSizes() {
        let l = Measure.layout(for: .zero)
        #expect(!l.isSpread)
        #expect(l.gutter == 24)
        #expect(l.columnWidth == 0)
    }
}
