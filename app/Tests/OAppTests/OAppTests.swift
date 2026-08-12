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
        await store.waitUntilLoaded()
        guard store.isRepo else {
            // non-git environment (e.g. CI tarball): nothing to assert
            return
        }
        #expect(store.loaded)
        // clean tree is a valid state; dirty tree produces sections
        #expect(store.sections.allSatisfy { !$0.path.isEmpty })
        if !store.sections.isEmpty {
            #expect(store.totalAdded + store.totalRemoved > 0)
        }
    }

    @Test func nonRepoDirectoryHandled() async {
        let store = DiffStore()
        let tmp = NSTemporaryDirectory()
        store.setDirectory(tmp)
        await store.waitUntilLoaded()
        #expect(!store.isRepo || store.sections.isEmpty) // /tmp may be inside a repo on some systems; both states safe
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

@MainActor
struct InspectTests {
    @Test func decodesAndAppliesInspectEvent() {
        let c = SessionController()
        c.apply(event(#"{"type":"session_opened","chatId":"s1","model":"m","messages":[]}"#))
        c.apply(event(#"{"type":"inspect","chatId":"s1","model":"m","workingDir":"/tmp","system":"SYS","tools":[{"name":"bash","description":"run shell"}],"messages":[{"role":"user","content":"hi"}]}"#))
        guard let snap = c.inspection else { Issue.record("no inspection"); return }
        #expect(snap.system == "SYS")
        #expect(snap.tools.map(\.name) == ["bash"])
        #expect(snap.messages.count == 1)
    }

    @Test func encodesInspectCommand() throws {
        let data = try JSONEncoder().encode(AgentCommand.inspect)
        let dict = try JSONSerialization.jsonObject(with: data) as? [String: String]
        #expect(dict == ["cmd": "inspect"])
    }
}

struct DiffParsingTests {
    let patch = """
    diff --git a/api/client.go b/api/client.go
    index 123..456 100644
    --- a/api/client.go
    +++ b/api/client.go
    @@ -10,3 +10,4 @@ func f() {
      context line
    -old line
    +new line
    +another new line
    diff --git a/deleted.txt b/deleted.txt
    deleted file mode 100644
    --- a/deleted.txt
    +++ /dev/null
    @@ -1,2 +0,0 @@
    -gone
    -also gone
    """

    @Test func sectionsHeaderAndThreading() {
        let sections = DiffStore.parseDiffSections(patch, statuses: ["api/client.go": " M", "deleted.txt": " D"])
        #expect(sections.count == 2)

        let go = sections[0]
        #expect(go.path == "api/client.go")
        #expect(go.added == 2 && go.removed == 1)
        // context at old/new 10; removed at old 11 (no newNo); added at new 11,12
        let ctx = go.lines.first { $0.kind == .context }
        #expect(ctx?.newNo == 10 && ctx?.oldNo == 10)
        let del = go.lines.first { $0.kind == .removed }
        #expect(del?.oldNo == 11 && del?.newNo == nil && del?.text == "old line")
        let adds = go.lines.filter { $0.kind == .added }
        #expect(adds.map(\.newNo) == [11, 12])

        let delSec = sections[1]
        #expect(delSec.added == 0 && delSec.removed == 2)
        // meta noise (deleted file mode / index) is filtered out
        #expect(!go.lines.contains { $0.text.hasPrefix("index ") })
    }

    @Test func hunkHeaderCountersResume() {
        let multi = """
        diff --git a/f.swift b/f.swift
        @@ -1,2 +1,2 @@
        -a
        +b
         keep
        @@ -20,1 +20,2 @@
        +x
         tail
        """
        let s = DiffStore.parseDiffSections(multi, statuses: [:]).first!
        let lines = s.lines.filter { $0.kind != .hunk }
        #expect(lines.map(\.text) == ["a", "b", "keep", "x", "tail"])
        #expect(lines.map(\.newNo) == [nil, 1, 2, 20, 21])
    }
}

struct SyntaxHighlighterTests {
    @Test func highlighterPreservesText() {
        let line = "func main() { // say \"hi\" }"
        #expect(String(DiffSyntax.highlight(line, path: "main.go").characters) == line)
    }

    @Test func keywordGetsSwiftUIColor() {
        let rendered = DiffSyntax.highlight("return nil", path: "x.go")
        var runs = rendered.runs.makeIterator()
        var keywordColored = false
        while let run = runs.next() {
            let text = String(rendered[run.range].characters)
            if text == "return" && run.attributes.foregroundColor != nil { keywordColored = true }
        }
        #expect(keywordColored)
    }

    @Test func plainLineKeepsContent() {
        let text = "just some plain code"
        #expect(String(DiffSyntax.highlight(text, path: "x.go").characters) == text)
    }
}

struct DiffEdgeCaseTests {
    @Test func binaryDetection() {
        let text = Data("hello\nworld\n".utf8)
        #expect(!DiffStore.isLikelyBinary(text))
        var binary = Data([0x7f, 0x45, 0x4c, 0x46])  // ELF magic
        binary.append(0x00)
        #expect(DiffStore.isLikelyBinary(binary))
        let undecodable = Data([0xff, 0xfe, 0xfd, 0xfc, 0xfb])
        #expect(DiffStore.isLikelyBinary(undecodable))
    }

    @Test func binaryPatchLineIsMeta() {
        let patch = """
        diff --git a/data.bin b/data.bin
        index 111..222 100644
        Binary files a/data.bin and b/data.bin differ
        """
        let sections = DiffStore.parseDiffSections(patch, statuses: [:])
        #expect(sections.count == 1)
        #expect(sections[0].lines.count == 1)
        #expect(sections[0].lines[0].kind == .meta)
        #expect(sections[0].lines[0].text.contains("binary"))
    }
}

@MainActor
struct ReviewCommentTests {
    @Test func promptAppendixFormatsLocationSnippetText() {
        let store = DiffStore()
        #expect(store.promptAppendix() == "") // empty when no comments

        store.addComment(CodeComment(
            id: UUID(), path: "agent/session.go", startLine: 646, endLine: 648,
            snippet: "for _, tc := range …\n    if tc…", text: "can we avoid the copy here?"))
        let appendix = store.promptAppendix()
        #expect(appendix.contains("agent/session.go:646-648"))
        #expect(appendix.contains("```"))
        #expect(appendix.contains("can we avoid the copy here?"))
    }

    @Test func removeAndClearComments() {
        let store = DiffStore()
        let c = CodeComment(id: UUID(), path: "a.go", startLine: 1, endLine: 1, snippet: "", text: "t1")
        store.addComment(c)
        store.addComment(CodeComment(id: UUID(), path: "b.go", startLine: 2, endLine: 5, snippet: "", text: "t2"))
        #expect(store.comments.count == 2)
        store.removeComment(c.id)
        #expect(store.comments.count == 1)
        store.clearComments()
        #expect(store.comments.isEmpty)
        #expect(store.promptAppendix() == "")
    }

    @Test func locationStringShape() {
        let single = CodeComment(id: UUID(), path: "x/y.swift", startLine: 7, endLine: 7, snippet: "", text: "")
        #expect(single.location == "x/y.swift:7")
        let range = CodeComment(id: UUID(), path: "x/y.swift", startLine: 7, endLine: 9, snippet: "", text: "")
        #expect(range.location == "x/y.swift:7-9")
    }
}

@MainActor
struct DiffStoreContentAttributionTests {
    /// Regression guard: every untracked section must contain ONLY its own
    /// file's content (view identity collisions once bled other files in).
    @Test func untrackedSectionsCarryOwnContent() async throws {
        let tmp = FileManager.default.temporaryDirectory
            .appendingPathComponent("odiff-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: tmp) }
        let dir = tmp.path

        _ = runProcessForOutput("/usr/bin/git", ["init", "-q"], cwd: dir)

        let fm = FileManager.default
        let markers: [String: String] = [
            "alpha.md": "ALPHA_ONLY_MARKER_001\nalpha body",
            "beta.md": "BETA_ONLY_MARKER_002\nbeta body",
        ]
        for (name, body) in markers {
            try body.write(to: tmp.appendingPathComponent(name), atomically: true, encoding: .utf8)
        }
        _ = fm // silence

        let store = DiffStore()
        store.setDirectory(dir)
        await store.waitUntilLoaded()
        #expect(store.isRepo)
        #expect(store.sections.count == 2)

        for section in store.sections {
            let marker = markers.keys.contains(section.path) ? markers[section.path]! : ""
            let own = section.lines.filter { $0.kind == .added }.map(\.text)
            #expect(own.contains(where: { $0.contains(marker.components(separatedBy: "\n").first!) }),
                    "section \(section.path) missing its own content")
            let other = markers.first(where: { $0.key != section.path })!.value.components(separatedBy: "\n").first!
            #expect(!own.contains(where: { $0.contains(other) }),
                    "section \(section.path) contains another file's content")
        }
    }
}

struct ToolSummaryTests {
    @Test func knownToolsPickTheRightField() {
        #expect(SessionController.toolSummary(name: "web_search", args: .object(["query": .string("ollama news")])) == "\"ollama news\"")
        #expect(SessionController.toolSummary(name: "web_fetch", args: .object(["url": .string("https://x.dev/y")])) == "https://x.dev/y")
        #expect(SessionController.toolSummary(name: "bash", args: .object(["command": .string("ls -la"), "timeout": .number(30)])) == "ls -la")
        #expect(SessionController.toolSummary(name: "edit_file", args: .object(["path": .string("a.go"), "old_text": .string("x")])) == "a.go")
        #expect(SessionController.toolSummary(name: "skill", args: .object(["name": .string("release-notes")])) == "/release-notes")
        #expect(SessionController.toolSummary(name: "subagents", args: .object(["query": .string("find uses"), "context": .string("…")])) == "find uses")
    }

    @Test func unknownToolsFallBackToCompactJSON() {
        let s = SessionController.toolSummary(name: "mystery", args: .object(["a": .string("b")]))
        #expect(s.contains("a: b"))
    }

    @Test func longSummariesTruncate() {
        let long = String(repeating: "x", count: 200)
        let s = SessionController.toolSummary(name: "bash", args: .object(["command": .string(long)]))
        #expect(s.count == 94 && s.hasSuffix("…"))
    }
}

@MainActor
struct TextScaleTests {
    @Test func stepsClampAndMap() {
        let s = SettingsStore.shared
        s.resetTextScale()
        #expect(s.dynamicTypeSize == .large)
        s.increaseTextScale(); #expect(s.dynamicTypeSize == .xLarge)
        s.increaseTextScale(); s.increaseTextScale(); s.increaseTextScale(); s.increaseTextScale()
        #expect(s.prefs.textScale == 1.4) // clamped
        #expect(s.dynamicTypeSize == .accessibility1)
        s.resetTextScale()
        s.decreaseTextScale(); #expect(s.dynamicTypeSize == .medium)
        s.decreaseTextScale(); s.decreaseTextScale(); s.decreaseTextScale()
        #expect(s.prefs.textScale == 0.7) // clamped
        #expect(s.dynamicTypeSize == .xSmall)
        s.resetTextScale()
    }
}

struct MarkdownParserTests {
    @Test func headingsBulletsRulesAndCode() {
        let md = """
        ### Option A — recommended
        Some **bold** and `code` words.

        - first bullet
          - nested bullet
        1. ordered one
        2. ordered two
        > a quote
        ---
        ```go
        func f() {}
        ```
        tail paragraph
        """
        let blocks = MarkdownText.Parser.blocks(md)
        guard case .heading(let level, let text) = blocks[0] else { Issue.record("\(blocks)"); return }
        #expect(level == 3 && text == "Option A — recommended")
        guard case .paragraph(let p) = blocks[1] else { Issue.record("block1: \(blocks[1])"); return }
        #expect(p.contains("**bold**")) // inline syntax preserved for the inline pass
        #expect(blocks.count == 10)
        guard case .bullet(let b1, let d1) = blocks[2], case .bullet(_, let d2) = blocks[3] else {
            Issue.record("bullets: \(blocks)"); return
        }
        #expect(b1 == "first bullet" && d1 == 0 && d2 == 1)
        guard case .numbered(let n, _) = blocks[4] else { Issue.record(); return }
        #expect(n == 1)
        guard case .quote = blocks[6], case .rule = blocks[7], case .code(let lang, let code) = blocks[blocks.count - 2],
              case .paragraph = blocks[blocks.count - 1] else { Issue.record("\(blocks)"); return }
        #expect(lang == "go" && code.contains("func f"))
    }

    @Test func inlineProducesBoldRuns() {
        let a = MarkdownText.Parser.inline("plain and **bold** here")
        var sawBold = false
        for run in a.runs where String(a[run.range].characters) == "bold" {
            if run.attributes.inlinePresentationIntent?.contains(.stronglyEmphasized) == true { sawBold = true }
        }
        #expect(sawBold)
    }

    @Test func hashWithoutSpaceIsNotHeading() {
        let blocks = MarkdownText.Parser.blocks("#notheading")
        guard case .paragraph = blocks.first else { Issue.record("\(blocks)"); return }
    }
}

struct ChatCodeHighlightTests {
    @Test func fenceLanguageDrivesColors() {
        // swift keyword colored
        let swiftLine = DiffSyntax.highlight("let x = 1 // ok", path: "snippet.swift")
        var keywordColored = false
        var commentDimmed = false
        for run in swiftLine.runs {
            let t = String(swiftLine[run.range].characters)
            if t == "let" && run.attributes.foregroundColor != nil { keywordColored = true }
            if t.contains("// ok") && run.attributes.foregroundColor == .secondary { commentDimmed = true }
        }
        #expect(keywordColored && commentDimmed)
    }

    @Test func shellFenceUsesHashComments() {
        let line = DiffSyntax.highlight("export PATH=$PATH # setup", path: "snippet.sh")
        var hashDimmed = false
        for run in line.runs where String(line[run.range].characters).contains("# setup") {
            if run.attributes.foregroundColor == .secondary { hashDimmed = true }
        }
        #expect(hashDimmed)
    }
}

@MainActor
struct SessionManagerTests {
    @Test func switchingThreadsRetainsLiveControllers() {
        let manager = SessionManager()
        let first = SessionController()
        let second = SessionController()
        manager.testingRegister(first, id: "s1")
        manager.testingRegister(second, id: "s2")

        manager.switchTo("s1")
        #expect(manager.active === first)
        manager.switchTo("s2")
        #expect(manager.active === second)
        // back to s1: same instance — its process (if any) is untouched
        manager.switchTo("s1")
        #expect(manager.active === first)
    }

    @Test func finishWhileHiddenMarksUnread() {
        let store = SessionListStore.shared
        store.noteActiveSession("visible-1")
        store.runFinished(sessionID: "visible-1")
        #expect(!store.unreadIDs.contains("visible-1"))

        store.runFinished(sessionID: "hidden-9")
        #expect(store.unreadIDs.contains("hidden-9"))

        // opening the unread session clears it
        store.noteActiveSession("hidden-9")
        #expect(!store.unreadIDs.contains("hidden-9"))
    }

    @Test func hiddenThenFinishedThenReadAgain() {
        let store = SessionListStore.shared
        store.runFinished(sessionID: "thread-x")
        #expect(store.unreadIDs.contains("thread-x"))
        store.noteSessionHidden("thread-x")
        #expect(store.unreadIDs.contains("thread-x")) // hiding doesn't clear by itself
        store.noteActiveSession("thread-x")
        #expect(store.unreadIDs.isEmpty || !store.unreadIDs.contains("thread-x"))
    }
}

@MainActor
struct UnreadVisibilityTests {
    @Test func switchAwayThenFinishMarksUnread() {
        let store = SessionListStore.shared
        // user is on the session, then switches away (hidden), then the
        // background run finishes -> unread
        store.noteActiveSession("sess-a")
        store.runStarted(sessionID: "sess-a")
        store.noteSessionHidden("sess-a")
        store.runFinished(sessionID: "sess-a")
        #expect(store.unreadIDs.contains("sess-a"))
        #expect(!store.runningIDs.contains("sess-a"))
        store.noteActiveSession("sess-a") // cleanup
    }

    @Test func runningThenFinishWhileVisibleIsNotUnread() {
        let store = SessionListStore.shared
        store.noteActiveSession("sess-b")
        store.runStarted(sessionID: "sess-b")
        #expect(store.runningIDs.contains("sess-b"))
        store.runFinished(sessionID: "sess-b")
        #expect(!store.unreadIDs.contains("sess-b"))
        #expect(!store.runningIDs.contains("sess-b"))
    }

    @Test func visibilityRefcountAcrossWindows() {
        let store = SessionListStore.shared
        store.noteActiveSession("sess-c") // window 1 shows it
        store.noteActiveSession("sess-c") // window 2 too
        store.noteSessionHidden("sess-c") // window 1 switches away
        // still visible in window 2: finishing here should NOT be unread
        store.runFinished(sessionID: "sess-c")
        #expect(!store.unreadIDs.contains("sess-c"))
        store.noteSessionHidden("sess-c") // last window leaves
        store.runFinished(sessionID: "sess-c")
        #expect(store.unreadIDs.contains("sess-c"))
        store.noteActiveSession("sess-c") // cleanup
    }
}
