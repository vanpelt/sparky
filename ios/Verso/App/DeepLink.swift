import Foundation

/// verso:// routes.
enum DeepLink: Hashable, Sendable {
    case contents, ledger, threads, turning, colophon, marginalia
    case chapter(Int)
    case thread(UUID)
    case log(UUID)

    init?(url: URL) {
        guard url.scheme == "verso" else { return nil }
        let host = url.host() ?? ""
        let arg = url.pathComponents.dropFirst().first
        switch host {
        case "contents", "": self = .contents
        case "ledger": self = .ledger
        case "threads": self = .threads
        case "turning": self = .turning
        case "colophon": self = .colophon
        case "marginalia": self = .marginalia
        case "chapter": guard let a = arg, let i = Int(a) else { return nil }; self = .chapter(i)
        case "thread": guard let a = arg, let id = UUID(uuidString: a) else { return nil }; self = .thread(id)
        case "log": guard let a = arg, let id = UUID(uuidString: a) else { return nil }; self = .log(id)
        default: return nil
        }
    }

    var url: URL {
        let s: String = switch self {
        case .contents: "verso://contents"
        case .ledger: "verso://ledger"
        case .threads: "verso://threads"
        case .turning: "verso://turning"
        case .colophon: "verso://colophon"
        case .marginalia: "verso://marginalia"
        case .chapter(let i): "verso://chapter/\(i)"
        case .thread(let id): "verso://thread/\(id.uuidString)"
        case .log(let id): "verso://log/\(id.uuidString)"
        }
        return URL(string: s)!
    }
}
