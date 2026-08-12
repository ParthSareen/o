import Foundation

struct GitChange: Identifiable, Hashable, Sendable {
    var id: String { path }
    var path: String
    var status: String  // " M", "A ", "??", ...
    var added: Int = 0
    var removed: Int = 0

    var isUntracked: Bool { status.contains("?") }
}

/// One parsed line of a unified diff. UUID identity keeps row recycling
/// safe across refreshes (offset-based ids can cross-contaminate sections).
struct DiffLine: Hashable, Identifiable, Sendable {
    enum Kind: String, Sendable { case added, removed, context, hunk, meta }
    var id = UUID()
    var kind: Kind
    var text: String
    var newNo: Int? = nil
    var oldNo: Int? = nil
}

/// A file's patch: header + all diff lines, ready to render.
struct FileSection: Identifiable, Sendable {
    var id: String { path }
    var path: String
    var status: String
    var added: Int = 0
    var removed: Int = 0
    var lines: [DiffLine] = []
    var truncated = false
}

/// A user comment anchored to a diff line range (Codex "local comment").
/// Attached to the next outgoing prompt as quoted context.
struct CodeComment: Identifiable, Equatable, Sendable {
    let id: UUID
    var path: String
    var startLine: Int
    var endLine: Int
    var oldSide = false // anchored to a removed line (no new-side number)
    var snippet: String
    var text: String

    var location: String {
        startLine == endLine ? "\(path):\(startLine)" : "\(path):\(startLine)-\(endLine)"
    }
}

/// Codex-style working-copy review: stacked per-file sections with syntax
/// colors and line numbers, plus a filterable file tree. All git work is
/// off-main; parsing is incremental-friendly.
@MainActor @Observable
final class DiffStore {
    private(set) var sections: [FileSection] = []
    private(set) var isRepo = false
    private(set) var loaded = false
    private(set) var branch = ""
    var filter = ""
    private(set) var comments: [CodeComment] = []

    func addComment(_ comment: CodeComment) {
        comments.append(comment)
    }

    func removeComment(_ id: UUID) {
        comments.removeAll { $0.id == id }
    }

    func clearComments() {
        comments.removeAll()
    }

    /// After a refresh, drop comments whose file left the diff; keep line
    /// anchors otherwise (best effort — the anchor display is indicative).
    func pruneComments() {
        let paths = Set(sections.map(\.path))
        comments.removeAll { !paths.contains($0.path) }
    }

    /// Formatted block that rides with the next user prompt.
    func promptAppendix() -> String {
        guard !comments.isEmpty else { return "" }
        var parts = ["", "---", "Review comments on the working-copy diff:", ""]
        for c in comments {
            var block = "### \(c.location)\(c.oldSide ? " (removed side)" : "")\n"
            let snippet = c.snippet.split(separator: "\n").prefix(12).joined(separator: "\n")
            if !snippet.isEmpty { block += "```\n\(snippet)\n```\n" }
            block += c.text
            parts.append(block)
        }
        return parts.joined(separator: "\n")
    }

    var totalAdded: Int { sections.reduce(0) { $0 + $1.added } }
    var totalRemoved: Int { sections.reduce(0) { $0 + $1.removed } }

    var filteredSections: [FileSection] {
        guard !filter.isEmpty else { return sections }
        let q = filter.lowercased()
        return sections.filter { $0.path.lowercased().contains(q) }
    }

    /// Tree rows: sections grouped by parent directory, sorted.
    var groupedSections: [(dir: String, files: [FileSection])] {
        var groups: [String: [FileSection]] = [:]
        for s in filteredSections {
            let dir = (s.path as NSString).deletingLastPathComponent
            groups[dir.isEmpty ? "." : dir, default: []].append(s)
        }
        return groups.keys.sorted().map { (dir: $0, files: groups[$0]!) }
    }

    private var directory: String = ""
    private var refreshToken = 0

    func setDirectory(_ dir: String) {
        guard dir != directory else { return }
        directory = dir
        sections = []
        branch = ""
        Task { await refresh() }
    }

