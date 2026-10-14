import SwiftUI
import VersoCore

/// The Ledger: what Book One still pays, set like an old accounts book.
struct LedgerView: View {
    @Environment(AppModel.self) private var model
    /// The two toggles are a saved setting, not screen state: the Day Rate widget states the rate the same way.
    private var view: DayRateView { model.state.book.settings.dayRate }
    private var afterTax: Bool { view.afterTax }
    private var perWorkingDay: Bool { view.perWorkingDay }
    private func setView(_ change: @escaping @Sendable (inout DayRateView) -> Void) {
        model.perform { try await $0.updateBook(.settings) { change(&$0.settings.dayRate) } }
    }

    var body: some View {
        let board = model.ledgerBoard
        // One essay, split editorially when the book lies open: what you are paid on the left page, the
        // instruments for weighing it (the last page, the marginal chapter, the line, the what-if) on the right.
        // Closed, verso then recto run on in one column, exactly the old order.
        Spread(folio: model.board.folio, rectoHead: model.isRecto ? "THE CHAPTERS" : "WEIGHING IT") {
            HeadBar(model.isRecto ? "WHAT BOOK ONE LEFT YOU" : "THE LEDGER") { PriceMark() }
        } verso: {
            if model.isRecto {
                RectoLedger(part: .sum)
            } else if let l = board.ledger {
                if model.whatIfPrice != nil { WhatIfChip() }
                if l.isComplete {
                    VStack(spacing: 10) {
                        Text(Glyph.fleuron).font(VersoFont.hero(64)).foregroundStyle(Palette.gilt)
                        Text("Book One is fully read.").font(VersoFont.his(24))
                        Aside("Staying pays only salary now.")
                    }
                    .frame(maxWidth: .infinity).padding(.vertical, 40)
                } else {
                    hero(l)
                    DinkusRule()
                    RemainingTable(l: l)
                    if let next = l.next {
                        DinkusRule()
                        NextChapterSection(next: next, l: l)
                    }
                }
            } else {
                empty
            }
        } recto: {
            if model.isRecto {
                RectoLedger(part: .chapters)
            } else if let l = board.ledger {
                Seam()
                if !l.isComplete {
                    LastPageScrubber(l: l)
                    DinkusRule()
                    MarginalChart(l: l)
                    if let e = l.enough {
                        DinkusRule()
                        EnoughSection(e: e, l: l)
                    }
                    DinkusRule()
                }
                WhatIfSlider(livePrice: model.board.ledger?.price ?? l.price)
                Text("\(MoneyFormat.footnote) Price held flat — futr is the deep model.")
                    .font(VersoFont.his(13)).foregroundStyle(Palette.fadedInk)
                    .padding(.top, 28)
            }
        }
        .navigationBarHidden(true)
        .refreshable { await model.refreshPrice(force: true) }
    }

    /// No grant or no price: one quiet prompt. On an open spread it stands alone on the left page.
    var empty: some View {
        VStack(spacing: 14) {
            Text("—").font(VersoFont.hero(96)).foregroundStyle(Palette.rule)
            Text(model.state.book.grant == nil ? "No grant yet." : "No price yet.")
                .font(VersoFont.his(22))
            QuietButton(model.state.book.grant == nil ? "Set yours in the Colophon ›" : "Set a price ›", color: Palette.oxblood) {
                if model.state.book.grant == nil { model.tab = .contents; model.contentsPath = [.colophon] }
                else { model.sheet = .price }
            }
        }
        .frame(maxWidth: .infinity).padding(.vertical, 60)
    }

    func hero(_ l: LedgerResult) -> some View {
        let rate = l.dayRate(view)
        return VStack(spacing: 14) {
            Text("You are paid").font(VersoFont.his(20)).foregroundStyle(Palette.fadedInk).padding(.top, 24)
            MoneyText(rate, style: .full, afterTax: afterTax)
                .font(VersoFont.hero(64)).foregroundStyle(Palette.oxblood)
                .lineLimit(1).minimumScaleFactor(0.5)
                .heroCap().heroReveal()
                .animation(.smooth(duration: 0.5), value: rate)
            Text(perWorkingDay ? "a working day to keep writing Book One." : "a day to keep writing Book One.")
                .font(VersoFont.his(20)).foregroundStyle(Palette.ink).multilineTextAlignment(.center)
            // One line at default sizes; stacks into two once Dynamic Type makes the pair too wide.
            ViewThatFits(in: .horizontal) {
                HStack(spacing: 18) { toggles }
                VStack(spacing: 8) { toggles }
            }
            .padding(.top, 4)
            HStack(spacing: 4) {
                Text("equity")
                MoneyText(l.equityRate(view), style: .full, afterTax: false)
                Text("· pay")
                MoneyText(l.payRate(view), style: .full, afterTax: false)
            }
            .font(VersoFont.body(15)).foregroundStyle(Palette.fadedInk)
        }
        .frame(maxWidth: .infinity)
    }

