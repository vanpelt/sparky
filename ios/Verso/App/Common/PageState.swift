import SwiftUI
import VersoCore

/// What the reader has open or half-written on a page, kept here instead of in the page's own views.
///
/// Folding or unfolding the Duo swaps Contents and Threads between one stack and a two-page split, and the
/// Ledger and Turning Point between one page and two (`BookSplit`, `Spread`). Their view trees are rebuilt, so
/// any `@State` in them starts over. Everything a reader would miss lives here, on the model, and the views
/// read and write it. None of it is saved: it lasts as long as the app runs, and never reaches the widgets.
@Observable @MainActor
final class PageState {
    /// Contents: "· · · all thirteen" has been opened.
    var showAllChapters = false
    /// Contents: the first line a long-press on the ❦ turned up.
    var openingLine: String?
    /// Threads: the list is being arranged.
    var arrangingThreads = false
    /// Ledger: the Last Page pick (-1 is "Now").
    var lastPage: Int? = -1
    /// Ledger: the what-if slider, as a share of the live price (-0.5…0.5).
    var whatIfShift: Double = 0
    /// Disclosures and toggles that are open, by key (`expanded(_:)`).
    var expanded: Set<String> = []

    /// Field text not yet saved, by field (`draft(_:saved:)`). An entry exists only while it differs from what
    /// is stored, and goes once its save lands (`commit(_:in:_:)`).
    var drafts: [String: String] = [:]
    /// Chapters' unsaved title, epigraph, notes and price at vest, by chapter. The hero's title field and the
    /// chapter's own page share it, so the right page of a spread and the pushed page in one column agree.
    var chapters: [Int: ChapterMeta] = [:]
    /// The Colophon's form while it is open (dropped when it is closed).
    var colophon: ColophonForm?

    /// The field that holds the keyboard, by key, and when its view last went away. A field that comes back
    /// (the same key, in the rebuilt tree) while it still holds it, or within a moment of its going, takes the
    /// keyboard back (`keepsFocus`).
    var focus: String?
    var focusLeftAt: Date?

    /// A disclosure's open state, as a binding.
    func expanded(_ key: String) -> Binding<Bool> {
        Binding { self.expanded.contains(key) } set: { on in
            if on { self.expanded.insert(key) } else { self.expanded.remove(key) }
        }
    }

    /// A text field's words: its draft while there is one, otherwise what is saved.
    func draft(_ key: String, saved: @escaping @MainActor () -> String) -> Binding<String> {
        Binding { self.drafts[key] ?? saved() } set: { new in
            self.drafts[key] = new == saved() ? nil : new
        }
    }

    /// Saves a draft through `op` and lets it go once the store holds it, unless it was typed on meanwhile.
    func commit(_ key: String, in model: AppModel, _ op: @escaping @Sendable (VersoStore) async throws -> Void) {
        let text = drafts[key]
        Task {
            await model.run(op)
            if drafts[key] == text { drafts[key] = nil }
        }
    }

    /// A chapter as the reader has it: the unsaved edits if there are any, otherwise the book's.
    func chapter(_ i: Int, in model: AppModel) -> ChapterMeta { chapters[i] ?? model.state.book.chapter(i) }

    /// Saves a chapter's edits; the draft goes once the store holds it, unless it was edited on meanwhile.
    func saveChapter(_ i: Int, in model: AppModel) {
        guard let m = chapters[i] else { return }
        guard m != model.state.book.chapter(i) else { chapters[i] = nil; return }
        Task {
            await model.run { try await $0.setChapter(m) }
            if chapters[i] == m { chapters[i] = nil }
        }
    }
}

/// The Colophon's form, as the reader has it.
struct ColophonForm: Equatable {
    var grant: Grant
    var hasGrant: Bool
    var settings: Settings
    var override: Double?
    var overrideOn: Bool
    var marginaliaTime: Date
    /// The grant has edits not yet set in type.
    var dirty: Bool
}

extension View {
    /// Keeps a field's keyboard across a fold: the field focused as its view goes away takes focus again when
    /// the rebuilt tree brings a field of the same key back, as long as that is within a second and a half. A
    /// field that loses focus while still on screen (scrolled away, ✓ tapped) lets the keyboard go for good.
    func keepsFocus(_ key: String?, _ focused: FocusState<Bool>.Binding) -> some View {
        modifier(KeepsFocus(key: key, focused: focused))
    }
}

private struct KeepsFocus: ViewModifier {
    let key: String?
    let focused: FocusState<Bool>.Binding
    @Environment(AppModel.self) private var model

    func body(content: Content) -> some View {
        // One shape whether or not there is a key: a field's focus mustn't hang on the key coming and going.
        content
            .onChange(of: focused.wrappedValue) { _, f in
                guard let key else { return }
                let pages = model.pages
                if f { pages.focus = key; pages.focusLeftAt = nil; return }
                // A beat later: a view going away loses focus before (or after) it disappears, and only a field
                // still on screen should let the keyboard go.
                Task { @MainActor in
                    try? await Task.sleep(for: .milliseconds(80))
                    if pages.focus == key, pages.focusLeftAt == nil { pages.focus = nil }
                }
            }
            .onAppear {
                let pages = model.pages
                // The rebuilt tree can appear before the old one disappears (no `focusLeftAt` yet) or just after.
                guard let key, pages.focus == key,
                      pages.focusLeftAt.map({ Date().timeIntervalSince($0) < 1.5 }) ?? true else { return }
                pages.focusLeftAt = nil
                // Once the keyboard the fold put away has finished going (RootView lets it go before the swap):
                // raised again mid-way down, the keyboard's animation never reports finished.
                Task { @MainActor in
                    try? await Task.sleep(for: .milliseconds(600))
                    focused.wrappedValue = true
                }
            }
            .onDisappear {
                if let key, model.pages.focus == key { model.pages.focusLeftAt = Date() }
            }
    }
}
