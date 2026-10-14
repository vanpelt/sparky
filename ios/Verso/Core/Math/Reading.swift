import Foundation

/// Output 2 (§6.6): a deterministic, template-built paragraph. Always ends in a question; never a verdict.
public enum Reading {
    public static let footer = "Verso can't know when. It can only show you what you've been saying."

    public static let questions: [String] = [
        "If the next chapter were already paid, what would you do on Monday?",
        "Which clause are you hoping stays unmet?",
        "If CRWV doubled tomorrow, would you stay longer? If not, money isn't the binding constraint.",
        "Read your first line from three months ago. Still true?",
        "At 80, which would you regret: leaving a quarter early, or starting Book Two a year late?",
        "What would Book Two's first page say?",
        "Which thread would you pick up first if Monday were empty?",
        "What are you waiting to be given permission for?",
        "If you rewrote one clause today, which, and would it be easier or harder to meet?",
        "Who would you tell first, and what would you say?",
    ]

    public struct ThreadNote: Sendable, Hashable {
        public let name: String
        public let reading: MomentumReading
        public let lastActivity: Date?
        public init(name: String, reading: MomentumReading, lastActivity: Date?) {
            self.name = name; self.reading = reading; self.lastActivity = lastActivity
        }
    }

    public struct Input: Sendable {
        public var clarity: ClarityResult
        public var clauses: [Clause]
        public var ledger: LedgerResult?
        public var enoughLine: Double?
        public var veiled: Bool
        public var pulse: PulseAnalysis
        public var latestRestlessness: Int?
        public var threads: [ThreadNote]
        public var turningPointSince: Date?
        public var today: Date
        public var calendar: Calendar
        public init(clarity: ClarityResult, clauses: [Clause], ledger: LedgerResult?, enoughLine: Double?, veiled: Bool,
                    pulse: PulseAnalysis, latestRestlessness: Int?, threads: [ThreadNote], turningPointSince: Date?,
                    today: Date, calendar: Calendar = .current) {
            self.clarity = clarity; self.clauses = clauses; self.ledger = ledger; self.enoughLine = enoughLine
            self.veiled = veiled; self.pulse = pulse; self.latestRestlessness = latestRestlessness
            self.threads = threads; self.turningPointSince = turningPointSince; self.today = today; self.calendar = calendar
        }
    }

    public static func question(for date: Date, calendar: Calendar = .current) -> String {
        questions[Days.isoWeekNumber(date, calendar: calendar) % questions.count]
    }

    public static func compose(_ i: Input) -> String {
        var s: [String] = []
        let cal = i.calendar

        // 1. Clauses
        let c = i.clarity
        if c.total == 0 && c.regardlessCount == 0 {
            s.append("You haven't written a clause yet.")
        } else {
            s.append("\(c.met) of \(c.total) \(c.total == 1 ? "clause is" : "clauses are") true; \(c.holding) \(c.holding == 1 ? "has" : "have") held for \(ClauseEvaluator.holdingDays) days running.")
        }
        if let waiting = i.clauses.first(where: { $0.role == .required && c.states[$0.id] == .unmet }) {
            s.append("Still waiting on \u{2018}\(waiting.text)\u{2019}.")
        }
        if c.regardlessMet { s.append("One of your \u{2018}regardless\u{2019} clauses is true.") }

        // 2. Money
        if let l = i.ledger {
            if l.isComplete {
                s.append("Book One is fully read; staying pays only salary now.")
            } else {
                let rate = MoneyFormat.string(l.dayRateAfter, style: .full, afterTax: false, veiled: i.veiled)
                let ebb = l.chapters.first.map { UnitFraction.render($0.ebbWeight) } ?? "0"
                var line = "Staying to the end pays \(i.veiled ? "" : "≈ ")\(rate) a day; the next chapter adds \(ebb) to all you'll have"
                if let E = i.enoughLine {
                    let diff = MoneyFormat.string(abs(l.dayRateAfter - E), style: .full, afterTax: false, veiled: i.veiled)
                    line += l.dayRateAfter >= E ? ", which is above your Enough Line by \(diff)" : ", which is below your Enough Line by \(diff)"
                }
                s.append(line + ".")
            }
        }

        // 3. Pulse (4+ records)
        if i.pulse.count >= 4 {
            if i.pulse.state == .pulls && i.pulse.leaning == nil {
                s.append("For \(i.pulse.stateWeeks) \(i.pulse.stateWeeks == 1 ? "week" : "weeks") Book Two has pulled harder than Book One holds.")
            } else {
                s.append(i.pulse.headline)
            }
            if let x = i.pulse.crossings.last, Days.between(x, i.today, calendar: cal) <= 84 {
                var style = Date.FormatStyle.dateTime.day().month(.wide)
                style.timeZone = cal.timeZone
                style.calendar = cal
                let mark = x == i.pulse.theX ? " (the X on the chart)" : ""
                s.append("Book Two last overtook Book One in the week of \(x.formatted(style))\(mark).")
            }
        }

        // 4. Momentum
        if !i.threads.isEmpty {
            let moving = i.threads.filter { $0.reading.status == .moving }.count
            s.append("\(spelled(moving).capitalized) of your threads moved this month.")
            if let idle = i.threads.filter({ $0.reading.isIdle }).max(by: { $0.reading.idleDays < $1.reading.idleDays }) {
                let when = idle.lastActivity.map { " since " + $0.formatted(.dateTime.month(.wide)) } ?? " yet"
                s.append("\(idle.name) hasn't moved\(when).")
            }
        }

        // 5. Squeeze / burnout
        let stalled = i.threads.filter { $0.reading.status == .stalled }.count
        if !i.threads.isEmpty, stalled * 2 > i.threads.count,
           i.pulse.state == .pulls || (i.latestRestlessness ?? 0) >= 4 {
            s.append("Your threads are quiet while the restlessness is loud. It may be the job crowding them out, not the new book calling.")
        }
        if i.pulse.burnout {
            s.append("Three weeks running you've been spent and restless at once; that can be tiredness talking as much as the book.")
        }

        // 6. Turning-point aftercare (once)
        if c.stage == .turningPoint, let since = i.turningPointSince {
            let days = Days.between(since, i.today, calendar: cal)
            if days >= 14 {
                let w = days / 7
                s.append("It's been \(w) \(w == 1 ? "week" : "weeks") since everything you said would be true was true.")
            }
        }

        // 7. One question
        s.append(question(for: i.today, calendar: cal))
        return s.joined(separator: " ")
    }

    static func spelled(_ n: Int) -> String {
        let w = ["none", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"]
        return n >= 0 && n < w.count ? w[n] : "\(n)"
    }
}
