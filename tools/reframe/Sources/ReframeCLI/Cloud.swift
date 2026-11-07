import AppKit
import Foundation
import ReframeKit

// reframe-cli cloud …: the Insta360 Cloud spike (CLOUD.md). Never prints the session token or
// signed URL queries.

func runCloud(_ args: [String]) async {
    let sub = args.first ?? "status"
    let rest = Array(args.dropFirst())
    func say(_ s: String) { print(s); fflush(stdout) }
    do {
        switch sub {
        case "login":
            cloudLogin()

        case "logout":
            CloudSession.delete()
            say("signed out (removed \(CloudSession.fileURL.path))")

        case "status":
            guard let s = CloudSession.load() else { throw CloudError.notSignedIn }
            say("session file: \(CloudSession.fileURL.path)")
            if let e = s.expires { say("token expires: \(e) (\(Int(e.timeIntervalSinceNow / 86400)) days)") }
            say("media host: \(s.mediaHost ?? "not yet discovered")")
            let c = CloudClient(session: s)
            let p = try await c.profile()
            say("profile: \(describe(p, depth: 1))")
            if let d = try? await c.regionalDomain() { say("regional domain: \(describe(d, depth: 2))") }

        case "list":
            let c = try CloudClient.signedIn()
            let all = try await c.listAll(log: say)
            if rest.contains("--raw"), let first = all.first {
                say("first item, every field:")
                for (k, v) in first.raw.sorted(by: { $0.key < $1.key }) { say("  \(k) = \(describe(v, depth: 1))") }
            }
            let df = DateFormatter(); df.dateFormat = "yyyy-MM-dd HH:mm"
            for m in all {
                let fr = m.raw["framerate"]?.number.map { String(format: "%.0ffps", $0) } ?? ""
                let res = (m.raw["width"]?.number).map { "\(Int($0))×\(Int(m.raw["height"]?.number ?? 0))" } ?? ""
                say(String(format: "%-38@ %7@ %10@ %8@ %9@  %@  %@", m.name as NSString, fr as NSString, res as NSString,
                           (m.duration.map { clockString($0) } ?? "") as NSString,
                           (m.size.map { gb($0) } ?? "") as NSString,
                           (m.created.map { df.string(from: $0) } ?? "") as NSString, m.id as NSString))
            }
            let total = all.compactMap(\.size).reduce(0, +)
            say("\(all.count) items, \(gb(total)) of originals")

        case "probe":
            guard let key = rest.first else { fail("usage: reframe-cli cloud probe <name|mediaId>") }
            let c = try CloudClient.signedIn()
            let all = try await c.listAll(log: say)
            guard let m = all.first(where: { $0.id == key || $0.name.contains(key) }) else { fail("no cloud media matches \(key)") }
            say("== \(m.name)  (\(m.id))")
            if let u = m.thumbnailURL {
                let r = try await c.range(u, from: 0, to: 1_000_000)
                let r2 = try await c.range(u, from: 0, to: 1_000_000, apiHeaders: true)
                say("cover \(CloudClient.redact(u.absoluteString)): HTTP \(r.status) plain (\(r.data.count) B), \(r2.status) with API headers (\(r2.data.count) B)")
            }
            try await probe(m, client: c, say: say)

        case "fetch":  // the app's "make available": trailer stub + LRV into the cloud cache
            guard let key = rest.first else { fail("usage: reframe-cli cloud fetch <name|mediaId> [--original]") }
            let c = try CloudClient.signedIn()
            guard let m = try await c.listAll().first(where: { $0.id == key || $0.name.contains(key) }) else { fail("no cloud media matches \(key)") }
            let t0 = Date()
            let url = try await CloudCache.fetch(m, client: c) { stage in
                if case .trailer = stage { print("reading trailer…") }
                if case .fromStudio = stage { print("using Insta360 Studio's download…") }
            }
            say("ready in \(ms(Date().timeIntervalSince(t0))): \(url.path) (stub \(CloudCache.isStub(url)), \(gb(CloudCache.diskUsage(m.id))) on disk)")
            if rest.contains("--original") {
                let t1 = Date()
                try await CloudCache.downloadOriginal(url, client: c) { _ in }
                let size = (try? FileManager.default.attributesOfItem(atPath: url.path)[.size] as? Int64) ?? 0
                say("original: \(gb(size)) in \(ms(Date().timeIntervalSince(t1))), stub \(CloudCache.isStub(url))")
            }

        case "migrate":  // point saved shots at cloud recordings instead of local files
            let dry = rest.contains("--dry-run")
            let c = try CloudClient.signedIn()
            let media = try await c.listAll().filter(\.isVideo)
            let plan = try CloudMigration.plan(media: media)
            for mv in plan.moves {
                let source = mv.cached ? "already cached" : mv.localCopy ? "seed from local copy" : "fetch from cloud"
                say("  move  \(mv.shot) of \(URL(fileURLWithPath: mv.from).lastPathComponent)  (\(source))")
            }
            for u in plan.unmatched { say("  keep  \(u.shot) of \(u.file)  (not in the cloud)") }
            say("\(plan.moves.count) shot\(plan.moves.count == 1 ? "" : "s") to move, \(plan.unmatched.count) staying local")
            let folderKey = "footageFolder" as CFString, app = "dev.sparky.reframe" as CFString
            let savedFolder = CFPreferencesCopyAppValue(folderKey, app) as? String
            let folderStore = savedFolder.map { ShotStore.url(for: URL(fileURLWithPath: $0)).standardizedFileURL }
            let folderKeepsShots = plan.unmatched.contains { $0.store.standardizedFileURL == folderStore }
            if dry {
                if let savedFolder {
                    say(folderKeepsShots ? "the saved footage folder (\(savedFolder)) stays: it still has local-only shots"
                        : "would forget the saved footage folder (\(savedFolder)), so Reframe opens without one")
                }
                say("dry run: nothing changed")
                break
            }
            if !NSRunningApplication.runningApplications(withBundleIdentifier: "dev.sparky.reframe").isEmpty {
                fail("Quit Reframe first: it would save its old shot list over the migrated one.")
            }
            let backups = try await CloudMigration.run(media: media, client: c, log: say)
            for b in backups { say("backup: \(b.path)") }
            if let savedFolder, !folderKeepsShots {
                CFPreferencesSetAppValue(folderKey, nil, app)
                CFPreferencesAppSynchronize(app)
                say("forgot the saved footage folder (\(savedFolder)); open one again with ⌘O")
            }

        case "studio":  // what Insta360 Studio has downloaded (Reframe clones these instead of fetching)
            guard StudioProject.isStudioInstalled else { say("Insta360 Studio isn't installed"); break }
            let all = StudioDownloads.all()
            for (id, rows) in all.sorted(by: { $0.key < $1.key }) {
                for r in rows {
                    let size = (try? FileManager.default.attributesOfItem(atPath: r.path)[.size] as? Int64) ?? -1
                    say("\(id)  \(r.name)  \(gb(r.size))  \(size == r.size ? "on disk" : size < 0 ? "missing" : "size differs")")
                }
            }
            say("\(all.count) recording\(all.count == 1 ? "" : "s") downloaded by Studio")

        case "get":  // debug: reframe-cli cloud get <host> <path> [key=value ...] (output redacted)
            guard rest.count >= 2 else { fail("usage: reframe-cli cloud get <host> <path> [k=v ...]") }
            var q: [String: String] = [:]
            for kv in rest.dropFirst(2) { let p = kv.split(separator: "=", maxSplits: 1); if p.count == 2 { q[String(p[0])] = String(p[1]) } }
            let d = try await CloudClient.signedIn().get(host: rest[0], path: rest[1], query: q)
            say(describe(d, depth: 4))

        case "trailer":  // debug: parse an .insv trailer from any range-capable URL
            guard let s = rest.first, let u = URL(string: s), let size = rest.dropFirst().first.flatMap({ UInt64($0) }) else {
                fail("usage: reframe-cli cloud trailer <url> <size>")
            }
            let remote = RemoteFile(url: u, size: size)
            let insv = try InsvFile(url: u, source: remote)
            let imu = try insv.readIMU()
            say("\(insv.info.camera) \(insv.info.colorLabel), calibration \(insv.info.calibration.isEmpty ? "missing" : "present"), gyro samples \(imu?.time.count ?? 0)")
            say("fetched \(gb(Int64(remote.bytesFetched))) in \(remote.requests) requests of \(gb(Int64(size)))")
            if let m = Motion(file: insv) { say("motion: \(m.times.count) orientations") }

        default:
            fail("usage: reframe-cli cloud login|logout|status|list [--raw]|probe <name|mediaId>")
        }
    } catch {
        fail("cloud: \(error)")
    }
}

