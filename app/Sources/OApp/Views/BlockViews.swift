import SwiftUI

struct BlockRow: View {
    let block: Block

    var body: some View {
        switch block {
        case .userMessage(_, let text):
            UserMessageRow(text: text)
        case .assistant(_, let text):
            AssistantRow(text: text)
        case .thinking(_, let text):
            ThinkingRow(text: text)
        case .tool(let tool):
            ToolCallRow(tool: tool)
        case .compaction(_, let phase, let detail):
            CompactionRow(phase: phase, detail: detail)
        case .error(_, let message):
            ErrorRow(message: message)
        }
    }
}

struct UserMessageRow: View {
    let text: String

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "person.fill")
                .foregroundStyle(.secondary)
                .frame(width: 18)
            Text(text)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 10)
                .padding(.vertical, 8)
                .background(Color.accentColor.opacity(0.10))
                .clipShape(RoundedRectangle(cornerRadius: 8))
        }
    }
}

struct AssistantRow: View {
    let text: String

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "sparkle")
                .foregroundStyle(.secondary)
                .frame(width: 18)
            MarkdownText(text)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

struct ThinkingRow: View {
    let text: String
    @State private var expanded = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Button {
                withAnimation(.easeInOut(duration: 0.15)) { expanded.toggle() }
            } label: {
                Label("thinking", systemImage: expanded ? "chevron.down" : "chevron.right")
                    .font(.caption)
                    .foregroundStyle(.tertiary)
            }
            .buttonStyle(.plain)
            if expanded {
                Text(text)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .italic()
                    .textSelection(.enabled)
            }
        }
        .padding(.leading, 26)
    }
}

struct ToolCallRow: View {
    let tool: ToolBlock
    @State private var expanded = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Button {
                withAnimation(.easeInOut(duration: 0.15)) { expanded.toggle() }
            } label: {
                HStack(spacing: 8) {
                    statusIcon
                    Text(tool.name)
                        .font(.system(.callout, design: .monospaced))
                        .fontWeight(.medium)
                    Text(summary)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Spacer()
                    if !tool.children.isEmpty {
                        Label("\(tool.children.count)", systemImage: "arrow.triangle.branch")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                    }
                    Image(systemName: expanded ? "chevron.down" : "chevron.right")
                        .font(.caption2)
                        .foregroundStyle(.tertiary)
                }
                .padding(.horizontal, 10)
                .padding(.vertical, 7)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)

            if expanded {
                VStack(alignment: .leading, spacing: 8) {
                    if let dir = tool.workingDir, !dir.isEmpty {
                        Label(dir, systemImage: "folder")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                    }
                    if let args = tool.argsText {
                        detailSection("arguments", text: args)
                    }
                    if let result = tool.result {
                        detailSection("result", text: result)
                    }
                    if let error = tool.errorText {
                        Text(error)
                            .font(.caption)
                            .foregroundStyle(.red)
                            .textSelection(.enabled)
                    }
                    ForEach(tool.children) { child in
                        BlockRow(block: child)
                            .padding(.leading, 8)
                    }
                    if !tool.subagentLive.isEmpty {
                        Text(tool.subagentLive)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .lineLimit(6)
                    }
                }
                .padding(.horizontal, 10)
                .padding(.bottom, 8)
            }
        }
        .background(Color.primary.opacity(0.035))
        .clipShape(RoundedRectangle(cornerRadius: 8))
        .padding(.leading, 26)
    }

    private var statusIcon: some View {
        let (name, color): (String, Color) = {
            switch tool.status {
            case .pending: return ("circle.dotted", .secondary)
            case .running: return ("circle.dotted", .orange)
            case .done: return ("checkmark.circle.fill", .green)
            case .failed: return ("xmark.circle.fill", .red)
            case .denied: return ("hand.raised.fill", .orange)
            case .disabled, .skipped: return ("minus.circle", .secondary)
            }
        }()
        return Image(systemName: name)
            .font(.caption)
            .foregroundStyle(color)
    }

    private var summary: String {
        switch tool.status {
        case .running: return "running…"
        case .pending: return "queued"
        default: return ""
        }
    }

    private func detailSection(_ label: String, text: String) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(label)
                .font(.caption2)
                .foregroundStyle(.tertiary)
            ScrollView {
                Text(text)
                    .font(.system(.caption, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .frame(maxHeight: 220)
        }
    }
}

struct CompactionRow: View {
    let phase: CompactionPhase
    let detail: String

    var body: some View {
        HStack(spacing: 8) {
            Rectangle().fill(Color.secondary.opacity(0.3)).frame(height: 1)
            Label(label, systemImage: "arrow.down.doc")
                .font(.caption2)
                .foregroundStyle(.tertiary)
                .fixedSize()
            Rectangle().fill(Color.secondary.opacity(0.3)).frame(height: 1)
        }
        .padding(.vertical, 4)
    }

    private var label: String {
        switch phase {
        case .running: return detail.isEmpty ? "compacting context…" : detail
        case .done: return "context compacted"
        case .skipped: return detail.isEmpty ? "compaction skipped" : "skipped: \(detail)"
        }
    }
}

struct ErrorRow: View {
    let message: String

    var body: some View {
        Label(message, systemImage: "exclamationmark.triangle.fill")
            .font(.callout)
            .foregroundStyle(.red)
            .textSelection(.enabled)
            .padding(.leading, 26)
    }
}
