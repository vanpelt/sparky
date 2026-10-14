import XCTest

/// Every iPhone Duo window, proxied on an iPad simulator through the `-versoFrame WxH` harness (DebugLaunch).
///
/// Run with `IPAD=1 SIM=<ipad-udid> scripts/sim.sh test -only-testing:VersoUITests/DuoLayoutUITests`. Each size
/// visits the four tabs and attaches a screenshot of the frame; `VERSO_SHOT_DIR=<dir>` also writes them as PNGs.
/// It checks the layout rule rather than pixels: two pages (`page.verso`, `page.recto`) side by side only when
/// the window is at least 860 wide and wider than tall, otherwise one column no wider than a reading measure;
/// and the pen always inside the window. The `ForeEdge` cases set the tabs down the side, as the system's bars go
/// on the closed Duo and the open one held landscape. Sizes that can't fit the simulator's screen either way round skip.
@MainActor
final class DuoLayoutUITests: XCTestCase {
    private var app: XCUIApplication!

    /// Tabs whose page splits into a spread at spread sizes.
    private let tabs = ["Contents", "Ledger", "Threads", "Turning Point"]
    /// A text line wider than this in single-column mode has escaped the measure (560, plus slack).
    private let maxColumn: CGFloat = 600

    // The outer display's safe areas are unpublished: budget a camera-and-status-bar band of 50pt over the
    // harness's default 24, so a page that fits here still fits on the device.
    func testOuter466x678() throws { try check(466, 678, insets: "50,0,20,0") }
    func testOuterSDK27_0_386x678() throws { try check(386, 678, insets: "50,0,20,0") }
    func testInner951x669() throws { try check(951, 669) }
    func testInnerSDK27_0_871x669() throws { try check(871, 669) }
    func testInnerPortrait669x951() throws { try check(669, 951) }
    func testSplitViewHalf475x669() throws { try check(475, 669) }

    // Bars set down the side (`toolbarVerticalEdge`, which the harness can't produce: `-versoBarEdge` stands in),
    // with the ~84pt column measured on the device as the frame's margin on that edge.
    func testForeEdgeOuter466x678() throws { try checkForeEdge(466, 678, edge: .trailing) }
    func testForeEdgeInner951x669() throws { try checkForeEdge(951, 669, edge: .trailing) }
    func testForeEdgeSplitViewLeading475x669() throws { try checkForeEdge(475, 669, edge: .leading) }

    // MARK: -

    private enum Edge: String { case leading, trailing }

    /// The tabs down the fore-edge: each inside the column, the pen above them, and no running foot.
    private func checkForeEdge(_ w: CGFloat, _ h: CGFloat, edge: Edge) throws {
        let size = CGSize(width: w, height: h)
        let tag = "\(Int(w))x\(Int(h))"
        app = XCUIApplication()
        app.launchArguments = ["-versoSample", "YES", "-versoSkipOnboarding", "YES", "-versoUnveil", "YES",
                               "-versoFrame", tag, "-versoBarEdge", edge.rawValue,
                               "-versoFrameInsets", edge == .trailing ? "0,0,20,84" : "0,84,20,0"]
        app.launch()
        try skipOnDuoInnerDisplay(app)
        continueAfterFailure = true
        addTeardownBlock { @MainActor in XCUIDevice.shared.orientation = .portrait }
        try orient(toFit: size)

        let window = app.windows.firstMatch.frame
        let frame = CGRect(x: window.midX - w / 2, y: window.midY - h / 2, width: w, height: h)
        XCTAssertTrue(app.descendants(matching: .any)["shelf.edge"].waitForExistence(timeout: 15), "\(tag): no fore-edge")
        // The running foot would be a second set of the same four tabs.
        XCTAssertEqual(app.buttons.matching(NSPredicate(format: "label == 'Contents'")).count, 1,
                       "\(tag): the running foot is built too")

        for name in tabs {
            let tab = tabButton(name)
            XCTAssertTrue(tab.waitForExistence(timeout: 5), "\(tag): no \(name) tab")
            guard tab.exists else { continue }
            if !tab.isSelected { tab.tap() }
            settle()
            save(cropped(XCUIScreen.main.screenshot().image, to: frame, in: window),
                 as: "duo-edge-\(edge.rawValue)-\(tag)-\(name.lowercased().replacingOccurrences(of: " ", with: "-"))")

            if edge == .trailing {
                XCTAssertGreaterThanOrEqual(tab.frame.minX, frame.maxX - 85, "\(tag) \(name): tab \(tab.frame) off the column")
            } else {
                XCTAssertLessThanOrEqual(tab.frame.maxX, frame.minX + 85, "\(tag) \(name): tab \(tab.frame) off the column")
            }
            XCTAssertTrue(frame.insetBy(dx: -1, dy: -1).contains(tab.frame), "\(tag) \(name): tab outside the window")
            XCTAssertTrue(tab.isSelected, "\(tag) \(name): the tab didn't take")
        }
        let pen = app.buttons["Log"], contents = tabButton("Contents")
        XCTAssertTrue(pen.waitForExistence(timeout: 2), "\(tag): no pen")
        XCTAssertLessThanOrEqual(pen.frame.maxY, contents.frame.minY + 0.5, "\(tag): the pen isn't above the tabs")
    }