/// Opens the sign-in window and runs AppKit until it finishes; exits the process.
func cloudLogin() {
    MainActor.assumeIsolated {
        let app = NSApplication.shared
        app.setActivationPolicy(.regular)
        // A bare process has no menu bar, so ⌘V/⌘C/⌘A have nothing to trigger: add the
        // standard Edit menu (key equivalents route through the main menu).
        let main = NSMenu(), appItem = NSMenuItem(), editItem = NSMenuItem()
        main.addItem(appItem); main.addItem(editItem)
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "Quit", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        let edit = NSMenu(title: "Edit")
        edit.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
        edit.addItem(withTitle: "Redo", action: Selector(("redo:")), keyEquivalent: "Z")
        edit.addItem(.separator())
        edit.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit
        app.mainMenu = main
        let signIn = CloudSignIn()
        print("A window opened on Insta360's sign-in page. Sign in there (Reframe never sees your password).")
        fflush(stdout)
        signIn.begin { session in
            guard let session else { fail("sign-in cancelled or timed out") }
            Task {
                do {
                    try session.save()
                    let c = CloudClient(session: session)
                    let p = try await c.profile()
                    print("signed in; profile: \(describe(p, depth: 1))")
                    if let e = session.expires { print("token valid until \(e)") }
                    print("session saved to \(CloudSession.fileURL.path) (owner-only)")
                    exit(0)
                } catch {
                    fail("captured a token but the API rejected it: \(error)")
                }
            }
        }
        withExtendedLifetime(signIn) { app.run() }
    }
}

