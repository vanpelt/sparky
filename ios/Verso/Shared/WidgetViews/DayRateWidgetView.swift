import WidgetKit
import SwiftUI
import VersoCore

/// Day Rate (§9.2): what Book One pays to stay, at today's price, stated the way the Ledger is set
/// (after tax or pre-tax, per day or per working day).
struct DayRateWidgetView: View {
    let entry: VersoEntry
    var family: WidgetFamily? = nil

    init(entry: VersoEntry, family: WidgetFamily? = nil) {
        self.entry = entry; self.family = family
    }

    var body: some View {
        FamilyReader(explicit: family) { f in
            content(f).environment(\.versoVeiled, !entry.moneyVisible(f))
        }
    }

    /// The Ledger's "after tax / pre-tax" and "per day / per working day", carried in the snapshot.
    private var view: DayRateView { entry.snapshot.dayRate }
    private var chapterLabel: String { entry.live.next.map { "Ch. \($0.numeral)" } ?? "Book One" }
    private var chaptersLeft: Int { entry.live.schedule?.chaptersLeft ?? 0 }

    @ViewBuilder
    func content(_ f: WidgetFamily) -> some View {
        if entry.unreadable {
            WidgetRefreshNotice(family: f, head: "BOOK ONE PAYS")
        } else if entry.isRecto {
            blank("Book One is closed. Nothing left to pay you to stay.", f)
        } else if !entry.hasGrant {
            blank("Set your grant in Verso.", f)
        } else if let l = entry.live.ledger, !l.isComplete {
            switch f {
            case .accessoryInline: inline(l, f)
            case .accessoryRectangular: rectangular(l, f)
            case .systemSmall: small(l)
            case .systemMedium:
                HStack(alignment: .top, spacing: 16) {
                    small(l)
                    Rectangle().fill(Palette.rule).frame(width: 0.5)
                    breakdown(l)
                }
            case .systemLarge: page(l, ahead: 6, xl: false)
            // The extra-large page has the height for every chapter ahead (thirteen at most), not "and n more".
            case .systemExtraLargePortrait: page(l, ahead: 13, xl: true)
            default: small(l)
            }
        } else if entry.live.ledger?.isComplete == true {
            blank("Book One is fully read. Staying pays salary only.", f)
        } else {
            blank("Set a price in Verso.", f)
        }
    }

    // MARK: - Families

    func inline(_ l: LedgerResult, _ f: WidgetFamily) -> some View {
        Group {
            if entry.moneyVisible(f) { Text("\(MoneyFormat.short(l.dayRate(view)))\(view.shortUnit) · \(chapterLabel)") }
            else { Text("\(chapterLabel) · \(chaptersLeft) \(chaptersLeft == 1 ? "chapter" : "chapters") left") }
        }
    }

