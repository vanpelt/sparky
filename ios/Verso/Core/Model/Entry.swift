import Foundation

/// One logged line of progress on a thread.
public struct Entry: Codable, Sendable, Identifiable, Hashable {
    public var id: UUID
    public var threadID: UUID
    public var date: Date
    public var amount: Double
    public var note: String?
    public var title: String?
    public var milestoneID: UUID?

    public init(id: UUID = UUID(), threadID: UUID, date: Date = Date(), amount: Double = 1,
                note: String? = nil, title: String? = nil, milestoneID: UUID? = nil) {
        self.id = id; self.threadID = threadID; self.date = date; self.amount = amount
        self.note = note; self.title = title; self.milestoneID = milestoneID
    }
}
