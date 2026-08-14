import SwiftUI

struct SidebarView: View {
    @Environment(\.openWindow) private var openWindow
    @Bindable var manager: SessionManager
    @State private var list = SessionListStore.shared
    @State private var selection: String? = nil
    @State private var editing = false
    @State private var trashSet: Set<String> = []

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            if let error = list.loadError {
                Text(error).font(.caption).foregroundStyle(.red).padding(8)
            }
            if editing {
                editList
                editBar
            } else {
                sessionList
            }
        }
        .onAppear { list.start() }
        .onChange(of: selection) { _, newValue in
            guard !editing, let id = newValue, id != manager.active.sessionID else { return }
            let dir = list.sessions.first(where: { $0.id == id })?.workingDir
            manager.switchTo(id, workingDir: dir.nilIfEmpty)
        }
        .onChange(of: manager.active.sessionID) { _, newValue in
            selection = newValue
        }
    }

    private var header: some View {
        HStack {
            Text("Sessions").font(.headline)
            if !list.unreadIDs.isEmpty {
                Text("\(list.unreadIDs.count)")
                    .font(.caption2)
                    .fontWeight(.semibold)
                    .foregroundStyle(.background)
                    .padding(.horizontal, 6)
                    .padding(.vertical, 1)
                    .background(Color.primary)
                    .clipShape(Capsule())
                    .help("\(list.unreadIDs.count) unread")
            }
            Spacer()
            if !editing {
                Button {
                    selection = nil
                    manager.startNewChat()
                } label: {
                    Image(systemName: "square.and.pencil")
                }
                .buttonStyle(.plain)
                .help("New chat in this window (⌘N for a new window)")
                Menu {
                    Button("Select…") { editing = true }
                    Button("Delete Empty Sessions") { list.deleteEmptySessions() }
                } label: {
                    Image(systemName: "ellipsis")
                        .padding(.horizontal, 6)
                }
                .menuStyle(.borderlessButton)
                .menuIndicator(.hidden)
                .fixedSize()
            } else {
                Button("Done") {
                    editing = false
                    trashSet = []
                }
                .font(.callout)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
    }

    private var sessionList: some View {
        List(selection: $selection) {
            ForEach(list.sessions) { session in
                SessionRow(
                    session: session,
                    isCurrent: session.id == manager.active.sessionID,
                    isUnread: list.unreadIDs.contains(session.id),
                    isRunning: list.runningIDs.contains(session.id)
                )
                .tag(session.id)
                .listRowSeparator(.hidden)
                .contextMenu { contextMenu(for: session) }
            }
        }
        .listStyle(.sidebar)
        .tint(.gray)
    }

    private var editList: some View {
        List(selection: $trashSet) {
            ForEach(list.sessions) { session in
                SessionRow(
                    session: session,
                    isCurrent: false,
                    isUnread: list.unreadIDs.contains(session.id),
                    isRunning: list.runningIDs.contains(session.id)
                )
                .tag(session.id)
                .listRowSeparator(.hidden)
            }
        }
        .listStyle(.sidebar)
        .tint(.gray)
    }

    private var editBar: some View {
        VStack(spacing: 0) {
            Divider()
            HStack {
                Button("Select All") {
                    trashSet = Set(list.sessions.map(\.id))
                }
                .font(.caption)
                Spacer()
                Button("Delete (\(trashSet.count))", role: .destructive) {
                    for id in trashSet {
                        manager.sessionDeleted(id)
                        list.delete(id)
                    }
                    trashSet = []
                }
                .font(.caption)
                .disabled(trashSet.isEmpty)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
        }
    }

    @ViewBuilder
    private func contextMenu(for session: SessionSummary) -> some View {
        Button("Open in New Window") {
            openWindow(value: SessionSpec(sessionID: session.id,
                                          workingDir: session.workingDir.nilIfEmpty))
        }
        if list.unreadIDs.contains(session.id) {
            Button("Mark as Read") { list.markRead(session.id) }
        } else {
            Button("Mark as Unread") { list.markUnread(session.id) }
        }
        Divider()
        Button("Delete", role: .destructive) {
            manager.sessionDeleted(session.id)
            list.delete(session.id)
        }
    }
}

private struct SessionRow: View {
    let session: SessionSummary
    let isCurrent: Bool
    let isUnread: Bool
    let isRunning: Bool

    var body: some View {
        HStack(alignment: .top, spacing: 7) {
            // indicator column (constant width): spinner = running in bg,
            // blue ring = current in this window, blue filled = unread
            Group {
                if isRunning {
                    ProgressView()
                        .controlSize(.mini)
                        .frame(width: 10)
                } else {
                    indicator
                }
            }
            .frame(width: 10)
            .padding(.top, 4)

            VStack(alignment: .leading, spacing: 2) {
                Text(session.displayTitle)
                    .fontWeight(isUnread ? .semibold : .regular)
                    .lineLimit(1)
                    .truncationMode(.tail)
                HStack(spacing: 6) {
                    Text(session.model).lineLimit(1)
                    Spacer(minLength: 4)
                    Text(compactRelativeAge(session.updatedAt))
                        .foregroundStyle(.primary.opacity(0.45))
                }
                .font(.caption2)
                .foregroundStyle(.primary.opacity(0.6))
            }
        }
        .padding(.vertical, 2)
    }

    @ViewBuilder
    private var indicator: some View {
        if isCurrent {
            Circle().strokeBorder(Color.primary, lineWidth: 1.4).frame(width: 6, height: 6)
        } else if isUnread {
            Circle().fill(Color.primary).frame(width: 6, height: 6)
        } else {
            Circle().fill(Color.clear).frame(width: 6, height: 6)
        }
    }
}
