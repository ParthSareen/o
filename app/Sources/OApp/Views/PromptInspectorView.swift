import SwiftUI

/// The TUI's /prompt command, as a sheet: what the model would see right now
/// — system prompt, tools, skills, and the current message list.
struct PromptInspectorView: View {
    let controller: SessionController

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            content
        }
        .frame(minWidth: 560, idealWidth: 640, minHeight: 480, idealHeight: 600)
        .onAppear { controller.requestInspection() }
    }

    private var header: some View {
        HStack {
            Text("Prompt").font(.headline)
            if let ts = controller.inspection?.takenAt {
                Text("snapshot \(ts, style: .time)")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Button {
                controller.requestInspection()
            } label: {
                Image(systemName: "arrow.clockwise")
            }
            .buttonStyle(.plain)
            .help("Refresh")
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
    }

    @ViewBuilder
    private var content: some View {
        if let snap = controller.inspection {
            snapshotView(snap)
        } else {
            VStack {
                Spacer()
                ProgressView().controlSize(.regular)
                Text("Loading…").font(.caption).foregroundStyle(.secondary).padding(.top, 6)
                Spacer()
            }
            .frame(maxWidth: .infinity)
        }
    }

    private func snapshotView(_ snap: PromptInspection) -> some View {
        List {
            Section("Overview") {
                overviewRow("Model", snap.model)
                overviewRow("Working directory", snap.workingDir.isEmpty ? "—" : snap.workingDir)
                overviewRow("Messages", "\(snap.messages.count)")
                if let tokens = controller.contextTokens {
                    overviewRow("Context tokens", "\(tokens)")
                }
            }

            Section("System Prompt") {
                Text(snap.system.isEmpty ? "(none)" : snap.system)
                    .font(.system(.caption, design: .monospaced))
                    .foregroundStyle(snap.system.isEmpty ? .secondary : .primary)
                    .textSelection(.enabled)
            }

            Section("Tools (\(snap.tools.count))") {
                ForEach(snap.tools, id: \.name) { tool in
                    VStack(alignment: .leading, spacing: 2) {
                        Text(tool.name).font(.system(.callout, design: .monospaced)).fontWeight(.medium)
                        if let d = tool.description, !d.isEmpty {
                            Text(d).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                        }
                    }
                    .padding(.vertical, 1)
                }
            }

            if !controller.skills.isEmpty {
                Section("Skills (\(controller.skills.count))") {
                    ForEach(controller.skills, id: \.name) { skill in
                        HStack(spacing: 6) {
                            Text("/" + skill.name).font(.system(.caption, design: .monospaced))
                            if let d = skill.description, !d.isEmpty {
                                Text(d).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                            }
                        }
                    }
                }
            }

            Section("Messages (\(snap.messages.count))") {
                ForEach(Array(snap.messages.enumerated()), id: \.offset) { _, message in
                    MessageRow(message: message)
                }
            }
        }
        .listStyle(.inset)
    }

    private func overviewRow(_ label: String, _ value: String) -> some View {
        HStack {
            Text(label).foregroundStyle(.secondary)
            Spacer()
            Text(value).font(.caption).textSelection(.enabled)
        }
    }
}

private struct MessageRow: View {
    let message: AgentMessage

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 6) {
                Text(message.role)
                    .font(.system(.caption2, design: .monospaced))
                    .fontWeight(.semibold)
                    .foregroundStyle(roleColor)
                    .padding(.horizontal, 5)
                    .padding(.vertical, 2)
                    .background(roleColor.opacity(0.12))
                    .clipShape(RoundedRectangle(cornerRadius: 4))
                if let toolName = message.toolName {
                    Text(toolName).font(.caption2).foregroundStyle(.secondary)
                }
                let calls = message.toolCalls ?? []
                if !calls.isEmpty {
                    Text("calls: " + calls.map(\.function.name).joined(separator: ", "))
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
            }
            if !message.content.isEmpty {
                Text(message.content)
                    .font(.system(.caption, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .lineLimit(4)
                    .textSelection(.enabled)
            }
        }
        .padding(.vertical, 2)
    }

    private var roleColor: Color {
        switch message.role {
        case "user": return .accentColor
        case "assistant": return .green
        case "tool": return .orange
        case "system": return .purple
        default: return .secondary
        }
    }
}
