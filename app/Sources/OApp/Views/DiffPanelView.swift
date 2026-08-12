import SwiftUI

/// Right-side inspector: working-tree changes for the session's directory.
struct DiffPanelView: View {
    @Bindable var store: DiffStore
    let workingDir: String

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            content
        }
        .onAppear { store.setDirectory(workingDir) }
        .onChange(of: workingDir) { _, dir in store.setDirectory(dir) }
        .onReceive(NotificationCenter.default.publisher(for: .oSessionsChanged)) { _ in
            // agent runs may have edited files
            Task { await store.refresh() }
        }
    }

    private var header: some View {
        HStack(spacing: 6) {
            Text("Changes").font(.headline)
            Text(workingDir.isEmpty ? "" : URL(fileURLWithPath: workingDir).lastPathComponent)
                .font(.caption)
                .foregroundStyle(.secondary)
            Spacer()
            Button {
                Task { await store.refresh() }
            } label: {
                Image(systemName: "arrow.clockwise")
            }
            .buttonStyle(.plain)
            .help("Refresh")
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 8)
    }

    @ViewBuilder
    private var content: some View {
        if workingDir.isEmpty {
            emptyState("No working directory yet")
        } else if store.loaded && !store.isRepo {
            emptyState("Not a git repository")
        } else if store.loaded && store.changes.isEmpty {
            emptyState("Working tree clean")
        } else {
            split
        }
    }

    private func emptyState(_ text: String) -> some View {
        VStack {
            Spacer()
            Text(text).foregroundStyle(.secondary).font(.callout)
            Spacer()
        }
        .frame(maxWidth: .infinity)
    }

    private var split: some View {
        VSplitView {
            fileList
                .frame(minHeight: 120)
            DiffView(text: store.diffText, path: store.selection ?? "")
                .frame(minHeight: 160)
        }
    }

    private var fileList: some View {
        List(store.changes, selection: $store.selection) { change in
            HStack(spacing: 6) {
                statusBadge(change.status)
                Text(change.path)
                    .font(.caption)
                    .lineLimit(1)
                    .truncationMode(.head)
                Spacer()
                if change.added > 0 { Text("+\(change.added)").foregroundStyle(.green) }
                if change.removed > 0 { Text("−\(change.removed)").foregroundStyle(.red) }
            }
            .font(.caption2)
            .tag(change.path)
        }
        .listStyle(.plain)
    }

    private func statusBadge(_ status: String) -> some View {
        let (label, color): (String, Color) = {
            if status.contains("?") { return ("+", .green) }
            if status.contains("M") { return ("M", .orange) }
            if status.contains("A") { return ("A", .green) }
            if status.contains("D") { return ("D", .red) }
            if status.contains("R") { return ("R", .blue) }
            return ("·", .secondary)
        }()
        return Text(label)
            .font(.system(.caption2, design: .monospaced))
            .foregroundStyle(color)
            .frame(width: 12)
    }
}

struct DiffView: View {
    let text: String
    let path: String

    var body: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 0) {
                ForEach(Array(text.split(separator: "\n", omittingEmptySubsequences: false).enumerated()),
                        id: \.offset) { _, line in
                    DiffLineRow(line: String(line))
                }
            }
            .padding(.vertical, 6)
        }
    }
}

private struct DiffLineRow: View {
    let line: String

    var body: some View {
        Text(line.isEmpty ? " " : line)
            .font(.system(.caption, design: .monospaced))
            .foregroundStyle(foreground)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 8)
            .background(background)
    }

    private var background: Color {
        if line.hasPrefix("+") && !line.hasPrefix("+++") { return .green.opacity(0.16) }
        if line.hasPrefix("-") && !line.hasPrefix("---") { return .red.opacity(0.16) }
        return .clear
    }

    private var foreground: Color {
        if line.hasPrefix("@@") { return .purple }
        if line.hasPrefix("diff ") || line.hasPrefix("index ") { return .secondary }
        return .primary
    }
}
