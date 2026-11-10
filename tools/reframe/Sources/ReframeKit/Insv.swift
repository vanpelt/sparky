import Foundation
import simd

/// Reader for Insta360 .insv trailers (ExtraInfo v3): metadata, lens calibration, IMU.
/// Swift port of insv.py; see README for the X6-specific findings.

public enum InsvError: Error {
    case noSignature
    case unsupportedVersion(UInt32)
    case noInstBox
    case noMetadata
    case badCalibration(String)
}

public struct InsvRecord: Sendable {
    public let id: UInt8
    public let format: UInt8
    public let offset: UInt64
    public let length: Int
}

// MARK: - Binary helpers

extension Data {
    func le<T: FixedWidthInteger>(_ offset: Int, as: T.Type = T.self) -> T {
        withUnsafeBytes { T(littleEndian: $0.loadUnaligned(fromByteOffset: offset, as: T.self)) }
    }

    func be32(_ offset: Int) -> UInt32 {
        withUnsafeBytes { UInt32(bigEndian: $0.loadUnaligned(fromByteOffset: offset, as: UInt32.self)) }
    }

    func f64(_ offset: Int) -> Double {
        Double(bitPattern: le(offset, as: UInt64.self))
    }
}

/// Random access to a file's bytes: local (`BinaryFile`) or remote (`RemoteFile`, HTTP ranges).
public protocol ByteSource: AnyObject {
    var size: UInt64 { get }
    func read(at offset: UInt64, count: Int) throws -> Data
}

final class BinaryFile: ByteSource {
    let handle: FileHandle
    let size: UInt64

    init(_ url: URL) throws {
        handle = try FileHandle(forReadingFrom: url)
        size = try handle.seekToEnd()
    }

    deinit { try? handle.close() }

    func read(at offset: UInt64, count: Int) throws -> Data {
        try handle.seek(toOffset: offset)
        return try handle.read(upToCount: count) ?? Data()
    }
}

// MARK: - Trailer

private let signature = Data("8db42d694ccc418790edff439fe026bf".utf8)

func readRecords(_ file: ByteSource) throws -> [InsvRecord] {
    let size = file.size
    let term = try file.read(at: size - 72, count: 72)
    guard term.count == 72, term.subdata(in: 40..<72) == signature else { throw InsvError.noSignature }
    let extra = UInt64(term.le(32, as: UInt32.self))
    let version: UInt32 = term.le(36)
    guard version == 3 else { throw InsvError.unsupportedVersion(version) }
    let payloadStart = size - extra
    let terminalStart = size - 72

    // X6 files carry a few zero bytes between the inst header and the payload.
    let head = try file.read(at: payloadStart - 32, count: 32)
    guard let at = head.range(of: Data("inst".utf8), options: .backwards)?.lowerBound, at - head.startIndex >= 4 else {
        throw InsvError.noInstBox
    }
    let rel = at - head.startIndex
    let boxStart = payloadStart - 32 + UInt64(rel) - 4
    guard UInt64(head.be32(rel - 4)) == size - boxStart else { throw InsvError.noInstBox }

    let footer = try file.read(at: terminalStart - 6, count: 6)
    let fmt = footer[footer.startIndex], rid = footer[footer.startIndex + 1]
    let len = UInt64(footer.le(2, as: UInt32.self))
    var records: [InsvRecord] = []
    if fmt == 0 && rid == 0 {
        let dir = try file.read(at: terminalStart - 6 - len, count: Int(len))
        for i in stride(from: 0, to: dir.count, by: 10) {
            let eid = dir[dir.startIndex + i], efmt = dir[dir.startIndex + i + 1]
            let elen: UInt32 = dir.le(i + 2), eoff: UInt32 = dir.le(i + 6)
            if eid == 0 && efmt == 0 && elen == 0 && eoff == 0 { continue }
            records.append(InsvRecord(id: eid, format: efmt, offset: payloadStart + UInt64(eoff), length: Int(elen)))
        }
    } else {
        var cursor = terminalStart
        while cursor > payloadStart {
            let f = try file.read(at: cursor - 6, count: 6)
            let l = UInt64(f.le(2, as: UInt32.self))
            let start = cursor - 6 - l
            records.append(InsvRecord(id: f[f.startIndex + 1], format: f[f.startIndex], offset: start, length: Int(l)))
            cursor = start
        }
        records.reverse()
    }
    return records
}

// MARK: - Protobuf (schema-less)

enum PBValue {
    case varint(UInt64)
    case fixed64(Double)
    case bytes(Data)
    case fixed32(Float)

    var int: Int? { if case .varint(let v) = self { return Int(truncatingIfNeeded: v) }; return nil }
    var double: Double? {
        switch self {
        case .fixed64(let d): return d
        case .fixed32(let f): return Double(f)
        case .varint(let v): return Double(v)
        default: return nil
        }
    }
    var data: Data? { if case .bytes(let d) = self { return d }; return nil }
    var string: String? {
        data.map { String(decoding: $0, as: UTF8.self).trimmingCharacters(in: CharacterSet(charactersIn: "\0 ")) }
    }
}

