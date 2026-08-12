import AppKit
import SwiftUI

struct ComposerView: View {
    @Bindable var controller: SessionController
    let diffStore: DiffStore
    @State private var draft = ""
    @Environment(\.chatTextScale) private var scale
    @State private var slashOpen = false
    @FocusState private var focused: Bool

    private var slashQuery: String? {
        guard draft.hasPrefix("/") else { return nil }
        let token = draft.split(separator: " ", maxSplits: 1, omittingEmptySubsequences: true)
            .first.map(String.init) ?? draft
        guard !token.contains(" ") else { return nil }
        return String(token.dropFirst())
    }

    private var filteredSkills: [SkillInfo] {
        guard let query = slashQuery else { return [] }
        let q = query.lowercased()
        if q.isEmpty { return controller.skills }
        return controller.skills.filter { $0.name.lowercased().hasPrefix(q) }
    }

    var body: some View {
        VStack(spacing: 6) {
            if !diffStore.comments.isEmpty {
                commentChips
            }
            HStack(alignment: .bottom, spacing: 8) {
                editor
                actionButton
            }
            statusBar
        }
        .padding(.horizontal, 12)
        .padding(.bottom, 8)
        .onAppear { focused = true }
        .onChange(of: slashQuery) { _, q in slashOpen = q != nil && !filteredSkills.isEmpty }
    }

