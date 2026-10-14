import XCTest

/// Drives the real first-run flow on whatever simulator runs it. The opening line once rendered nothing on
/// iOS 27 (zero-height field), and then trapped you: ✓ typed a newline and the keyboard covered the only button.
final class OnboardingUITests: XCTestCase {
    private var app: XCUIApplication!

    override func setUpWithError() throws {
        continueAfterFailure = false
        app = XCUIApplication()
        // A fresh install every time: the real store lives in the App Group, which survives an uninstall and
        // remembers the last run's onboarding.
        app.launchArguments = ["-versoFirstRun", "YES"]
        app.launch()
        try skipOnDuoInnerDisplay(app)
    }

    /// A fresh install plays a title card before the title page settles; a tap during that is lost.
    private func focusField() {
        XCTAssertTrue(field.waitForExistence(timeout: 15), "no opening-line field")
        for _ in 0..<5 {
            if field.isHittable { field.tap() }
            if app.keyboards.firstMatch.waitForExistence(timeout: 2) { return }
        }
        XCTFail("couldn't focus the opening line")
    }

    /// The opening line by name: Contents' own fields (a chapter title, once there is a grant) stay in the
    /// accessibility tree beneath the title page.
    private var field: XCUIElement {
        app.descendants(matching: .any).matching(identifier: "opening-line").firstMatch
    }

    func testOpeningLineShowsWhatYouType() throws {
        focusField()
        attach("before")
        app.typeText("The second book starts here")
        attach("typed")
        XCTAssertEqual(field.value as? String, "The second book starts here")
    }

    func testReturnFinishesTheLineAndYouCanTurnThePage() throws {
        focusField()
        app.typeText("The second book starts here\n")
        XCTAssertTrue(app.keyboards.firstMatch.waitForNonExistence(timeout: 3), "return didn't put the keyboard away")
        XCTAssertEqual(field.value as? String, "The second book starts here", "return was typed into the line")
        attach("after-return")
        let turn = app.buttons["Turn the page"]
        XCTAssertTrue(turn.isHittable)
        turn.tap()
        XCTAssertTrue(app.staticTexts["The grant"].waitForExistence(timeout: 5), "didn't reach the grant card")
    }

    func testTappingThePageDismissesTheKeyboard() throws {
        focusField()
        app.typeText("half a thought")
        XCTAssertTrue(app.keyboards.firstMatch.exists)
        // Blank paper between the running head and the title, away from every control.
        app.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.13)).tap()
        XCTAssertTrue(app.keyboards.firstMatch.waitForNonExistence(timeout: 3), "tapping the page didn't dismiss the keyboard")
    }

    func testWholeFirstRunLandsTheLineInOpeningLines() throws {
        focusField()
        app.typeText("It begins on a Tuesday.\n")
        app.buttons["Turn the page"].tap()
        XCTAssertTrue(app.staticTexts["The grant"].waitForExistence(timeout: 5))
        app.buttons["Turn the page"].tap()
        XCTAssertTrue(app.staticTexts["Threads"].waitForExistence(timeout: 5))
        // Thread presets are buttons inside a card that also dismisses the keyboard on tap: both must work.
        let move = app.buttons.containing(NSPredicate(format: "label CONTAINS 'Move'")).firstMatch
        XCTAssertTrue(move.waitForExistence(timeout: 3))
        move.tap()
        attach("threads-card")
        app.buttons["Start plotting"].tap()
        let turning = app.buttons["Turning Point"]
        XCTAssertTrue(turning.waitForExistence(timeout: 10), "onboarding didn't finish")
        turning.tap()
        let line = app.staticTexts.containing(NSPredicate(format: "label CONTAINS 'begins on a Tuesday'")).firstMatch
        for _ in 0..<6 where !line.exists { app.swipeUp() }
        attach("opening-lines")
        XCTAssertTrue(line.exists, "the first line never reached Opening Lines")
    }

    private func attach(_ name: String) {
        let a = XCTAttachment(screenshot: app.screenshot()); a.name = name; a.lifetime = .keepAlways; add(a)
    }
}