    func refresh() async {
        guard !directory.isEmpty else { return }
        refreshToken += 1
        let token = refreshToken
        let dir = directory

        let result = await Task.detached { () -> (Bool, String, [FileSection]) in
            let (_, statusCode) = runProcessForOutput("/usr/bin/git", ["rev-parse", "--is-inside-work-tree"], cwd: dir)
            guard statusCode == 0 else { return (false, "", []) }

            let (branchOut, _) = runProcessForOutput("/usr/bin/git", ["branch", "--show-current"], cwd: dir)
            let branch = branchOut.trimmingCharacters(in: .whitespacesAndNewlines)

            // status: names + statuses; -uall so untracked dirs expand into
            // their files instead of one synthesized "directory" entry
            let (porcelain, _) = runProcessForOutput("/usr/bin/git",
                ["status", "--porcelain=v1", "--untracked-files=all"], cwd: dir)
            var order: [String] = []
            var statuses: [String: String] = [:]
            for line in porcelain.split(separator: "\n") {
                guard line.count > 3 else { continue }
                var path = String(line.dropFirst(3))
                if path.hasPrefix("\"") && path.hasSuffix("\"") && path.count > 1 {
                    path = String(path.dropFirst().dropLast())
                }
                if let arrow = path.range(of: " -> ") { path = String(path[arrow.upperBound...]) }
                order.append(path)
                statuses[path] = String(line.prefix(2))
            }

            // full patch: HEAD diff covers tracked changes; fall back when
            // there are no commits yet
            var (patch, code) = runProcessForOutput("/usr/bin/git", ["diff", "HEAD", "--"], cwd: dir)
            if code != 0 {
                let (wt, _) = runProcessForOutput("/usr/bin/git", ["diff", "--"], cwd: dir)
                let (staged, _) = runProcessForOutput("/usr/bin/git", ["diff", "--cached", "--"], cwd: dir)
                patch = wt + staged
                if wt.isEmpty && staged.isEmpty { code = 0 }
            }

            var sections = Self.parseDiffSections(patch, statuses: statuses)

            // untracked files never appear in the patch: synthesize sections.
            // Skip directories and binary blobs entirely — cat-ing a Mach-O
            // into a text view is how you get a 300-line garbage section.
            for path in order where statuses[path]?.contains("?") == true {
                if !sections.contains(where: { $0.path == path }) {
                    let full = URL(fileURLWithPath: dir).appendingPathComponent(path)
                    var isDir: ObjCBool = false
                    guard FileManager.default.fileExists(atPath: full.path, isDirectory: &isDir),
                          !isDir.boolValue,
                          let data = FileManager.default.contents(atPath: full.path) else { continue }

                    var sec = FileSection(path: path, status: "??")
                    if Self.isLikelyBinary(data) {
                        sec.lines = [DiffLine(kind: .meta, text: "(binary file, \(data.count) bytes — not shown)")]
                    } else {
                        let text = String(decoding: data, as: UTF8.self)
                        let raw = text.split(separator: "\n", omittingEmptySubsequences: false)
                        sec.lines = raw.prefix(300).enumerated().map { i, l in
                            DiffLine(kind: .added, text: String(l), newNo: i + 1)
                        }
                        sec.added = sec.lines.count
                        if raw.count > 300 { sec.truncated = true }
                    }
                    sections.append(sec)
                }
            }
            sections.sort { $0.path < $1.path }
            return (true, branch, sections)
        }.value

        guard token == refreshToken else { return }
        isRepo = result.0
        branch = result.1
        sections = result.2
        loaded = true
        pruneComments()
    }

    /// Heuristic: NUL byte or mostly-undecodable UTF-8 in the first 8KB.
    nonisolated static func isLikelyBinary(_ data: Data) -> Bool {
        let sample = data.prefix(8 * 1024)
        if sample.contains(0) { return true }
        return String(data: Data(sample), encoding: .utf8) == nil
    }

    /// Split a unified diff into per-file sections with line numbers threaded.
    nonisolated static func parseDiffSections(_ patch: String, statuses: [String: String]) -> [FileSection] {
        var sections: [FileSection] = []
        var current: FileSection? = nil
        var oldNo = 0, newNo = 0
        let maxLines = 2000

        func finishCurrent() {
            if var c = current {
                if c.lines.count > maxLines {
                    c.lines = Array(c.lines.prefix(maxLines))
                    c.truncated = true
                }
                sections.append(c)
            }
            current = nil
        }

        for rawLine in patch.split(separator: "\n", omittingEmptySubsequences: false) {
            let line = String(rawLine)

            if line.hasPrefix("diff --git ") {
                finishCurrent()
                // path from the b side; deleted files keep the a side
                let parts = line.components(separatedBy: " b/")
                let path = parts.count > 1 ? parts[1] : (line.split(separator: " ").last.map(String.init) ?? line)
                current = FileSection(path: path, status: statuses[path] ?? " M")
                continue
            }
            guard var c = current else { continue }

            if line.hasPrefix("index ") || line.hasPrefix("old mode") || line.hasPrefix("new mode")
                || line.hasPrefix("new file mode") || line.hasPrefix("deleted file mode")
                || line.hasPrefix("similarity index") || line.hasPrefix("rename from") || line.hasPrefix("rename to") {
                continue // skip git meta noise entirely
            }
            if line.hasPrefix("---") || line.hasPrefix("+++") {
                c.lines.append(DiffLine(kind: .meta, text: line))
            } else if line.hasPrefix("@@") {
                // @@ -a[,b] +c[,d] @@ → thread counters
                if let plus = line.range(of: "+"), let space = line[plus.lowerBound...].firstIndex(of: " ") {
                    let startStr = line[line.index(after: plus.lowerBound)..<space]
                        .split(separator: ",").first.map(String.init) ?? "0"
                    newNo = Int(startStr) ?? 0
                }
                if let minus = line.range(of: "-") {
                    let after = line.index(after: minus.lowerBound)
                    let digits = line[after...].prefix(while: { $0.isNumber })
                    oldNo = Int(digits) ?? 0
                }
                c.lines.append(DiffLine(kind: .hunk, text: line))
            } else if line.hasPrefix("+") {
                c.added += 1
                c.lines.append(DiffLine(kind: .added, text: String(line.dropFirst()), newNo: newNo))
                newNo += 1
            } else if line.hasPrefix("-") {
                c.removed += 1
                c.lines.append(DiffLine(kind: .removed, text: String(line.dropFirst()), oldNo: oldNo))
                oldNo += 1
            } else if line.hasPrefix(" ") {
                c.lines.append(DiffLine(kind: .context, text: String(line.dropFirst()), newNo: newNo, oldNo: oldNo))
                oldNo += 1
                newNo += 1
            } else if line.hasPrefix("\\") {
                c.lines.append(DiffLine(kind: .meta, text: line))
            } else if line.hasPrefix("Binary files") || line.hasPrefix("GIT binary patch") {
                c.lines.append(DiffLine(kind: .meta, text: "(binary content changed — not shown)"))
            }
            current = c
        }
        finishCurrent()
        return sections
    }
}
