import Foundation

/// Owns one `o --pipe` child process: launch, NDJSON encode/decode, cancel,
/// teardown. One process hosts one session; the app spawns one per window.
actor OProcess {
    struct Launch: Sendable {
        var model: String? = nil
        var resumeID: String? = nil
        var name: String? = nil
        var systemPrompt: String? = nil
        var contextWindowTokens: Int = 0
        var allowAllTools: Bool = true
        var workingDir: String? = nil // nil → home directory
    }

    private var process: Process?
    private var stdinHandle: FileHandle?
    private var lineDecoder: LineDecoder?
    private var stderrBox = TailBuffer(capacity: 8 * 1024)
    private var expectExit = false

    nonisolated let events: AsyncStream<AgentEvent>
    private let eventsContinuation: AsyncStream<AgentEvent>.Continuation
    nonisolated let exits: AsyncStream<Int32>
    private let exitsContinuation: AsyncStream<Int32>.Continuation

    init() {
        (events, eventsContinuation) = AsyncStream.makeStream()
        (exits, exitsContinuation) = AsyncStream.makeStream()
    }

    enum LaunchError: LocalizedError {
        case binaryNotFound
        case failedToLaunch(String)

        var errorDescription: String? {
            switch self {
            case .binaryNotFound:
                return "Could not find the `o` binary. It should be bundled with the app (or set O_BINARY)."
            case .failedToLaunch(let msg):
                return "Failed to launch `o`: \(msg)"
            }
        }
    }

    func start(_ launch: Launch) throws {
        guard let binary = OBinary.locate() else { throw LaunchError.binaryNotFound }

        var args = ["--pipe"]
        args.append(launch.allowAllTools ? "--allow-all-tools" : "--allow-all-tools=false")
        if let name = launch.name, !name.isEmpty { args += ["--name", name] }
        if let system = launch.systemPrompt, !system.isEmpty { args += ["--system", system] }
        if launch.contextWindowTokens > 0 {
            args += ["--context-window", String(launch.contextWindowTokens)]
        }
        if let resumeID = launch.resumeID { args += ["--resume-id", resumeID] }
        if let model = launch.model, !model.isEmpty { args.append(model) }

        let proc = Process()
        proc.executableURL = binary
        proc.arguments = args
        var env = ProcessInfo.processInfo.environment
        env["NO_COLOR"] = "1"
        proc.environment = env
        // Apps launched via `open` inherit "/" as cwd; the harness uses the
        // process cwd as the agent's working directory, so set it explicitly.
        proc.currentDirectoryURL = URL(fileURLWithPath: launch.workingDir ?? NSHomeDirectory())

        let stdinPipe = Pipe()
        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        proc.standardInput = stdinPipe
        proc.standardOutput = stdoutPipe
        proc.standardError = stderrPipe

        let decoder = LineDecoder { [eventsContinuation] event in
            eventsContinuation.yield(event)
        }
        self.lineDecoder = decoder

        stdoutPipe.fileHandleForReading.readabilityHandler = { handle in
            let data = handle.availableData
            if data.isEmpty {
                decoder.finish()
                return
            }
            decoder.feed(data)
        }
        let stderrBox = self.stderrBox
        stderrPipe.fileHandleForReading.readabilityHandler = { handle in
            let data = handle.availableData
            guard !data.isEmpty else { return }
            stderrBox.append(data)
        }
        proc.terminationHandler = { [weak self] _ in
            guard let self else { return }
            Task { await self.didTerminate() }
        }

        do {
            try proc.run()
        } catch {
            throw LaunchError.failedToLaunch(error.localizedDescription)
        }
        self.process = proc
        self.stdinHandle = stdinPipe.fileHandleForWriting
    }

    var stderrTail: String { stderrBox.text }
    var isRunning: Bool { process?.isRunning ?? false }

    func send(_ command: AgentCommand) throws {
        guard let handle = stdinHandle, process?.isRunning == true else { return }
        var data = try JSONEncoder().encode(command)
        data.append(0x0a) // \n
        try handle.write(contentsOf: data)
    }

    /// Ask the agent to cancel the in-flight run (process stays alive).
    func cancelRun() {
        try? send(.cancel)
    }

    func terminate() {
        expectExit = true
        stdinHandle?.closeFile()
        stdinHandle = nil
        process?.terminate()
    }

    /// Detach with the run in flight: closing stdin makes `o` finish the
    /// current run (and persist its messages) before exiting. The actor keeps
    /// itself alive until exit so the child is reaped and observers can be
    /// notified. Callers must stop consuming `events`/`exits` first.
    func detach() {
        expectExit = true
        stdinHandle?.closeFile()
        stdinHandle = nil
        Task { [self] in
            for await _ in exits { }
            // the run we let finish has been persisted by now
            NotificationCenter.default.post(name: .oSessionsChanged, object: nil)
        }
    }

    private func didTerminate() {
        let status = process?.terminationStatus ?? -1
        let code: Int32 = (process?.terminationReason == .exit) ? status : -1
        lineDecoder?.finish()
        exitsContinuation.yield(expectExit ? 0 : code)
        exitsContinuation.finish()
        process = nil
    }
}

/// Accumulates bytes, decodes one JSON event per line. Thread-safe: driven by
/// the stdout readabilityHandler queue plus the actor's teardown path.
final class LineDecoder: @unchecked Sendable {
    private var buffer = Data()
    private let lock = NSLock()
    private let decoder = JSONDecoder()
    private let yield: (AgentEvent) -> Void
    private var finished = false

    init(yield: @escaping (AgentEvent) -> Void) { self.yield = yield }

    func feed(_ data: Data) {
        var events: [AgentEvent] = []
        lock.lock()
        buffer.append(data)
        while let nl = buffer.firstIndex(of: 0x0a) {
            let line = buffer.prefix(upTo: nl)
            buffer = buffer.suffix(from: buffer.index(after: nl))
            guard !line.isEmpty,
                  let ev = try? decoder.decode(AgentEvent.self, from: line) else { continue }
            events.append(ev)
        }
        lock.unlock()
        for ev in events { yield(ev) }
    }

    func finish() {
        lock.lock()
        defer { lock.unlock() }
        guard !finished else { return }
        finished = true
        // Flush any trailing partial line (shouldn't happen; NDJSON is
        // newline-terminated) by dropping it silently.
        buffer.removeAll()
    }
}

/// Small ring buffer for the child's stderr, shown if the process dies.
final class TailBuffer: @unchecked Sendable {
    private var data = Data()
    private let capacity: Int
    private let lock = NSLock()

    init(capacity: Int) { self.capacity = capacity }

    func append(_ chunk: Data) {
        lock.lock()
        data.append(chunk)
        if data.count > capacity {
            data = data.suffix(capacity)
        }
        lock.unlock()
    }

    var text: String {
        lock.lock()
        defer { lock.unlock() }
        return String(decoding: data, as: UTF8.self)
    }
}
