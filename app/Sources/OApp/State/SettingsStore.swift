import Foundation

struct UIPreferences: Codable, Equatable {
    var allowAllTools: Bool = true
    var rlm: Bool = true
    var contextWindowTokens: Int = 0      // 0 = model default
    var defaultSystemPrompt: String = ""
    var selectedModel: String = ""        // empty = o's last model
    var defaultWorkingDir: String = ""    // empty = home directory
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