    @ViewBuilder var toggles: some View {
        TextToggle(options: [(true, "after tax"), (false, "pre-tax")],
                   selection: Binding(get: { afterTax }, set: { v in setView { $0.afterTax = v } }))
        TextToggle(options: [(false, "per day"), (true, "per working day")],
                   selection: Binding(get: { perWorkingDay }, set: { v in setView { $0.perWorkingDay = v } }))
    }

}

// MARK: - Running-head price

/// "CRWV $—.— ·" — the trailing dot is gilt when fresh, rust with an age when stale. Tap → price sheet.
struct PriceMark: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        let p = model.state.price
        let now = model.now()
        let fresh = p.isFresh(now: now), stale = p.isStale(now: now, calendar: model.calendar)
        Button { model.sheet = .price } label: {
            HStack(spacing: 4) {
                Text("· \(model.state.book.settings.ticker) \(MoneyFormat.price(p.effective))")
                    .runningHeadStyle(Palette.ink)
                    .contentTransition(.numericText(value: p.effective ?? 0))
                if p.isManual {
                    Text("manual").runningHeadStyle(Palette.rust, size: 10)
                } else if stale {
                    Text("· \(p.ageLabel(now: now))").runningHeadStyle(Palette.rust, size: 10)
                } else {
                    Circle().fill(fresh ? Palette.gilt : Palette.fadedInk).frame(width: 4, height: 4)
                        .phaseAnimator(fresh ? [1.0, 0.3] : [1.0]) { v, o in v.opacity(o) } animation: { _ in .easeInOut(duration: 1.2) }
                }
            }
            .frame(minHeight: 32)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("Price \(MoneyFormat.price(p.effective)), \(p.ageLabel(now: now))")
    }
}

// MARK: - Remaining in Book One

struct RemainingTable: View {
    let l: LedgerResult
    var body: some View {
        SectionTitle("Remaining in Book One")
        Grid(alignment: .trailing, horizontalSpacing: 12, verticalSpacing: 9) {
            GridRow {
                Text("").gridColumnAlignment(.leading)
                Text("SHARES").runningHeadStyle(size: 10)
                Text("PRE-TAX").runningHeadStyle(size: 10)
                Text("AFTER").runningHeadStyle(size: 10)
            }
            GridRow {
                Text("Revest").font(VersoFont.body(16))
                SharesText(l.remRevestShares)
                MoneyText(l.remRevestPre, style: .compact, afterTax: false)
                MoneyText(l.remRevestAfter, style: .compact, afterTax: false)
            }
            GridRow {
                Text("RSU").font(VersoFont.body(16))
                SharesText(l.remRsuShares)
                MoneyText(l.remRsuPre, style: .compact, afterTax: false)
                MoneyText(l.remRsuAfter, style: .compact, afterTax: false)
            }
            GridRow {
                VStack(alignment: .leading, spacing: 0) {
                    Text("Salary & bonus").font(VersoFont.body(16))
                    Text("through \(DateText.monthYear(l.finalDate))").font(VersoFont.body(12)).foregroundStyle(Palette.fadedInk)
                }
                Text("")
                MoneyText(l.wagesPre, style: .compact, afterTax: false)
                MoneyText(l.wagesAfter, style: .compact, afterTax: false)
            }
            GridRow { DoubleRule().gridCellColumns(4) }
            GridRow {
                Text("Total").font(VersoFont.serif(.body, weight: .semibold))
                SharesText(l.remShares)
                MoneyText(l.walkAwayPre, style: .compact, afterTax: false)
                MoneyText(l.walkAwayAfter, style: .compact, afterTax: true).foregroundStyle(Palette.oxblood)
            }
        }
        .font(VersoFont.ledger(.subheadline))
        .foregroundStyle(Palette.ink)
        .lineLimit(1).minimumScaleFactor(0.7)

        HStack(spacing: 4) {
            Text("\(l.fuel.formatted(.percent.precision(.fractionLength(0...1)))) of the grant is still unread ·")
            Text("\(l.chaptersLeft) chapters")
        }
        .font(VersoFont.his(15)).foregroundStyle(Palette.fadedInk).padding(.top, 12)

        if l.rolloverShares > 0 {
            Well {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Rollover grant · already yours").runningHeadStyle(size: 10)
                    HStack(spacing: 4) {
                        MoneyText(l.rolloverValue, style: .compact, afterTax: true)
                            .font(VersoFont.ledger(.body)).foregroundStyle(Palette.ink)
                        Text(l.rolloverPending ? "vests 2026 regardless" : "vested").font(VersoFont.his(15)).foregroundStyle(Palette.fadedInk)
                    }
                    Text("Excluded from every walking-away figure.").font(VersoFont.body(12)).foregroundStyle(Palette.fadedInk)
                }
            }
            .padding(.top, 14)
        }
    }
}

