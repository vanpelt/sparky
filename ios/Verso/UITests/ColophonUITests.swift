import XCTest

/// The Colophon: reachable from a visible control on Contents, and its number pads (no return key) let go.
final class ColophonUITests: XCTestCase {
    private var app: XCUIApplication!

    override func setUpWithError() throws {
        continueAfterFailure = false
        app = XCUIApplication()
        app.launchArguments = ["-versoSkipOnboarding", "YES"]
        app.launch()
        try skipOnDuoInnerDisplay(app)
    }

    func testGearOpensTheColophonAndTheNumberPadDismisses() throws {
        let gear = app.buttons["Colophon"]
        XCTAssertTrue(gear.waitForExistence(timeout: 15), "no settings control on Contents")
        XCTAssertGreaterThanOrEqual(gear.frame.width, 44, "settings target too small to hit")
        gear.tap()
        let numberField = app.textFields["number:Revest shares"]
        for _ in 0..<6 where !numberField.isHittable { app.swipeUp() }
        XCTAssertTrue(numberField.isHittable, "no editable figure in the Colophon")
        numberField.tap()
        let kb = app.keyboards.firstMatch
        XCTAssertTrue(kb.waitForExistence(timeout: 3))
        let a = XCTAttachment(screenshot: app.screenshot()); a.name = "colophon-numpad"; a.lifetime = .keepAlways; add(a)
        // A number pad has no return key: tapping the page has to be the way out.
        app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.12)).tap()
        XCTAssertTrue(kb.waitForNonExistence(timeout: 3), "number pad has no way out")
    }

    func testTabBarSwitchesPages() throws {
        for tab in ["Ledger", "Threads", "Turning Point", "Contents"] {
            let b = app.buttons[tab]
            XCTAssertTrue(b.waitForExistence(timeout: 10), "no \(tab) tab")
            b.tap()
            XCTAssertTrue(b.isSelected, "\(tab) tab didn't select")
        }
        let a = XCTAttachment(screenshot: app.screenshot()); a.name = "contents-tabbar"; a.lifetime = .keepAlways; add(a)
    }
}
