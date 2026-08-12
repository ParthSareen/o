import Foundation

extension String {
    var nilIfEmpty: String? { isEmpty ? nil : self }
}

extension Optional where Wrapped == String {
    var nilIfEmpty: String? { (self?.isEmpty ?? true) ? nil : self }
}

/// Run a short-lived child process synchronously (background-thread callers
/// only). Streams stdout while waiting so large output can't deadlock the
/// pipe. Returns stdout text and exit code (-1 launch error, -2 timeout).
func runProcessForOutput(_ executable: String, _ args: [String], cwd: String, timeout: TimeInterval = 20) -> (String, Int32) {
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: executable)
    proc.arguments = args
    proc.currentDirectoryURL = URL(fileURLWithPath: cwd)
    let out = Pipe()

    final class Box: @unchecked Sendable {
        var data = Data()
        let lock = NSLock()
        func append(_ d: Data) { lock.lock(); data.append(d); lock.unlock() }
    }
    let box = Box()
    out.fileHandleForReading.readabilityHandler = { handle in
        let chunk = handle.availableData
        if !chunk.isEmpty { box.append(chunk) }
    }

    proc.standardOutput = out
    proc.standardError = Pipe()
    do {
        try proc.run()
    } catch {
        return ("", -1)
    }

    let done = DispatchSemaphore(value: 0)
    Thread.detachNewThread {
        proc.waitUntilExit()
        done.signal()
    }
    if done.wait(timeout: .now() + timeout) == .timedOut {
        proc.terminate()
        return ("", -2)
    }
    out.fileHandleForReading.readabilityHandler = nil
    // drain whatever arrived between the last handler tick and exit
    box.append(out.fileHandleForReading.readDataToEndOfFile())
    return (String(decoding: box.data, as: UTF8.self), proc.terminationStatus)
}