    private func check(_ w: CGFloat, _ h: CGFloat, insets: String? = nil) throws {
        let size = CGSize(width: w, height: h)
        let tag = "\(Int(w))x\(Int(h))"
        let spread = w >= 860 && w > h

        app = XCUIApplication()
        app.launchArguments = ["-versoSample", "YES", "-versoSkipOnboarding", "YES", "-versoUnveil", "YES",
                               "-versoFrame", tag]
        if let insets { app.launchArguments += ["-versoFrameInsets", insets] }
        app.launch()
        try skipOnDuoInnerDisplay(app)
        // Keep checking the remaining tabs after a miss: one run should report every page that's off.
        continueAfterFailure = true
        addTeardownBlock { @MainActor in XCUIDevice.shared.orientation = .portrait }
        try orient(toFit: size)

        let window = app.windows.firstMatch.frame
        let frame = CGRect(x: window.midX - w / 2, y: window.midY - h / 2, width: w, height: h)

        for name in tabs {
            let tab = tabButton(name)
            XCTAssertTrue(tab.waitForExistence(timeout: 15), "\(tag): no \(name) tab")
            guard tab.exists else { continue }
            if !tab.isSelected { tab.tap() }
            settle()

            // The screen's screenshot, not the app's: in landscape the app's comes back with its pixels
            // mislabelled (a portrait-shaped capture of the rotated window), the screen's is upright.
            let shot = cropped(XCUIScreen.main.screenshot().image, to: frame, in: window)
            let slug = "duo-\(tag)-\(name.lowercased().replacingOccurrences(of: " ", with: "-"))"
            save(shot, as: slug)

            XCTAssertTrue(frame.insetBy(dx: -1, dy: -1).contains(tab.frame),
                          "\(tag) \(name): tab \(tab.frame) outside the window \(frame)")
            let pen = app.buttons["Log"]
            if pen.waitForExistence(timeout: 2) {
                XCTAssertTrue(frame.insetBy(dx: -1, dy: -1).contains(pen.frame), "\(tag) \(name): pen outside the window")
            } else {
                XCTFail("\(tag) \(name): no pen")
            }

            let verso = app.descendants(matching: .any)["page.verso"]
            let recto = app.descendants(matching: .any)["page.recto"]
            if spread {
                XCTAssertTrue(verso.waitForExistence(timeout: 3), "\(tag) \(name): spread size but no page.verso")
                XCTAssertTrue(recto.waitForExistence(timeout: 1), "\(tag) \(name): spread size but no page.recto")
                if verso.exists, recto.exists {
                    // Contents' and Threads' left page is laid out 14pt wider, under the spine, and clipped at the
                    // midline (BookSplit): its accessibility frame keeps those 14pt, though nothing shows there.
                    XCTAssertLessThanOrEqual(verso.frame.maxX, recto.frame.minX + 15,
                                             "\(tag) \(name): the pages overlap")
                    XCTAssertGreaterThan(recto.frame.minX, frame.midX - w * 0.1,
                                         "\(tag) \(name): the right page doesn't start near the middle")
                }
            } else {
                XCTAssertFalse(recto.exists, "\(tag) \(name): a right page at a single-column size")
                if let widest = try widestText() {
                    XCTAssertLessThanOrEqual(widest.width, maxColumn,
                                             "\(tag) \(name): \"\(widest.label.prefix(40))\" runs \(Int(widest.width))pt wide")
                }
            }
        }
    }

