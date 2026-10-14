import Foundation

public enum UnitFraction {
    /// The nearest unit fraction 1/n with n = round(1/x), as "1⁄6" (U+2044 fraction slash).
    /// x ≥ ~0.67 renders "all"; x ≤ 0 renders "0".
    public static func render(_ x: Double) -> String {
        guard x.isFinite, x > 0 else { return "0" }
        let n = Int((1 / x).rounded())
        if n <= 1 { return "all" }
        return "1\u{2044}\(n)"
    }

    public static func denominator(_ x: Double) -> Int? {
        guard x.isFinite, x > 0 else { return nil }
        return max(1, Int((1 / x).rounded()))
    }
}