func probe(_ m: CloudMedia, client c: CloudClient, say: (String) -> Void) async throws {
    // Detail: list its fields, and look for proxy/stream URLs (DASH .mpd and the like).
    let detail = try await c.detail(m.id)
    say("detail fields: \(describe(detail, depth: 1))")
    var streams: [String] = []
    collectStrings(detail) { s in if s.contains(".mpd") || s.contains(".m3u8") || s.contains("/m4s/") { streams.append(s) } }

    let files = try await c.files(for: m.id)
    say("files:")
    for f in files { say("  \(f.name)  \(gb(f.size))  \(f.url.host ?? "")\(f.url.path)") }

    for f in files {
        say("-- \(f.name)")
        let head = try await c.range(f.url, from: 0, to: 1023)
        say("   first 1 KB: HTTP \(head.status), Content-Range \(head.contentRange ?? "none"), \(head.data.count) B, \(ms(head.seconds))")
        guard head.status == 206 else { say("   no ranged reads; stopping here"); continue }
        let size = f.size > 0 ? f.size : Int64(head.contentRange?.split(separator: "/").last.flatMap { Int64($0) } ?? 0)
        let mid = size / 2
        let r = try await c.range(f.url, from: mid, to: mid + (4 << 20) - 1)
        let mbps = Double(r.data.count) * 8 / 1e6 / max(r.seconds, 1e-3)
        say("   4 MB from the middle: HTTP \(r.status), \(r.data.count) B in \(ms(r.seconds)) (\(Int(mbps)) Mbit/s)")

        if f.name.lowercased().hasSuffix(".insv") {
            let remote = RemoteFile(url: f.url, size: UInt64(size))
            let t0 = Date()
            let insv = try InsvFile(url: URL(fileURLWithPath: "/cloud/\(f.name)"), source: remote)
            let imu = try insv.readIMU()
            let i = insv.info
            say("   trailer over HTTP: \(i.camera) \(i.colorLabel), \(String(format: "%.1f", Double(i.durationMs ?? 0) / 1000)) s, calibration \(i.calibration.isEmpty ? "missing" : "present")")
            if let imu, let a = imu.time.first, let b = imu.time.last {
                say("   gyro: \(imu.time.count) samples over \(String(format: "%.1f", b - a)) s")
            } else { say("   gyro: none found") }
            say("   fetched \(gb(Int64(remote.bytesFetched))) in \(remote.requests) requests, \(ms(Date().timeIntervalSince(t0))) — vs \(gb(size)) for the whole file")
            if imu != nil, let motion = Motion(file: insv) {
                say("   motion solved from remote gyro: \(motion.times.count) orientations")
            }
        }
    }

    for s in streams.prefix(3) {
        guard let u = URL(string: s) else { continue }
        let plain = try await c.range(u, from: 0, to: 2047)
        let authed = try await c.range(u, from: 0, to: 2047, apiHeaders: true)
        say("stream \(CloudClient.redact(s)): HTTP \(plain.status) plain, \(authed.status) with API headers")
        if authed.status / 100 == 2 || plain.status / 100 == 2 {
            let text = String(decoding: (authed.status / 100 == 2 ? authed : plain).data, as: UTF8.self)
            for line in text.split(separator: "\n") where line.contains("Representation id") { say("   \(line.trimmingCharacters(in: .whitespaces))") }
        }
    }
    if streams.isEmpty { say("no stream (.mpd/.m3u8) URLs in the detail response") }
}

func collectStrings(_ v: AnyCodableValue, _ f: (String) -> Void) {
    switch v {
    case let .string(s): f(s)
    case let .array(a): a.forEach { collectStrings($0, f) }
    case let .object(o): o.values.forEach { collectStrings($0, f) }
    default: break
    }
}

/// Field listing, credentials elided (`AnyCodableValue.description` redacts).
func describe(_ v: AnyCodableValue, depth: Int) -> String {
    guard depth > 0 else { return v.description }
    switch v {
    case let .object(o):
        return "{" + o.sorted { $0.key < $1.key }.map { "\($0.key): \(describe($0.value, depth: depth - 1))" }.joined(separator: ", ") + "}"
    case let .array(a):
        return a.isEmpty ? "[]" : "[\(a.count)× \(describe(a[0], depth: depth - 1))]"
    default:
        return v.description
    }
}

func gb(_ bytes: Int64) -> String {
    let b = Double(bytes)
    return b >= 1e9 ? String(format: "%.2f GB", b / 1e9) : b >= 1e6 ? String(format: "%.1f MB", b / 1e6) : String(format: "%.0f KB", b / 1e3)
}
func ms(_ s: Double) -> String { String(format: "%.0f ms", s * 1000) }
func clockString(_ t: Double) -> String { String(format: "%d:%02d", Int(t) / 60, Int(t) % 60) }
