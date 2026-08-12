import Foundation
import SwiftUI

struct UIPreferences: Codable, Equatable {
    var allowAllTools: Bool = true
    var rlm: Bool = true
    var contextWindowTokens: Int = 0      // 0 = model default
    var defaultSystemPrompt: String = ""
    var selectedModel: String = ""        // empty = o's last model
    var defaultWorkingDir: String = ""    // empty = home directory
    var textScale: Double = 1.0           // 0.7…1.4, ⌘+/⌘- steps of 0.1
}

/// App preferences at ~/.o/ui.json, applied to every new session process.
/// Also keeps ~/.ollama/config.json's last_model in sync so the CLI and the
/// app agree on the default model.
@MainActor @Observable
final class SettingsStore {
    static let shared = SettingsStore()

    var prefs = UIPreferences() {
        didSet { save() }
    }
    var availableModels: [String] = []
    var modelFetchError: String? = nil

    // MARK: text scale (⌘+ / ⌘-)

    func increaseTextScale() { prefs.textScale = min(1.4, (prefs.textScale * 10 + 1).rounded() / 10) }
    func decreaseTextScale() { prefs.textScale = max(0.7, (prefs.textScale * 10 - 1).rounded() / 10) }
    func resetTextScale() { prefs.textScale = 1.0 }

    /// Maps the 0.7–1.4 scale onto DynamicTypeSize steps: every styled font
    /// in the detail content (body, caption, monospaced variants) follows.
    var dynamicTypeSize: DynamicTypeSize {
        switch prefs.textScale {
        case ..<0.75: return .xSmall
        case ..<0.85: return .small
        case ..<0.95: return .medium
        case ..<1.05: return .large // system default
        case ..<1.15: return .xLarge
        case ..<1.25: return .xxLarge
        case ..<1.35: return .xxxLarge
        default: return .accessibility1
        }
    }

    private init() { load() }

    private var prefsURL: URL {
        URL(fileURLWithPath: NSHomeDirectory() + "/.o/ui.json")
    }

    private func load() {
        guard let data = try? Data(contentsOf: prefsURL),
              let decoded = try? JSONDecoder().decode(UIPreferences.self, from: data) else { return }
        prefs = decoded
    }

    private func save() {
        let dir = prefsURL.deletingLastPathComponent()
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        if let data = try? JSONEncoder.pretty.encode(prefs) {
            try? data.write(to: prefsURL, options: .atomic)
        }
        if !prefs.selectedModel.isEmpty { syncLastModel(prefs.selectedModel) }
    }

    /// Merge last_model into ~/.ollama/config.json without touching other keys.
    private func syncLastModel(_ model: String) {
        let url = URL(fileURLWithPath: NSHomeDirectory() + "/.ollama/config.json")
        var json: [String: Any] = [:]
        if let data = try? Data(contentsOf: url),
           let parsed = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
            json = parsed
        }
        json["last_model"] = model
        if let data = try? JSONSerialization.data(withJSONObject: json, options: [.prettyPrinted, .sortedKeys]) {
            try? FileManager.default.createDirectory(
                at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            try? data.write(to: url, options: .atomic)
        }
    }

    /// Fetch locally installed models from the ollama server.
    func reloadModels() async {
        let host = ProcessInfo.processInfo.environment["OLLAMA_HOST"] ?? "http://127.0.0.1:11434"
        guard let url = URL(string: host + "/api/tags") else { return }
        struct TagsResponse: Decodable {
            struct Model: Decodable { let model: String?; let name: String? }
            let models: [Model]
        }
        do {
            let (data, _) = try await URLSession.shared.data(from: url)
            let decoded = try JSONDecoder().decode(TagsResponse.self, from: data)
            availableModels = decoded.models.compactMap { $0.model ?? $0.name }.sorted()
            modelFetchError = nil
        } catch {
            modelFetchError = error.localizedDescription
        }
    }
}

private extension JSONEncoder {
    static let pretty: JSONEncoder = {
        let e = JSONEncoder()
        e.outputFormatting = [.prettyPrinted, .sortedKeys]
        return e
    }()
}
