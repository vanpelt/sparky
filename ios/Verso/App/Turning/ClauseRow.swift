import SwiftUI
import VersoCore

/// One clause: drop-cap number, his text in italic, the kind hint in small caps, the state mark.
/// Manual clauses toggle on tap; the rest expand to show their evidence.
struct ClauseRow: View {
    let number: Int
    let clause: Clause
    @Environment(AppModel.self) private var model
    /// Open on its evidence: in the model, keyed by clause, so a fold keeps it open (`PageState.expanded`).
    private var expanded: Bool {
        get { model.pages.expanded.contains("clause-\(clause.id)") }
        nonmutating set { model.pages.expanded("clause-\(clause.id)").wrappedValue = newValue }
    }
    @State private var shimmer = 0
    @State private var lastState: TickState?

    var body: some View {
        let state = model.board.clauseState(clause)
        VStack(alignment: .leading, spacing: 0) {
            Button(action: tap) {
                HStack(alignment: .top, spacing: 12) {
                    Text("\(number)")
                        .font(VersoFont.hero(38)).foregroundStyle(clause.role == .regardless ? Palette.rust : Palette.oxblood)
                        .frame(width: 30, alignment: .leading)
                        .padding(.top, -4)
                    VStack(alignment: .leading, spacing: 5) {
                        Text(clause.text).font(VersoFont.his(18)).foregroundStyle(Palette.ink)
                            .fixedSize(horizontal: false, vertical: true)
                            .multilineTextAlignment(.leading)
                        HStack(spacing: 6) {
                            if clause.role == .required { Text(Glyph.section).font(VersoFont.serif(.caption)).foregroundStyle(Palette.oxblood) }
                            if clause.role == .regardless { Text("‼︎").font(VersoFont.serif(.caption)).foregroundStyle(Palette.rust) }
                            Text(model.board.clauseHint(clause)).runningHeadStyle(size: 10)
                        }
                    }
                    Spacer(minLength: 8)
                    Text(state.mark)
                        .font(VersoFont.serif(.title3))
                        .foregroundStyle(state == .holding ? Palette.gilt : (state == .met ? Palette.ink : Palette.rule))
                        .contentTransition(.symbolEffect(.replace))
                        .padding(.top, 2)
                        .accessibilityLabel(state == .holding ? "holding" : state == .met ? "met" : "unmet")
                }
                .padding(.vertical, 14)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .disabled(model.isRecto)

            if expanded { evidence(state).transition(.opacity.combined(with: .move(edge: .top))) }
        }
        .giltShimmer(shimmer)
        .sensoryFeedback(Haptics.clauseMet, trigger: shimmer)
        .onAppear { lastState = state }
        .onChange(of: state) { old, new in
            if old == .unmet && new != .unmet { shimmer += 1 }
            lastState = new
        }
        .accessibilityElement(children: .combine)
        .accessibilityHint(clause.kind.isManual ? "Tap to mark true or not yet" : "Tap for evidence")
    }

    func tap() {
        if case .manual(let checked) = clause.kind {
            let id = clause.id
            model.perform { try await $0.setManual(id, checked: !checked) }
        } else {
            withAnimation(.smooth(duration: 0.3)) { expanded.toggle() }
        }
    }

    @ViewBuilder func evidence(_ state: TickState) -> some View {
        let ctx = model.board.context
        let close = ClauseEvaluator.closeness(clause, ctx)
        VStack(alignment: .leading, spacing: 8) {
            Text(ClauseText.evidence(clause, board: model.board, veiled: model.veiled))
                .font(VersoFont.body(15)).foregroundStyle(Palette.ink)
            if state == .unmet {
                RibbonBar(progress: close, tint: Palette.fadedInk).frame(maxWidth: 180)
            }
            if let since = clause.metSince, state != .unmet {
                let days = Days.between(since, model.board.today, calendar: model.calendar)
                Text(state == .holding ? "True since \(DateText.long(since)) — holding."
                                       : "True since \(DateText.long(since)); holding after 14 days (\(max(0, 14 - days)) to go).")
                    .font(VersoFont.his(14)).foregroundStyle(state == .holding ? Palette.gilt : Palette.fadedInk)
            }
            HStack(spacing: 18) {
                QuietButton("Edit", color: Palette.oxblood) { model.sheet = .editClause(clause.id) }
                Text("written \(clause.createdAt.formatted(.dateTime.day().month(.abbreviated).year()))").runningHeadStyle(size: 9)
            }
        }
        .padding(.leading, 42)
        .padding(.bottom, 14)
    }
}

/// Human descriptions of clause kinds.
enum ClauseText {
    static func describe(_ k: ClauseKind, threads: [PlotThread]) -> String {
        switch k {
        case .date(let d): "on or after \(DateText.long(d))"
        case .tranche(let i): "Chapter \(Roman.chapter(i)) has vested"
        case .remainingBelow(let a): "less than \(MoneyFormat.compact(a)) left to vest"
        case .dayRateBelowEnough: "the day rate falls below the Enough Line"
        case .priceBelow(let x): "the price is below \(MoneyFormat.price(x))"
        case .priceAbove(let x): "the price is above \(MoneyFormat.price(x))"
        case .threadTarget(let id): "\(threads.first { $0.id == id }?.name ?? "a thread") reaches its target"
        case let .milestone(tid, mid):
            "\u{2018}\(threads.first { $0.id == tid }?.milestones.first { $0.id == mid }?.title ?? "a scene")\u{2019} is done"
        case let .pulse(m, w, n): m == .pulls ? "Book Two pulls \(w) of the last \(n) weeks" : "pull outruns energy \(w) of the last \(n) weeks"
        case .manual: "when you say so"
        }
    }

