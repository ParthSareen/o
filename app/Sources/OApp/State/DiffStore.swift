import Foundation

struct GitChange: Identifiable, Hashable, Sendable {
    var id: String { path }
    var path: String
    var status: String  // " M", "A ", "??", ...
    var added: Int = 0
    var removed: Int = 0

    var isUntracked: Bool { status.contains("?") }
}

/// Codex-style git view of the session's working directory: changed files
/// with +/− counts, and the unified diff (or new-file contents) for the
/// selection. Runs `git` in-process, off the main thread.
@MainActor @Observable
final class DiffStore {
    private(set) var changes: [GitChange] = []
    private(set) var diffText: String = ""
    private(set) var isRepo = false
    private(set) var loaded = false
    var selection: String? = nil {
        didSet { Task { await reloadDiff() } }
    }

    private var directory: String = ""
    private var refreshToken = 0

    func setDirectory(_ dir: String) {
        guard dir != directory else { return }
        directory = dir
        changes = []
        diffText = ""
        selection = nil
        Task { await refresh() }
    }

    func refresh() async {
        guard !directory.isEmpty else { return }
        refreshToken += 1
        let token = refreshToken
        let dir = directory

        let result = await Task.detached { () -> (isRepo: Bool, changes: [GitChange]) in
            let (_, statusCode) = runProcessForOutput("/usr/bin/git", ["rev-parse", "--is-inside-work-tree"], cwd: dir)
            guard statusCode == 0 else { return (false, []) }

            let (porcelain, _) = runProcessForOutput("/usr/bin/git",
                ["status", "--porcelain=v1", "--untracked-files=normal"], cwd: dir)
            var files: [GitChange] = porcelain.split(separator: "\n").compactMap { line in
                guard line.count > 3 else { return nil }
                var path = String(line.dropFirst(3))
                // quoted paths
                if path.hasPrefix("\"") && path.hasSuffix("\"") && path.count > 1 {
                    path = String(path.dropFirst().dropLast())
                }
                // renames: "R  old -> new" — show the destination
                if let arrow = path.range(of: " -> ") { path = String(path[arrow.upperBound...]) }
                return GitChange(path: path, status: String(line.prefix(2)))
            }

            let (numstat, _) = runProcessForOutput("/usr/bin/git",
                ["diff", "HEAD", "--numstat"], cwd: dir)
            var counts: [String: (Int, Int)] = [:]
            for line in numstat.split(separator: "\n") {
                let cols = line.split(separator: "\t")
                guard cols.count >= 3,
                      let added = Int(cols[0]), let removed = Int(cols[1]) else { continue }
                counts[String(cols[2])] = (added, removed)
            }
            for i in files.indices {
                if let (a, r) = counts[files[i].path] { files[i].added = a; files[i].removed = r }
            }
            files.sort { $0.path < $1.path }
            return (true, files)
        }.value

        guard token == refreshToken else { return } // a newer refresh superseded
        isRepo = result.isRepo
        loaded = true
        changes = result.changes
        if selection == nil { selection = changes.first?.path }
        if selection != nil { await reloadDiff() } else { diffText = "" }
    }

    private func reloadDiff() async {
        guard let path = selection, !directory.isEmpty else { diffText = ""; return }
        let dir = directory
        let untracked = changes.first(where: { $0.path == path })?.isUntracked ?? false
        let text = await Task.detached { () -> String in
            if untracked {
                // show new-file contents prefixed as additions
                let (content, _) = runProcessForOutput("/bin/cat", [path], cwd: dir)
                return content.split(separator: "\n", omittingEmptySubsequences: false)
                    .prefix(400).map { "+" + $0 }.joined(separator: "\n")
            }
            let (diff, code) = runProcessForOutput("/usr/bin/git",
                ["diff", "HEAD", "--", path], cwd: dir)
            if code != 0 { return "" }
            return String(diff.prefix(400_000))
        }.value
        guard directory == dir, selection == path else { return }
        diffText = text
    }
}
