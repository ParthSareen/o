import Foundation

/// Wire types for the `o --pipe` NDJSON protocol. Mirrors agent.Event in the
/// Go harness; all fields optional so new event shapes decode gracefully.

struct AgentToolCall: Codable, Equatable, Sendable {
    struct Function: Codable, Equatable, Sendable {
        var name: String = ""
        var arguments: JSONValue? = nil
    }
    var id: String = ""
    var function: Function = .init()
}

struct AgentMessage: Codable, Equatable, Sendable {
    var role: String
    var content: String = ""
    var thinking: String? = nil
    var toolCalls: [AgentToolCall]? = nil
    var toolName: String? = nil
    var toolCallID: String? = nil

    enum CodingKeys: String, CodingKey {
        case role, content, thinking
        case toolCalls = "tool_calls"
        case toolName = "tool_name"
        case toolCallID = "tool_call_id"
    }
}

/// One available skill from session_opened (the "/" palette).
struct SkillInfo: Codable, Equatable, Sendable {
    var name: String
    var description: String? = nil
}

/// One registered tool from an inspect event.
struct ToolInfo: Codable, Equatable, Sendable {
    var name: String
    var description: String? = nil
}

enum AgentEventType: String, Codable, Sendable {
    case sessionOpened = "session_opened"
    case messageDelta = "message_delta"
    case thinkingDelta = "thinking_delta"
    case toolCallDetected = "tool_call_detected"
    case toolStarted = "tool_started"
    case toolFinished = "tool_finished"
    case compactionStarted = "compaction_started"
    case compactionProgress = "compaction_progress"
    case compacted = "compacted"
    case compactionSkipped = "compaction_skipped"
    case runFinished = "run_finished"
    case error = "error"
    case inspect = "inspect"
    case sessionAssigned = "session_assigned"
    case backgroundTasks = "background_tasks"
    case unknown
}

struct AgentEvent: Decodable, Sendable {
    var type: AgentEventType
    var runId: String?
    var chatId: String?
    var model: String?
    var name: String?
    var status: String?
    var toolStatus: String?
    var compactionTrigger: String?
    var toolCallId: String?
    var toolName: String?
    var workingDir: String?
    var content: String?
    var thinking: String?
    var toolCalls: [AgentToolCall]?
    var messages: [AgentMessage]?
    var args: [String: JSONValue]?
    var tokens: Int?
    var error: String?
    var subagentId: String?
    var skills: [SkillInfo]?
    var system: String?
    var tools: [ToolInfo]?

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let rawType = try c.decodeIfPresent(String.self, forKey: .type) ?? ""
        type = AgentEventType(rawValue: rawType) ?? .unknown
        runId = try c.decodeIfPresent(String.self, forKey: .runId)
        chatId = try c.decodeIfPresent(String.self, forKey: .chatId)
        model = try c.decodeIfPresent(String.self, forKey: .model)
        name = try c.decodeIfPresent(String.self, forKey: .name)
        status = try c.decodeIfPresent(String.self, forKey: .status)
        toolStatus = try c.decodeIfPresent(String.self, forKey: .toolStatus)
        compactionTrigger = try c.decodeIfPresent(String.self, forKey: .compactionTrigger)
        toolCallId = try c.decodeIfPresent(String.self, forKey: .toolCallId)
        toolName = try c.decodeIfPresent(String.self, forKey: .toolName)
        workingDir = try c.decodeIfPresent(String.self, forKey: .workingDir)
        content = try c.decodeIfPresent(String.self, forKey: .content)
        thinking = try c.decodeIfPresent(String.self, forKey: .thinking)
        toolCalls = try c.decodeIfPresent([AgentToolCall].self, forKey: .toolCalls)
        messages = try c.decodeIfPresent([AgentMessage].self, forKey: .messages)
        args = try c.decodeIfPresent([String: JSONValue].self, forKey: .args)
        tokens = try c.decodeIfPresent(Int.self, forKey: .tokens)
        error = try c.decodeIfPresent(String.self, forKey: .error)
        subagentId = try c.decodeIfPresent(String.self, forKey: .subagentId)
        skills = try c.decodeIfPresent([SkillInfo].self, forKey: .skills)
        system = try c.decodeIfPresent(String.self, forKey: .system)
        tools = try c.decodeIfPresent([ToolInfo].self, forKey: .tools)
    }

    private enum CodingKeys: String, CodingKey {
        case type, runId, chatId, model, name, status, toolStatus
        case compactionTrigger, toolCallId, toolName, workingDir
        case content, thinking, toolCalls = "toolCalls", messages, args
        case tokens, error, subagentId, skills, system, tools
    }
}

/// Commands the app sends to `o --pipe` on stdin.
enum AgentCommand: Encodable, Sendable {
    case prompt(text: String, skill: String? = nil)
    case cancel
    case inspect
    case compact
    case setThink(String)
    case setTools(on: Bool)

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        switch self {
        case .prompt(let text, let skill):
            try c.encode("prompt", forKey: .cmd)
            try c.encode(text, forKey: .text)
            if let skill { try c.encode(skill, forKey: .skill) }
        case .cancel:
            try c.encode("cancel", forKey: .cmd)
        case .inspect:
            try c.encode("inspect", forKey: .cmd)
        case .compact:
            try c.encode("compact", forKey: .cmd)
        case .setThink(let value):
            try c.encode("set_think", forKey: .cmd)
            try c.encode(value, forKey: .value)
        case .setTools(let on):
            try c.encode("set_tools", forKey: .cmd)
            try c.encode(on ? "on" : "off", forKey: .value)
        }
    }

    private enum CodingKeys: String, CodingKey { case cmd, text, skill, value }
}
