import Foundation
import Observation

extension Notification.Name {
    static let oSessionsChanged = Notification.Name("oSessionsChanged")
}

/// Which conversation a window hosts, and where/how to launch it.
struct SessionSpec: Codable, Hashable, Sendable {
    var sessionID: String? = nil    // nil = new session
    var name: String? = nil
    var model: String? = nil        // nil = session's stored model / last used
    var workingDir: String? = nil   // nil = prefs default → home

    static let new = SessionSpec()
}

/// Per-window session state: owns the o child process, folds the event
/// stream into transcript blocks, coalesces token deltas to ~30fps so the UI
/// renders once per frame at most, never once per delta.
@MainActor @Observable
final class SessionController {
    enum Phase: Equatable {
        case starting, idle, running, disconnected
    }

    private(set) var phase: Phase = .starting
    private(set) var blocks: [Block] = []
    private(set) var liveAssistant = ""
    private(set) var liveThinking = ""
    private(set) var sessionID: String? = nil
    private(set) var sessionName = ""
    private(set) var model = ""
    private(set) var workingDir = ""
    private(set) var skills: [SkillInfo] = []
    private(set) var lastRunStatus: String? = nil
    var contextTokens: Int? = nil
    var errorBanner: String? = nil

    private struct PendingText {
        var assistant = ""
        var thinking = ""

        var isEmpty: Bool { assistant.isEmpty && thinking.isEmpty }
    }

    private var pendingMain = PendingText()
    private var pendingSub: [String: PendingText] = [:] // keyed by parent tool-call id
    private var liveDirty = false

    private var process: OProcess?
    private var pumpTask: Task<Void, Never>?
    private var exitTask: Task<Void, Never>?
    private var flushTask: Task<Void, Never>?
    private(set) var spec: SessionSpec = .new

    // MARK: lifecycle

    func start(_ spec: SessionSpec) {
        self.spec = spec
        phase = .starting
        errorBanner = nil
        blocks = []
        liveAssistant = ""
        liveThinking = ""
        pendingMain = PendingText()
        pendingSub = [:]
        contextTokens = nil
        lastRunStatus = nil
        skills = []

        startFlushLoopIfNeeded()

        let settings = SettingsStore.shared
        let defaultModel = settings.prefs.selectedModel.isEmpty ? nil : settings.prefs.selectedModel
        let launch = OProcess.Launch(
            model: spec.model ?? (spec.sessionID == nil ? defaultModel : nil),
            resumeID: spec.sessionID,
            name: spec.name,
            systemPrompt: settings.prefs.defaultSystemPrompt.isEmpty ? nil : settings.prefs.defaultSystemPrompt,
            contextWindowTokens: settings.prefs.contextWindowTokens,
            allowAllTools: settings.prefs.allowAllTools,
            rlm: settings.prefs.rlm,
            workingDir: spec.workingDir ?? settings.prefs.defaultWorkingDir.nilIfEmpty
        )

        let proc = OProcess()
        process = proc
        pumpTask = Task { [weak self] in
            for await event in proc.events {
                self?.apply(event)
            }
        }
        exitTask = Task { [weak self] in
            for await code in proc.exits {
                self?.processExited(code)
            }
        }
        Task { [weak self] in
            do {
                try await proc.start(launch)
            } catch {
                self?.phase = .disconnected
                self?.errorBanner = error.localizedDescription
            }
        }
    }

    func restart(with newSpec: SessionSpec) {
        tearDownProcess()
        start(newSpec)
    }

    /// "New chat": fresh session in this window. If a run is in flight it is
    /// NOT killed — the old process detaches (stdin closes, the run finishes
    /// and persists, then the process exits) while the window moves on.
    func startNewChat() {
        pumpTask?.cancel()
        pumpTask = nil
        exitTask?.cancel()
        exitTask = nil
        if let old = process {
            let running = phase == .running
            Task { running ? await old.detach() : await old.terminate() }
        }
        process = nil
        start(.new)
    }

    func stop() {
        tearDownProcess()
        flushTask?.cancel()
        flushTask = nil
    }

    private func tearDownProcess() {
        pumpTask?.cancel()
        pumpTask = nil
        exitTask?.cancel()
        exitTask = nil
        if let proc = process {
            Task { await proc.terminate() }
        }
        process = nil
    }