    /// Staged review comments — they ride with the next prompt.
    private var commentChips: some View {
        HStack(spacing: 6) {
            Image(systemName: "text.bubble.fill")
                .font(.caption2)
                .foregroundStyle(Color.accentColor)
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 6) {
                    ForEach(diffStore.comments) { comment in
                        HStack(spacing: 4) {
                            Text(shortLocation(comment))
                                .font(.caption2)
                            Button {
                                diffStore.removeComment(comment.id)
                            } label: {
                                Image(systemName: "xmark")
                                    .font(.system(size: 8, weight: .bold))
                            }
                            .buttonStyle(.plain)
                        }
                        .padding(.horizontal, 8)
                        .padding(.vertical, 4)
                        .background(Color.accentColor.opacity(0.10))
                        .clipShape(Capsule())
                    }
                }
            }
            Spacer()
            Button("Clear") { diffStore.clearComments() }
                .buttonStyle(.plain)
                .font(.caption2)
                .foregroundStyle(.secondary)
        }
    }

    private func shortLocation(_ c: CodeComment) -> String {
        let file = (c.path as NSString).lastPathComponent
        return c.startLine == c.endLine ? "\(file):\(c.startLine)" : "\(file):\(c.startLine)-\(c.endLine)"
    }

    // MARK: editor

    private var editor: some View {
        ZStack(alignment: .topLeading) {
            if draft.isEmpty {
                Text(placeholder)
                    .font(ChatFont.prose(min(scale, 1.15)))
                    .foregroundStyle(.tertiary)
                    .padding(.leading, 5)
                    .padding(.top, 6)
                    .allowsHitTesting(false)
            }
            TextEditor(text: $draft)
                .font(ChatFont.prose(min(scale, 1.15)))
                .scrollContentBackground(.hidden)
                .focused($focused)
                .frame(minHeight: 24, maxHeight: 140)
                .fixedSize(horizontal: false, vertical: true)
                .onKeyPress(keys: [.return], phases: .down) { key in
                    guard key.modifiers.isEmpty else { return .ignored }
                    if slashOpen { insertSkill(filteredSkills.first); return .handled }
                    send()
                    return .handled
                }
                .onKeyPress(keys: [.escape], phases: .down) { _ in
                    if slashOpen { slashOpen = false; return .handled }
                    return .ignored
                }
        }
        .padding(.horizontal, 6)
        .padding(.vertical, 4)
        .background(Color(nsColor: .textBackgroundColor))
        .clipShape(RoundedRectangle(cornerRadius: 10))
        .overlay(
            RoundedRectangle(cornerRadius: 10)
                .stroke(focused ? Color.accentColor.opacity(0.55) : Color.primary.opacity(0.15),
                        lineWidth: focused ? 1.5 : 1)
        )
        .popover(isPresented: $slashOpen, arrowEdge: .bottom) {
            SlashPalette(skills: filteredSkills) { skill in
                insertSkill(skill)
                focused = true
            }
        }
    }

    private func insertSkill(_ skill: SkillInfo?) {
        slashOpen = false
        guard let skill else { return }
        let rest = draft.split(separator: " ", maxSplits: 1, omittingEmptySubsequences: true)
        draft = "/\(skill.name) " + (rest.count > 1 ? String(rest[1]) : "")
    }

    @ViewBuilder
    private var actionButton: some View {
        if controller.phase == .running {
            Button(action: controller.cancelRun) {
                Image(systemName: "stop.circle.fill").font(.title2)
            }
            .buttonStyle(.plain)
            .foregroundStyle(.red)
            .help("Cancel run")
            .padding(.bottom, 6)
        } else {
            Button(action: send) {
                Image(systemName: "arrow.up.circle.fill").font(.title2)
            }
            .buttonStyle(.plain)
            .disabled(sendDisabled)
            .help("Send (Return)")
            .padding(.bottom, 6)
        }
    }

    // MARK: status bar

    private var statusBar: some View {
        HStack(spacing: 10) {
            modelMenu
            skillsButton
            textScaleControl
            workingDirLabel
            Spacer()
            if let tokens = controller.contextTokens {
                Text("ctx \(tokens) tok")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
            phaseLabel
        }
    }

    /// Always-reachable text size control (the ⌘ shortcuts ride the menu).
    private var textScaleControl: some View {
        HStack(spacing: 0) {
            Button {
                SettingsStore.shared.decreaseTextScale()
            } label: {
                Text("A−").font(.caption2)
                    .padding(.horizontal, 6).padding(.vertical, 3)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help("Smaller text (⌘-)")
            Button {
                SettingsStore.shared.increaseTextScale()
            } label: {
                Text("A+").font(.caption2)
                    .padding(.horizontal, 6).padding(.vertical, 3)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help("Bigger text (⌘+)")
        }
        .foregroundStyle(.secondary)
        .background(Color.primary.opacity(0.05))
        .clipShape(RoundedRectangle(cornerRadius: 5))
    }

    @State private var modelPickerOpen = false

    private var modelMenu: some View {
        Button { modelPickerOpen = true } label: {
            HStack(spacing: 3) {
                Text(controller.model.isEmpty ? "model…" : shortModelName(controller.model))
                    .font(.caption2)
                Image(systemName: "chevron.up.chevron.down")
                    .font(.system(size: 8))
            }
            .foregroundStyle(.secondary)
            .padding(.horizontal, 6)
            .padding(.vertical, 3)
            .background(Color.primary.opacity(0.05))
            .clipShape(RoundedRectangle(cornerRadius: 5))
        }
        .buttonStyle(.plain)
        .disabled(controller.phase == .running || controller.phase == .starting)
        .help("Switch model (reloads this session)")
        .popover(isPresented: $modelPickerOpen, arrowEdge: .top) {
            ModelPicker(
                models: SettingsStore.shared.availableModels,
                current: controller.model,
                fetchError: SettingsStore.shared.modelFetchError,
                onSelect: { model in
                    selectModel(model)
                    modelPickerOpen = false
                },
                onCancel: { modelPickerOpen = false }
            )
        }
    }

    private func shortModelName(_ m: String) -> String {
        m.hasSuffix(":latest") ? String(m.dropLast(7)) : m
    }

    private func selectModel(_ m: String) {
        SettingsStore.shared.prefs.selectedModel = m
        controller.switchModel(m)
    }

    private var skillsButton: some View {
        Button {
            draft = "/"
            slashOpen = !controller.skills.isEmpty
            focused = true
        } label: {
            Text("/")
                .font(.caption2)
                .fontWeight(.semibold)
                .foregroundStyle(.secondary)
                .padding(.horizontal, 7)
                .padding(.vertical, 3)
                .background(Color.primary.opacity(0.05))
                .clipShape(RoundedRectangle(cornerRadius: 5))
        }
        .buttonStyle(.plain)
        .help("Skills palette")
    }

    private var workingDirLabel: some View {
        Group {
            if !controller.workingDir.isEmpty {
                Text(controller.workingDir.replacingOccurrences(of: NSHomeDirectory(), with: "~"))
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .help(controller.workingDir)
            }
        }
    }

    private var placeholder: String {
        switch controller.phase {
        case .starting: return "Starting session…"
        case .running: return "Working…"
        case .disconnected: return "Session disconnected"
        case .idle: return "Message o — / for skills"
        }
    }

    private var sendDisabled: Bool {
        draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            || controller.phase != .idle
    }

    private var phaseLabel: some View {
        Group {
            switch controller.phase {
            case .starting:
                Label("starting", systemImage: "circle.dotted")
            case .idle:
                if let status = controller.lastRunStatus, status != "done" {
                    Label(status, systemImage: "exclamationmark.circle")
                } else {
                    Label("ready", systemImage: "circle")
                }
            case .running:
                HStack(spacing: 4) {
                    ProgressView().controlSize(.small)
                    Text("running")
                }
            case .disconnected:
                Label("disconnected", systemImage: "bolt.slash")
            }
        }
        .font(.caption2)
        .foregroundStyle(.secondary)
    }

    private func send() {
        let text = draft
        let suffix = diffStore.promptAppendix()
        draft = ""
        slashOpen = false
        controller.sendPrompt(text, wireSuffix: suffix.isEmpty ? nil : suffix)
        if controller.phase == .running {
            diffStore.clearComments() // consumed by the prompt
        }
    }
}

/// The "/" skill picker: lists catalog skills, filters as you type.
struct SlashPalette: View {
    let skills: [SkillInfo]
    let onSelect: (SkillInfo) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            if skills.isEmpty {
                Text("No skills available")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .padding(12)
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 0) {
                        ForEach(skills, id: \.name) { skill in
                            Button {
                                onSelect(skill)
                            } label: {
                                VStack(alignment: .leading, spacing: 2) {
                                    Text("/" + skill.name)
                                        .font(.system(.callout, design: .monospaced))
                                        .fontWeight(.medium)
                                    if let desc = skill.description, !desc.isEmpty {
                                        Text(desc)
                                            .font(.caption)
                                            .foregroundStyle(.secondary)
                                            .lineLimit(2)
                                    }
                                }
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .padding(.horizontal, 10)
                                .padding(.vertical, 7)
                                .contentShape(Rectangle())
                            }
                            .buttonStyle(.plain)
                        }
                    }
                }
                .frame(maxHeight: 320)
            }
        }
        .frame(minWidth: 300, idealWidth: 380)
        .presentationCompactAdaptation(.popover)
    }
}

