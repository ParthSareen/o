import AppKit
import Foundation
import SQLite3

struct SessionSummary: Identifiable, Hashable, Sendable {
    let id: String
    var name: String
    var title: String
    var model: String
    var workingDir: String
    var updatedAt: Date

    var displayTitle: String {
        if !name.isEmpty { return name }
        if !title.isEmpty { return title }
        return "Untitled session"
    }
}

/// Reads ~/.o/sessions.db directly (read-only) for the sidebar. The schema
/// lives in this repo (sessionstore), so a straight query stays cheap and
/// instant; writes always go through the `o` process itself.
@MainActor @Observable
final class SessionListStore {
    static let shared = SessionListStore()

    private(set) var sessions: [SessionSummary] = []
    private(set) var loadError: String? = nil

    /// Sessions that finished a run while not visible in any window.
    private(set) var unreadIDs: Set<String> = []
    /// Session currently active in at least one window.
    private var visibleIDs: Set<String> = []

    func noteActiveSession(_ id: String?) {
        if let id {
            visibleIDs.insert(id)
            unreadIDs.remove(id)
        }
    }

    /// Called by windows when one of their sessions finished a run.
    func runFinished(sessionID: String) {
        if !visibleIDs.contains(sessionID) {
            unreadIDs.insert(sessionID)
        }
    }

    func refreshFromRun() { refresh() }

    /// Stale-window housekeeping: drop visibility a window no longer shows.
    func noteSessionHidden(_ id: String) {
        visibleIDs.remove(id)
    }

    private var observersStarted = false

    func start() {
        guard !observersStarted else { return }
        observersStarted = true
        refresh()
        NotificationCenter.default.addObserver(
            forName: .oSessionsChanged, object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor in self?.refresh() }
        }
        // Refresh when the app reactivates: sessions may have been added by
        // other windows or the CLI.
        NotificationCenter.default.addObserver(
            forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor in self?.refresh() }
        }
    }

    private var dbPath: String {
        NSHomeDirectory() + "/.o/sessions.db"
    }

    func refresh() {
        var db: OpaquePointer?
        guard FileManager.default.fileExists(atPath: dbPath) else {
            sessions = []
            return
        }
        guard sqlite3_open_v2(dbPath, &db, SQLITE_OPEN_READONLY | SQLITE_OPEN_FULLMUTEX, nil) == SQLITE_OK else {
            loadError = "could not open sessions.db"
            return
        }
        defer { sqlite3_close(db) }

        let sql = """
            SELECT id, name, title, model, working_dir, updated_at
            FROM sessions ORDER BY updated_at DESC, rowid DESC LIMIT 200
            """
        var stmt: OpaquePointer?
        guard sqlite3_prepare_v2(db, sql, -1, &stmt, nil) == SQLITE_OK else {
            loadError = "sessions query failed"
            return
        }
        defer { sqlite3_finalize(stmt) }

        var rows: [SessionSummary] = []
        while sqlite3_step(stmt) == SQLITE_ROW {
            func col(_ i: Int32) -> String {
                guard let c = sqlite3_column_text(stmt, i) else { return "" }
                return String(cString: c)
            }
            rows.append(SessionSummary(
                id: col(0), name: col(1), title: col(2), model: col(3), workingDir: col(4),
                updatedAt: Date(timeIntervalSince1970: TimeInterval(sqlite3_column_int64(stmt, 5)))
            ))
        }
        sessions = rows
        loadError = nil
    }

    func delete(_ id: String) {
        var db: OpaquePointer?
        guard sqlite3_open_v2(dbPath, &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_FULLMUTEX, nil) == SQLITE_OK else { return }
        defer { sqlite3_close(db) }
        for table in ["messages", "prompt_history", "sessions"] {
            let sql = "DELETE FROM \(table) WHERE \(table == "sessions" ? "id" : "session_id") = ?"
            var stmt: OpaquePointer?
            if sqlite3_prepare_v2(db, sql, -1, &stmt, nil) == SQLITE_OK {
                sqlite3_bind_text(stmt, 1, (id as NSString).utf8String, -1, nil)
                sqlite3_step(stmt)
            }
            sqlite3_finalize(stmt)
        }
        refresh()
    }
}
