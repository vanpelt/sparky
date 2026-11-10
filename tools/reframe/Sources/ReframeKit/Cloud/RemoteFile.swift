import Foundation

/// A cloud file read through HTTP range requests, in cached 1 MB blocks, so the .insv trailer
/// (metadata, calibration, gyro) can be parsed without downloading the video.
public final class RemoteFile: ByteSource, @unchecked Sendable {
    public let url: URL
    public let size: UInt64
    public private(set) var bytesFetched = 0
    public private(set) var requests = 0
    private let block: UInt64
    private var cache: [UInt64: Data] = [:]
    private let session = URLSession(configuration: .ephemeral)
    private let lock = NSLock()

    public init(url: URL, size: UInt64, blockSize: Int = 1 << 20) {
        self.url = url
        self.size = size
        block = UInt64(blockSize)
    }

    public func read(at offset: UInt64, count: Int) throws -> Data {
        guard count > 0, offset < size else { return Data() }
        let end = min(offset + UInt64(count), size)
        let first = offset / block, last = (end - 1) / block
        // Fetch missing blocks as one contiguous request.
        let missing = (first...last).filter { cache[$0] == nil }
        if let a = missing.first, let b = missing.last {
            let data = try fetch(from: a * block, to: min((b + 1) * block, size) - 1)
            for i in a...b {
                let lo = Int((i - a) * block), hi = min(lo + Int(block), data.count)
                if lo < hi { cache[i] = data.subdata(in: lo..<hi) }
            }
        }
        var out = Data(capacity: Int(end - offset))
        for i in first...last {
            guard let d = cache[i] else { throw CloudError.badResponse("short read at block \(i)") }
            let base = i * block
            let lo = Int(max(offset, base) - base), hi = Int(min(end, base + UInt64(d.count)) - base)
            if lo < hi { out.append(d.subdata(in: lo..<hi)) }
        }
        return out
    }

    private func fetch(from: UInt64, to: UInt64) throws -> Data {
        var req = URLRequest(url: url)
        req.setValue("bytes=\(from)-\(to)", forHTTPHeaderField: "Range")
        req.timeoutInterval = 120
        let sem = DispatchSemaphore(value: 0)
        var result: Result<Data, Error> = .failure(CloudError.badResponse("no response"))
        session.dataTask(with: req) { data, resp, err in
            if let err { result = .failure(err) } else {
                let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
                if status == 206, let data { result = .success(data) }
                else { result = .failure(CloudError.http(status, "range \(from)-\(to)")) }
            }
            sem.signal()
        }.resume()
        sem.wait()
        let d = try result.get()
        lock.lock(); bytesFetched += d.count; requests += 1; lock.unlock()
        return d
    }
}
