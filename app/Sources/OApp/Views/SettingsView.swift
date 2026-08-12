import AppKit
import SwiftUI

struct SettingsView: View {
    @State private var settings = SettingsStore.shared

    var body: some View {
        Form {
            Section("Model") {
                if settings.availableModels.isEmpty {
                    HStack {
                        TextField("Model (blank = last used)", text: modelBinding)
                        Button("Refresh") { Task { await settings.reloadModels() } }
                    }
                    if let error = settings.modelFetchError {
                        Text("Could not list models: \(error)")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                } else {
                    Picker("Model", selection: modelBinding) {
                        Text("Last used").tag("")
                        ForEach(settings.availableModels, id: \.self) { model in
                            Text(model).tag(model)
                        }
                    }
                }
            }

            Section("Agent") {
                Toggle("Full tool access (no approval prompts)", isOn: $settings.prefs.allowAllTools)
                Toggle("Sub-agents (RLM)", isOn: $settings.prefs.rlm)
                HStack {
                    Text("Context window tokens")
                    Spacer()
                    TextField("model default", value: $settings.prefs.contextWindowTokens, format: .number)
                        .multilineTextAlignment(.trailing)
                        .frame(width: 120)
                }
                HStack {
                    Text("Default working directory")
                    Spacer()
                    Text(settings.prefs.defaultWorkingDir.isEmpty
                         ? "Home" : abbrevHome(settings.prefs.defaultWorkingDir))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.head)
                    Button("Choose…") {
                        let panel = NSOpenPanel()
                        panel.canChooseFiles = false
                        panel.canChooseDirectories = true
                        if panel.runModal() == .OK, let url = panel.url {
                            settings.prefs.defaultWorkingDir = url.path
                        }
                    }
                }
            }

            Section("System prompt") {
                TextEditor(text: $settings.prefs.defaultSystemPrompt)
                    .font(.callout)
                    .frame(minHeight: 60)
                    .overlay(
                        RoundedRectangle(cornerRadius: 6)
                            .stroke(Color.primary.opacity(0.1))
                    )
                Text("Prepended to the harness system prompt for new sessions.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }

            Section {
                Text("Settings apply to newly launched sessions (new windows).")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
        .frame(width: 460, height: 500)
        .onAppear { Task { await settings.reloadModels() } }
    }

    private var modelBinding: Binding<String> {
        Binding(
            get: { settings.prefs.selectedModel },
            set: { settings.prefs.selectedModel = $0 }
        )
    }

    private func abbrevHome(_ path: String) -> String {
        path.replacingOccurrences(of: NSHomeDirectory(), with: "~")
    }
}
