import Foundation

public enum MomentumStatus: String, Codable, Sendable, Hashable {
    case moving, slow, stalled
    public var word: String { rawValue }
}

public struct MomentumReading: Sendable, Hashable {
    public let status: MomentumStatus
    /// 0…1+ (can exceed 1 when ahead of pace).
    public let pace: Double
    /// Days since the last entry (or since creation if none).
    public let idleDays: Int
    /// No entry for more than 45 days.
    public var isIdle: Bool { idleDays > 45 }
}

/// Strand 3 (§6.3) plus the per-thread figures the UI and widgets show.
public enum Momentum {
    public static let idleThreshold = 45

    public static func status(thread: PlotThread, entries allEntries: [Entry], today now: Date,
                              calendar: Calendar = .current) -> MomentumReading {
        let today = calendar.startOfDay(for: now)
        let entries = allEntries.filter { $0.threadID == thread.id }
        let since28 = Days.add(-27, to: today, calendar: calendar)   // last 28 days inclusive of today
        let recent = entries.filter { calendar.startOfDay(for: $0.date) >= since28 && $0.date <= Days.add(1, to: today, calendar: calendar) }

        let pace: Double
        switch thread.kind {
        case let .count(target, deadline):
            let span = deadline.map { max(1, Days.between(thread.createdAt, $0, calendar: calendar)) } ?? 365
            let expected = Double(target) * 28 / Double(span)
            let got = recent.reduce(0) { $0 + $1.amount }
            pace = expected > 0 ? got / expected : 0
        case let .habit(perWeek):
            pace = Double(ThreadProgress.metWeeksInWindow(thread: thread, entries: entries, today: today,
                                                          calendar: calendar, perWeek: perWeek)) / 4
        case let .practice(_, weeklyTarget):
            let got = recent.reduce(0) { $0 + $1.amount }
            pace = weeklyTarget > 0 ? got / (weeklyTarget * 4) : 0
        case let .project(milestones):
            let since60 = Days.add(-60, to: today, calendar: calendar)
            if milestones.contains(where: { ($0.doneAt.map { $0 >= since60 }) ?? false }) { pace = 1 }
            else { pace = recent.isEmpty ? 0 : 0.5 }
        }

        let last = lastActivity(thread: thread, entries: entries)
        let idle = Days.between(last ?? thread.createdAt, today, calendar: calendar)
        let status: MomentumStatus = pace >= 0.75 ? .moving : (pace >= 0.3 ? .slow : .stalled)
        return MomentumReading(status: status, pace: pace, idleDays: max(0, idle))
    }

    /// The latest entry or milestone completion.
    public static func lastActivity(thread: PlotThread, entries: [Entry]) -> Date? {
        let e = entries.filter { $0.threadID == thread.id }.map(\.date).max()
        let m = thread.milestones.compactMap(\.doneAt).max()
        switch (e, m) {
        case let (a?, b?): return max(a, b)
        default: return e ?? m
        }
    }

    /// "threads moving: 3 of 5"
    public static func rollup(_ readings: [MomentumReading]) -> (moving: Int, total: Int) {
        (readings.filter { $0.status == .moving }.count, readings.count)
    }
}

/// Ring / label / streak figures for one thread.
public struct ThreadProgress: Sendable, Hashable {
    /// Ring fill 0…1 (clamped).
    public let progress: Double
    /// "7 / 12", "3 / 4", "80 / 120 min", "2 / 6"
    public let label: String
    /// "3/4 this week", "by 31 Dec", "next scene: Script draft"
    public let periodLabel: String
    /// "+1", "Done today", "+20 min", "Next scene"
    public let logLabel: String
    /// Consecutive weeks meeting target (habit/practice); 0 otherwise.
    public let streakWeeks: Int
    /// Oldest → newest, last 6 weeks: filled when the week met its pace.
    public let weekTicks: [Bool]
    /// Monday-start dots for the current week (7).
    public let weekDots: [Bool]
    /// The count/project target has been reached.
    public let targetReached: Bool
    /// For count threads: total logged.
    public let total: Double
    /// Next undone milestone (project).
    public let nextMilestone: Milestone?

