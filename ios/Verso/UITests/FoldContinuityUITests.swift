import XCTest

/// Folding and unfolding the Duo resizes the running app, and Verso rebuilds Contents, Threads, the Ledger and
/// the Turning Point between one page and two. Nothing a reader has open or half-written may go with them.
///
/// Proxied on an iPad simulator (an `IPAD=1` build) through the `-versoFrame` harness: the app launches in the
/// closed 466×678 window, and `-versoFoldTo 951x669` adds a fold button (`debug.fold`) that resizes the frame in
/// place. Skips where both sizes can't fit the screen in landscape (an iPhone, or a build without `IPAD=1`).
@MainActor
final class FoldContinuityUITests: XCTestCase {
    private var app: XCUIApplication!

    func testPagesKeepWhatIsOpenAcrossAFold() throws {
        XCUIDevice.shared.orientation = .landscapeLeft
        addTeardownBlock { @MainActor in XCUIDevice.shared.orientation = .portrait }
        app = XCUIApplication()
        app.launchArguments = ["-versoSample", "YES", "-versoSkipOnboarding", "YES", "-versoUnveil", "YES",
                               "-versoFrame", "466x678", "-versoFoldTo", "951x669"]
        app.launch()
        try skipOnDuoInnerDisplay(app)
        let window = app.windows.firstMatch
        XCTAssertTrue(window.waitForExistence(timeout: 15))
        guard window.frame.width >= 951, window.frame.height >= 678 else {
            throw XCTSkip("466x678 and 951x669 don't both fit this simulator (\(window.frame.size))")
        }
        let fold = app.buttons["debug.fold"]
        XCTAssertTrue(fold.waitForExistence(timeout: 5), "no fold button: is this a Debug build?")
        let verso = app.descendants(matching: .any)["page.verso"]
        let recto = app.descendants(matching: .any)["page.recto"]

        // Closed: open "all thirteen", open the current chapter, and leave half a note in it, keyboard up.
        let more = app.buttons.matching(NSPredicate(format: "label BEGINSWITH %@", "· · · all")).firstMatch
        XCTAssertTrue(more.waitForExistence(timeout: 15), "no \"all thirteen\" on Contents")
        more.tap()
        XCTAssertTrue(more.waitForNonExistence(timeout: 3), "\"all thirteen\" didn't expand")
        let current = app.buttons.matching(NSPredicate(format: "value == %@", "current chapter")).firstMatch
        XCTAssertTrue(current.waitForExistence(timeout: 3), "no current chapter row")
        current.tap()
        let notes = field("chapter-notes-")
        XCTAssertTrue(notes.waitForExistence(timeout: 5), "the chapter didn't open")
        scroll(to: notes, in: app)
        notes.tap()
        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 3))
        app.typeText("half a thought")

        // Unfold.
        fold.tap()
        XCTAssertTrue(recto.waitForExistence(timeout: 5), "unfolded, but no right page")
        let notesOpen = field("chapter-notes-")
        XCTAssertTrue(notesOpen.waitForExistence(timeout: 3), "unfolded, the chapter is no longer open")
        XCTAssertEqual(notesOpen.value as? String, "half a thought", "unfolding lost the unfinished note")
        XCTAssertTrue(recto.frame.contains(notesOpen.frame) || !notesOpen.isHittable,
                      "the open chapter isn't on the right page")
        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 3), "unfolding put the keyboard away")
        XCTAssertFalse(more.exists, "unfolding folded \"all thirteen\" away again")
        app.typeText("\n")
        XCTAssertTrue(app.keyboards.firstMatch.waitForNonExistence(timeout: 3))

        // Open: pick a Last Page, start arranging the threads, leave half a stay case, keyboard up.
        tab("Ledger").tap()
        XCTAssertTrue(recto.waitForExistence(timeout: 3))
        // The choice beside "Now", which the strip starts centred on, so it is on screen.
        let choices = app.descendants(matching: .any).matching(NSPredicate(format: "label BEGINSWITH %@", "After chapter"))
        XCTAssertTrue(choices.firstMatch.waitForExistence(timeout: 5), "no Last Page choices")
        let label = choices.element(boundBy: 0).label
        let pick = app.descendants(matching: .any).matching(NSPredicate(format: "label == %@", label)).firstMatch
        // At the head of the right page. The strip's mask leaves its cells unhittable to XCUITest, so tap the spot.
        pick.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        XCTAssertTrue(waitUntil { pick.isSelected }, "the Last Page pick didn't take")

        tab("Threads").tap()
        let arrange = app.buttons["Arrange threads"]
        XCTAssertTrue(arrange.waitForExistence(timeout: 5))
        arrange.tap()
        XCTAssertTrue(app.buttons["Done arranging"].waitForExistence(timeout: 3))

        tab("Turning Point").tap()
        let stay = field("stay-case")
        XCTAssertTrue(stay.waitForExistence(timeout: 5))
        scroll(to: stay, in: verso)
        stay.tap()
        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 3))
        app.typeText("the people ")
        let typed = stay.value as? String

        // Fold.
        fold.tap()
        XCTAssertTrue(recto.waitForNonExistence(timeout: 5), "folded, but still two pages")
        let stayClosed = field("stay-case")
        XCTAssertTrue(stayClosed.waitForExistence(timeout: 3))
        XCTAssertEqual(stayClosed.value as? String, typed, "folding lost the unfinished stay case")
        XCTAssertTrue(app.keyboards.firstMatch.waitForExistence(timeout: 3), "folding put the keyboard away")
        app.typeText("\n")
        XCTAssertTrue(app.keyboards.firstMatch.waitForNonExistence(timeout: 3))

        tab("Threads").tap()
        XCTAssertTrue(app.buttons["Done arranging"].waitForExistence(timeout: 3), "folding stopped the arranging")

        tab("Ledger").tap()
        let pickClosed = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label == %@", label)).firstMatch
        XCTAssertTrue(pickClosed.waitForExistence(timeout: 3))
        XCTAssertTrue(waitUntil { pickClosed.isSelected }, "folding lost the Last Page pick")

        tab("Contents").tap()
        let notesClosed = field("chapter-notes-")
        XCTAssertTrue(notesClosed.waitForExistence(timeout: 3), "folded, the chapter is no longer open")
        XCTAssertEqual(notesClosed.value as? String, "half a thought", "the note didn't keep")
        // Last: a screen screenshot leaves the app reporting animations in flight (60s idle waits) for a while.
        shoot("fold-closed-contents")
    }

    // MARK: -

    /// A PaperField by identifier prefix: a one-line field is a text field, a wrapping one a text view.
    private func field(_ id: String) -> XCUIElement {
        let p = NSPredicate(format: "identifier BEGINSWITH %@", id)
        let f = app.textFields.matching(p).firstMatch
        return f.exists ? f : app.textViews.matching(p).firstMatch
    }

    private func tab(_ name: String) -> XCUIElement {
        let labels = name == "Turning Point" ? ["Turning Point", "The Turn"] : [name]
        let t = app.buttons.matching(NSPredicate(format: "label IN %@", labels)).firstMatch
        XCTAssertTrue(t.waitForExistence(timeout: 5), "no \(name) tab")
        return t
    }

    private func scroll(to element: XCUIElement, in page: XCUIElement) {
        for _ in 0..<12 where !element.isHittable { page.swipeUp(velocity: .slow) }
        XCTAssertTrue(element.isHittable, "\(element) never scrolled into view")
    }

    private func waitUntil(_ timeout: TimeInterval = 3, _ condition: () -> Bool) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while !condition() {
            if Date() > deadline { return false }
            RunLoop.current.run(until: Date().addingTimeInterval(0.2))
        }
        return true
    }

    private func shoot(_ name: String) {
        let a = XCTAttachment(screenshot: XCUIScreen.main.screenshot())
        a.name = name
        a.lifetime = .keepAlways
        add(a)
    }
}