// MARK: - Next chapter

struct NextChapterSection: View {
    let next: NextVest
    let l: LedgerResult
    var body: some View {
        SectionTitle("Next chapter")
        HStack(alignment: .lastTextBaseline) {
            VStack(alignment: .leading, spacing: 0) {
                Text("CHAPTER \(next.numeral)").runningHeadStyle(Palette.oxblood)
                Text(DateText.long(next.date)).font(VersoFont.serif(.title3)).italic()
            }
            Spacer()
            Text("\(next.daysToNext)").font(VersoFont.hero(52)).monospacedDigit()
                .contentTransition(.numericText(value: Double(next.daysToNext))).heroCap()
            Text("days").font(VersoFont.his(16)).foregroundStyle(Palette.fadedInk)
        }
        VStack(alignment: .leading, spacing: 6) {
            // Same basis as Chapter detail's "What this chapter pays": equity plus the pay earned on the way,
            // so "It pays" ÷ the day count is exactly "Per day until then".
            LeaderRow { Text("Its equity").font(VersoFont.body(16)).foregroundStyle(Palette.fadedInk) } trailing: {
                MoneyText(next.after, style: .full).font(VersoFont.ledger(.body)).foregroundStyle(Palette.fadedInk)
            }
            LeaderRow { Text("Pay, \(next.daysToNext) \(next.daysToNext == 1 ? "day" : "days")").font(VersoFont.body(16)).foregroundStyle(Palette.fadedInk) } trailing: {
                MoneyText(l.wageRateAfter * Double(next.daysToNext), style: .full).font(VersoFont.ledger(.body)).foregroundStyle(Palette.fadedInk)
            }
            LeaderRow { Text("It pays").font(VersoFont.body(16)) } trailing: {
                MoneyText(next.rate * Double(next.daysToNext), style: .full).font(VersoFont.ledger(.body))
            }
            LeaderRow { Text("Per day until then").font(VersoFont.body(16)) } trailing: {
                MoneyText(next.rate, style: .full).font(VersoFont.ledger(.body)).foregroundStyle(Palette.oxblood)
            }
            LeaderRow { Text("Next launch window").font(VersoFont.body(16)) } trailing: {
                Text(DateText.dayMonth(next.launchWindow)).font(VersoFont.ledger(.body)).foregroundStyle(Palette.gilt)
            }
        }
        .padding(.top, 10)
        Aside("This rises as the vest gets closer: the same lump spread over fewer days. The cheapest days to leave are the days just after a vest.")
            .padding(.top, 10)
    }
}

// MARK: - Enough Line

struct EnoughSection: View {
    let e: EnoughReading
    let l: LedgerResult
    @Environment(AppModel.self) private var model
    var ticker: String { model.state.book.settings.ticker }
    var body: some View {
        SectionTitle("Against your Enough Line")
        HStack(spacing: 6) {
            Text(e.isAbove ? "Above the line by" : "Below the line by")
            MoneyText(abs(e.margin), style: .full, afterTax: false).foregroundStyle(e.isAbove ? Palette.oxblood : Palette.verdigris)
            Text("a day.")
        }
        .font(VersoFont.his(19))
        EnoughGauge(rate: l.dayRateAfter, line: e.line).padding(.vertical, 14)
        Group {
            switch e.crossover {
            case .price(let p):
                Text("If \(ticker) were below \(Text(MoneyFormat.price(p)).font(VersoFont.ledger(.body).bold()).foregroundStyle(Palette.oxblood)), staying would pay you less than your Enough Line.")
            case .payAloneClears:
                Text("Your salary alone clears your Enough Line; the stock can\u{2019}t push you below it.")
            case .none:
                Text("Nothing left to vest; only pay counts now.")
            }
        }
        .font(VersoFont.body(16)).foregroundStyle(Palette.ink)
        HStack(spacing: 4) {
            Text("$1 of \(ticker) is")
            MoneyText(e.priceSensitivity, style: .compact)
            Text("of what remains.")
        }
        .font(VersoFont.body(13)).foregroundStyle(Palette.fadedInk).padding(.top, 6)
    }
}

