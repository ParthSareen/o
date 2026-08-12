import Foundation

struct GitChange: Identifiable, Hashable, Sendable {
    var id: String { path }
    var path: String
    var status: String  // " M", "A ", "??", ...
    var added: Int = 0
    var removed: Int = 0

    var isUntracked: Bool { status.contains("?") }
}

/// One parsed line of a unified diff.
struct DiffLine: Hashable, Sendable {
    enum Kind: String, Sendable { case added, removed, context, hunk, meta }
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

            // status: names + statuses (rename destinations shown)
            let (porcelain, _) = runProcessForOutput("/usr/bin/git",
                ["status", "--porcelain=v1", "--untracked-files=normal"], cwd: dir)
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

            // untracked files never appear in the patch: synthesize sections
            for path in order where statuses[path]?.contains("?") == true {
                if !sections.contains(where: { $0.path == path }) {
                    let (content, _) = runProcessForOutput("/bin/cat", [path], cwd: dir)
                    let raw = content.split(separator: "\n", omittingEmptySubsequences: false)
                    var lines = raw.prefix(300).enumerated().map { i, l in
                        DiffLine(kind: .added, text: String(l), newNo: i + 1)
                    }
                    var sec = FileSection(path: path, status: "??", added: lines.count)
                    if raw.count > 300 { sec.truncated = true }
                    sec.lines = lines
                    lines = []
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
            }
            current = c
        }
        finishCurrent()
        return sections
    }
}
