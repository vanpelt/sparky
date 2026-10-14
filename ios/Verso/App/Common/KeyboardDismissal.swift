import UIKit

/// One rule for the whole app: a tap anywhere that isn't a text field puts the keyboard away.
///
/// Per-field fixes kept missing cases (the number pad has no return key at all; a paging TabView swallows
/// taps meant for the page behind it), so this lives on the window instead, where every screen and sheet
/// gets it. The recognizer never cancels or delays touches and recognizes alongside everything else, so the
/// tap still lands on whatever button it hit.
@MainActor
enum KeyboardDismissal {
    private static let delegate = Delegate()

    static func install() {
        for case let scene as UIWindowScene in UIApplication.shared.connectedScenes {
            for window in scene.windows where !(window.gestureRecognizers ?? []).contains(where: { $0.delegate === delegate }) {
                let tap = UITapGestureRecognizer(target: delegate, action: #selector(Delegate.tapped(_:)))
                tap.cancelsTouchesInView = false
                tap.delaysTouchesEnded = false
                tap.delegate = delegate
                window.addGestureRecognizer(tap)
            }
        }
    }

    private final class Delegate: NSObject, UIGestureRecognizerDelegate {
        @objc func tapped(_ g: UITapGestureRecognizer) { g.view?.endEditing(true) }

        /// Taps on a text input (or inside one) are how you move the caret; leave those alone.
        func gestureRecognizer(_ g: UIGestureRecognizer, shouldReceive touch: UITouch) -> Bool {
            var v = touch.view
            while let view = v {
                if view is UITextField || view is UITextView || view is UISearchBar { return false }
                v = view.superview
            }
            return true
        }

        func gestureRecognizer(_ g: UIGestureRecognizer,
                               shouldRecognizeSimultaneouslyWith other: UIGestureRecognizer) -> Bool { true }
    }
}
