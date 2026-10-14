import SwiftUI
import VersoCore

/// A running head with the veil eye at the trailing edge, never wider than the text column.
///
/// On an open book it sets itself as a printed book's heads are: the verso's flush left with its folio at the
/// outer corner and the spread's one eye (and gear) at the inner end; the recto's flush right, folio outermost,
/// with only its `end` control (a menu, say) at the inner end. `end` sits after the eye in one column.
struct HeadBar<Trailing: View, End: View>: View {
    let text: String
    let trailing: Trailing
    let end: End
    var onSettings: (() -> Void)? = nil
    @Environment(AppModel.self) private var model
    @Environment(\.bookPage) private var bookPage
    @Environment(\.headFolio) private var folio

    init(_ text: String, onSettings: (() -> Void)? = nil, @ViewBuilder trailing: () -> Trailing,
         @ViewBuilder end: () -> End) {
        self.text = text; self.onSettings = onSettings; self.trailing = trailing(); self.end = end()
    }

    var body: some View {
        HStack(spacing: 6) {
            if bookPage == .recto {
                end
                Spacer(minLength: 8)
                Text(text).runningHeadStyle().lineLimit(1).minimumScaleFactor(0.8)
                trailing
                folioMark.padding(.leading, 10)
            } else {
                if bookPage == .verso { folioMark.padding(.trailing, 10) }
                Text(text).runningHeadStyle().lineLimit(1).minimumScaleFactor(0.8)
                trailing
                Spacer(minLength: 8)
                VeilEye()
                if let onSettings {
                    // The Colophon used to hide behind a 1-glyph fleuron in the running head. A plain gear, the
                    // same weight as the eye beside it, with a full 44pt target.
                    Button(action: onSettings) {
                        Image(systemName: "gearshape")
                            .font(.system(size: 19, weight: .light))
                            .foregroundStyle(Palette.fadedInk)
                            .frame(width: 44, height: 44)
                            .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel("Colophon")
                    .accessibilityHint("Grant, price, taxes and reminders")
                }
                end
            }
        }
        // On a spread both heads are as tall as the verso's gear row, so the two share a line.
        .frame(minHeight: bookPage == .single ? 32 : 44)
        // Held to the text column, so on a wide window the eye and gear stay over the text instead of drifting
        // to the window's corners.
        .frame(maxWidth: Measure.text)
        .frame(maxWidth: .infinity)
    }

    @ViewBuilder private var folioMark: some View {
        if let folio {
            Text(folio.formatted(.number.grouping(.automatic)))
                .font(VersoFont.serif(.caption)).foregroundStyle(Palette.fadedInk)
                .monospacedDigit()
                .accessibilityLabel("Page \(folio)")
        }
    }
}

extension HeadBar where End == EmptyView {
    init(_ text: String, onSettings: (() -> Void)? = nil, @ViewBuilder trailing: () -> Trailing) {
        self.init(text, onSettings: onSettings, trailing: trailing) { EmptyView() }
    }
}

extension HeadBar where Trailing == EmptyView, End == EmptyView {
    init(_ text: String, onSettings: (() -> Void)? = nil) { self.init(text, onSettings: onSettings) { EmptyView() } }
}

/// The eye in the running head: lifts the veil for the session.
struct VeilEye: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        if model.state.book.settings.veilByDefault {
            Button {
                withAnimation(.easeOut(duration: 0.45)) { model.veilLifted.toggle() }
            } label: {
                Image(systemName: model.veilLifted ? "eye" : "eye.slash")
                    .font(.system(size: 14, weight: .light))
                    .foregroundStyle(Palette.fadedInk)
                    .frame(width: 32, height: 32)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(model.veilLifted ? "Veil figures" : "Lift the veil")
            .sensoryFeedback(.selection, trigger: model.veilLifted)
        }
    }
}
