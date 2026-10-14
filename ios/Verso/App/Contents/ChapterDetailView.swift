import SwiftUI
import VersoCore

/// One chapter (vest tranche): what it pays, what staying for it costs, and its weight.
struct ChapterDetailView: View {
    let index: Int
    @Environment(AppModel.self) private var model
    @FocusState private var titleFocused: Bool
    @FocusState private var epigraphFocused: Bool
    @Environment(\.bookPage) private var bookPage

    /// The chapter as the reader has it, unsaved edits and all. It lives in the model (`PageState.chapters`), not
    /// here: a fold rebuilds this page, and the rebuilt one must not read the book back before the save lands.
    private var meta: ChapterMeta {
        get { model.pages.chapter(index, in: model) }
        nonmutating set { model.pages.chapters[index] = newValue }
    }
    private var metaBinding: Binding<ChapterMeta> { Binding { meta } set: { meta = $0 } }

    var body: some View {
        let ledger = model.board.ledger
        let schedule = ledger ?? model.scheduleLedger
        Page(folio: model.board.folio) {
            // The right page of a spread: the left one's head already reads "BOOK ONE".
            HeadBar(bookPage == .recto ? "CHAPTER \(Roman.chapter(index))" : "BOOK ONE · CHAPTER \(Roman.chapter(index))")
        } content: {
            if let schedule, schedule.tranches.indices.contains(index) {
                let t = schedule.tranches[index]
                titlePage(t)
                if t.isVested { vested(t, ledger: ledger) } else { unvested(t, ledger: ledger) }
                DinkusRule()
                SectionTitle("Notes", glyph: Glyph.pilcrow)
                PaperField(label: nil, text: notes, prompt: "What do you want to have done by the end of this chapter?",
                           axis: .vertical, id: "chapter-notes-\(index)", onCommit: save)
            } else {
                Aside("This chapter isn't in the schedule.")
            }
        }
        .navigationBarTitleDisplayMode(.inline)
        .onDisappear(perform: save)
    }

    var notes: Binding<String> { Binding { meta.notes ?? "" } set: { meta.notes = $0.isEmpty ? nil : $0 } }
    var title: Binding<String> { Binding { meta.title ?? "" } set: { meta.title = $0.isEmpty ? nil : $0 } }
    var epigraph: Binding<String> { Binding { meta.epigraph ?? "" } set: { meta.epigraph = $0.isEmpty ? nil : $0 } }

    func save() { model.pages.saveChapter(index, in: model) }

    @ViewBuilder func titlePage(_ t: TrancheRow) -> some View {
        VStack(spacing: 8) {
            Text("CHAPTER").runningHeadStyle(Palette.oxblood, size: 12)
            Text(t.numeral).font(VersoFont.hero(88)).foregroundStyle(t.isVested ? Palette.fadedInk : Palette.oxblood)
                .heroCap().heroReveal()
            TextField("", text: title, prompt: Text("Untitled").foregroundStyle(Palette.rule))
                .displayFont(24).italic().multilineTextAlignment(.center)
                .focused($titleFocused)
                .keepsFocus("chapter-title-\(index)", $titleFocused)
                .onSubmit(save)
            TextField("", text: epigraph, prompt: Text("an epigraph for this chapter").foregroundStyle(Palette.rule), axis: .vertical)
                .font(VersoFont.his(16)).foregroundStyle(Palette.fadedInk).multilineTextAlignment(.center)
                .lineLimit(1...4)
                .padding(.horizontal, 24)
                .focused($epigraphFocused)
                .submitLabel(.done)
                .returnEndsEditing(text: epigraph, focused: $epigraphFocused)
                .keepsFocus("chapter-epigraph-\(index)", $epigraphFocused)
                .onSubmit(save)
            Text(DateText.long(t.date)).runningHeadStyle(size: 11).padding(.top, 6)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 24)
    }

    // MARK: Vested

    @ViewBuilder func vested(_ t: TrancheRow, ledger: LedgerResult?) -> some View {
        VStack(spacing: 6) {
            Text("Read.").font(VersoFont.hero(64)).foregroundStyle(Palette.ink).heroCap()
            Text("vested \(DateText.long(t.date))").font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
        }
        .frame(maxWidth: .infinity).padding(.bottom, 12)
        DinkusRule()
        SectionTitle("What it paid")
        let px = meta.priceAtVest ?? ledger?.price
        PaysTable(tranche: t, priceRatio: px.flatMap { p in ledger.map { p / max(0.0001, $0.price) } } ?? 1,
                  wages: nil, hasPrice: ledger != nil)
        OptionalMoneyRow(label: "Price at vest", value: metaBinding.priceAtVest)
            .padding(.top, 12)
            .onSubmit(save)
        Aside(meta.priceAtVest == nil ? "Valued at today's price until you set the price at vest." : "Valued at the price at vest.")
    }

