import XCTest

extension XCTestCase {
    /// Skips on the iPhone Duo simulator's inner display (Xcode 27.1), where XCUITest can't drive the app: it treats
    /// the outer display as the screen (678x466 against a 951x669 window), reports element frames in the wrong
    /// space, and no tap reaches the app, by element or by coordinate. Check the open Duo by screenshot instead
    /// (`xcrun simctl io <udid> screenshot --display=3`), and its layout through the iPad `-versoFrame` harness.
    func skipOnDuoInnerDisplay(_ app: XCUIApplication) throws {
        let window = app.windows.firstMatch
        guard window.waitForExistence(timeout: 15) else { return }
        let screen = XCUIScreen.main.screenshot().image.size
        let w = window.frame.size
        let matches = abs(max(screen.width, screen.height) - max(w.width, w.height)) < 2
        if !matches {
            throw XCTSkip("the app's window (\(w)) isn't on XCUITest's screen (\(screen)): an iPhone Duo's inner display")
        }
    }
}
