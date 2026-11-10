import Foundation

/// Interest-map channels from a clip's MobileCLIP index (SEMANTIC.md, "Better suggestions"):
/// - `appeal`: which way is worth looking, judged zero-shot: how much more a view resembles
///   "things people film" prompts than "junk" prompts, relative to the other headings at that
///   moment (so a dull stretch still has a best direction and a great one doesn't flood every
///   heading). Things fixed to the camera (your own face on the stick, a passenger) are discounted
///   like faces are.
/// - `dull`: views that look like the junk prompts outright (the vehicle's interior, the ground,
///   a dashboard). A penalty, *not* discounted when it's always there: the roll cage being in
///   every frame is exactly why the camera shouldn't drift onto it.
/// Both are cheap to derive (two matrix-vector products over the mapped index), so they're
/// computed whenever an index exists, including for analyses made before it did.
public enum SemanticSignals {
    /// Bump when the prompts or the normalization change: analyses recompute these channels.
    public static let version = 1

    static let positive = [
        "a beautiful landscape", "a scenic view of mountains or canyons", "friends smiling at the camera",
        "a group of people posing for a photo", "a person riding a bike", "a vehicle driving off-road",
        "a waterfall", "people hiking", "a person laughing", "a dramatic sky",
    ]
    static let negative = [
        "the inside of a car", "the ground", "a blurry photo", "a close-up of a person's leg",
        "a hand holding a camera", "the roof of a vehicle", "an empty blue sky", "a dashboard",
        "a backpack", "a seat belt",
    ]

    /// Appeal above the moment's median heading, in cosine units, that counts as fully appealing.
    static let appealSpread: Float = 0.04
    /// Junk-minus-good margin (cosine) where a view starts to count as dull, and its full scale.
    static let dullFloor: Float = 0.0
    static let dullSpread: Float = 0.04

    nonisolated(unsafe) private static var prompts: (pos: [Float], neg: [Float])?
    private static let lock = NSLock()

    /// Unit means of each prompt set's text embeddings, computed once per process.
    public static func promptVectors(_ encoder: CLIPEncoder) throws -> (pos: [Float], neg: [Float]) {
        if let p = lock.withLock({ prompts }) { return p }
        func mean(_ list: [String]) throws -> [Float] {
            var m = [Float](repeating: 0, count: CLIPEncoder.dimension)
            for p in list { for (i, x) in try encoder.embed(text: p).enumerated() { m[i] += x } }
            let n = max(sqrt(m.reduce(0) { $0 + $1 * $1 }), 1e-6)
            return m.map { $0 / n }
        }
        let p = (try mean(positive), try mean(negative))
        lock.withLock { prompts = p }
        return p
    }

    /// Good-minus-junk cosine margin per (sample, view), [time][view].
    public static func margin(_ index: SemanticIndex, encoder: CLIPEncoder) throws -> [Float] {
        let (pos, neg) = try promptVectors(encoder)
        return zip(index.similarities(pos), index.similarities(neg)).map { $0 - $1 }
    }

    /// Adds `.appeal` and `.dull` to a heading grid. `forward` is where the camera faced per cell.
    public static func apply(_ index: SemanticIndex, to grid: inout HeadingGrid, forward: [Float],
                             encoder: CLIPEncoder) throws {
        let d = try margin(index, encoder: encoder)
        guard !d.isEmpty else { return }
        let V = index.viewCount, T = index.times.count
        var appeal = [Float](repeating: 0, count: d.count), dull = appeal
        for t in 0..<T {
            let row = Array(d[(t * V)..<((t + 1) * V)])
            let median = percentile(row, 0.5)
            for v in 0..<V {
                let x = row[v]
                appeal[t * V + v] = clamp01((x - median) / appealSpread)
                dull[t * V + v] = clamp01((dullFloor - x) / dullSpread)
            }
        }
        let a = index.headingChannel(appeal, rate: grid.rate, count: grid.count)
        grid[.appeal] = DirectionalAnalysis.persistentDamped(a, forward: forward, grid: grid)
        grid[.dull] = index.headingChannel(dull, rate: grid.rate, count: grid.count)
    }
}

extension ClipAnalysis {
    /// Whether the appeal channels are present and current.
    public var hasSemanticSignals: Bool { semanticVersion == SemanticSignals.version && heading?.channels[.appeal] != nil }

    /// This analysis with `.appeal` and `.dull` from `index` and suggestions re-ranked.
    public func addingSemanticSignals(_ index: SemanticIndex, encoder: CLIPEncoder) throws -> ClipAnalysis {
        guard var grid = heading else { return self }
        try SemanticSignals.apply(index, to: &grid, forward: forward, encoder: encoder)
        var a = self
        a.heading = grid
        a.semanticVersion = SemanticSignals.version
        return a.reweighted(weights)
    }
}
