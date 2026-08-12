import AppKit
import SwiftUI

struct ComposerView: View {
    @Bindable var controller: SessionController
    @State private var draft = ""
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

    // MARK: editor

    private var editor: some View {
        ZStack(alignment: .topLeading) {
            if draft.isEmpty {
                Text(placeholder)
                    .font(.body)
                    .foregroundStyle(.tertiary)
                    .padding(.leading, 5)
                    .padding(.top, 6)
                    .allowsHitTesting(false)
            }
            TextEditor(text: $draft)
                .font(.body)
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

    private var modelMenu: some View {
        Menu {
            if SettingsStore.shared.availableModels.isEmpty {
                Text("No models listed — is ollama up?")
            } else {
                ForEach(SettingsStore.shared.availableModels, id: \.self) { m in
                    Button {
                        selectModel(m)
                    } label: {
                        if m == controller.model {
                            Label(m, systemImage: "checkmark")
                        } else {
                            Text(m)
                        }
                    }
                }
            }
        } label: {
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
        .menuStyle(.borderlessButton)
        .disabled(controller.phase == .running || controller.phase == .starting)
        .help("Switch model (reloads this session)")
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
        draft = ""
        slashOpen = false
        controller.sendPrompt(text)
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
