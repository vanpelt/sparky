import WidgetKit
import SwiftUI
import VersoCore

/// Turning Point (§9.3): his clauses as ticks on an arc, and the stage word. Never a score or a percentage.
struct TurningPointWidgetView: View {
    let entry: VersoEntry
    var family: WidgetFamily? = nil

    init(entry: VersoEntry, family: WidgetFamily? = nil) {
        self.entry = entry; self.family = family
    }

    var body: some View {
        FamilyReader(explicit: family) { f in content(f) }
    }

    private var c: ClarityResult { entry.live.clarity }
    private var pulseLine: String {
        entry.snapshot.pulseLine ?? entry.snapshot.pulseState.copy(weeks: entry.snapshot.pulseStateWeeks)
    }

    @ViewBuilder
    func content(_ f: WidgetFamily) -> some View {
        if entry.unreadable {
            WidgetRefreshNotice(family: f, head: "THE TURNING POINT")
        } else if c.total == 0 && !entry.isRecto {
            noClauses(f)
        } else {
            switch f {
            case .accessoryInline:
                Text(entry.isRecto ? "Recto · Book Two" : "\(c.met)/\(c.total) · \(c.stage.title)")
            case .accessoryCircular: circular
            case .accessoryRectangular: rectangular
            case .systemSmall: arc.frame(maxWidth: .infinity, maxHeight: .infinity)
            case .systemMedium: medium
            case .systemLarge: page(rows: 6, xl: false)
            case .systemExtraLargePortrait: page(rows: 14, xl: true)
            default: arc.frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
    }

    private var regardless: TickState? {
        c.regardlessCount > 0 ? (c.regardlessMet ? .met : .unmet) : nil
    }

    var arc: some View {
        VStack(spacing: 2) {
            Text(entry.isRecto ? "RECTO" : "THE TURNING POINT").runningHeadStyle(size: 9).lineLimit(1)
                .minimumScaleFactor(0.8)
            Spacer(minLength: 0)
            TickArc(ticks: c.tickStates, regardless: regardless)
                .frame(maxWidth: 120)
                .overlay(alignment: .bottom) {
                    Text(c.total > 0 ? "\(c.met) of \(c.total)" : Glyph.fleuron)
                        .font(VersoFont.ledger(.caption2)).foregroundStyle(Palette.fadedInk)
                        .padding(.bottom, 4)
                }
            Text(c.stage.title).font(VersoFont.serif(.subheadline)).italic()
                .foregroundStyle(Palette.stage(c.stage))
                .lineLimit(1).minimumScaleFactor(0.7)
                .widgetAccentable()
            Spacer(minLength: 0)
        }
    }

    /// The arc itself with "met/total" — never a letter, so it can't be mistaken for a Thread ring's initial.
    var circular: some View {
        ZStack {
            AccessoryWidgetBackground()
            VStack(spacing: 0) {
                TickArc(ticks: c.tickStates, regardless: regardless, lineWidth: 2)
                    .frame(maxWidth: 48)
                Text("\(c.met)/\(c.total)").font(VersoFont.ledger(.caption)).monospacedDigit()
                    .padding(.top, -10)
            }
        }
        .widgetAccentable()
        .accessibilityElement()
        .accessibilityLabel("\(c.met) of \(c.total) clauses, \(c.stage.title)")
    }

    var rectangular: some View {
        VStack(alignment: .leading, spacing: 1) {
            Text("THE TURNING POINT").font(VersoFont.runningHead(11)).tracking(1).widgetAccentable()
            Text(entry.isRecto ? "Recto · Book Two" : "\(c.met) of \(c.total) · \(c.stage.title)")
                .font(VersoFont.serif(.headline)).lineLimit(1).minimumScaleFactor(0.8)
            Text(pulseLine).font(VersoFont.serif(.caption2)).lineLimit(1).minimumScaleFactor(0.8)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// The clauses the snapshot thought nearest, re-marked with their live state (a date clause may have
    /// flipped since the app last ran).
    private var nearest: [ClauseLine] {
        entry.snapshot.nearestClauses.map { line in
            var l = line
            l.state = c.states[line.id] ?? line.state
            return l
        }
    }

    var medium: some View {
        HStack(alignment: .center, spacing: 14) {
            // A cap, not a fixed width: a narrower medium slot (iPhone Duo's outer display) gives the clauses room.
            arc.frame(minWidth: 96, maxWidth: 120)
            VStack(alignment: .leading, spacing: 6) {
                if nearest.isEmpty {
                    Text(c.met == c.total && c.total > 0 ? "Every clause is met." : "No clauses waiting.")
                        .font(VersoFont.his(13)).foregroundStyle(Palette.ink)
                }
                ForEach(nearest) { line in
                    HStack(alignment: .firstTextBaseline, spacing: 5) {
                        Text(line.state.mark).font(VersoFont.serif(.caption))
                            .foregroundStyle(line.state == .unmet ? Palette.fadedInk : Palette.gilt)
                        VStack(alignment: .leading, spacing: 0) {
                            Text(line.text).font(VersoFont.his(13)).foregroundStyle(Palette.ink).lineLimit(2)
                            if !line.hint.isEmpty {
                                Text(line.hint).font(VersoFont.serif(.caption2)).foregroundStyle(Palette.fadedInk).lineLimit(1)
                            }
                        }
                    }
                }
                Spacer(minLength: 0)
                if let s = entry.snapshot.latestSentence {
                    Text("“\(s)”").font(VersoFont.his(11)).foregroundStyle(Palette.fadedInk).lineLimit(2)
                }
                if entry.live.marginaliaDue {
                    Link(destination: VersoWidgetURL.marginalia) {
                        Text("\(Glyph.pilcrow) MARGINALIA DUE").runningHeadStyle(Palette.oxblood, size: 9)
                            .padding(.horizontal, 7).padding(.vertical, 3)
                            .background(Capsule().fill(Palette.deckle))
                    }
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        }
    }

    // MARK: - Large and extra-large: the whole Turning Point

    /// His latest Marginalia sentence (or the book's epigraph) set as the page's epigraph, the arc as its
    /// frontispiece, then every clause in his words with its mark, the ordinary ones first and "Or, regardless —"
    /// after. `rows` caps the clause lines; the rest are counted, not dropped silently.
    func page(rows: Int, xl: Bool) -> some View {
        let ordinary = clauseLines.filter { $0.role != .regardless }
        let regardless = clauseLines.filter { $0.role == .regardless }
        let shownOrdinary = Array(ordinary.prefix(rows))
        let shownRegardless = Array(regardless.prefix(max(0, rows - shownOrdinary.count)))
        let hidden = clauseLines.count - shownOrdinary.count - shownRegardless.count
        return VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .firstTextBaseline) {
                Text(entry.isRecto ? "RECTO" : "THE TURNING POINT").runningHeadStyle(size: 10)
                Spacer(minLength: 8)
                if c.total > 0 {
                    Text("\(c.met) OF \(c.total)").runningHeadStyle(size: 10).monospacedDigit()
                }
            }
            if let s = entry.snapshot.latestSentence {
                epigraph(s, xl: xl).padding(.top, xl ? 18 : 10)
            }
            frontispiece(xl: xl).padding(.top, xl ? 18 : 8)
            Hairline().padding(.top, xl ? 16 : 8).padding(.bottom, xl ? 12 : 8)
            // Extra-large spends its height on air between the clauses (and a bigger gauge), not a blank foot.
            VStack(alignment: .leading, spacing: xl ? 16 : 6) {
                if clauseLines.isEmpty {
                    Text("No clauses waiting.").font(VersoFont.his(14)).foregroundStyle(Palette.ink)
                }
                ForEach(shownOrdinary) { clauseRow($0, xl: xl) }
                if !shownRegardless.isEmpty {
                    Text("OR, REGARDLESS \u{2014}").runningHeadStyle(Palette.rust, size: 8).padding(.top, 2)
                    ForEach(shownRegardless) { clauseRow($0, xl: xl) }
                }
                if hidden > 0 {
                    Text("and \(hidden) more in Verso").font(VersoFont.his(11)).foregroundStyle(Palette.fadedInk)
                }
            }
            Spacer(minLength: 0)
            if entry.live.marginaliaDue { marginaliaChip.frame(maxWidth: .infinity, alignment: .trailing) }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    /// Set right, in his italic, the way a book sets its epigraph. Unattributed on purpose: it is his latest
    /// Marginalia sentence or, before there is one, the book's own epigraph (`Board.footerEpigraph`).
    func epigraph(_ s: String, xl: Bool) -> some View {
        Text("“\(s)”").font(VersoFont.his(xl ? 17 : 14)).foregroundStyle(Palette.ink)
            .multilineTextAlignment(.trailing).lineLimit(xl ? 4 : 2)
            .frame(maxWidth: .infinity, alignment: .trailing)
            .padding(.leading, xl ? 40 : 56)
    }

    /// The arc beside the stage word on a large page; centred above it, larger, on an extra-large one.
    @ViewBuilder
    func frontispiece(xl: Bool) -> some View {
        let gauge = TickArc(ticks: c.tickStates, regardless: regardless, lineWidth: xl ? 3 : 2.5)
            .overlay(alignment: .bottom) {
                Text(c.total > 0 ? "\(c.met) of \(c.total)" : Glyph.fleuron)
                    .font(VersoFont.ledger(xl ? .footnote : .caption2)).foregroundStyle(Palette.fadedInk)
                    .padding(.bottom, xl ? 8 : 5)
            }
        let stage = Text(c.stage.title).font(VersoFont.serif(xl ? .title2 : .title3)).italic()
            .foregroundStyle(Palette.stage(c.stage))
            .lineLimit(1).minimumScaleFactor(0.7)
            .widgetAccentable()
        if xl {
            VStack(spacing: 6) {
                gauge.frame(maxWidth: 260)
                stage
                Text(pulseLine).font(VersoFont.serif(.caption)).foregroundStyle(Palette.fadedInk)
                    .multilineTextAlignment(.center).lineLimit(2)
            }
            .frame(maxWidth: .infinity)
        } else {
            HStack(alignment: .center, spacing: 16) {
                gauge.frame(minWidth: 104, maxWidth: 136)
                VStack(alignment: .leading, spacing: 4) {
                    stage
                    Text(pulseLine).font(VersoFont.serif(.caption2)).foregroundStyle(Palette.fadedInk)
                        .lineLimit(3).minimumScaleFactor(0.8)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    func clauseRow(_ line: ClauseLine, xl: Bool) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Text(line.state.mark).font(VersoFont.serif(xl ? .footnote : .caption))
                .foregroundStyle(line.state == .unmet ? Palette.fadedInk : Palette.gilt)
                .frame(width: 12)
            VStack(alignment: .leading, spacing: 0) {
                Text(line.text).font(VersoFont.his(xl ? 15 : 13))
                    .foregroundStyle(line.state == .unmet ? Palette.ink : Palette.fadedInk)
                    .lineLimit(xl ? 2 : 1)
                if !line.hint.isEmpty {
                    // Tracked small caps, as the app sets the same hint under a clause.
                    Text(line.hint).runningHeadStyle(size: xl ? 9 : 8).lineLimit(1).minimumScaleFactor(0.8)
                }
            }
        }
        .accessibilityElement(children: .combine)
    }

    private var marginaliaChip: some View {
        Link(destination: VersoWidgetURL.marginalia) {
            Text("\(Glyph.pilcrow) MARGINALIA DUE").runningHeadStyle(Palette.oxblood, size: 9)
                .padding(.horizontal, 7).padding(.vertical, 3)
                .background(Capsule().fill(Palette.deckle))
        }
    }

    /// Every clause in clause order with its live state. Hints exist only for the nearest unmet ones (the
    /// snapshot carries no others), so the rest show the mark and his words.
    private var clauseLines: [ClauseLine] {
        let hints = Dictionary(entry.snapshot.nearestClauses.map { ($0.id, $0.hint) }, uniquingKeysWith: { a, _ in a })
        return entry.snapshot.clauses.map { cl in
            ClauseLine(id: cl.id, text: cl.text, role: cl.role, state: c.states[cl.id] ?? .unmet, hint: hints[cl.id] ?? "")
        }
    }

    @ViewBuilder
    func noClauses(_ f: WidgetFamily) -> some View {
        switch f {
        case .accessoryInline: Text("No clauses yet")
        case .accessoryCircular: Text(Glyph.fleuron).font(VersoFont.display(22))
        case .accessoryRectangular:
            VStack(alignment: .leading, spacing: 1) {
                Text("THE TURNING POINT").font(VersoFont.runningHead(11)).tracking(1)
                Text("I go when…").font(VersoFont.his(14))
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        case .systemSmall, .systemMedium:
            WidgetBlankPage(line: "Write your first clause: “I go when…”", head: "THE TURNING POINT")
        case .systemLarge, .systemExtraLargePortrait:
            WidgetBlankPage(line: "Write your first clause: “I go when…”", head: "THE TURNING POINT", large: true)
        default: WidgetBlankPage(line: "Write your first clause: “I go when…”", head: "THE TURNING POINT")
        }
    }
}
