import SwiftUI

/// Codex-style review pane: stacked per-file diff sections with syntax
/// highlighting and line numbers, plus a filterable file tree on the right.
struct DiffPanelView: View {
    @Bindable var store: DiffStore
    let workingDir: String
    @State private var showFileTree = true

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            content
        }
        .onAppear { store.setDirectory(workingDir) }
        .onChange(of: workingDir) { _, dir in store.setDirectory(dir) }
        .onReceive(NotificationCenter.default.publisher(for: .oSessionsChanged)) { _ in
            Task { await store.refresh() } // agent runs may have edited files
        }
    }

    // MARK: header

    private var header: some View {
        HStack(spacing: 8) {
            Text("Changes").font(.headline)
            if !store.branch.isEmpty {
                Text(store.branch)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            if !store.sections.isEmpty {
                Text("+\(store.totalAdded)").foregroundStyle(.green)
                    .font(.caption.monospacedDigit())
                Text("−\(store.totalRemoved)").foregroundStyle(.red)
                    .font(.caption.monospacedDigit())
            }
            Spacer()
            Button { Task { await store.refresh() } } label: {
                Image(systemName: "arrow.clockwise")
            }
            .buttonStyle(.plain)
            .help("Refresh")
            Toggle(isOn: $showFileTree) {
                Image(systemName: "sidebar.right")
            }
            .toggleStyle(.button)
            .help(showFileTree ? "Hide files" : "Show files")
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 8)
    }

    // MARK: content

    @ViewBuilder
    private var content: some View {
        if workingDir.isEmpty {
            emptyState("No working directory yet")
        } else if store.loaded && !store.isRepo {
            emptyState("Not a git repository")
        } else if store.loaded && store.sections.isEmpty {
            emptyState("Working tree clean")
        } else {
            HStack(spacing: 0) {
                stackView
                if showFileTree {
                    Divider()
                    fileTree
                }
            }
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

    // MARK: stacked diff (all files)

    private var stackView: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 0, pinnedViews: [.sectionHeaders]) {
                    ForEach(store.filteredSections) { section in
                        Section(header: FileSectionHeader(section: section)) {
                            ForEach(section.lines) { line in
                                DiffLineRow(line: line, path: section.path)
                            }
                            if section.truncated {
                                Text("  … truncated …")
                                    .font(.caption2)
                                    .foregroundStyle(.secondary)
                                    .padding(.vertical, 4)
                                    .padding(.horizontal, 8)
                            }
                            Color.clear.frame(height: 10)
                        }
                        .id(section.path)
                    }
                }
            }
            .onAppear { scrollProxy = proxy }
            .onDisappear { scrollProxy = nil }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    @State private var scrollProxy: ScrollViewProxy? = nil

    // MARK: file tree (right column)

    private var fileTree: some View {
        VStack(spacing: 0) {
            HStack(spacing: 6) {
                Image(systemName: "magnifyingglass")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                TextField("Filter files…", text: $store.filter)
                    .textFieldStyle(.plain)
                    .font(.caption)
            }
            .padding(6)
            .background(Color.primary.opacity(0.04))
            .clipShape(RoundedRectangle(cornerRadius: 6))
            .padding(8)

            List {
                ForEach(store.groupedSections, id: \.dir) { group in
                    Section {
                        ForEach(group.files) { file in
                            Button {
                                withAnimation { scrollProxy?.scrollTo(file.path, anchor: .top) }
                            } label: {
                                FileTreeRow(file: file)
                            }
                            .buttonStyle(.plain)
                        }
                    } header: {
                        Label(group.dir == "." ? "/" : group.dir, systemImage: "folder")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                    }
                }
            }
            .listStyle(.sidebar)
            .scrollContentBackground(.hidden)
        }
        .frame(width: 210)
    }
}

private struct FileTreeRow: View {
    let file: FileSection

    var body: some View {
        HStack(spacing: 6) {
            statusBadge
            Text((file.path as NSString).lastPathComponent)
                .font(.caption)
                .lineLimit(1)
                .truncationMode(.middle)
            Spacer(minLength: 4)
            counts
        }
        .padding(.vertical, 1)
        .contentShape(Rectangle())
    }

    private var statusBadge: some View {
        let (label, color): (String, Color) = {
            if file.status.contains("?") { return ("+", .green) }
            if file.status.contains("A") { return ("+", .green) }
            if file.status.contains("D") { return ("−", .red) }
            if file.status.contains("R") { return ("→", .blue) }
            return ("•", .orange)
        }()
        return Text(label)
            .font(.system(.caption2, design: .monospaced))
            .foregroundStyle(color)
            .frame(width: 10)
    }

    @ViewBuilder
    private var counts: some View {
        HStack(spacing: 3) {
            if file.added > 0 { Text("+\(file.added)").foregroundStyle(.green) }
            if file.removed > 0 { Text("−\(file.removed)").foregroundStyle(.red) }
        }
        .font(.system(.caption2, design: .monospaced))
    }
}

private struct FileSectionHeader: View {
    let section: FileSection

    var body: some View {
        HStack(spacing: 8) {
            Text(section.path)
                .font(.system(.caption, design: .monospaced))
                .fontWeight(.medium)
                .lineLimit(1)
                .truncationMode(.middle)
            HStack(spacing: 4) {
                if section.added > 0 { Text("+\(section.added)").foregroundStyle(.green) }
                if section.removed > 0 { Text("−\(section.removed)").foregroundStyle(.red) }
            }
            .font(.system(.caption, design: .monospaced))
            Spacer()
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
        .background(.bar)
    }
}

struct DiffLineRow: View {
    let line: DiffLine
    let path: String

    var body: some View {
        HStack(spacing: 0) {
            // gutter: new-side number for added/context, blank for removed
            Text(line.newNo.map(String.init) ?? "")
                .font(.system(.caption2, design: .monospaced))
                .foregroundStyle(.tertiary)
                .frame(width: 44, alignment: .trailing)
                .padding(.trailing, 8)
            marker
            text
            Spacer(minLength: 0)
        }
        .background(background)
    }

    private var marker: some View {
        Text(markerText)
            .font(.system(.caption, design: .monospaced))
            .foregroundStyle(markerColor)
            .frame(width: 14)
    }

    private var text: some View {
        Group {
            switch line.kind {
            case .hunk:
                Text(line.text)
                    .font(.system(.caption, design: .monospaced))
                    .foregroundStyle(.purple)
            case .meta:
                Text(line.text)
                    .font(.system(.caption, design: .monospaced))
                    .foregroundStyle(.tertiary)
            case .added, .removed, .context:
                Text(DiffSyntax.highlight(line.text, path: path))
                    .font(.system(.caption, design: .monospaced))
            }
        }
    }

    private var markerText: String {
        switch line.kind {
        case .added: return "+"
        case .removed: return "−"
        default: return " "
        }
    }

    private var markerColor: Color {
        switch line.kind {
        case .added: return .green
        case .removed: return .red
        default: return .clear
        }
    }

    private var background: Color {
        switch line.kind {
        case .added: return .green.opacity(0.13)
        case .removed: return .red.opacity(0.13)
        case .hunk: return .purple.opacity(0.06)
        default: return .clear
        }
    }
}
