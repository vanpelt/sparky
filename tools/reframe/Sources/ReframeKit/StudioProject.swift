import AppKit
import Foundation

/// Writes Reframe shots as an Insta360 Studio (6.x) timeline project, so selects can be finished there.
///
/// Studio keeps each timeline as pretty-printed JSON:
///   ~/Documents/Insta360/Studio/TimelineProject/<name>/<uuid>.insproj  (+ <uuid>_backup.insproj)
///   ~/Documents/Insta360/Studio/TimelineProject/<name>/project_album_info.data  (media bin)
///   ~/Library/Application Support/Insta360/Insta360 Studio/nle/pro.insproj     (project list)
/// Studio rewrites the project list while it runs, so register only while it is quit.
///
/// Mapping, checked against projects saved by Studio 6.0.8:
/// - Timeline and trims are in frames at the project's rate (see `build`). `startFrame` is the clip's place on the timeline,
///   `leftTrim`/`rightTrim` are frames cut from the start/end of the source.
/// - Keyframes: pan/tilt/roll in radians, `fov` the vertical field of view in radians,
///   `distance` Studio's lens pull-back (Reframe's `distortion`). Between keyframes sits an
///   easing node (node_type 1). Reframe's Mega/Dewarp/Linear presets reproduce Studio's values.
/// - Keyframe `time`/`src_time` are source frames (markers land where expected on a trimmed clip).
/// - Keyframe pan is a world direction, not relative to where the camera points: left-positive
///   (like Reframe's yaw), zero = the camera's heading on the file's first frame, and Studio
///   interpolates between keyframes in that world frame. Measured on six frames of VID_009
///   (headings 100°–343°): Reframe's raw heading at t=0 matched the fitted zero to within 2°.
///   So pan = Reframe view-base heading(t) + yaw(t) - heading(0), and a Follow shot needs extra
///   keyframes wherever the camera turns, or Studio's view would hold still while Reframe's turns.
/// - Tilt is positive up, like Reframe's pitch.
public enum StudioProject {
    /// Largest error (degrees) Studio's straight-line interpolation may leave against Reframe's path
    /// before another keyframe is added.
    public static var tolerance = 2.0

    /// A shot ready to place on Studio's timeline.
    public struct Item {
        public var shot: Shot
        /// The source file's length in seconds.
        public var duration: Double
        /// Studio's timeline rate for this recording (48 -> 24). The project takes the first clip's.
        public var fps: Double
        /// Reframe's view-base heading (degrees, continuous) at a source time, for the shot's mode.
        public var viewHeading: (Double) -> Double
        /// The world heading Studio calls pan 0: the camera's heading on the file's first frame.
        public var zeroHeading: Double
        /// Set when the recording's original is only in Insta360 Cloud: Studio then references it
        /// by media ID and streams it, rather than needing a local file (see CLOUD.md).
        public var cloudMediaId: String?
        /// Recording time, for the album entry of a cloud recording.
        public var captureDate: Date?

        public init(shot: Shot, duration: Double, fps: Double,
                    viewHeading: @escaping (Double) -> Double, zeroHeading: Double) {
            self.shot = shot
            self.duration = duration
            self.fps = fps
            self.viewHeading = viewHeading
            self.zeroHeading = zeroHeading
        }

        public init?(shot: Shot, clip: Clip) {
            guard let sourceFPS = clip.info.nominalFPS, sourceFPS > 0 else { return nil }
            let fps = StudioProject.timelineFPS(Double(sourceFPS))
            let motion = clip.motion
            // Studio always stabilizes; an unstabilized shot's shaky heading would need a keyframe
            // on nearly every frame, so it goes over as Follow.
            let mode: Motion.Stabilization = shot.stabilization == .none ? .follow : shot.stabilization
            self.init(shot: shot, duration: clip.duration, fps: fps,
                      viewHeading: { t in
                          motion?.baseHeading(at: t, mode: mode, smoothing: shot.smoothing,
                                              lockTime: shot.lockTime) ?? 0
                      },
                      zeroHeading: (motion?.heading(at: 0, smoothing: 0) ?? 0) * 180 / .pi)
            if CloudCache.isStub(clip.url) { cloudMediaId = CloudCache.record(for: clip.url)?.mediaId }
            captureDate = clip.captureDate
        }

