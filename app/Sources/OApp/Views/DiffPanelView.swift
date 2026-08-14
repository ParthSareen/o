import SwiftUI

/// Codex-style review surface, used both as the narrow inspector and as the
/// full-screen detail view. Stacked per-file diff sections with syntax
/// highlighting, line numbers, inline code comments, and a filterable file
/// tree column.
struct ReviewSurface: View {
    @Bindable var store: DiffStore
    var compact = true
    var onClose: (() -> Void)? = nil

    private struct CommentDraft: Equatable {
        enum Phase: Equatable { case anchored, editing }
        var path: String
        var start: Int
        var end: Int
        var oldSide: Bool
        var phase: Phase = .anchored
    }
    @State private var draft: CommentDraft? = nil
    @State private var draftText = ""
    @State private var showFileTree = true
    @State private var scrollProxy: ScrollViewProxy? = nil
    @State private var hoveredLine: LineKey? = nil

    private typealias LineKey = String // "\(section)#\(index)"

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            content
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    // MARK: header

    private var header: some View {
        HStack(spacing: 8) {
            Text("Changes").font(compact ? .headline : .title3).fontWeight(.semibold)
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
            if !store.comments.isEmpty {
                Label("\(store.comments.count)", systemImage: "text.bubble")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
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
            if let onClose {
                Button(action: onClose) {
                    Image(systemName: "arrow.down.right.and.arrow.up.left")
                }
                .buttonStyle(.plain)
                .help("Back to chat")
            }
        }
        .padding(.horizontal, compact ? 10 : 16)
        .padding(.vertical, 8)
    }

    // MARK: content

    @ViewBuilder
    private var content: some View {
        if !store.loaded {
            loading
        } else if !store.isRepo {
            emptyState("Not a git repository")
        } else if store.sections.isEmpty {
            emptyState("Working tree clean")
        } else {
            HStack(spacing: 0) {
                stackView
                if showFileTree {
                    Divider()
                    fileTree
                        .frame(width: compact ? 210 : 260)
                }
            }
        }
    }

    private var loading: some View {
        VStack { Spacer(); ProgressView(); Spacer() }
            .frame(maxWidth: .infinity)
    }

    private func emptyState(_ text: String) -> some View {
        VStack {
            Spacer()
            Text(text).foregroundStyle(.secondary).font(.callout)
            Spacer()
        }
        .frame(maxWidth: .infinity)
    }

    // MARK: stacked diff with comments

    private var stackView: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 0, pinnedViews: [.sectionHeaders]) {
                    ForEach(store.filteredSections) { section in
                        Section(header: FileSectionHeader(section: section)) {
                            sectionRows(section)
                            if section.truncated {
                                Text("  … truncated …")
                                    .font(.caption2).foregroundStyle(.secondary)
                                    .padding(.vertical, 4).padding(.horizontal, 8)
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

    @ViewBuilder
    private func sectionRows(_ section: FileSection) -> some View {
        // Identity MUST be the line's own UUID: id-by-offset causes rows to
        // be reused across sections (per-section offsets collide), which
        // renders other files' content under a section header.
        ForEach(Array(section.lines.enumerated()), id: \.element.id) { index, line in
            let key = "\(section.path)#\(index)"
            // + only under the mouse cursor (the range preview tint already
            // shows what extending would cover); hidden while editing
            let showAdd = (draft?.phase ?? .anchored) != .editing && hoveredLine == key
            DiffReviewLineRow(
                line: line, path: section.path,
                showAdd: showAdd,
                highlight: lineHighlight(section: section, line: line),
                onAdd: { addTapped(section: section, line: line) }
            )
            .onHover { hovering in hoveredLine = hovering ? key : nil }

            // anchored-start hint: tap + on another line to extend, or on
            // this line again to comment on just it
            if let d = draft, d.phase == .anchored, d.path == section.path, lineAnchor(line) == d.start {
                Text("＋ again on this line for a single-line comment, or ＋ on another line to set the range · esc to cancel")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                    .padding(.leading, 70)
                    .padding(.vertical, 2)
            }
            // inline draft editor / saved comment bubbles under the anchor end
            if let d = draft, d.phase == .editing, d.path == section.path, lineAnchor(line) == d.end {
                CommentEditorRow(text: $draftText,
                                 rangeLabel: rangeLabel(path: section.path, d.start, d.end),
                                 onAdd: commitDraft,
                                 onCancel: { draft = nil; draftText = "" })
            }
            ForEach(commentsEnding(at: section, line: line)) { comment in
                CommentBubbleRow(comment: comment, onDelete: { store.removeComment(comment.id) })
            }
        }
    }

    // MARK: comment interactions

    private func lineAnchor(_ line: DiffLine) -> Int? {
        line.newNo ?? line.oldNo
    }

    /// Visual emphasis for a line: anchor pop, live range preview while
    /// hovering a candidate end, persistent range tint while editing, and a
    /// marker for lines that already carry a comment.
    private func lineHighlight(section: FileSection, line: DiffLine) -> DiffReviewLineRow.Highlight {
        let oldSide = line.newNo == nil
        if let n = lineAnchor(line) {
            if let d = draft, d.path == section.path, d.oldSide == oldSide {
                switch d.phase {
                case .anchored:
                    if n == d.start { return .anchor }
                    // range preview: tint everything from the anchor to the
                    // line currently hovered in this section
                    if let hovered = hoveredLine,
                       hovered.hasPrefix("\(section.path)#"),
                       let hoverIdx = Int(hovered.split(separator: "#").last ?? ""),
                       section.lines.indices.contains(hoverIdx) {
                        let hoverLine = section.lines[hoverIdx]
                        if let hoverN = lineAnchor(hoverLine), (hoverLine.newNo == nil) == oldSide {
                            let lo = Swift.min(d.start, hoverN), hi = Swift.max(d.start, hoverN)
                            if n >= lo && n <= hi { return .draftRange }
                        }
                    }
                case .editing:
                    if n >= d.start && n <= d.end { return .draftRange }
                }
            }
            if store.comments.contains(where: {
                $0.path == section.path && $0.oldSide == oldSide && n >= $0.startLine && n <= $0.endLine
            }) { return .commented }
        }
        return .none
    }

    private func addTapped(section: FileSection, line: DiffLine) {
        guard let n = lineAnchor(line) else { return }
        let oldSide = line.newNo == nil
        if let d = draft, d.phase == .anchored {
            if d.path == section.path && d.oldSide == oldSide {
                if n == d.start {
                    // second tap on the same line: single-line comment
                    draft = CommentDraft(path: d.path, start: n, end: n, oldSide: oldSide, phase: .editing)
                } else {
                    // extend the range to this line
                    draft = CommentDraft(path: d.path,
                                         start: min(d.start, n), end: max(d.start, n),
                                         oldSide: oldSide, phase: .editing)
                }
                return
            }
            // different file/side: re-anchor there
        }
        draft = CommentDraft(path: section.path, start: n, end: n, oldSide: oldSide)
        draftText = ""
    }

    private func commitDraft() {
        guard let d = draft, !draftText.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            draft = nil
            return
        }
        let snippet = snippetFor(path: d.path, start: d.start, end: d.end, oldSide: d.oldSide)
        store.addComment(CodeComment(id: UUID(), path: d.path, startLine: d.start, endLine: d.end,
                                     oldSide: d.oldSide, snippet: snippet,
                                     text: draftText.trimmingCharacters(in: .whitespacesAndNewlines)))
        draft = nil
        draftText = ""
    }

    private func snippetFor(path: String, start: Int, end: Int, oldSide: Bool) -> String {
        guard let section = store.sections.first(where: { $0.path == path }) else { return "" }
        return section.lines.filter { line in
            if oldSide { return line.oldNo.map { ($0 >= start && $0 <= end) } ?? false }
            return line.newNo.map { ($0 >= start && $0 <= end) && line.kind != .removed } ?? false
        }
        .filter { $0.kind == .added || $0.kind == .removed || $0.kind == .context }
        .map(\.text).joined(separator: "\n")
    }

    private func commentsEnding(at section: FileSection, line: DiffLine) -> [CodeComment] {
        guard let n = lineAnchor(line) else { return [] }
        return store.comments.filter { $0.path == section.path && $0.endLine == n }
    }

    private func rangeLabel(path: String, _ start: Int, _ end: Int) -> String {
        let file = (path as NSString).lastPathComponent
        return start == end ? "\(file):\(start)" : "\(file):\(start)-\(end)"
    }

    // MARK: file tree

    private var fileTree: some View {
        VStack(spacing: 0) {
            HStack(spacing: 6) {
                Image(systemName: "magnifyingglass")
                    .font(.caption2).foregroundStyle(.secondary)
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
    }
}

// MARK: - rows

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

/// One diff line with a hover "+" affordance in the gutter for commenting.
struct DiffReviewLineRow: View {
    enum Highlight { case none, anchor, draftRange, commented }

    let line: DiffLine
    let path: String
    let showAdd: Bool
    var highlight: Highlight = .none
    let onAdd: () -> Void

    var body: some View {
        HStack(spacing: 0) {
            // comment-range edge marker
            Rectangle()
                .fill(edgeColor)
                .frame(width: 3)
            ZStack {
                if showAdd && (line.newNo != nil || line.oldNo != nil) {
                    Button(action: onAdd) {
                        Image(systemName: "plus")
                            .font(.system(size: 9, weight: .bold))
                            .foregroundStyle(.primary)
                            .frame(width: 16, height: 16)
                            .background(Color.primary.opacity(0.1))
                            .clipShape(RoundedRectangle(cornerRadius: 4))
                    }
                    .buttonStyle(.plain)
                }
            }
            .frame(width: 22)
            Text(line.newNo.map(String.init) ?? "")
                .font(.system(.caption2, design: .monospaced))
                .foregroundStyle(.tertiary)
                .frame(width: 40, alignment: .trailing)
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

    private var edgeColor: Color {
        switch highlight {
        case .anchor, .draftRange: return Color.primary
        case .commented: return Color.primary.opacity(0.5)
        case .none: return .clear
        }
    }

    private var background: Color {
        switch highlight {
        case .anchor: return Color.primary.opacity(0.16)
        case .draftRange: return Color.primary.opacity(0.09)
        case .commented: return Color.primary.opacity(0.05)
        case .none:
            switch line.kind {
            case .added: return .green.opacity(0.13)
            case .removed: return .red.opacity(0.13)
            case .hunk: return .purple.opacity(0.06)
            default: return .clear
            }
        }
    }
}

/// Inline "local comment" editor under the selected lines.
private struct CommentEditorRow: View {
    @Binding var text: String
    let rangeLabel: String
    let onAdd: () -> Void
    let onCancel: () -> Void
    @FocusState private var focused: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Label("Local comment", systemImage: "text.bubble")
                    .font(.caption)
                    .fontWeight(.medium)
                Spacer()
                Text("on lines \(rangeLabel)")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            .padding(.horizontal, 12)
            .padding(.top, 10)

            TextEditor(text: $text)
                .font(.callout)
                .scrollContentBackground(.hidden)
                .focused($focused)
                .frame(minHeight: 44, maxHeight: 120)
                .padding(.horizontal, 10)
                .onKeyPress(keys: [.escape], phases: .down) { _ in
                    onCancel()
                    return .handled
                }

            HStack {
                Spacer()
                Button("Cancel", action: onCancel)
                    .buttonStyle(.plain)
                    .foregroundStyle(.secondary)
                Button("Comment", action: onAdd)
                    .buttonStyle(.borderedProminent)
                    .controlSize(.small)
                    .disabled(text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
            .padding(.horizontal, 12)
            .padding(.bottom, 10)
        }
        .background(Color(nsColor: .windowBackgroundColor))
        .overlay(
            RoundedRectangle(cornerRadius: 10)
                .stroke(Color.primary.opacity(0.12), lineWidth: 1)
        )
        .clipShape(RoundedRectangle(cornerRadius: 10))
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
        .onAppear { focused = true }
    }
}

/// A saved comment rendered as a bubble under its anchor line.
private struct CommentBubbleRow: View {
    let comment: CodeComment
    let onDelete: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "text.bubble.fill")
                .foregroundStyle(.secondary)
                .font(.caption)
                .padding(.top, 1)
            VStack(alignment: .leading, spacing: 2) {
                Text(comment.text)
                    .font(.callout)
                    .textSelection(.enabled)
                Text(comment.location)
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Button(action: onDelete) {
                Image(systemName: "xmark.circle")
                    .foregroundStyle(.secondary)
                    .font(.caption)
            }
            .buttonStyle(.plain)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(Color.primary.opacity(0.05))
        .padding(.vertical, 2)
    }
}

/// Inspector-host wrapper (kept for the narrow right-side presentation).
struct DiffPanelView: View {
    @Bindable var store: DiffStore
    let workingDir: String

    var body: some View {
        ReviewSurface(store: store, compact: true)
            .onAppear { store.setDirectory(workingDir) }
            .onChange(of: workingDir) { _, dir in store.setDirectory(dir) }
            .onReceive(NotificationCenter.default.publisher(for: .oSessionsChanged)) { _ in
                Task { await store.refresh() }
            }
    }
}
