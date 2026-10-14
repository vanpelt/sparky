import Foundation

/// Calendar helpers. Every day count in Verso goes through `days(from:to:)`.
public enum Days {
    /// Whole calendar days between the start-of-days of two dates (negative if `to` is earlier).
    public static func between(_ from: Date, _ to: Date, calendar: Calendar = .current) -> Int {
        let a = calendar.startOfDay(for: from), b = calendar.startOfDay(for: to)
        return calendar.dateComponents([.day], from: a, to: b).day ?? 0
    }

    public static func add(_ days: Int, to date: Date, calendar: Calendar = .current) -> Date {
        calendar.date(byAdding: .day, value: days, to: date) ?? date
    }

    /// A Monday-start, ISO-ish calendar derived from `calendar` (same time zone and locale).
    public static func isoCalendar(from calendar: Calendar = .current) -> Calendar {
        var c = Calendar(identifier: .iso8601)
        c.timeZone = calendar.timeZone
        c.locale = calendar.locale
        c.firstWeekday = 2
        c.minimumDaysInFirstWeek = 4
        return c
    }

    /// The Monday (start of day) of the ISO week containing `date`.
    public static func weekStart(_ date: Date, calendar: Calendar = .current) -> Date {
        let iso = isoCalendar(from: calendar)
        let day = iso.startOfDay(for: date)
        return iso.dateInterval(of: .weekOfYear, for: day)?.start ?? day
    }

    public static func isoWeekNumber(_ date: Date, calendar: Calendar = .current) -> Int {
        isoCalendar(from: calendar).component(.weekOfYear, from: date)
    }

    /// Clamped calendar date: day is limited to the month's length.
    public static func date(year: Int, month: Int, day: Int, calendar: Calendar = .current) -> Date {
        let first = calendar.date(from: DateComponents(year: year, month: month, day: 1)) ?? Date()
        let len = calendar.range(of: .day, in: .month, for: first)?.count ?? 28
        return calendar.date(from: DateComponents(year: year, month: month, day: min(max(1, day), len))) ?? first
    }
}