func decodeProtobuf(_ buf: Data) -> [Int: [PBValue]] {
    var out: [Int: [PBValue]] = [:]
    let bytes = [UInt8](buf)
    var i = 0
    func varint() -> UInt64 {
        var result: UInt64 = 0, shift: UInt64 = 0
        while i < bytes.count {
            let b = bytes[i]; i += 1
            result |= UInt64(b & 0x7F) << shift
            if b & 0x80 == 0 { break }
            shift += 7
        }
        return result
    }
    while i < bytes.count {
        let key = varint()
        let num = Int(key >> 3), wire = key & 7
        let value: PBValue
        switch wire {
        case 0: value = .varint(varint())
        case 1:
            guard i + 8 <= bytes.count else { return out }
            value = .fixed64(Data(bytes[i..<i + 8]).f64(0)); i += 8
        case 2:
            let n = Int(varint())
            guard i + n <= bytes.count else { return out }
            value = .bytes(Data(bytes[i..<i + n])); i += n
        case 5:
            guard i + 4 <= bytes.count else { return out }
            value = .fixed32(Float(bitPattern: Data(bytes[i..<i + 4]).le(0, as: UInt32.self))); i += 4
        default:
            return out
        }
        out[num, default: []].append(value)
    }
    return out
}

// MARK: - Calibration

public struct Lens: Sendable {
    public var xi, fx, fy, cx, cy: Double
    public var ex, ey, ez: Double
    /// V6 layout: k1...k5, p1...p4, s1...s4. V3 calibrations are mapped onto it.
    public var distortion: [Double]
    public var canvasWidth: Double  // both lenses side by side
    public var canvasHeight: Double
    public var lensType: Int
    public var generation: Int

    /// Body -> lens rotation (lens optical axis +Z). Body: +X forward, +Y left, +Z up.
    public func rotation(index: Int) -> simd_double3x3 {
        let r = rot(.z, ez) * rot(.y, ey + 90) * rot(.x, ex)
        return index == 1 ? rot(.y, 180) * r : r
    }
}

public enum Axis { case x, y, z }

public func rot(_ axis: Axis, _ degrees: Double) -> simd_double3x3 {
    let c = cos(degrees * .pi / 180), s = sin(degrees * .pi / 180)
    switch axis {
    case .x: return simd_double3x3(rows: [[1, 0, 0], [0, c, -s], [0, s, c]])
    case .y: return simd_double3x3(rows: [[c, 0, s], [0, 1, 0], [-s, 0, c]])
    case .z: return simd_double3x3(rows: [[c, -s, 0], [s, c, 0], [0, 0, 1]])
    }
}

public func parseCalibration(_ s: String) throws -> [Lens] {
    let toks = s.trimmingCharacters(in: CharacterSet(charactersIn: "\0 ")).split(separator: "_").map(String.init)
    guard toks.first == "2" else { throw InsvError.badCalibration("expected 2 lenses") }
    let per: Int, gen: Int
    switch toks.count {
    case 56: per = 27; gen = 6
    case 40: per = 19; gen = 3
    default: throw InsvError.badCalibration("unsupported token count \(toks.count)")
    }
    guard let packed = Double(toks.last!), Int(packed) >> 16 == gen else {
        throw InsvError.badCalibration("packed generation mismatch")
    }
    var lenses: [Lens] = []
    for li in 0..<2 {
        let v = toks[(1 + li * per)..<(1 + (li + 1) * per)].map { Double($0) ?? .nan }
        let dist: [Double]
        if gen == 6 {
            dist = Array(v[11..<24])
        } else {
            // V3 radtan: x += 2*p1*xy + p2*(r2+2x2). In V6 form that is p1' = p2, p2' = p1.
            dist = [v[11], v[12], v[13], 0, 0, v[15], v[14], 0, 0, 0, 0, 0, 0]
        }
        lenses.append(Lens(xi: v[0], fx: v[1], fy: v[2], cx: v[3], cy: v[4], ex: v[5], ey: v[6], ez: v[7],
                           distortion: dist, canvasWidth: v[per - 3], canvasHeight: v[per - 2],
                           lensType: Int(v[per - 1]), generation: gen))
    }
    return lenses
}

// MARK: - Metadata

public struct InsvInfo: Sendable {
    public var camera = ""
    public var serial = ""
    public var firmware = ""
    /// "I_Log", "standard", "Dolby_Vision", ...
    public var colorProfile = ""
    public var captureTime = ""
    public var groupType = 0
    public var nominalFPS: Int?
    public var durationMs: Int?
    public var frameCount: Int?
    public var rollingShutterMs: Double?
    public var rawIMU = false
    public var imuRanges: (accel: Double, gyro: Double)?
    public var trackOrder: Int?
    public var firstFrameTimestamp: Int?
    public var timelapseIntervalMs: Int?
    public var calibration = ""
    public var records: [InsvRecord] = []