/// Type-to-filter model picker (Xcode-style): ⌘ search field on top, list of
/// models, Enter picks the top match, Esc dismisses.
struct ModelPicker: View {
    let models: [String]
    let current: String
    let fetchError: String?
    let onSelect: (String) -> Void
    let onCancel: () -> Void

    @State private var query = ""
    @FocusState private var focused: Bool

    private var filtered: [String] {
        let q = query.lowercased()
        guard !q.isEmpty else { return models }
        return models.filter { $0.lowercased().contains(q) }
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 6) {
                Image(systemName: "magnifyingglass")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                TextField("Search models…", text: $query)
                    .textFieldStyle(.plain)
                    .font(.callout)
                    .focused($focused)
                    .onKeyPress(keys: [.return], phases: .down) { _ in
                        guard let first = filtered.first else { return .handled }
                        onSelect(first)
                        return .handled
                    }
                    .onKeyPress(keys: [.escape], phases: .down) { _ in
                        onCancel()
                        return .handled
                    }
            }
            .padding(8)
            .background(Color.primary.opacity(0.05))
            .clipShape(RoundedRectangle(cornerRadius: 6))
            .padding(8)

            Divider()

            if models.isEmpty && query.isEmpty {
                VStack(spacing: 4) {
                    Text("No models listed")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                    if let fetchError {
                        Text(fetchError)
                            .font(.caption2)
                            .foregroundStyle(.tertiary)
                            .lineLimit(2)
                    }
                }
                .padding(16)
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 0) {
                        ForEach(filtered, id: \.self) { model in
                            Button { onSelect(model) } label: {
                                HStack(spacing: 6) {
                                    if model == current {
                                        Image(systemName: "checkmark")
                                            .font(.caption2)
                                            .foregroundStyle(Color.accentColor)
                                    }
                                    Text(model)
                                        .font(.callout)
                                        .lineLimit(1)
                                    Spacer()
                                    if filtered.count == 1 || (model == filtered.first && !query.isEmpty) {
                                        Text("⏎").font(.caption2).foregroundStyle(.tertiary)
                                    }
                                }
                                .padding(.horizontal, 10)
                                .padding(.vertical, 6)
                                .contentShape(Rectangle())
                            }
                            .buttonStyle(.plain)
                            if model != filtered.last { Divider().padding(.leading, 10) }
                        }
                        if filtered.isEmpty {
                            Text("No match")
                                .font(.callout)
                                .foregroundStyle(.secondary)
                                .padding(12)
                        }
                    }
                }
                .frame(minHeight: 60, maxHeight: 300)
            }
        }
        .frame(minWidth: 260, idealWidth: 300)
        .presentationCompactAdaptation(.popover)
        .onAppear { focused = true }
    }
}
