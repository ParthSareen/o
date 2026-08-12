import Foundation
import Testing
@testable import OApp

func event(_ json: String) -> AgentEvent {
    try! JSONDecoder().decode(AgentEvent.self, from: Data(json.utf8))
}

struct EventDecodeTests {
    @Test func decodesSessionOpened() {
        let ev = event(#"{"type":"session_opened","chatId":"abc","model":"m1","name":"demo","workingDir":"/tmp","messages":[{"role":"user","content":"hi"}]}"#)
        #expect(ev.type == .sessionOpened)
        #expect(ev.chatId == "abc")
        #expect(ev.messages?.first?.content == "hi")
    }

    @Test func decodesToolStartedWithArgs() {
        let ev = event(#"{"type":"tool_started","toolStatus":"running","toolCallId":"call_1","toolName":"bash","args":{"command":"ls","timeout":30},"workingDir":"/tmp"}"#)
        #expect(ev.type == .toolStarted)
        #expect(ev.toolCallId == "call_1")
        #expect(ev.args?["command"] == .string("ls"))
        #expect(ev.args?["timeout"] == .number(30))
    }

    @Test func decodesDeltasAndRunFinished() {
        let delta = event(#"{"type":"message_delta","content":"PIPE"}"#)
        #expect(delta.type == .messageDelta && delta.content == "PIPE")
        let fin = event(#"{"type":"run_finished","status":"done"}"#)
        #expect(fin.type == .runFinished && fin.status == "done")
    }

    @Test func unknownTypeDecodesAsUnknown() {
        let ev = event(#"{"type":"future_event","foo":1}"#)
        #expect(ev.type == .unknown)
    }

    @Test func encodesPromptAndCancel() throws {
        let prompt = try JSONEncoder().encode(AgentCommand.prompt(text: "hi there"))
        let dict = try JSONSerialization.jsonObject(with: prompt) as? [String: String]
        #expect(dict == ["cmd": "prompt", "text": "hi there"])
        let cancel = try JSONEncoder().encode(AgentCommand.cancel)
        let cancelDict = try JSONSerialization.jsonObject(with: cancel) as? [String: String]
        #expect(cancelDict == ["cmd": "cancel"])
    }
}

struct JSONValueTests {
    @Test func prettyPrintsObject() {
        let value = event(#"{"type":"tool_started","toolName":"bash","args":{"command":"ls -la","timeout":30}}"#).args
        let pretty = JSONValue.object(value ?? [:]).pretty
        #expect(pretty.contains(#""command": "ls -la""#))
    }

    @Test func roundTrips() throws {
        let original = JSONValue.object(["a": .array([.number(1), .bool(true), .null]), "b": .string("x")])
        let data = try JSONEncoder().encode(original)
        let decoded = try JSONDecoder().decode(JSONValue.self, from: data)
        #expect(decoded == original)
    }
}

struct HistoryRebuildTests {
    @Test func rebuildsFullTurn() {
        let messages: [AgentMessage] = [
            AgentMessage(role: "user", content: "list files"),
            AgentMessage(role: "assistant", thinking: "i should run ls",
                         toolCalls: [AgentToolCall(id: "c1", function: .init(name: "bash", arguments: .object(["command": .string("ls")]))) ]),
            AgentMessage(role: "tool", content: "file1\nfile2", toolName: "bash", toolCallID: "c1"),
            AgentMessage(role: "assistant", content: "here are your files"),
        ]
        let blocks = SessionController.blocksFromHistory(messages)
        #expect(blocks.count == 4)
        guard case .userMessage(_, let userText) = blocks[0] else {
            Issue.record("block 0 = \(blocks[0])"); return
        }
        #expect(userText == "list files")
        guard case .thinking = blocks[1] else { Issue.record("block 1"); return }
        guard case .tool(let tool) = blocks[2] else { Issue.record("block 2"); return }
        #expect(tool.status == .done)
        #expect(tool.result == "file1\nfile2")
        #expect(tool.argsText?.contains("ls") == true)
        guard case .assistant(_, let text) = blocks[3] else { Issue.record("block 3"); return }
        #expect(text == "here are your files")
    }

    @Test func orphanedToolResultStillRenders() {
        let messages = [AgentMessage(role: "tool", content: "out", toolName: "web", toolCallID: "gone")]
        let blocks = SessionController.blocksFromHistory(messages)
        guard case .tool(let tool) = blocks.first else { Issue.record(); return }
        #expect(tool.name == "web" && tool.result == "out")
    }
}

@MainActor
struct ReducerTests {
    func makeController() -> SessionController { SessionController() }

    @Test func openedThenStreamedTurn() {
        let c = makeController()
        c.apply(event(#"{"type":"session_opened","chatId":"s1","model":"m1","messages":[]}"#))
        #expect(c.phase == .idle && c.sessionID == "s1")

        c.sendPromptForTest("hello")
        c.apply(event(#"{"type":"message_delta","content":"he"}"#))
        c.apply(event(#"{"type":"message_delta","content":"llo"}"#))
        c.apply(event(#"{"type":"run_finished","status":"done"}"#))

        #expect(c.phase == .idle)
        #expect(c.blocks.count == 2)
        guard case .assistant(_, let text) = c.blocks[1] else {
            Issue.record("blocks = \(c.blocks)"); return
        }
        #expect(text == "hello")
    }

    @Test func toolLifecycleInterleavesText() {
        let c = makeController()
        c.apply(event(#"{"type":"session_opened","chatId":"s1","model":"m","messages":[]}"#))
        c.sendPromptForTest("run ls")
        c.apply(event(#"{"type":"message_delta","content":"let me check"}"#))
        c.apply(event(#"{"type":"tool_call_detected","toolCalls":[{"id":"c1","function":{"name":"bash","arguments":{"command":"ls"}}}]}"#))
        c.apply(event(#"{"type":"tool_started","toolStatus":"running","toolCallId":"c1","toolName":"bash","args":{"command":"ls"}}"#))
        c.apply(event(#"{"type":"tool_finished","toolStatus":"done","toolCallId":"c1","toolName":"bash","content":"file1"}"#))
        c.apply(event(#"{"type":"message_delta","content":"done"}"#))
        c.apply(event(#"{"type":"run_finished","status":"done"}"#))

        #expect(c.blocks.count == 4) // user, pre-tool text, tool, post-tool text
        guard case .tool(let tool) = c.blocks[2] else {
            Issue.record("blocks = \(c.blocks)"); return
        }
        #expect(tool.status == .done && tool.result == "file1" && tool.name == "bash")
        guard case .assistant(_, let post) = c.blocks[3] else {
            Issue.record("blocks = \(c.blocks)"); return
        }
        #expect(post == "done")
    }

    @Test func subagentEventsNestUnderParentTool() {
        let c = makeController()
        c.apply(event(#"{"type":"session_opened","chatId":"s1","model":"m","messages":[]}"#))
        c.sendPromptForTest("delegate")
        c.apply(event(#"{"type":"tool_started","toolStatus":"running","toolCallId":"parent1","toolName":"subagents"}"#))
        c.apply(event(#"{"type":"message_delta","content":"child says hi","subagentId":"parent1"}"#))
        c.apply(event(#"{"type":"tool_started","toolStatus":"running","toolCallId":"c9","toolName":"bash","subagentId":"parent1","args":{"command":"pwd"}}"#))
        c.apply(event(#"{"type":"tool_finished","toolStatus":"done","toolCallId":"c9","toolName":"bash","subagentId":"parent1","content":"/tmp"}"#))
        c.apply(event(#"{"type":"run_finished","status":"done","subagentId":"parent1"}"#))
        c.apply(event(#"{"type":"tool_finished","toolStatus":"done","toolCallId":"parent1","toolName":"subagents","content":"subagent done"}"#))
        c.apply(event(#"{"type":"run_finished","status":"done"}"#))

        guard c.blocks.count == 2, case .tool(let parent) = c.blocks[1] else {
            Issue.record("blocks = \(c.blocks)"); return
        }
        #expect(parent.status == .done && parent.result == "subagent done")
        guard parent.children.count == 2 else {
            Issue.record("children = \(parent.children)"); return
        }
        guard case .tool(let child) = parent.children[1] else {
            Issue.record("children = \(parent.children)"); return
        }
        #expect(child.name == "bash" && child.result == "/tmp")
    }

    @Test func compactionLifecycle() {
        let c = makeController()
        c.apply(event(#"{"type":"session_opened","chatId":"s1","model":"m","messages":[]}"#))
        c.sendPromptForTest("hi")
        c.apply(event(#"{"type":"compaction_started","compactionTrigger":"estimate"}"#))
        c.apply(event(#"{"type":"compaction_progress","tokens":4200}"#))
        c.apply(event(#"{"type":"compacted","content":"summary"}"#))
        c.apply(event(#"{"type":"message_delta","content":"ok"}"#))
        c.apply(event(#"{"type":"run_finished","status":"done"}"#))
        guard case .compaction(_, let phase, _) = c.blocks[1] else {
            Issue.record("blocks = \(c.blocks)"); return
        }
        #expect(phase == .done)
        #expect(c.contextTokens == 4200)
    }

    @Test func errorBlockAndUnknownIgnored() {
        let c = makeController()
        c.apply(event(#"{"type":"session_opened","chatId":"s1","model":"m","messages":[]}"#))
        c.apply(event(#"{"type":"whatever_new"}"#))
        c.apply(event(#"{"type":"error","error":"boom"}"#))
        guard c.blocks.count == 1, case .error(_, let msg) = c.blocks[0] else {
            Issue.record("blocks = \(c.blocks)"); return
        }
        #expect(msg == "boom")
    }
}

/// Test hook: append the user block + flip to running without a process.
extension SessionController {
    func sendPromptForTest(_ text: String) {
        testingInjectUserTurn(text)
    }
}

struct SlashAndSpecTests {
    @Test func slashParsesKnownSkill() {
        let skills = [SkillInfo(name: "release-notes"), SkillInfo(name: "work-memory")]
        let (text, skill) = SessionController.splitSlashInvocation("/work-memory save this", skills: skills)
        #expect(skill == "work-memory")
        #expect(text == "save this")
    }

    @Test func slashOnlySkillNoArgs() {
        let skills = [SkillInfo(name: "greet")]
        let (text, skill) = SessionController.splitSlashInvocation("/greet", skills: skills)
        #expect(skill == "greet")
        #expect(text == "")
    }

    @Test func unknownSlashStaysPlainText() {
        let (text, skill) = SessionController.splitSlashInvocation("/nope hi", skills: [])
        #expect(skill == nil)
        #expect(text == "/nope hi")
    }

    @Test func plainTextUnaffected() {
        let skills = [SkillInfo(name: "greet")]
        let (text, skill) = SessionController.splitSlashInvocation("hello /greet", skills: skills)
        #expect(skill == nil)
        #expect(text == "hello /greet")
    }

    @Test func promptCommandEncodesSkill() throws {
        let data = try JSONEncoder().encode(AgentCommand.prompt(text: "do it", skill: "greet"))
        let dict = try JSONSerialization.jsonObject(with: data) as? [String: String]
        #expect(dict == ["cmd": "prompt", "text": "do it", "skill": "greet"])
        // no skill → key absent
        let plain = try JSONEncoder().encode(AgentCommand.prompt(text: "hi"))
        let plainDict = try JSONSerialization.jsonObject(with: plain) as? [String: String]
        #expect(plainDict == ["cmd": "prompt", "text": "hi"])
    }

    @Test func sessionSpecRoundTrips() throws {
        let spec = SessionSpec(sessionID: "abc", model: "muse", workingDir: "/tmp/x")
        let data = try JSONEncoder().encode(spec)
        let decoded = try JSONDecoder().decode(SessionSpec.self, from: data)
        #expect(decoded == spec)
        #expect(SessionSpec.new.sessionID == nil)
    }

    @Test func decodesSkillsFromSessionOpened() {
        let ev = event(#"{"type":"session_opened","chatId":"s1","skills":[{"name":"greet","description":"Greet warmly."}]}"#)
        #expect(ev.skills?.first?.name == "greet")
        #expect(ev.skills?.first?.description == "Greet warmly.")
    }
}

@MainActor
struct DiffStoreTests {
    /// The worktree itself has uncommitted changes while developing; this
    /// test only asserts real repo behavior, not specific content.
    @Test func readsChangedFiles() async {
        let repo = FileManager.default.currentDirectoryPath
            .replacingOccurrences(of: "/app", with: "") // tests run from package dir
        let store = DiffStore()
        store.setDirectory(repo)
        await store.refresh()
        guard store.isRepo else {
            // non-git environment (e.g. CI tarball): nothing to assert
            return
        }
        #expect(store.loaded)
        // repo has at least README or tracked files; with dirty tree changes is non-empty,
        // clean tree is also a valid state
        if store.changes.isEmpty {
            #expect(store.diffText == "")
        } else {
            #expect(store.changes.allSatisfy { !$0.path.isEmpty })
        }
    }

    @Test func nonRepoDirectoryHandled() async {
        let store = DiffStore()
        let tmp = NSTemporaryDirectory()
        store.setDirectory(tmp)
        await store.refresh()
        #expect(!store.isRepo || store.changes.isEmpty) // /tmp may be inside a repo on some systems; both states safe
    }
}

struct UtilTests {
    @Test func compactRelativeAges() {
        let now = Date()
        #expect(compactRelativeAge(now.addingTimeInterval(-5), now: now) == "now")
        #expect(compactRelativeAge(now.addingTimeInterval(-4 * 60), now: now) == "4m")
        #expect(compactRelativeAge(now.addingTimeInterval(-9 * 3600 - 120), now: now) == "9h")
        #expect(compactRelativeAge(now.addingTimeInterval(-40 * 86400), now: now) == "40d")
        // future-ish dates clamp to "now"
        #expect(compactRelativeAge(now.addingTimeInterval(60), now: now) == "now")
    }
}