    /// UI renders at most ~30 text updates/sec regardless of delta rate.
    private func startFlushLoopIfNeeded() {
        guard flushTask == nil else { return }
        flushTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .milliseconds(33))
                guard let self else { return }
                self.flushLiveText()
            }
        }
    }

    private func flushLiveText() {
        guard liveDirty else { return }
        liveDirty = false
        if liveAssistant != pendingMain.assistant { liveAssistant = pendingMain.assistant }
        if liveThinking != pendingMain.thinking { liveThinking = pendingMain.thinking }
        for (parentID, pending) in pendingSub {
            mutateTool(parentID, in: &blocks) { $0.subagentLive = pending.assistant }
        }
    }

    // MARK: commands

    func sendPrompt(_ text: String) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, phase == .idle else { return }
        errorBanner = nil
        lastRunStatus = nil
        appendBlock(.userMessage(id: UUID(), text: trimmed), scope: nil)
        phase = .running
        let (body, skill) = Self.splitSlashInvocation(trimmed, skills: skills)
        Task { [process] in
            do {
                try await process?.send(.prompt(text: body, skill: skill))
            } catch {
                self.applyError("failed to send prompt: \(error.localizedDescription)")
            }
        }
    }

    /// Parse a leading `/skillname` (matching a catalog skill) out of the
    /// prompt. Unknown slash tokens are left as plain text.
    nonisolated static func splitSlashInvocation(_ input: String, skills: [SkillInfo]) -> (text: String, skill: String?) {
        guard input.hasPrefix("/") else { return (input, nil) }
        let parts = input.split(separator: " ", maxSplits: 1, omittingEmptySubsequences: true)
        guard let first = parts.first else { return (input, nil) }
        let token = String(first.dropFirst())
        guard skills.contains(where: { $0.name == token }) else { return (input, nil) }
        let rest = parts.count > 1 ? String(parts[1]) : ""
        return (rest, token)
    }

    /// Respawn this window's process against a different model. History is
    /// reloaded from the store; a model differs from the session's stored
    /// model gets persisted by `o` itself (SetModel on resume).
    func switchModel(_ newModel: String) {
        guard !newModel.isEmpty, newModel != model, phase == .idle || phase == .disconnected else { return }
        restart(with: SessionSpec(sessionID: sessionID, model: newModel,
                                  workingDir: workingDir.isEmpty ? nil : workingDir))
    }

    /// Point this window at a different working directory (respawns the
    /// process there; resumed session history is preserved).
    func changeWorkingDir(_ path: String) {
        guard !path.isEmpty, path != workingDir else { return }
        SettingsStore.shared.prefs.defaultWorkingDir = path
        if phase == .running { Task { [process] in await process?.cancelRun() } }
        restart(with: SessionSpec(sessionID: sessionID, workingDir: path))
    }

    func cancelRun() {
        guard phase == .running else { return }
        Task { [process] in await process?.cancelRun() }
    }

    /// Test hook: performs sendPrompt's visible state changes without a live
    /// process, so reducer tests can drive apply(_:) deterministically.
    func testingInjectUserTurn(_ text: String) {
        errorBanner = nil
        lastRunStatus = nil
        appendBlock(.userMessage(id: UUID(), text: text), scope: nil)
        phase = .running
    }

    // MARK: event reducer

    func apply(_ ev: AgentEvent) {
        let scope = ev.subagentId?.isEmpty == false ? ev.subagentId : nil
        switch ev.type {
        case .sessionOpened:
            sessionID = ev.chatId
            sessionName = ev.name ?? ""
            model = ev.model ?? ""
            workingDir = ev.workingDir ?? ""
            skills = ev.skills ?? []
            blocks = Self.blocksFromHistory(ev.messages ?? [])
            phase = .idle
            NotificationCenter.default.post(name: .oSessionsChanged, object: nil)

        case .messageDelta:
            guard let content = ev.content else { return }
            if let scope { pendingSub[scope, default: PendingText()].assistant += content }
            else { pendingMain.assistant += content }
            liveDirty = true

        case .thinkingDelta:
            guard let text = ev.thinking else { return }
            if let scope { pendingSub[scope, default: PendingText()].thinking += text }
            else { pendingMain.thinking += text }
            liveDirty = true

        case .toolCallDetected:
            finalizeLiveText(scope: scope)
            for call in ev.toolCalls ?? [] {
                var tool = ToolBlock(id: UUID())
                tool.callID = call.id
                tool.name = call.function.name
                tool.argsText = call.function.arguments?.pretty
                appendBlock(.tool(tool), scope: scope)
            }

        case .toolStarted:
            finalizeLiveText(scope: scope)
            guard let name = ev.toolName else { return }
            mutateTool(id: ev.toolCallId ?? "", name: name, scope: scope) { tool in
                tool.status = .running
                tool.name = name
                if let args = ev.args { tool.argsText = JSONValue.object(args).pretty }
                tool.workingDir = ev.workingDir
            }

        case .toolFinished:
            guard let name = ev.toolName else { return }
            mutateTool(id: ev.toolCallId ?? "", name: name, scope: scope) { tool in
                tool.status = ToolRunStatus(wire: ev.toolStatus)
                tool.name = name
                if let content = ev.content, !content.isEmpty { tool.result = content }
                if let err = ev.error, !err.isEmpty { tool.errorText = err }
            }
            // safety net: finish any live sub-agent text under this call
            if let scope = ev.toolCallId, pendingSub[scope] != nil {
                finalizeLiveText(scope: scope)
            }

        case .compactionStarted:
            finalizeLiveText(scope: scope)
            appendBlock(.compaction(id: UUID(), phase: .running,
                                    detail: ev.compactionTrigger ?? ""), scope: scope)

        case .compactionProgress:
            if let tokens = ev.tokens {
                contextTokens = tokens
                mutateLastCompaction(scope: scope) { detail in
                    detail = "compacting… \(tokens) tokens"
                }
            }

        case .compacted:
            mutateLastCompaction(scope: scope) { $0 = ev.content ?? "context compacted" }
            setLastCompactionPhase(.done, scope: scope)

        case .compactionSkipped:
            setLastCompactionPhase(.skipped, scope: scope, detail: ev.content)

        case .runFinished:
            finalizeLiveText(scope: scope)
            if scope == nil {
                phase = .idle
                lastRunStatus = ev.status
                NotificationCenter.default.post(name: .oSessionsChanged, object: nil)
            }

        case .error:
            finalizeLiveText(scope: scope)
            appendBlock(.error(id: UUID(), message: ev.error ?? "unknown error"), scope: scope)
            if scope == nil { phase = phase == .running ? .running : .idle }

        case .unknown:
            break // forward compatibility: ignore events we don't know
        }
    }

    private func applyError(_ message: String) {
        finalizeLiveText(scope: nil)
        blocks.append(.error(id: UUID(), message: message))
        phase = .idle
    }

    private func processExited(_ code: Int32) {
        Task { [weak self] in
            guard let self, let proc = self.process else { return }
            let stderr = await proc.stderrTail
            if code != 0 {
                let detail = stderr.trimmingCharacters(in: .whitespacesAndNewlines)
                self.errorBanner = detail.isEmpty
                    ? "o exited unexpectedly (code \(code))"
                    : "o exited (code \(code)): \(detail)"
            }
            self.phase = .disconnected
        }
    }

    // MARK: block editing helpers

    /// Commit any streamed-but-unrendered text into finished blocks, and
    /// clear the live text areas. Called at event boundaries (tool calls,
    /// run end) so text and tools interleave in the right order.
    private func finalizeLiveText(scope: String?) {
        flushLiveText()
        let pending: PendingText
        if let scope {
            pending = pendingSub[scope] ?? PendingText()
            pendingSub[scope] = nil
        } else {
            pending = pendingMain
            pendingMain = PendingText()
            liveAssistant = ""
            liveThinking = ""
        }
        if !pending.thinking.isEmpty {
            appendBlock(.thinking(id: UUID(), text: pending.thinking), scope: scope)
        }
        if !pending.assistant.isEmpty {
            appendBlock(.assistant(id: UUID(), text: pending.assistant), scope: scope)
        }
    }

    private func appendBlock(_ block: Block, scope: String?) {
        guard let scope else {
            blocks.append(block)
            return
        }
        let found = mutateTool(scope, in: &blocks) { $0.children.append(block) }
        if !found { blocks.append(block) }
    }

    private func mutateLastCompaction(scope: String?, _ update: (inout String) -> Void) {
        for i in blocks.indices.reversed() {
            if case .compaction(let id, let phase, var detail) = blocks[i], phase == .running {
                update(&detail)
                blocks[i] = .compaction(id: id, phase: phase, detail: detail)
                return
            }
        }
        _ = scope
    }

    private func setLastCompactionPhase(_ phase: CompactionPhase, scope: String?, detail: String? = nil) {
        for i in blocks.indices.reversed() {
            if case .compaction(let id, let oldPhase, var d) = blocks[i], oldPhase == .running {
                if let detail, !detail.isEmpty { d = detail }
                blocks[i] = .compaction(id: id, phase: phase, detail: d)
                return
            }
        }
        _ = scope
    }

    /// Update a tool block at the given scope. Matches by call ID first,
    /// then by the first matching-name block still pending/running (some
    /// models emit tool calls without stable IDs).
    private func mutateTool(id: String, name: String, scope: String?, _ update: (inout ToolBlock) -> Void) {
        if !id.isEmpty, mutateTool(id, in: &blocks, update) { return }
        // fallback: nearest unfinished block with this name at this scope
        if let scope {
            if updateChildTool(parentID: scope, name: name, update) { return }
            // no matching child block (sub-agent tool calls may arrive
            // without a tool_call_detected first): create one under the parent
            var tool = ToolBlock(id: UUID())
            tool.callID = id
            tool.name = name
            update(&tool)
            _ = mutateTool(scope, in: &blocks) { $0.children.append(.tool(tool)) }
            return
        }
        for i in blocks.indices.reversed() {
            if case .tool(var tool) = blocks[i],
               tool.name == name, tool.status == .pending || tool.status == .running {
                update(&tool)
                blocks[i] = .tool(tool)
                return
            }
        }
        // no block at all (e.g. resumed history edge): create one
        var tool = ToolBlock(id: UUID())
        tool.callID = id
        tool.name = name
        update(&tool)
        appendBlock(.tool(tool), scope: nil)
    }

    /// Update the nearest unfinished child tool with this name under a parent
    /// tool block. Returns true if a child was updated.
    private func updateChildTool(parentID: String, name: String, _ update: (inout ToolBlock) -> Void) -> Bool {
        var didUpdate = false
        _ = mutateTool(parentID, in: &blocks) { parent in
            for i in parent.children.indices.reversed() {
                if case .tool(var child) = parent.children[i],
                   child.name == name, child.status == .pending || child.status == .running {
                    update(&child)
                    parent.children[i] = .tool(child)
                    didUpdate = true
                    return
                }
            }
        }
        return didUpdate
    }

    @discardableResult
    private func mutateTool(_ callID: String, in list: inout [Block], _ update: (inout ToolBlock) -> Void) -> Bool {
        for i in list.indices {
            guard case .tool(var tool) = list[i] else { continue }
            if tool.callID == callID {
                update(&tool)
                list[i] = .tool(tool)
                return true
            }
            var children = tool.children
            if mutateTool(callID, in: &children, update) {
                tool.children = children
                list[i] = .tool(tool)
                return true
            }
        }
        return false
    }

    // MARK: history rebuild

    /// Fold a persisted message list (from session_opened) into finished
    /// blocks so resumed sessions render exactly what happened.
    nonisolated static func blocksFromHistory(_ messages: [AgentMessage]) -> [Block] {
        var blocks: [Block] = []
        var toolIndexByCallID: [String: Int] = [:]

        func attachResult(_ content: String, callID: String, toolName: String) {
            if let i = toolIndexByCallID[callID], case .tool(var tool) = blocks[i] {
                tool.result = content
                tool.status = .done
                blocks[i] = .tool(tool)
                return
            }
            var tool = ToolBlock(id: UUID())
            tool.callID = callID
            tool.name = toolName
            tool.status = .done
            tool.result = content
            blocks.append(.tool(tool))
            toolIndexByCallID[callID] = blocks.count - 1
        }

        for message in messages {
            switch message.role {
            case "user":
                if !message.content.isEmpty {
                    blocks.append(.userMessage(id: UUID(), text: message.content))
                }
            case "assistant":
                if let thinking = message.thinking, !thinking.isEmpty {
                    blocks.append(.thinking(id: UUID(), text: thinking))
                }
                if !message.content.isEmpty {
                    blocks.append(.assistant(id: UUID(), text: message.content))
                }
                for call in message.toolCalls ?? [] {
                    var tool = ToolBlock(id: UUID())
                    tool.callID = call.id
                    tool.name = call.function.name
                    tool.argsText = call.function.arguments?.pretty
                    tool.status = .pending // resolved when the tool result message arrives
                    blocks.append(.tool(tool))
                    if !call.id.isEmpty { toolIndexByCallID[call.id] = blocks.count - 1 }
                }
            case "tool":
                attachResult(message.content,
                             callID: message.toolCallID ?? "",
                             toolName: message.toolName ?? "tool")
            default:
                continue // system etc.: not rendered
            }
        }
        // Any tool call that never got a recorded result shows as done-with-no-output.
        for i in blocks.indices {
            if case .tool(var tool) = blocks[i], tool.status == .pending {
                tool.status = .done
                blocks[i] = .tool(tool)
            }
        }
        return blocks
    }
}