    func rectangular(_ l: LedgerResult, _ f: WidgetFamily) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            if entry.moneyVisible(f) {
                Text("\(MoneyFormat.short(l.dayRate(view)))\(view.shortUnit) to stay")
                    .font(VersoFont.serif(.headline)).privacySensitive().widgetAccentable().lineLimit(1)
                if let e = l.enough {
                    Text("Enough \(MoneyFormat.short(e.line)) \(e.isAbove ? "▲" : "▼")")
                        .font(VersoFont.serif(.caption)).privacySensitive()
                }
            } else {
                let ebb = l.chapters.first.map { UnitFraction.render($0.ebbWeight) } ?? "—"
                let days = l.next?.daysToNext ?? 0
                Text("\(days) \(days == 1 ? "day" : "days") · \(chaptersLeft) \(chaptersLeft == 1 ? "chapter" : "chapters") left")
                    .font(VersoFont.serif(.headline)).widgetAccentable().lineLimit(1).minimumScaleFactor(0.8)
                Text("this chapter adds \(ebb)").font(VersoFont.serif(.caption)).lineLimit(1)
            }
            let p = entry.priceLine
            Text("\(entry.snapshot.ticker ?? "CRWV") · \(p.stale && !entry.snapshot.price.isManual ? "stale " : "")\(entry.snapshot.price.ageLabel(now: entry.date))")
                .font(VersoFont.serif(.caption2)).lineLimit(1)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    func small(_ l: LedgerResult) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("BOOK ONE PAYS").runningHeadStyle(size: 10)
            Spacer(minLength: 0)
            MoneyText(l.dayRate(view), style: .full, afterTax: false)
                .font(VersoFont.hero(38)).foregroundStyle(Palette.oxblood)
                .minimumScaleFactor(0.5).lineLimit(1)
                .widgetAccentable()
            Text("a \(view.unit) to stay\(view.afterTax ? "" : ", pre-tax")").font(VersoFont.his(13)).foregroundStyle(Palette.fadedInk)
            if let e = l.enough { enoughMarker(l, e).padding(.top, 6) }
            Spacer(minLength: 0)
            let p = entry.priceLine
            WidgetFooter(text: p.text, warn: p.stale)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
    }

    /// A hairline from 0 to a little past the larger of rate and line, with a tick at the Enough line and the
    /// day rate as a dot. ▲ when staying still beats the line, ▼ when it no longer does.
    func enoughMarker(_ l: LedgerResult, _ e: EnoughReading) -> some View {
        let span = max(l.dayRateAfter, e.line) * 1.2
        let xr = span > 0 ? l.dayRateAfter / span : 0
        let xe = span > 0 ? e.line / span : 0
        return VStack(alignment: .leading, spacing: 3) {
            GeometryReader { g in
                ZStack(alignment: .leading) {
                    Rectangle().fill(Palette.rule).frame(height: 0.5)
                    Rectangle().fill(Palette.ink).frame(width: 0.75, height: 8).offset(x: g.size.width * xe)
                    Circle().fill(Palette.oxblood).frame(width: 5, height: 5)
                        .offset(x: g.size.width * xr - 2.5).widgetAccentable()
                }
                .frame(maxHeight: .infinity)
            }
            .frame(height: 8)
            Text("\(e.isAbove ? "▲" : "▼") Enough line").runningHeadStyle(e.isAbove ? Palette.fadedInk : Palette.rust, size: 8)
        }
        .accessibilityElement()
        .accessibilityLabel(e.isAbove ? "Above your Enough line" : "Below your Enough line")
    }

    /// Right half of the medium widget: where the day rate comes from and how marginal this chapter is.
    func breakdown(_ l: LedgerResult) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(view.caption).runningHeadStyle(size: 9).lineLimit(1).minimumScaleFactor(0.8)
            row("Equity", l.equityRate(view))
            row("Pay", l.payRate(view))
            // Only stated after tax by the ledger; shown that way only when the rest is.
            if view.afterTax, let n = l.next { row("Ch. \(n.numeral) alone", view.scale(n.rate)) }
            Hairline()
            // A lump sum, not a rate: its own caption so it can't read as "per day".
            Text(view.afterTax ? "IN ALL, AFTER TAX" : "IN ALL, PRE-TAX").runningHeadStyle(size: 8).lineLimit(1)
            row("Walk away", l.walkAway(view))
            Spacer(minLength: 0)
            if let c = l.chapters.first {
                Text("this chapter adds \(UnitFraction.render(c.ebbWeight)) to all you'll have banked")
                    .font(VersoFont.his(11)).foregroundStyle(Palette.fadedInk)
                    .lineLimit(2).minimumScaleFactor(0.8)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    func row(_ label: String, _ v: Double, font: Font.TextStyle = .caption) -> some View {
        LeaderRow {
            Text(label).font(VersoFont.serif(font)).foregroundStyle(Palette.fadedInk).lineLimit(1)
        } trailing: {
            MoneyText(v, style: .short, afterTax: false).font(VersoFont.ledger(font)).foregroundStyle(Palette.ink)
        }
    }

    // MARK: - Large and extra-large: the Ledger's page

    /// The rate as a page heading, the Enough line drawn across the full measure, then where the rate comes from
    /// and what each chapter ahead is worth on its own. Large sets those two side by side; extra-large stacks them.
    func page(_ l: LedgerResult, ahead: Int, xl: Bool) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .firstTextBaseline) {
                Text("BOOK ONE PAYS").runningHeadStyle(size: 10)
                Spacer(minLength: 8)
                Text(view.caption).runningHeadStyle(size: 9).lineLimit(1).minimumScaleFactor(0.8)
            }
            MoneyText(l.dayRate(view), style: .full, afterTax: false)
                .font(VersoFont.hero(xl ? 72 : 54)).foregroundStyle(Palette.oxblood)
                .minimumScaleFactor(0.5).lineLimit(1)
                .widgetAccentable()
                .padding(.top, xl ? 14 : 6)
            Text("a \(view.unit) to stay\(view.afterTax ? "" : ", pre-tax")")
                .font(VersoFont.his(xl ? 16 : 14)).foregroundStyle(Palette.fadedInk).lineLimit(1).minimumScaleFactor(0.8)
            if let e = l.enough {
                enoughScale(l, e).padding(.top, xl ? 20 : 12)
            }
            Hairline().padding(.vertical, xl ? 18 : 12)
            if xl {
                VStack(alignment: .leading, spacing: 18) {
                    sources(l, font: .subheadline)
                    chaptersAhead(l, rows: ahead, font: .subheadline)
                }
            } else {
                HStack(alignment: .top, spacing: 16) {
                    sources(l, font: .caption)
                    Rectangle().fill(Palette.rule).frame(width: 0.5)
                    chaptersAhead(l, rows: ahead, font: .caption)
                }
                .fixedSize(horizontal: false, vertical: true)
            }
            Spacer(minLength: 0)
            let p = entry.priceLine
            WidgetFooter(text: p.text, warn: p.stale)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    /// The small widget's Enough marker drawn wide and labelled: the line as a tick with its figure under it, the
    /// day rate as the dot, and the margin between them said in words.
    func enoughScale(_ l: LedgerResult, _ e: EnoughReading) -> some View {
        let span = max(l.dayRateAfter, e.line) * 1.2
        let xr = span > 0 ? l.dayRateAfter / span : 0
        let xe = span > 0 ? e.line / span : 0
        return VStack(alignment: .leading, spacing: 4) {
            GeometryReader { g in
                ZStack(alignment: .leading) {
                    Rectangle().fill(Palette.rule).frame(height: 0.5)
                    Rectangle().fill(Palette.ink).frame(width: 0.75, height: 12).offset(x: g.size.width * xe)
                    Circle().fill(Palette.oxblood).frame(width: 7, height: 7)
                        .offset(x: g.size.width * xr - 3.5).widgetAccentable()
                }
                .frame(maxHeight: .infinity)
            }
            .frame(height: 12)
            HStack(alignment: .firstTextBaseline, spacing: 4) {
                Text("\(e.isAbove ? "▲" : "▼") ENOUGH").runningHeadStyle(e.isAbove ? Palette.fadedInk : Palette.rust, size: 9)
                MoneyText(e.line, style: .short, afterTax: false).font(VersoFont.ledger(.caption2)).foregroundStyle(Palette.ink)
                Spacer(minLength: 6)
                MoneyText(abs(e.margin), style: .short, afterTax: false).font(VersoFont.ledger(.caption2))
                    .foregroundStyle(e.isAbove ? Palette.ink : Palette.rust)
                Text(e.isAbove ? "above the line" : "below the line").font(VersoFont.his(11)).foregroundStyle(Palette.fadedInk)
            }
            .lineLimit(1).minimumScaleFactor(0.8)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(e.isAbove ? "Above your Enough line" : "Below your Enough line")
    }

    /// Where the rate comes from: the medium widget's breakdown, set one size up.
    func sources(_ l: LedgerResult, font: Font.TextStyle) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("WHERE IT COMES FROM").runningHeadStyle(size: 8).lineLimit(1)
            row("Equity", l.equityRate(view), font: font)
            row("Pay", l.payRate(view), font: font)
            Hairline()
            Text(view.afterTax ? "IN ALL, AFTER TAX" : "IN ALL, PRE-TAX").runningHeadStyle(size: 8).lineLimit(1)
            row("Walk away", l.walkAway(view), font: font)
        }
        .frame(maxWidth: .infinity, alignment: .topLeading)
    }

    /// Each chapter ahead on its own, at the Ledger's after-tax rate. Pre-tax there is no per-chapter rate, so
    /// the column shows what each adds to all he'll have banked instead.
    func chaptersAhead(_ l: LedgerResult, rows: Int, font: Font.TextStyle) -> some View {
        let ahead = Array(l.chapters.prefix(rows))
        return VStack(alignment: .leading, spacing: 6) {
            Text(view.afterTax ? "EACH CHAPTER ALONE" : "WHAT EACH ADDS").runningHeadStyle(size: 8).lineLimit(1)
            ForEach(ahead) { c in
                LeaderRow {
                    HStack(spacing: 5) {
                        Text(c.numeral).font(VersoFont.serif(font))
                            .foregroundStyle(c.index == l.next?.index ? Palette.oxblood : Palette.ink)
                        Text(c.date.formatted(.dateTime.month(.abbreviated).day()))
                            .font(VersoFont.serif(.caption2)).foregroundStyle(Palette.fadedInk)
                    }
                    .lineLimit(1)
                } trailing: {
                    if view.afterTax {
                        MoneyText(view.scale(c.rate), style: .short, afterTax: false)
                            .font(VersoFont.ledger(font)).foregroundStyle(Palette.ink)
                    } else {
                        Text("+\(UnitFraction.render(c.ebbWeight))").font(VersoFont.ledger(font)).foregroundStyle(Palette.fadedInk)
                    }
                }
            }
            if l.chapters.count > ahead.count {
                Text("and \(l.chapters.count - ahead.count) more").font(VersoFont.his(11)).foregroundStyle(Palette.fadedInk)
            }
        }
        .frame(maxWidth: .infinity, alignment: .topLeading)
    }

    @ViewBuilder
    func blank(_ line: String, _ f: WidgetFamily) -> some View {
        switch f {
        case .accessoryInline: Text(line)
        case .accessoryRectangular: Text(line).font(VersoFont.his(13)).lineLimit(3).frame(maxWidth: .infinity, alignment: .leading)
        case .systemSmall, .systemMedium: WidgetBlankPage(line: line, head: "BOOK ONE PAYS")
        case .systemLarge, .systemExtraLargePortrait: WidgetBlankPage(line: line, head: "BOOK ONE PAYS", large: true)
        default: WidgetBlankPage(line: line, head: "BOOK ONE PAYS")
        }
    }
}