    public var isTimelapse: Bool { groupType == 2 || groupType == 8 || timelapseIntervalMs != nil }

    public var colorLabel: String {
        switch colorProfile {
        case "I_Log": return "I-Log"
        case "Dolby_Vision": return "Dolby Vision"
        case "standard", "": return "Standard"
        default: return colorProfile.replacingOccurrences(of: "_", with: " ")
        }
    }

    public var lenses: [Lens]? { try? parseCalibration(calibration) }
}

public struct IMUData: Sendable {
    /// Camera-clock seconds.
    public var time: [Double]
    /// Specific force (g) and angular velocity (rad/s), in raw IMU axes.
    public var accel: [SIMD3<Double>]
    public var gyro: [SIMD3<Double>]
}

public final class InsvFile: @unchecked Sendable {
    public let url: URL
    public let info: InsvInfo
    private let file: ByteSource

    public convenience init(url: URL) throws {
        try self.init(url: url, source: BinaryFile(url))
    }

    /// Reads the trailer from any byte source (e.g. a cloud original via HTTP ranges); `url` is
    /// only an identifier then.
    public init(url: URL, source: ByteSource) throws {
        self.url = url
        file = source
        let records = try readRecords(file)
        guard let meta = records.first(where: { $0.id == 1 && $0.format == 1 }) else { throw InsvError.noMetadata }
        let m = decodeProtobuf(try file.read(at: meta.offset, count: meta.length))
        func first(_ k: Int) -> PBValue? { m[k]?.first }
        var info = InsvInfo()
        info.records = records
        info.serial = first(1)?.string ?? ""
        info.camera = first(2)?.string ?? ""
        info.firmware = first(3)?.string ?? ""
        info.colorProfile = first(22)?.string ?? ""
        info.captureTime = first(7)?.int.map(String.init) ?? ""
        info.nominalFPS = first(20)?.int
        info.firstFrameTimestamp = first(24)?.int
        info.rollingShutterMs = first(25)?.double
        info.rawIMU = (first(62)?.int ?? 0) != 0
        info.trackOrder = first(80)?.int
        info.timelapseIntervalMs = first(133)?.int
        info.frameCount = first(40)?.int
        info.durationMs = first(93)?.int
        if let g = first(26)?.data { info.groupType = decodeProtobuf(g)[1]?.first?.int ?? 0 }
        if let r = first(65)?.data {
            let rr = decodeProtobuf(r)
            info.imuRanges = (Double(rr[1]?.first?.int ?? 8), Double(rr[2]?.first?.int ?? 2000))
        }
        for k in [111, 112, 54, 56, 53, 55] {
            if let s = first(k)?.string, !s.isEmpty { info.calibration = s; break }
        }
        self.info = info
    }

    public func readIMU() throws -> IMUData? {
        guard let rec = info.records.first(where: { $0.id == 3 }) else { return nil }
        let buf = try file.read(at: rec.offset, count: rec.length)
        var t: [Double] = [], acc: [SIMD3<Double>] = [], gyr: [SIMD3<Double>] = []
        if info.rawIMU {
            let n = buf.count / 20
            t.reserveCapacity(n); acc.reserveCapacity(n); gyr.reserveCapacity(n)
            let (ar, gr) = info.imuRanges ?? (8, 2000)
            buf.withUnsafeBytes { p in
                for i in 0..<n {
                    let o = i * 20
                    let ts = Int64(littleEndian: p.loadUnaligned(fromByteOffset: o, as: Int64.self))
                    func axis(_ k: Int) -> Double {
                        Double(UInt16(littleEndian: p.loadUnaligned(fromByteOffset: o + 8 + 2 * k, as: UInt16.self))) - 32768
                    }
                    t.append(Double(ts) / 1e6)
                    acc.append(SIMD3(axis(0), axis(1), axis(2)) * (ar / 32768))
                    gyr.append(SIMD3(axis(3), axis(4), axis(5)) * (gr / 32768 * .pi / 180))
                }
            }
        } else {
            let n = buf.count / 56
            buf.withUnsafeBytes { p in
                for i in 0..<n {
                    let o = i * 56
                    func f(_ k: Int) -> Double {
                        Double(bitPattern: UInt64(littleEndian: p.loadUnaligned(fromByteOffset: o + 8 + 8 * k, as: UInt64.self)))
                    }
                    t.append(Double(Int64(littleEndian: p.loadUnaligned(fromByteOffset: o, as: Int64.self))) / 1e3)
                    acc.append(SIMD3(f(0), f(1), f(2)))
                    gyr.append(SIMD3(f(3), f(4), f(5)))
                }
            }
        }
        return IMUData(time: t, accel: acc, gyro: gyr)
    }

    /// Embedded preview image (record 0x02), if present.
    public func thumbnailData() -> Data? {
        guard let rec = info.records.first(where: { $0.id == 2 }) else { return nil }
        return try? file.read(at: rec.offset, count: rec.length)
    }
}