    public static func compute(thread: PlotThread, entries allEntries: [Entry], today now: Date,
                               calendar: Calendar = .current) -> ThreadProgress {
        let today = calendar.startOfDay(for: now)
        let entries = allEntries.filter { $0.threadID == thread.id }
        let week = Days.weekStart(today, calendar: calendar)
        let dots: [Bool] = (0..<7).map { d in
            let day = Days.add(d, to: week, calendar: calendar)
            return entries.contains { calendar.isDate($0.date, inSameDayAs: day) }
        }
        let weekTicks: [Bool] = (0..<6).reversed().map { back in
            let start = Days.add(-7 * back, to: week, calendar: calendar)
            return weekMet(thread: thread, entries: entries, weekStart: start, calendar: calendar)
        }

        switch thread.kind {
        case let .count(target, deadline):
            let total = entries.reduce(0) { $0 + $1.amount }
            let period = deadline.map { "by " + $0.formatted(.dateTime.day().month(.abbreviated)) } ?? "this year"
            return ThreadProgress(progress: clamp(total / Double(max(1, target))),
                                  label: "\(fmt(total)) / \(target)", periodLabel: period,
                                  logLabel: "+" + fmt(thread.defaultAmount), streakWeeks: 0, weekTicks: weekTicks,
                                  weekDots: dots, targetReached: total >= Double(target), total: total, nextMilestone: nil)
        case let .habit(perWeek):
            let n = distinctDays(entries, from: week, days: 7, calendar: calendar)
            let streak = streakWeeks(thread: thread, entries: entries, today: today, calendar: calendar)
            return ThreadProgress(progress: clamp(Double(n) / Double(max(1, perWeek))),
                                  label: "\(n) / \(perWeek)", periodLabel: "\(n)/\(perWeek) this week",
                                  logLabel: "Done today", streakWeeks: streak, weekTicks: weekTicks, weekDots: dots,
                                  targetReached: false, total: Double(n), nextMilestone: nil)
        case let .practice(unit, target):
            let sum = sumIn(entries, from: week, days: 7, calendar: calendar)
            let streak = streakWeeks(thread: thread, entries: entries, today: today, calendar: calendar)
            let u = unit == .minutes ? " min" : ""
            return ThreadProgress(progress: clamp(sum / max(1, target)),
                                  label: "\(fmt(sum)) / \(fmt(target))\(u)",
                                  periodLabel: "\(fmt(sum))/\(fmt(target))\(u) this week",
                                  logLabel: "+" + fmt(thread.defaultAmount) + (unit == .minutes ? " min" : ""),
                                  streakWeeks: streak, weekTicks: weekTicks, weekDots: dots, targetReached: false,
                                  total: sum, nextMilestone: nil)
        case let .project(milestones):
            let done = milestones.filter { $0.doneAt != nil }.count
            let nextM = milestones.first { $0.doneAt == nil }
            return ThreadProgress(progress: milestones.isEmpty ? 0 : clamp(Double(done) / Double(milestones.count)),
                                  label: "\(done) / \(milestones.count)",
                                  periodLabel: nextM.map { "next scene: \($0.title)" } ?? "all scenes shot",
                                  logLabel: "Next scene", streakWeeks: 0, weekTicks: weekTicks, weekDots: dots,
                                  targetReached: !milestones.isEmpty && done == milestones.count,
                                  total: Double(done), nextMilestone: nextM)
        }
    }

    /// Did the ISO week starting at `weekStart` meet this thread's pace?
    public static func weekMet(thread: PlotThread, entries: [Entry], weekStart: Date, calendar: Calendar) -> Bool {
        switch thread.kind {
        case let .habit(perWeek): return distinctDays(entries, from: weekStart, days: 7, calendar: calendar) >= perWeek
        case let .practice(_, target): return sumIn(entries, from: weekStart, days: 7, calendar: calendar) >= target
        case let .count(target, deadline):
            let span = deadline.map { max(7, Days.between(thread.createdAt, $0, calendar: calendar)) } ?? 365
            let perWeek = Double(target) * 7 / Double(span)
            return sumIn(entries, from: weekStart, days: 7, calendar: calendar) >= perWeek * 0.999
        case let .project(milestones):
            let end = Days.add(7, to: weekStart, calendar: calendar)
            return entries.contains { $0.date >= weekStart && $0.date < end }
                || milestones.contains { ($0.doneAt.map { $0 >= weekStart && $0 < end }) ?? false }
        }
    }

    /// Consecutive met weeks ending at the last complete week (plus the current one if already met).
    public static func streakWeeks(thread: PlotThread, entries: [Entry], today: Date, calendar: Calendar) -> Int {
        let week = Days.weekStart(today, calendar: calendar)
        var streak = weekMet(thread: thread, entries: entries, weekStart: week, calendar: calendar) ? 1 : 0
        var start = Days.add(-7, to: week, calendar: calendar)
        let floor = Days.weekStart(thread.createdAt, calendar: calendar)
        while start >= floor, weekMet(thread: thread, entries: entries, weekStart: start, calendar: calendar) {
            streak += 1
            start = Days.add(-7, to: start, calendar: calendar)
        }
        return streak
    }

    /// Weeks meeting `perWeek` in a 4-week window: the current week counts only if already met,
    /// otherwise the window is the previous four complete weeks.
    static func metWeeksInWindow(thread: PlotThread, entries: [Entry], today: Date, calendar: Calendar, perWeek: Int) -> Int {
        let week = Days.weekStart(today, calendar: calendar)
        let currentMet = weekMet(thread: thread, entries: entries, weekStart: week, calendar: calendar)
        let backs = currentMet ? Array(0..<4) : Array(1...4)
        return backs.filter { weekMet(thread: thread, entries: entries, weekStart: Days.add(-7 * $0, to: week, calendar: calendar), calendar: calendar) }.count
    }

    static func distinctDays(_ entries: [Entry], from start: Date, days: Int, calendar: Calendar) -> Int {
        let end = Days.add(days, to: start, calendar: calendar)
        return Set(entries.filter { $0.date >= start && $0.date < end }.map { calendar.startOfDay(for: $0.date) }).count
    }

    static func sumIn(_ entries: [Entry], from start: Date, days: Int, calendar: Calendar) -> Double {
        let end = Days.add(days, to: start, calendar: calendar)
        return entries.filter { $0.date >= start && $0.date < end }.reduce(0) { $0 + $1.amount }
    }

    static func clamp(_ x: Double) -> Double { min(1, max(0, x.isFinite ? x : 0)) }
    static func fmt(_ x: Double) -> String {
        x.rounded() == x ? String(Int(x)) : x.formatted(.number.precision(.fractionLength(0...1)))
    }
}
