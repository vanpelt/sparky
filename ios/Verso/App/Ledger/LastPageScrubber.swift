import SwiftUI
import VersoCore

/// § The Last Page: a strip of chapter numerals that snaps; a sentence reads the choice back.
struct LastPageScrubber: View {
    let l: LedgerResult
    @Environment(AppModel.self) private var model
    /// The pick (-1 = "Now"), in the model so a fold, which rebuilds the Ledger, keeps it (`PageState.lastPage`).
    private var selected: Int? {
        get { model.pages.lastPage }
        nonmutating set { model.pages.lastPage = newValue }
    }
    /// The strip's own width. Half of it, less half a numeral, either side lets the first and last numerals
    /// reach the centre at any width (a fixed 120 only managed it on a phone).
    /// Starts at a 402pt phone's column so the first layout is already centred.
    @State private var stripWidth: CGFloat = 354
    private static let cell: CGFloat = 64

    var body: some View {
        SectionTitle("The Last Page")
        let choices = l.lastPage
        ScrollView(.horizontal) {
            HStack(spacing: 0) {
                ForEach(choices) { c in
                    let on = (selected ?? -1) == c.id
                    VStack(spacing: 4) {
                        Text(c.label)
                            .font(c.index == nil ? VersoFont.his(20) : VersoFont.hero(on ? 34 : 24))
                            .foregroundStyle(on ? Palette.oxblood : Palette.fadedInk)
                            .frame(height: 40)
                        Rectangle().fill(on ? Palette.oxblood : Palette.rule).frame(width: on ? 18 : 6, height: on ? 2 : 1)
                    }
                    .frame(width: Self.cell)
                    .contentShape(Rectangle())
                    .onTapGesture { withAnimation(.smooth) { selected = c.id } }
                    .id(c.id)
                    .accessibilityLabel(c.index == nil ? "Now" : "After chapter \(c.index! + 1)")
                    .accessibilityAddTraits(on ? .isSelected : [])
                }
            }
            .scrollTargetLayout()
        }
        .contentMargins(.horizontal, max(0, (stripWidth - Self.cell) / 2), for: .scrollContent)
        .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { stripWidth = $0 }
        .scrollIndicators(.hidden)
        .scrollTargetBehavior(.viewAligned)
        .scrollPosition(id: Binding { selected } set: { selected = $0 }, anchor: .center)
        .sensoryFeedback(.selection, trigger: selected)
        .frame(height: 60)
        .mask(LinearGradient(colors: [.clear, .black, .black, .clear], startPoint: .leading, endPoint: .trailing))

        if let c = choices.first(where: { $0.id == (selected ?? -1) }) {
            sentence(c)
                .font(VersoFont.his(18)).foregroundStyle(Palette.ink)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.top, 14)
                .animation(.smooth(duration: 0.4), value: c.id)
        }
    }

    @ViewBuilder func sentence(_ c: LastPageChoice) -> some View {
        let back = c.daysBack.formatted(.number.grouping(.automatic))
        VStack(alignment: .leading, spacing: 8) {
            if c.index == nil {
                HStack(alignment: .firstTextBaseline, spacing: 4) {
                    Text("Ending now leaves")
                    MoneyText(c.forfeit, style: .compact).foregroundStyle(Palette.oxblood)
                    Text("on the table")
                }
                Text("and gives you back \(back) days, at")
                HStack(spacing: 4) { MoneyText(c.impliedRate, style: .full).foregroundStyle(Palette.oxblood); Text("a day.") }
            } else {
                HStack(alignment: .firstTextBaseline, spacing: 4) {
                    Text("Ending after Chapter \(c.label) keeps")
                    MoneyText(c.kept, style: .compact).foregroundStyle(Palette.oxblood)
                }
                HStack(alignment: .firstTextBaseline, spacing: 4) {
                    Text("leaves")
                    MoneyText(c.forfeit, style: .compact).foregroundStyle(Palette.oxblood)
                    Text("on the table,")
                }
                if c.daysBack > 0 {
                    Text("and gives you back \(back) days, at")
                    HStack(spacing: 4) { MoneyText(c.impliedRate, style: .full).foregroundStyle(Palette.oxblood); Text("a day.") }
                } else {
                    Text("and reads Book One to the end.")
                }
                Text("You'd leave on \(DateText.long(c.leaveOn)).").font(VersoFont.body(14)).foregroundStyle(Palette.fadedInk)
            }
        }
    }
}
