import Foundation

/// Per-window owner of live sessions: one SessionController (and thus one
/// `o --pipe` process) per session, kept alive while you switch threads, so
/// in-flight runs keep going in the background. The window shows the active
/// one; closing the window tears down all of them.
@MainActor @Observable
final class SessionManager {
    private(set) var active: SessionController
    private var live: [String: SessionController] = [:] // keyed by sessionID once known
    private var keyAliases: [ObjectIdentifier: String] = [:] // controller → sessionID

    init() {
        active = SessionController()
        wire(active)
    }

    /// Start the window's initial conversation from its scene spec.
    func boot(_ spec: SessionSpec) {
        if let id = spec.sessionID {
            live[id] = active
            keyAliases[ObjectIdentifier(active)] = id
        }
        active.start(spec)
    }

    // MARK: wiring

    private func wire(_ controller: SessionController) {
        controller.onSessionOpened = { [weak self, weak controller] sessionID in
            guard let self, let controller else { return }
            self.live[sessionID] = controller
            self.keyAliases[ObjectIdentifier(controller)] = sessionID
            if self.active === controller {
                SessionListStore.shared.noteActiveSession(sessionID)
            }
        }
        controller.onRunFinished = { [weak self, weak controller] in
            guard let self, let controller else { return }
            if let id = self.keyAliases[ObjectIdentifier(controller)] ?? controller.sessionID {
                SessionListStore.shared.runFinished(sessionID: id)
            }
            // refresh listing (title/timestamp changed)
            SessionListStore.shared.refreshFromRun()
        }
    }

    // MARK: switching (never kills a process)

    func switchTo(_ sessionID: String, workingDir: String? = nil) {
        if let existing = live[sessionID] {
            active = existing
        } else {
            let controller = SessionController()
            wire(controller)
            live[sessionID] = controller
            keyAliases[ObjectIdentifier(controller)] = sessionID
            controller.start(SessionSpec(sessionID: sessionID, workingDir: workingDir))
            // NOTE: if the resume fails (session deleted elsewhere) the
            // controller disconnects; the store guards against stale entries.
            active = controller
        }
        SessionListStore.shared.noteActiveSession(sessionID)
    }

    func startNewChat() {
        // the in-flight session (if any) keeps its process running — this is
        // the whole point versus the old detach behavior
        let controller = SessionController()
        wire(controller)
        controller.start(.new)
        active = controller
        SessionListStore.shared.noteActiveSession(nil)
    }

    /// Session selected row deleted from the store: tear down its live process.
    func sessionDeleted(_ sessionID: String) {
        guard let controller = live[sessionID] else { return }
        if active === controller { startNewChat() }
        Task { await controller.stopAndTerminate() }
        live[sessionID] = nil
        keyAliases[ObjectIdentifier(controller)] = nil
    }

    func stopAll() {
        for (_, controller) in live { Task { await controller.stopAndTerminate() } }
        Task { await active.stopAndTerminate() }
    }

    // MARK: test hooks

    /// Register a controller without spawning a process (unit tests).
    func testingRegister(_ controller: SessionController, id: String) {
        wire(controller)
        live[id] = controller
        keyAliases[ObjectIdentifier(controller)] = id
    }

    var testingActive: SessionController { active }
}
