import XCTest

/// Inline fields on the Turning Point: the keyboard must not bring stray chrome with it, and must go away.
final class TurningPointUITests: XCTestCase {
    private var app: XCUIApplication!

    override func setUpWithError() throws {
        continueAfterFailure = false
        app = XCUIApplication()
        app.launchArguments = ["-versoSkipOnboarding", "YES"]
        app.launch()
        try skipOnDuoInnerDisplay(app)
        let tab = app.buttons["Turning Point"]
        XCTAssertTrue(tab.waitForExistence(timeout: 15))
        tab.tap()
    }

    private var stayCase: XCUIElement {
        let f = app.textFields["stay-case"]
        return f.exists ? f : app.textViews["stay-case"]
    }

    private func focusStayCase() {
        for _ in 0..<6 where !stayCase.isHittable { app.swipeUp() }
        XCTAssertTrue(stayCase.isHittable, "stay-case field not on screen")
        stayCase.tap()
        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 3))
    }

    func testKeyboardBringsNoStrayDoneAndNoFloatingPen() throws {
        let pen = app.buttons["Log"]
        XCTAssertTrue(pen.waitForExistence(timeout: 5))
        focusStayCase()
        let kb = app.keyboards.firstMatch
        XCTAssertFalse(app.toolbars.buttons["Done"].exists, "the sealed letter's Done leaked onto another field")
        XCTAssertTrue(pen.waitForNonExistence(timeout: 2), "the pen floats over the keyboard")
        let a = XCTAttachment(screenshot: app.screenshot()); a.name = "stay-case-keyboard"; a.lifetime = .keepAlways; add(a)
        app.typeText("A founder I believe in\n")
        XCTAssertTrue(kb.waitForNonExistence(timeout: 3), "✓ didn't put the keyboard away")
        XCTAssertTrue(pen.waitForExistence(timeout: 2), "the pen didn't come back")
    }

    func testScrollingPutsTheKeyboardAway() throws {
        focusStayCase()
        // Drag the page, well above the field: on a short window (the closed iPhone Duo, 678pt) the middle of the
        // app, where `swipeUp()` starts, is the multi-line field itself, which scrolls its own text instead.
        let w = app.windows.firstMatch
        w.coordinate(withNormalizedOffset: CGVector(dx: 0.4, dy: 0.3))
            .press(forDuration: 0.05, thenDragTo: w.coordinate(withNormalizedOffset: CGVector(dx: 0.4, dy: 0.1)))
        XCTAssertTrue(app.keyboards.firstMatch.waitForNonExistence(timeout: 3), "scrolling didn't dismiss the keyboard")
    }

    func testTappingOffAFieldPutsTheKeyboardAway() throws {
        focusStayCase()
        app.staticTexts["THE TURNING POINT"].firstMatch.tap()
        XCTAssertTrue(app.keyboards.firstMatch.waitForNonExistence(timeout: 3), "tapping the page didn't dismiss the keyboard")
    }

    /// Opened into a spread (the Duo's inner display, proxied on an iPad through `-versoFrame`), the Turning Point
    /// is a book: his case (running head, clauses, the stay case) on the left page, the evidence (the Reading) on
    /// the right, the two pages side by side. Skips where 951x669 can't fit (an iPhone, or a build without `IPAD=1`).
    func testSpreadPutsTheCaseLeftAndTheEvidenceRight() throws {
        app.terminate()
        XCUIDevice.shared.orientation = .landscapeLeft
        addTeardownBlock { @MainActor in XCUIDevice.shared.orientation = .portrait }
        app.launchArguments = ["-versoSample", "YES", "-versoSkipOnboarding", "YES", "-versoUnveil", "YES",
                               "-versoFrame", "951x669"]
        app.launch()
        try skipOnDuoInnerDisplay(app)
        let window = app.windows.firstMatch.frame
        guard window.width >= 951, window.height >= 669 else {
            throw XCTSkip("a 951x669 window doesn't fit this simulator (\(window.size))")
        }
        let tab = app.buttons["Turning Point"]
        XCTAssertTrue(tab.waitForExistence(timeout: 15))
        tab.tap()

        let verso = app.descendants(matching: .any)["page.verso"]
        let recto = app.descendants(matching: .any)["page.recto"]
        XCTAssertTrue(verso.waitForExistence(timeout: 5), "no left page")
        XCTAssertTrue(recto.waitForExistence(timeout: 2), "no right page")
        XCTAssertLessThanOrEqual(verso.frame.maxX, recto.frame.minX + 1, "the pages overlap")

        let head = app.staticTexts["THE TURNING POINT"].firstMatch
        XCTAssertTrue(head.exists && verso.frame.contains(head.frame), "the running head isn't on the left page")
        let reading = app.staticTexts["The Reading"].firstMatch
        XCTAssertTrue(reading.waitForExistence(timeout: 2), "no Reading")
        XCTAssertTrue(recto.frame.contains(reading.frame), "the Reading isn't on the right page")
        for _ in 0..<6 where !stayCase.isHittable { verso.swipeUp() }
        XCTAssertTrue(stayCase.isHittable, "stay-case field not on screen")
        XCTAssertTrue(verso.frame.contains(stayCase.frame), "the stay case isn't on the left page")

        let a = XCTAttachment(screenshot: XCUIScreen.main.screenshot()); a.name = "turning-point-spread"
        a.lifetime = .keepAlways; add(a)
    }
}