/// A hairline scale with a gilt tick for the Enough Line and an oxblood mark for the day rate.
struct EnoughGauge: View {
    let rate: Double
    let line: Double
    var body: some View {
        GeometryReader { g in
            let top = max(rate, line) * 1.25
            let x = { (v: Double) in g.size.width * CGFloat(v / max(1, top)) }
            ZStack(alignment: .leading) {
                Hairline().frame(maxHeight: .infinity)
                Rectangle().fill(Palette.oxblood).frame(width: x(rate), height: 2)
                Rectangle().fill(Palette.gilt).frame(width: 1.5, height: 18).offset(x: x(line))
                Text("enough").runningHeadStyle(Palette.gilt, size: 9).offset(x: x(line) - 18, y: 16)
            }
        }
        .frame(height: 30)
        .accessibilityHidden(true)
    }
}

// MARK: - What-if

struct WhatIfSlider: View {
    let livePrice: Double
    @Environment(AppModel.self) private var model
    /// In the model (`PageState.whatIfShift`): the hypothetical price outlives a fold, so its slider must too.
    private var pct: Double {
        get { model.pages.whatIfShift }
        nonmutating set { model.pages.whatIfShift = newValue }
    }

    var body: some View {
        SectionTitle("What if")
        Slider(value: Binding { pct } set: { pct = $0 }, in: -0.5...0.5, step: 0.05) {
            Text("What-if price")
        } minimumValueLabel: {
            Text("−50%").font(VersoFont.body(12)).foregroundStyle(Palette.fadedInk)
        } maximumValueLabel: {
            Text("+50%").font(VersoFont.body(12)).foregroundStyle(Palette.fadedInk)
        }
        .tint(Palette.rust)
        .sensoryFeedback(.selection, trigger: pct)
        .onChange(of: pct) { _, v in
            withAnimation(.smooth(duration: 0.5)) { model.whatIfPrice = abs(v) < 0.001 ? nil : livePrice * (1 + v) }
        }
        .onChange(of: model.whatIfPrice) { _, p in if p == nil { pct = 0 } }
        Aside(pct == 0 ? "Slide to re-read every figure at another price. Never saved; never reaches the widgets."
                       : "At \(MoneyFormat.price(livePrice * (1 + pct))), \(pct > 0 ? "+" : "")\(Int((pct * 100).rounded()))% from today.")
    }
}

struct WhatIfChip: View {
    @Environment(AppModel.self) private var model
    var body: some View {
        if let p = model.whatIfPrice {
            Button { withAnimation(.smooth) { model.whatIfPrice = nil } } label: {
                Text("HYPOTHETICAL \(MoneyFormat.price(p)) ×").runningHeadStyle(Palette.paper, size: 11)
                    .padding(.horizontal, 10).padding(.vertical, 5)
                    .background(Palette.rust, in: Capsule())
            }
            .buttonStyle(.plain)
            .frame(maxWidth: .infinity)
            .padding(.top, 8)
            .transition(.scale.combined(with: .opacity))
        }
    }
}

// MARK: - Recto

/// "What Book One left you": frozen vested totals at each chapter's price at vest. In two parts, so an open
/// spread can set the sum on the left page and the chapters that made it on the right.
struct RectoLedger: View {
    enum Part { case sum, chapters }
    let part: Part
    @Environment(AppModel.self) private var model
    var body: some View {
        if let g = model.state.book.grant {
            let closed = model.state.book.state.closedAt ?? model.now()
            let px = model.state.price.effective ?? 0
            let l = Ledger.compute(grant: g, price: px, today: closed, calendar: model.calendar)
            let rows = l.tranches.filter(\.isVested)
            switch part {
            case .sum:
                let total = rows.reduce(0.0) { acc, t in
                    let p = model.state.book.chapter(t.index).priceAtVest ?? px
                    return acc + t.after * (px > 0 ? p / px : 0)
                }
                VStack(spacing: 6) {
                    Text("Book One left you").font(VersoFont.his(20)).foregroundStyle(Palette.fadedInk).padding(.top, 24)
                    MoneyText(total, style: .full).font(VersoFont.hero(56)).foregroundStyle(Palette.gilt).heroCap()
                    Text("across \(rows.count) chapters").font(VersoFont.his(17))
                }
                .frame(maxWidth: .infinity)
            case .chapters:
                Seam()
                ForEach(rows) { t in
                    let p = model.state.book.chapter(t.index).priceAtVest ?? px
                    LeaderRow {
                        Text("\(t.numeral).  \(model.state.book.chapter(t.index).title ?? "")").font(VersoFont.serif(.body))
                    } trailing: {
                        HStack(spacing: 10) {
                            Text(MoneyFormat.price(p)).font(VersoFont.ledger(.caption)).foregroundStyle(Palette.fadedInk)
                            MoneyText(t.after * (px > 0 ? p / px : 0), style: .compact, afterTax: false).font(VersoFont.ledger(.subheadline))
                        }
                    }
                    .padding(.vertical, 6)
                }
            }
        }
    }
}

#Preview {
    NavigationStack { LedgerView() }
        .environment(AppModel.preview)
        .environment(\.versoVeiled, false)
}