    /// Turns the simulator so the frame fits (the iPad mini needs landscape for the open spread), or skips.
    private func orient(toFit size: CGSize) throws {
        func fits(_ r: CGRect) -> Bool { r.width >= size.width && r.height >= size.height }
        let window = app.windows.firstMatch
        XCTAssertTrue(window.waitForExistence(timeout: 15))
        if fits(window.frame) { return }
        let turned = CGSize(width: window.frame.height, height: window.frame.width)
        guard turned.width >= size.width, turned.height >= size.height else {
            throw XCTSkip("a \(Int(size.width))×\(Int(size.height)) frame doesn't fit this \(Int(window.frame.width))×"
                          + "\(Int(window.frame.height)) screen: use an iPad simulator and an IPAD=1 build")
        }
        XCUIDevice.shared.orientation = size.width > size.height ? .landscapeLeft : .portrait
        let deadline = Date().addingTimeInterval(5)
        // Mid-turn the window reports in-between frames, some of them big enough already: wait for it to come to
        // rest at the screen's origin as well.
        func settled(_ r: CGRect) -> Bool { fits(r) && abs(r.minX) < 1 && abs(r.minY) < 1 }
        while !settled(window.frame), Date() < deadline { RunLoop.current.run(until: Date().addingTimeInterval(0.2)) }
        XCTAssertTrue(settled(window.frame), "rotating didn't make room for the frame")
    }

    private func tabButton(_ name: String) -> XCUIElement {
        // Turning Point's tab reads "The Turn" once the book is in recto mode.
        let labels = name == "Turning Point" ? ["Turning Point", "The Turn"] : [name]
        return app.buttons.matching(NSPredicate(format: "label IN %@", labels)).firstMatch
    }

    /// Lets the tab switch and any spring settle before measuring or shooting.
    private func settle() {
        RunLoop.current.run(until: Date().addingTimeInterval(0.8))
    }

    /// The widest run of prose, from one accessibility snapshot (querying each element is slow). Only labels of
    /// a sentence or more count: a centred head like PartTitle spans its row by design, which isn't a measure.
    private func widestText() throws -> (width: CGFloat, label: String)? {
        var best: (width: CGFloat, label: String)?
        func walk(_ s: XCUIElementSnapshot) {
            if s.elementType == .staticText, s.label.count >= 60, s.frame.width > (best?.width ?? 0) {
                best = (s.frame.width, s.label)
            }
            s.children.forEach(walk)
        }
        walk(try app.snapshot())
        return best
    }

    /// The frame's part of a full-screen screenshot. Drawing (not CGImage cropping) honours the image's
    /// orientation, which is what makes a landscape shot come out upright.
    private func cropped(_ image: UIImage, to frame: CGRect, in window: CGRect) -> UIImage {
        let format = UIGraphicsImageRendererFormat()
        format.scale = image.scale
        return UIGraphicsImageRenderer(size: frame.size, format: format).image { _ in
            image.draw(in: CGRect(origin: CGPoint(x: window.minX - frame.minX, y: window.minY - frame.minY),
                                  size: window.size))
        }
    }

    /// Attaches the shot to the result bundle and, given `VERSO_SHOT_DIR` (`TEST_RUNNER_VERSO_SHOT_DIR` to
    /// xcodebuild; scripts/sim.sh sets it), writes it there as a PNG.
    private func save(_ image: UIImage, as name: String) {
        let a = XCTAttachment(image: image)
        a.name = name
        a.lifetime = .keepAlways
        add(a)
        guard let dir = ProcessInfo.processInfo.environment["VERSO_SHOT_DIR"], !dir.isEmpty else { return }
        do {
            let url = URL(fileURLWithPath: dir, isDirectory: true)
            try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
            guard let png = image.pngData() else { return XCTFail("\(name): couldn't encode the PNG") }
            try png.write(to: url.appendingPathComponent("\(name).png"))
        } catch {
            XCTFail("\(name): couldn't write to VERSO_SHOT_DIR: \(error)")
        }
    }
}