    /// The evidence line for an expanded clause.
    static func evidence(_ c: Clause, board: Board, veiled: Bool) -> String {
        let ctx = board.context
        switch c.kind {
        case .date(let d):
            let n = Days.between(board.today, d, calendar: board.calendar)
            return n > 0 ? "\(n) days to go, until \(DateText.long(d))." : "That date has come."
        case .tranche(let i):
            guard let t = ctx.ledger?.tranches.first(where: { $0.index == i }) else { return "No grant yet." }
            let n = Days.between(board.today, t.date, calendar: board.calendar)
            return t.isVested ? "Chapter \(t.numeral) vested on \(DateText.long(t.date))." : "Chapter \(t.numeral) vests in \(n) days."
        case .remainingBelow(let a):
            guard let l = ctx.ledger else { return "No price yet." }
            return "Left to vest: \(MoneyFormat.string(l.remAfter, veiled: veiled)), against your line of \(MoneyFormat.string(a, afterTax: false, veiled: veiled))."
        case .dayRateBelowEnough:
            guard let l = ctx.ledger, let e = ctx.enoughLine else { return "Set an Enough Line in the Colophon." }
            return "Staying pays \(MoneyFormat.string(l.dayRateAfter, style: .full, veiled: veiled)) a day; your line is \(MoneyFormat.string(e, style: .full, afterTax: false, veiled: veiled))."
        case .priceBelow(let x), .priceAbove(let x):
            return "\(board.state.book.settings.ticker) is \(MoneyFormat.price(ctx.price)); your threshold is \(MoneyFormat.price(x))."
        case .threadTarget(let id):
            guard let line = board.threads.first(where: { $0.id == id }) else { return "That thread is archived or gone." }
            switch line.thread.kind {
            case .habit, .practice: return "\(line.progress.streakWeeks) of 6 weeks running. \(line.progress.periodLabel.capitalizedFirst)."
            default: return "\(line.progress.label) so far."
            }
        case let .milestone(tid, mid):
            let t = board.state.threads.first { $0.id == tid }
            let m = t?.milestones.first { $0.id == mid }
            return m?.doneAt.map { "Done on \(DateText.long($0))." } ?? "Not yet. \(t?.name ?? "")."
        case let .pulse(metric, w, n):
            let got = metric == .pulls ? ctx.pulse.pullsWeeks(ofLast: n) : ctx.pulse.divergence(ofLast: n)
            return "\(got) of the last \(min(n, ctx.pulse.count)) recorded weeks; you asked for \(w)."
        case .manual(let checked):
            return checked ? "You said it\u{2019}s true." : "You haven\u{2019}t said so yet."
        }
    }
}

extension String {
    var capitalizedFirst: String { prefix(1).uppercased() + dropFirst() }
}