    // MARK: Unvested

    @ViewBuilder func unvested(_ t: TrancheRow, ledger: LedgerResult?) -> some View {
        let line = ledger?.chapter(t.index) ?? model.scheduleLedger?.chapter(t.index)
        DinkusRule()
        SectionTitle("What this chapter pays")
        PaysTable(tranche: t, priceRatio: 1, wages: line.map { (ledger?.wageRateAfter ?? 0) * Double($0.gap) },
                  hasPrice: ledger != nil, gap: line?.gap)

        if let line {
            DinkusRule()
            SectionTitle("What staying for it costs")
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text("\(line.gap)").font(VersoFont.hero(54)).foregroundStyle(Palette.ink).heroCap()
                Text(line.gap == 1 ? "day" : "days").font(VersoFont.his(20)).foregroundStyle(Palette.fadedInk)
                Spacer()
            }
            HStack(spacing: 4) {
                Text("of Book One, at")
                MoneyText(ledger == nil ? nil : line.rate, style: .full).foregroundStyle(Palette.oxblood)
                Text("a day.")
            }
            .font(VersoFont.body(16)).foregroundStyle(Palette.ink)

            if ledger != nil {
                DinkusRule()
                SectionTitle("Its weight")
                VStack(alignment: .leading, spacing: 14) {
                    WeightLine(fraction: line.ebbWeight, text: "to all of Book One you\u{2019}ll have banked", verb: "adds")
                    WeightLine(fraction: line.shareOfRemaining, text: "of what\u{2019}s still at stake", verb: "")
                }
                Aside("Every chapter pays about the same. What shrinks is how much each one adds to what you'll have.")
                    .padding(.top, 10)
            }
        }
    }
}

/// "adds 1⁄6 to all of Book One you'll have banked" — the fraction in Didot.
struct WeightLine: View {
    let fraction: Double
    let text: String
    let verb: String
    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            if !verb.isEmpty { Text(verb).font(VersoFont.his(18)).foregroundStyle(Palette.fadedInk) }
            Text(UnitFraction.render(fraction)).font(VersoFont.hero(44)).foregroundStyle(Palette.oxblood).heroCap()
            Text(text).font(VersoFont.his(18)).foregroundStyle(Palette.ink).fixedSize(horizontal: false, vertical: true)
        }
        .accessibilityElement(children: .combine)
    }
}

/// The ledger table: Revest, RSU, (Salary & bonus), Total.
struct PaysTable: View {
    let tranche: TrancheRow
    /// Scales today's values to another price (price at vest).
    let priceRatio: Double
    let wages: Double?
    let hasPrice: Bool
    var gap: Int? = nil

    var body: some View {
        Grid(alignment: .trailing, horizontalSpacing: 12, verticalSpacing: 8) {
            GridRow {
                Text("").gridColumnAlignment(.leading)
                Text("SHARES").runningHeadStyle(size: 10)
                Text("PRE-TAX").runningHeadStyle(size: 10)
                Text("AFTER").runningHeadStyle(size: 10)
            }
            row("Revest", shares: tranche.revestShares, pre: tranche.revestPre, after: tranche.revestAfter)
            row("RSU", shares: tranche.rsuShares, pre: tranche.rsuPre, after: tranche.rsuAfter)
            if let wages {
                GridRow {
                    Text(gap.map { "Pay, \($0) days" } ?? "Salary & bonus").font(VersoFont.body(16)).gridColumnAlignment(.leading)
                    Text("")
                    Text("")
                    MoneyText(hasPrice ? wages : nil, style: .full, afterTax: false)
                }
            }
            GridRow { DoubleRule().gridCellColumns(4) }
            GridRow {
                Text("Total").font(VersoFont.serif(.body, weight: .semibold))
                SharesText(tranche.shares)
                MoneyText(hasPrice ? tranche.pre * priceRatio : nil, style: .full, afterTax: false)
                MoneyText(hasPrice ? tranche.after * priceRatio + (wages ?? 0) : nil, style: .full, afterTax: false)
                    .foregroundStyle(Palette.oxblood)
            }
        }
        .font(VersoFont.ledger(.subheadline))
        .foregroundStyle(Palette.ink)
        .lineLimit(1).minimumScaleFactor(0.7)
    }

    func row(_ label: String, shares: Double, pre: Double, after: Double) -> some View {
        GridRow {
            Text(label).font(VersoFont.body(16))
            SharesText(shares)
            MoneyText(hasPrice ? pre * priceRatio : nil, style: .full, afterTax: false)
            MoneyText(hasPrice ? after * priceRatio : nil, style: .full, afterTax: false)
        }
    }
}

#Preview {
    NavigationStack { ChapterDetailView(index: 6) }
        .environment(AppModel.preview)
        .environment(\.versoVeiled, false)
}
