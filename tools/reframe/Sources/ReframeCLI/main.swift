import Foundation
import ReframeKit
import simd

// reframe-cli: headless access to ReframeKit, mainly to check the Metal renderer against render.py.
//
//   reframe-cli info <file.insv>
//   reframe-cli still <file.insv> <seconds> <out.png> [--equirect] [--width N] [--height N]
//                     [--yaw D] [--pitch D] [--hfov D] [--proxy] [--lut file.cube] [--no-level]

func fail(_ msg: String) -> Never {
    FileHandle.standardError.write((msg + "\n").data(using: .utf8)!)
    exit(1)
}

var args = Array(CommandLine.arguments.dropFirst())
guard let command = args.first else { fail("usage: reframe-cli info|still ...") }
args.removeFirst()

func option(_ name: String) -> String? {
    guard let i = args.firstIndex(of: name), i + 1 < args.count else { return nil }
    return args[i + 1]
}
func flag(_ name: String) -> Bool { args.contains(name) }

switch command {
case "info":
    guard let path = args.first else { fail("info <file.insv>") }
    let clip = try Clip(url: URL(fileURLWithPath: path))
    let i = clip.info
    print("camera      \(i.camera)  fw \(i.firmware)  serial \(i.serial)")
    print("profile     \(i.colorLabel)  fps \(i.nominalFPS ?? 0)  duration \(clip.duration)s  timelapse \(i.isTimelapse)")
    print("readout     \(i.rollingShutterMs ?? 0) ms  raw imu \(i.rawIMU)  track order \(i.trackOrder ?? 0)")
    print("proxy       \(clip.proxyURL?.lastPathComponent ?? "none")")
    for (n, lens) in clip.rig.lenses.enumerated() {
        print("lens \(n)      xi \(lens.xi) f \(lens.fx) c (\(lens.cx), \(lens.cy)) type \(lens.lensType) V\(lens.generation)")
    }
    let t0 = Date()
    if let m = clip.motion {
        let s = m.shakiness()
        print(String(format: "motion      %d samples in %.2fs  median %.1f°/s  p95 %.1f°/s",
                     m.times.count, Date().timeIntervalSince(t0), s.median, s.p95))
    }

case "still":
    guard args.count >= 3, let t = Double(args[1]) else { fail("still <file.insv> <seconds> <out.png> [options]") }
    let clip = try Clip(url: URL(fileURLWithPath: args[0]))
    let out = URL(fileURLWithPath: args[2])
    let useProxy = flag("--proxy")
    guard let source = useProxy ? clip.proxyURL : clip.url else { fail("no proxy for this clip") }
    let reader = try await FrameReader(url: source)
    guard let frames = try reader.frame(at: t) else { fail("no frame at \(t)") }
    let renderer = try ReframeRenderer()
    if let lutPath = option("--lut") { renderer.setLUT(try CubeLUT(url: URL(fileURLWithPath: lutPath))) }

    var view = ViewSettings()
    view.projection = flag("--equirect") ? .equirect : .flat
    view.hfov = Double(option("--hfov") ?? "90") ?? 90
    view.applyLUT = option("--lut") != nil
    let yaw = Double(option("--yaw") ?? "0") ?? 0, pitch = Double(option("--pitch") ?? "0") ?? 0
    let user = rot(.z, yaw) * rot(.y, -pitch)
    if flag("--no-level") {
        view.bodyFromCamera = user
    } else if let m = clip.motion {
        // Same as render.py stills: world-levelled, heading from the gyro integration.
        view.bodyFromCamera = simd_double3x3(m.orientation(at: frames.time)).transpose * user
    }
    let width = Int(option("--width") ?? "") ?? (view.projection == .flat ? 1920 : 3840)
    let height = Int(option("--height") ?? "") ?? (view.projection == .flat ? 1080 : width / 2)
    let start = Date()
    guard let image = renderer.renderImage(frames: frames, rig: clip.rig, view: view, width: width, height: height) else {
        fail("render failed")
    }
    try writePNG(image, to: out)
    print(String(format: "wrote %@ %dx%d (frame %.3fs, render %.0f ms, %@)", out.path, width, height, frames.time,
                 Date().timeIntervalSince(start) * 1000, useProxy ? "proxy" : "master"))

case "export":
    // reframe-cli export <file.insv> <in> <out> <dest-dir> [--yaw --pitch --hfov --stab follow|lock|none
    //                    --size hd|uhd|vertical|square --codec proRes422HQ --name N --bake-lut file.cube]
    guard args.count >= 4, let tIn = Double(args[1]), let tOut = Double(args[2]) else {
        fail("export <file.insv> <in> <out> <dest-dir> [options]")
    }
    let clip = try Clip(url: URL(fileURLWithPath: args[0]))
    var options = ExportOptions(destination: URL(fileURLWithPath: args[3]))
    options.size = ExportOptions.Size(rawValue: option("--size") ?? "hd") ?? .hd
    options.codec = ExportOptions.Codec(rawValue: option("--codec") ?? "proRes422HQ") ?? .proRes422HQ
    var lut: CubeLUT?
    if let path = option("--bake-lut") { lut = try CubeLUT(url: URL(fileURLWithPath: path)); options.bakeLook = true }
    let framing = Framing(yaw: Double(option("--yaw") ?? "0") ?? 0, pitch: Double(option("--pitch") ?? "0") ?? 0,
                          hfov: Double(option("--hfov") ?? "100") ?? 100)
    let shot = Shot(clipPath: clip.url.path, name: option("--name") ?? "\(clip.name) shot", inTime: tIn, outTime: tOut,
                    framing: framing, stabilization: Motion.Stabilization(rawValue: option("--stab") ?? "follow") ?? .follow,
                    smoothing: 0.8, lockTime: tIn)
    let exporter = try ShotExporter(lut: lut)
    let start = Date()
    let result = try await exporter.export(shot, clip: clip, options: options, progress: { _ in }, isCancelled: { false })
    let secs = Date().timeIntervalSince(start)
    print(String(format: "wrote %@: %d frames in %.1fs (%.1f fps), timecode frame %d, audio %@",
                 result.url.lastPathComponent, result.frameCount, secs, Double(result.frameCount) / secs,
                 result.timecodeFrames, result.hasAudio ? "yes" : "no"))
    let dest = URL(fileURLWithPath: args[3])
    try Interchange.fcpxml([result], title: "Reframe selects").write(to: dest.appendingPathComponent("Reframe selects.fcpxml"),
                                                                   atomically: true, encoding: .utf8)
    try Interchange.otio([result], title: "Reframe selects").write(to: dest.appendingPathComponent("Reframe selects.otio"))
    print("wrote timelines to \(dest.path)")

default:
    fail("unknown command \(command)")
}
