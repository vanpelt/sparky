import Foundation
import Metal

/// A 3D .cube LUT (Adobe/Resolve format), e.g. Insta360's X6 I-Log -> Rec.709 LUTs.
public struct CubeLUT: Sendable {
    public let title: String
    public let size: Int
    /// RGB triples, red varying fastest, then green, then blue.
    public let values: [Float]
    public let url: URL

    public init(url: URL) throws {
        self.url = url
        let text = try String(contentsOf: url, encoding: .utf8)
        var size = 0, title = url.deletingPathExtension().lastPathComponent
        var values: [Float] = []
        values.reserveCapacity(65 * 65 * 65 * 3)
        for line in text.split(whereSeparator: \.isNewline) {
            let l = line.trimmingCharacters(in: .whitespaces)
            if l.isEmpty || l.hasPrefix("#") { continue }
            if l.hasPrefix("TITLE") {
                title = l.dropFirst(5).trimmingCharacters(in: CharacterSet(charactersIn: " \""))
            } else if l.hasPrefix("LUT_3D_SIZE") {
                size = Int(l.split(separator: " ").last ?? "") ?? 0
            } else if let first = l.first, first.isNumber || first == "-" || first == "." {
                for p in l.split(separator: " ") { if let v = Float(p) { values.append(v) } }
            }
        }
        guard size > 1, values.count == size * size * size * 3 else {
            throw NSError(domain: "CubeLUT", code: 1, userInfo: [NSLocalizedDescriptionKey: "bad LUT \(url.lastPathComponent)"])
        }
        self.size = size
        self.title = title
        self.values = values
    }

    /// A short label from Insta360's file naming, e.g. "Rec.709", "HLG", "PQ".
    public var shortName: String { CubeLUT.shortName(url) }

    public static func shortName(_ url: URL) -> String {
        let n = url.lastPathComponent
        let size = n.contains("s65") ? " (65)" : n.contains("s33") ? " (33)" : ""
        if n.contains("Rec709") { return "Rec.709" + size }
        if n.contains("HLG") { return "HLG" + size }
        if n.contains("St2084") { return "PQ" + size }
        return url.deletingPathExtension().lastPathComponent
    }

    func makeTexture(device: MTLDevice) -> MTLTexture? {
        let d = MTLTextureDescriptor()
        d.textureType = .type3D
        d.pixelFormat = .rgba32Float
        d.width = size; d.height = size; d.depth = size
        d.usage = .shaderRead
        guard let tex = device.makeTexture(descriptor: d) else { return nil }
        var rgba = [Float](repeating: 1, count: size * size * size * 4)
        for i in 0..<(size * size * size) {
            rgba[i * 4] = values[i * 3]; rgba[i * 4 + 1] = values[i * 3 + 1]; rgba[i * 4 + 2] = values[i * 3 + 2]
        }
        rgba.withUnsafeBytes { p in
            tex.replace(region: MTLRegionMake3D(0, 0, 0, size, size, size), mipmapLevel: 0, slice: 0,
                        withBytes: p.baseAddress!, bytesPerRow: size * 16, bytesPerImage: size * size * 16)
        }
        return tex
    }

    /// LUTs in a folder, Rec.709 first (the one that suits an SDR preview).
    public static func find(in folder: URL) -> [URL] {
        let files = (try? FileManager.default.contentsOfDirectory(at: folder, includingPropertiesForKeys: nil)) ?? []
        return files.filter { $0.pathExtension.lowercased() == "cube" }
            .sorted { a, b in
                let ra = a.lastPathComponent.contains("Rec709"), rb = b.lastPathComponent.contains("Rec709")
                if ra != rb { return ra }
                let sa = a.lastPathComponent.contains("s65"), sb = b.lastPathComponent.contains("s65")
                if sa != sb { return sa }
                return a.lastPathComponent < b.lastPathComponent
            }
    }
}
