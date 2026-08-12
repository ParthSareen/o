import Foundation

/// Locates the `o` binary: app bundle first, then O_BINARY env override,
/// then common install paths.
enum OBinary {
    static let resourceName = "o-core"

    static func locate() -> URL? {
        let fm = FileManager.default
        if let bundled = Bundle.main.resourceURL?
            .appendingPathComponent(resourceName),
           fm.isExecutableFile(atPath: bundled.path) {
            return bundled
        }
        if let env = ProcessInfo.processInfo.environment["O_BINARY"], !env.isEmpty,
           fm.isExecutableFile(atPath: env) {
            return URL(fileURLWithPath: env)
        }
        let home = fm.homeDirectoryForCurrentUser.path
        for path in [
            "/opt/homebrew/bin/o",
            "/usr/local/bin/o",
            "\(home)/go/bin/o",
            "\(home)/.local/bin/o",
            "\(home)/bin/o",
            "\(home)/.herdr/worktrees/o/ui/o",   // worktree dev build
        ] where fm.isExecutableFile(atPath: path) {
            return URL(fileURLWithPath: path)
        }
        return nil
    }
}
