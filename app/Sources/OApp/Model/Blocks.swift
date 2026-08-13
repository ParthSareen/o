import Foundation

/// Transcript block model. The Go side owns the canonical conversation; the
/// app owns this display transcript, rebuilt from session_opened history and
/// folded from live events.

enum ToolRunStatus: String, Equatable, Sendable {
    case pending, running, done, failed, denied, disabled, skipped

    init(wire: String?) {
        self = ToolRunStatus(rawValue: wire ?? "") ?? .pending
    }
}

struct ToolBlock: Equatable, Identifiable, Sendable {
    let id: UUID
    var callID: String = ""
    var name: String = ""
    var argsText: String? = nil       // pretty JSON
    var argsSummary: String = ""      // friendly one-liner for the collapsed row
    var status: ToolRunStatus = .pending
    var result: String? = nil
    var errorText: String? = nil
    var workingDir: String? = nil
    var children: [Block] = []        // sub-agent activity (rlm)
    var subagentLive: String = ""     // streaming text from a sub-agent run
}

enum CompactionPhase: Equatable, Sendable {
    case running, done, skipped
}

enum Block: Equatable, Identifiable, Sendable {
    case userMessage(id: UUID, text: String)
    case assistant(id: UUID, text: String)
    case thinking(id: UUID, text: String)
    case tool(ToolBlock)
    case compaction(id: UUID, phase: CompactionPhase, detail: String)
    case error(id: UUID, message: String)

    var id: UUID {
        switch self {
        case .userMessage(let id, _), .assistant(let id, _), .thinking(let id, _),
             .compaction(let id, _, _), .error(let id, _):
            return id
        case .tool(let t): return t.id
        }
    }
}
