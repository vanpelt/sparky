import Foundation

public enum Roman {
    /// 1 → "I", 13 → "XIII". Non-positive values render as "—".
    public static func numeral(_ n: Int) -> String {
        guard n > 0 && n < 4000 else { return "—" }
        let table: [(Int, String)] = [(1000, "M"), (900, "CM"), (500, "D"), (400, "CD"), (100, "C"), (90, "XC"),
                                      (50, "L"), (40, "XL"), (10, "X"), (9, "IX"), (5, "V"), (4, "IV"), (1, "I")]
        var n = n, out = ""
        for (v, s) in table { while n >= v { out += s; n -= v } }
        return out
    }

    /// Chapter numeral for a 0-based tranche index.
    public static func chapter(_ trancheIndex: Int) -> String { numeral(trancheIndex + 1) }

    /// Spelled-out number for Siri: 6 → "Six". Covers 0…20, falls back to digits.
    public static func spelled(_ n: Int) -> String {
        let words = ["Zero", "One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight", "Nine", "Ten", "Eleven",
                     "Twelve", "Thirteen", "Fourteen", "Fifteen", "Sixteen", "Seventeen", "Eighteen", "Nineteen", "Twenty"]
        return n >= 0 && n < words.count ? words[n] : "\(n)"
    }
}
