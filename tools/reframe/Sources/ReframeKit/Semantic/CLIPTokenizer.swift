import Foundation

/// OpenAI CLIP's byte-level BPE tokenizer (the one MobileCLIP's text encoder was trained with),
/// read from Hugging Face's `vocab.json` and `merges.txt` for openai/clip-vit-base-patch32.
/// Lowercases and collapses whitespace like CLIP's `whitespace_clean`; skips ftfy (queries are typed).
public final class CLIPTokenizer: @unchecked Sendable {
    public static let startToken: Int32 = 49406
    public static let endToken: Int32 = 49407

    private let encoder: [String: Int32]
    private let ranks: [String: Int]  // "a b" -> merge rank
    private let byteEncoder: [Character]
    private var cache: [String: [String]] = [:]
    private let lock = NSLock()
    private let pattern = try! NSRegularExpression(
        pattern: #"<\|startoftext\|>|<\|endoftext\|>|'s|'t|'re|'ve|'m|'ll|'d|[\p{L}]+|[\p{N}]|[^\s\p{L}\p{N}]+"#,
        options: [.caseInsensitive])

    public init(vocab: URL, merges: URL) throws {
        encoder = try JSONDecoder().decode([String: Int32].self, from: Data(contentsOf: vocab))
        let lines = try String(contentsOf: merges, encoding: .utf8).split(separator: "\n", omittingEmptySubsequences: true)
        var r: [String: Int] = [:]
        for (i, line) in lines.dropFirst().enumerated() { r[String(line)] = i }  // first line is "#version"
        ranks = r
        // bytes_to_unicode: printable bytes map to themselves, the rest to U+0100 onwards.
        var bs = Array(33...126) + Array(161...172) + Array(174...255)
        var cs = bs
        var n = 0
        for b in 0..<256 where !bs.contains(b) { bs.append(b); cs.append(256 + n); n += 1 }
        var table = [Character](repeating: " ", count: 256)
        for (b, c) in zip(bs, cs) { table[b] = Character(UnicodeScalar(c)!) }
        byteEncoder = table
    }

    /// Token ids padded with zeros to `length` (start and end tokens included, truncated to fit).
    public func encode(_ text: String, length: Int = 77) -> [Int32] {
        let clean = text.lowercased().split(whereSeparator: \.isWhitespace).joined(separator: " ")
        var ids: [Int32] = [CLIPTokenizer.startToken]
        let ns = clean as NSString
        for m in pattern.matches(in: clean, range: NSRange(location: 0, length: ns.length)) {
            let token = String(ns.substring(with: m.range).utf8.map { byteEncoder[Int($0)] })
            for piece in bpe(token) { if let id = encoder[piece] { ids.append(id) } }
        }
        ids = Array(ids.prefix(length - 1)) + [CLIPTokenizer.endToken]
        return ids + [Int32](repeating: 0, count: length - ids.count)
    }

    private func bpe(_ token: String) -> [String] {
        if let c = lock.withLock({ cache[token] }) { return c }
        var word = token.map { String($0) }
        guard !word.isEmpty else { return [] }
        word[word.count - 1] += "</w>"
        while word.count > 1 {
            var best: (rank: Int, i: Int)?
            for i in 0..<(word.count - 1) {
                if let r = ranks[word[i] + " " + word[i + 1]], r < (best?.rank ?? .max) { best = (r, i) }
            }
            guard let (_, _) = best else { break }
            let first = word[best!.i], second = word[best!.i + 1]
            // Merge every occurrence of this pair, left to right.
            var merged: [String] = []
            var i = 0
            while i < word.count {
                if i < word.count - 1, word[i] == first, word[i + 1] == second {
                    merged.append(first + second); i += 2
                } else {
                    merged.append(word[i]); i += 1
                }
            }
            word = merged
        }
        lock.withLock { cache[token] = word }
        return word
    }
}