        /// What Studio identifies the media by: a cloud media ID, or the file path.
        var reference: String { cloudMediaId ?? shot.clipPath }
    }

    /// Studio's timeline rate for a recording rate (48 -> 24, 60/120 -> 30); seen for 48 fps X6 footage.
    public static func timelineFPS(_ sourceFPS: Double) -> Double {
        var f = sourceFPS
        while f > 30.5 { f /= 2 }
        return f
    }

    public static let bundleID = "com.insta360.studio"
    public static var timelineRoot: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Documents/Insta360/Studio/TimelineProject", isDirectory: true)
    }
    public static var indexURL: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/Insta360/Insta360 Studio/nle/pro.insproj")
    }
    public static var isStudioRunning: Bool {
        !NSRunningApplication.runningApplications(withBundleIdentifier: bundleID).isEmpty
    }
    public static var isStudioInstalled: Bool {
        NSWorkspace.shared.urlForApplication(withBundleIdentifier: bundleID) != nil
    }

    /// Asks Studio to quit (it may ask to save first) and waits up to `timeout` seconds.
    /// Returns false if it is still running.
    public static func quitStudio(timeout: Double = 90) async -> Bool {
        for app in NSRunningApplication.runningApplications(withBundleIdentifier: bundleID) { app.terminate() }
        let deadline = Date().addingTimeInterval(timeout)
        while isStudioRunning {
            if Date() > deadline { return false }
            try? await Task.sleep(for: .milliseconds(250))
        }
        try? await Task.sleep(for: .milliseconds(500))  // let it finish writing its project list
        return true
    }

    public static func openStudio() {
        guard let url = NSWorkspace.shared.urlForApplication(withBundleIdentifier: bundleID) else { return }
        NSWorkspace.shared.openApplication(at: url, configuration: NSWorkspace.OpenConfiguration())
    }

    /// `base`, or `base 2`, `base 3`… so an earlier export (maybe edited in Studio since) is never replaced.
    public static func availableName(_ base: String, root: URL? = nil) -> String {
        let dir = root ?? timelineRoot
        var name = base, n = 2
        while FileManager.default.fileExists(atPath: dir.appendingPathComponent(name).path) {
            name = "\(base) \(n)"
            n += 1
        }
        return name
    }

    public enum Failure: Error, CustomStringConvertible {
        case noShots, exists(URL), studioRunning
        public var description: String {
            switch self {
            case .noShots: return "no shots to write"
            case .exists(let u): return "\(u.path) already exists"
            case .studioRunning: return "quit Insta360 Studio first: it rewrites its project list while running"
            }
        }
    }

    /// Writes the project folder and, if `register`, adds it to Studio's project list.
    /// `replace` moves an existing project of the same name to the Trash first. Returns the .insproj URL.
    @discardableResult
    /// `fps` is the timeline rate (24, 25, 30...); nil takes the first clip's, as Studio does.
    public static func write(name: String, items: [Item], fps: Double? = nil, register: Bool,
                             replace: Bool = false, root: URL? = nil) throws -> URL {
        guard !items.isEmpty else { throw Failure.noShots }
        if (register || replace) && isStudioRunning { throw Failure.studioRunning }
        let folder = (root ?? timelineRoot).appendingPathComponent(name, isDirectory: true)
        if FileManager.default.fileExists(atPath: folder.path) {
            guard replace else { throw Failure.exists(folder) }
            try FileManager.default.trashItem(at: folder, resultingItemURL: nil)
        }
        let id = UUID().uuidString.lowercased()
        let project = build(name: name, id: id, folder: folder, items: items, fps: fps)

        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        let data = try json(project)
        let file = folder.appendingPathComponent("\(id).insproj")
        try data.write(to: file, options: .atomic)
        try data.write(to: folder.appendingPathComponent("\(id)_backup.insproj"), options: .atomic)
        try json(album(items)).write(to: folder.appendingPathComponent("project_album_info.data"), options: .atomic)

        if register { try self.register(project, folder: folder) }
        return file
    }

    /// Adds (or refreshes) a project's entry in Studio's project list. Studio must be quit.
    static func register(_ project: [String: Any], folder: URL) throws {
        var index: [String: Any] = ["projects": [Any]()]
        if let d = try? Data(contentsOf: indexURL),
           let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any] { index = o }
        var list = index["projects"] as? [[String: Any]] ?? []
        var entry = project
        entry.removeValue(forKey: "tracks")
        if let i = list.firstIndex(where: { ($0["projectPath"] as? String) == folder.path }) {
            list[i] = entry
        } else {
            list.append(entry)
        }
        index["projects"] = list
        // Keep the list as it was before Reframe first touched it.
        let backup = indexURL.deletingPathExtension().appendingPathExtension("insproj.reframe-backup")
        if FileManager.default.fileExists(atPath: indexURL.path) && !FileManager.default.fileExists(atPath: backup.path) {
            try FileManager.default.copyItem(at: indexURL, to: backup)
        }
        try FileManager.default.createDirectory(at: indexURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        try json(index).write(to: indexURL, options: .atomic)
    }

    // MARK: Building

    /// Every frame number in a project (each clip's length, trims, keyframes, place) is counted at
    /// the project's rate, whatever the clip was shot at: Studio read a 48 fps clip's 342 frames as
    /// 11.4 s on a 30 fps timeline. A clip has floor(duration * rate) frames (88.208 s -> 2116 at 24,
    /// 360.99 s -> 10829 at 30). Without a chosen rate the project takes the first clip's, like
    /// Studio's auto rate; a chosen rate turns auto off so Studio keeps it.
    static func build(name: String, id: String, folder: URL, items: [Item], fps chosen: Double? = nil) -> [String: Any] {
        let fps = (chosen ?? items[0].fps).rounded()
        var clips: [[String: Any]] = []
        var position = 0
        for item in items {
            let c = clip(item, fps: fps, startFrame: position, folder: folder, projectName: name)
            clips.append(c)
            position += (c["sourceFrames"] as! Int) - (c["leftTrim"] as! Int) - (c["rightTrim"] as! Int)
        }
        let now = timestamp(Date())
        return [
            "aspectRatio": "16:9", "autoFps": chosen == nil, "creationTime": now, "enableEffects": true,
            "fps": Int(fps), "frames": position, "hdrColorSpaceAdaptAccepted": false, "hdrPreviewEnabled": false,
            "id": id, "isPanoramic": false, "lastModifiedTime": now, "mainMagnetizable": true,
            "markTrimEnd": position, "markTrimIn": 0, "markTrimVisible": false, "name": name,
            "projectPath": folder.path, "skipHdrColorSpaceAdaptDialog": false,
            "tracks": [
                ["audible": true, "clips": clips, "id": uuid(), "locked": false, "transitions": [Any](),
                 "type": 0, "visible": true, "z": 0],
                ["audible": true, "clips": [Any](), "id": uuid(), "locked": false, "transitions": [Any](),
                 "type": 2, "visible": true, "z": -1],
            ],
            "watermarkPanelApplyAll": true,
            "watermarkPanelConfigJson": #"{"flat_logo_watermark":{"enabled":false,"position":0,"style_path":"","transparency":100},"logo_watermark":{"corner_radius":0,"enabled":false,"feather":0,"position":0,"rotate":0,"size":30,"style_path":""},"time_watermark":{"enabled":false,"format":1,"transparency":100},"version":1}"#,
        ]
    }

    static func clip(_ item: Item, fps: Double, startFrame: Int, folder: URL, projectName: String) -> [String: Any] {
        let s = item.shot, n = Int((item.duration * fps).rounded(.down))
        let inFrame = min(max(Int((s.inTime * fps).rounded()), 0), n - 1)
        let outFrame = min(max(Int((s.outTime * fps).rounded()), inFrame + 1), n)
        let id = uuid()
        var c = template
        c["id"] = id
        c["name"] = displayName(s.clipPath)
        c["url"] = s.clipPath
        if let cloud = item.cloudMediaId {
            // As Studio saves a cloud clip it hasn't downloaded: the media ID in place of a path.
            c["url"] = cloud
            c["resourceId"] = cloud
            c["online"] = true
        }
        c["fps"] = Int(fps.rounded())
        c["sourceFrames"] = n
        c["source_total_frames"] = n
        c["startFrame"] = startFrame
        for k in ["leftTrim", "source_left_trim"] { c[k] = inFrame }
        for k in ["rightTrim", "source_right_trim"] { c[k] = n - outFrame }

        var deep = c["deep_track"] as! [String: Any]
        deep["save_path"] = folder.appendingPathComponent(projectName).appendingPathComponent(id).path
        c["deep_track"] = deep
        // Keyframes carry world directions, so every mode (Lock included) is plain stabilization.
        var stab = c["stab_setting"] as! [String: Any]
        stab["enableStab"] = true
        stab["enableDirectionalLock"] = false
        c["stab_setting"] = stab

        // Reframe's view on every Studio frame of the shot, as Studio angles. Pan is unwrapped so
        // Studio turns the same way Reframe does.
        let frames = Array(inFrame..<outFrame)
        var path: [(pan: Double, framing: Framing)] = []
        for f in frames {
            let t = Double(f) / fps
            let framing = s.framing(at: t)
            var pan = item.viewHeading(t) + framing.yaw - item.zeroHeading
            if let last = path.last?.pan { pan -= 360 * ((pan - last) / 360).rounded() }
            path.append((pan, framing))
        }
        let shift = 360 * (path[0].pan / 360).rounded()  // start within ±180°
        for i in path.indices { path[i].pan -= shift }

        // Keyframes: the shot's own, its ends, and wherever straight-line interpolation strays.
        var keep = Set([0, path.count - 1])
        for k in s.keyframes {
            let i = Int((k.time * fps).rounded()) - inFrame
            if i > 0 && i < path.count - 1 { keep.insert(i) }
        }
        func refine(_ a: Int, _ b: Int) {
            guard b - a > 1 else { return }
            var worst = 0.0, at = a
            for i in (a + 1)..<b {
                let u = Double(i - a) / Double(b - a)
                func err(_ v: (Int) -> Double) -> Double { abs(v(i) - (v(a) + (v(b) - v(a)) * u)) }
                let e = max(err { path[$0].pan }, err { path[$0].framing.pitch }, err { path[$0].framing.hfov })
                if e > worst { worst = e; at = i }
            }
            guard worst > tolerance else { return }
            keep.insert(at)
            refine(a, at)
            refine(at, b)
        }
        let anchors = keep.sorted()
        for (a, b) in zip(anchors, anchors.dropFirst()) { refine(a, b) }

        var nodes: [[String: Any]] = []
        var previousName: String?
        for i in keep.sorted() {
            let (pan, f) = path[i]
            let time = frames[i]
            let name = uuid()
            if let p = previousName {
                nodes.append(["name": "\(p)-\(name)", "node_type": 1, "point1X": 0.5, "point1Y": 0.5,
                              "point2X": 0.5, "point2Y": 0.5, "type": 1])
            }
            nodes.append(["auto_fov": 0, "distance": f.distortion, "fov": verticalFOV(f.hfov), "is_headtrack": 0,
                          "name": name, "node_type": 0, "pan": pan * .pi / 180, "roll": 0,
                          "src_time": time, "state": 7, "tilt": f.pitch * .pi / 180, "time": time])
            previousName = name
        }
        c["key_frame_track"] = ["node_list": nodes]

        let first = path[0].framing
        let transform: [String: Any] = [
            "distance": first.distortion,
            "eulers": ["pitch": ["type": 0, "value": 0], "roll": ["type": 0, "value": 0], "yaw": ["type": 0, "value": 0]],
            "fov": ["type": 0, "value": verticalFOV(first.hfov)],
        ]
        c["camera_transform"] = transform
        c["original_camera_transform"] = transform
        return c
    }

    /// Studio stores the vertical field of view (radians) of its 16:9 frame; Reframe's hfov is horizontal.
    static func verticalFOV(_ hfovDegrees: Double, aspect: Double = 9.0 / 16.0) -> Double {
        2 * atan(tan(hfovDegrees * .pi / 360) * aspect)
    }

    static func album(_ items: [Item]) -> [String: Any] {
        var seen: [String] = []
        var byRef: [String: Item] = [:]
        for i in items where byRef[i.reference] == nil { seen.append(i.reference); byRef[i.reference] = i }
        let nodes = seen.map { ref -> [String: Any] in
            ["media_card_uuid": ref, "name": displayName(byRef[ref]!.shot.clipPath),
             "node_uuid": uuid(), "parent_uuid": "", "type": "Footage"]
        }
        var snapshots: [String: Any] = [:]
        let iso = ISO8601DateFormatter()
        for ref in seen {
            let item = byRef[ref]!
            let cloud = item.cloudMediaId != nil
            snapshots[ref] = ["create_time": cloud ? item.captureDate.map(iso.string(from:)) ?? "" : "",
                              "display_name": displayName(item.shot.clipPath),
                              "is_dng_group": false, "is_photo_group": false, "remark_name": "",
                              "source": cloud ? "cloud" : "local", "uuid": ref]
        }
        return ["album_tree": ["nodes": nodes, "root_uuids": nodes.map { $0["node_uuid"]! }, "version": 1],
                "media_card_snapshots": snapshots, "tab_media_uuids": seen, "version": 1]
    }

    /// Studio's short name for camera files: VID_20261023_094147_00_009.insv -> VID_009.insv.
    static func displayName(_ path: String) -> String {
        let file = URL(fileURLWithPath: path).lastPathComponent
        let parts = file.split(separator: "_")
        guard parts.count == 5, let last = parts.last else { return file }
        return "\(parts[0])_\(last)"
    }

    static func uuid() -> String { UUID().uuidString.lowercased() }

    static func timestamp(_ d: Date) -> String {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "yyyy.MM.dd HH:mm:ss"
        return f.string(from: d)
    }

    public static func json(_ o: Any) throws -> Data {
        try JSONSerialization.data(withJSONObject: o, options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes])
    }

    /// Default per-clip settings, from a clip Studio 6.0.8 saved (stabilization on, no effects).
    static let template: [String: Any] = {
        let s = #"""
        {
            "aspect_ratio_info": {"currentAspectRatio": "8", "current_aspect_ratio": 8, "supportAspectRatios": null, "support_aspect_ratios": [1, 3, 8, 6, 7, 5]},
            "asteroidPitchOffset": 0,
            "audio_setting": {"audioDetached": false, "currentDenoiseType": 1, "currentFadeIn": 0, "currentFadeOut": 0, "currentMixingWeight": 50, "currentTracksDelay": -2147483648, "currentVolume": 100, "defaultTracksDelay": -2147483648, "enableDenoise": false, "enableMixing": true, "maxFadeIn": 300, "maxFadeOut": 300, "preDetachVolume": 100, "supportAudio": true, "supportDenoise": true, "supportDenoiseTypes": [1, 2], "supportMixing": false},
            "baseViewAutoApplied": false,
            "camera_movement": {"bottom_by_category": [], "camera_movement_models": [], "distance": 0.6, "fov": 1.3089969389957472, "pitch_offset": 0, "roll_offset": 0, "view_category_filter": 1, "yaw_offset": 0},
            "dashboard_setting": {"dashboardDataId": "", "dashboardDataType": 0, "dashboardSourceType": "TypeNoneData", "dashboardStyle": "", "enableDashboard": true, "supportDashboard": true},
            "deep_track": {"cache_path": "", "deep_track_list": [], "original_clip_id": "", "save_path": ""},
            "default_factor": 1,
            "dewarp_setting": {"aspectRatio": 0, "fov_distance_range": {"is_max_x": true, "is_min_x": true, "max_distance": 1.7999999523162842, "max_fov": 178, "min_distance": 0, "min_fov": 1}, "removeDistortion": false},
            "effect_filter": {"externfilterId": "", "externfilterName": "", "externfilterOpacity": 70, "externfilterResourcePath": ""},
            "enable_user_keyframe": true,
            "flat_logo_watermark": {"clipAspectRatio": 8, "enableFlatLogoWatermark": false, "flatLogoWatermarkTransparency": 100, "logoWatermarkLocationType": 0, "selectedLogoWatermarkPath": ""},
            "has_set_play_rate_manually": false,
            "hdrPreviewEnabled": false,
            "hdrPreviewUserToggled": false,
            "head_track": {"distance": 0.4, "fov": 0.9952148917524772, "head_track_list": [], "pitch_offset": 0, "roll_offset": 0, "yaw_offset": 0},
            "highlight_data": false,
            "inAsteroidMode": false,
            "lastTrimEditTime": 0,
            "legacy_camera_movement": false,
            "logo_watermark": {"enableLogoWatermark": false, "logoWatermarkCornerRadius": 0, "logoWatermarkFeather": 0, "logoWatermarkLocationType": 0, "logoWatermarkRotate": 0, "logoWatermarkSize": 30, "selectedLogoWatermarkPath": ":resource/default_logo_0.png"},
            "media_process": {"colorAdjustAutoadjust": 0, "colorAdjustBlackpoint": 0, "colorAdjustBrightness": 0, "colorAdjustBrilliance": 0, "colorAdjustCommonMax": 100, "colorAdjustCommonMin": -100, "colorAdjustContrast": 0, "colorAdjustDefinition": 0, "colorAdjustDefinitionMin": 0, "colorAdjustExposure": 0, "colorAdjustHighlights": 0, "colorAdjustSaturation": 0, "colorAdjustShadows": 0, "colorAdjustTint": 0, "colorAdjustVibrance": 0, "colorAdjustWarmth": 0, "colorPlusStrength": 30, "colorPlusStrengthMax": 100, "colorPlusStrengthMin": 0, "enableAe": false, "enableBeautyMode": false, "enableColorAdjust": false, "enableColorPlus": false, "enableHorizontalCorrect": false, "enableLocalToneMapping": false, "enableLut": false, "enableMotionBlur": false, "enableMultiFrameDenoise": true, "enablePureShot": false, "enableSingleDenoise": false, "enableTimeLogo": false, "enableUnderWaterCorrect": false, "horizontalCorrectAngle": 0, "horizontalCorrectDefaultAngle": 0, "localToneMappingStrength": 30, "localToneMappingStrengthMax": 100, "localToneMappingStrengthMin": 0, "motionBlurEdited": false, "motionBlurStrength": 50, "motionBlurThreshold": 85, "supportAe": false, "supportColorAdjust": true, "supportColorPlus": true, "supportHorizontalCorrect": false, "supportLocalToneMapping": true, "supportLut": true, "supportMotionBlur": true, "supportPureShot": false, "supportSingleDenoise": false, "supportUnderWaterCorrect": true, "timeLogoFormat": 0, "timeLogoTransparency": 100, "underWaterStrength": 100, "underWaterStrengthMax": 100, "underWaterStrengthMin": 0, "underWaterStyle": 0},
            "online": false,
            "panoAnimationPresetId": -1,
            "play_rate_track": {},
            "popupApplyDefaultCameraTransformTips": true,
            "ratio": 8,
            "resourceId": "",
            "roughTrimmed": false,
            "srcId": "",
            "stab_setting": {"enableDirectionalLock": false, "enableFpv": false, "enableImmerrsionStab": false, "enablePanoFpv": false, "enableStab": true, "modifiable": true, "stabLevel": 0, "supportBoostStabLevel": false, "supportDirectionalLock": true, "supportFpv": false, "supportHighStabLevel": false, "supportPanoFpv": true, "supportStab": true},
            "state": 0,
            "stitching_optimization": {"enableAIStitching": true, "enableDynamicStitching": false, "enableImageFusionStitching": true, "enableOpticalFlowStitching": false, "ignoreDynamicStitching": false, "ignoreImageFusionStitching": false, "ignoreOpticalFlowStitching": false, "optimizedOffset": "", "supportAIStitching": true, "supportDynamicStitching": true, "supportImageFusionStitching": true, "supportOpticalFlowStitching": true},
            "stitching_setting": {"cameraAccessory": 0, "customStitchingValue": 0, "customStitchingValueMax": 7, "enableColdShoe": false, "enableCoolingShell": false, "propellerGuardStatus": 0, "supportCameraAccessory": [0, 42, 43, 40, 11, 12], "supportColdShoe": false, "supportCoolingShell": false, "supportCustomStitching": true, "supportPropellerGuard": false, "supportStitching": true},
            "subtype": 1,
            "supplementFrameType": "SupplementFrameTypeNormal",
            "supplement_frame_type": 0,
            "time_watermark": {"enableTimeWatermark": false, "timeWatermarkFormat": 1, "timeWatermarkTransparency": 100},
            "type": 1,
            "userRotateDegree": 0,
            "viewType": 0
        }
        """#
        return try! JSONSerialization.jsonObject(with: Data(s.utf8)) as! [String: Any]
    }()
}
