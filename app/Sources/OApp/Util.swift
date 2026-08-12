import Foundation

extension String {
    var nilIfEmpty: String? { isEmpty ? nil : self }
}

extension Optional where Wrapped == String {
    var nilIfEmpty: String? { (self?.isEmpty ?? true) ? nil : self }
}

/// "now" / "4m" / "9h" / "12d" — compact relative age for narrow sidebars.
func compactRelativeAge(_ date: Date, now: Date = Date()) -> String {
    let s = max(0, now.timeIntervalSince(date))
    switch s {
    case ..<60: return "now"
    case ..<3600: return "\(Int(s / 60))m"
    case ..<86400: return "\(Int(s / 3600))h"
    default: return "\(Int(s / 86400))d"
    }
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
import SwiftUI

/// Deterministic chat text scaling: textScale (0.7–1.6) multiplies point
/// sizes directly. (dynamicTypeSize only reaches style-based fonts and
/// proved unreliable for AttributedString-backed text.)
private struct ChatTextScaleKey: EnvironmentKey {
    static let defaultValue: Double = 1.1
}

extension EnvironmentValues {
    var chatTextScale: Double {
        get { self[ChatTextScaleKey.self] }
        set { self[ChatTextScaleKey.self] = newValue }
    }
}

/// Chat-area fonts, multiplied by the scale environment value.
enum ChatFont {
    static func prose(_ scale: Double) -> Font { .system(size: 13.5 * scale) }
    static func mono(_ scale: Double) -> Font { .system(size: 12.5 * scale, design: .monospaced) }
    static func detail(_ scale: Double) -> Font { .system(size: 11.5 * scale) }
    static func detailMono(_ scale: Double) -> Font { .system(size: 11 * scale, design: .monospaced) }
    static func heading(_ level: Int, _ scale: Double) -> Font {
        let base: Double = switch level {
        case 1: 20.5
        case 2: 17.5
        case 3: 15
        default: 13.8
        }
        return .system(size: base * scale, weight: .semibold)
    }
}
