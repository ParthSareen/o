import SwiftUI

struct SidebarView: View {
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openSettings) private var openSettings
    @Bindable var manager: SessionManager
    @State private var list = SessionListStore.shared
    @State private var selection: String? = nil
    @State private var editing = false
    @State private var trashSet: Set<String> = []

    var body: some View {
        VStack(spacing: 0) {
            header
            if let error = list.loadError {
                Text(error).font(.caption).foregroundStyle(.red).padding(8)
            }
            if editing {
                editList
                editBar
            } else {
                HStack {
                    Text("History")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                    Spacer()
                }
                .padding(.horizontal, 16)
                .padding(.top, 4)
                sessionList
            }
            footer
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
        HStack(spacing: 10) {
            if !editing {
                Button {
                    selection = nil
                    manager.startNewChat()
                } label: {
                    Image(systemName: "square.and.pencil")
                        .font(.system(size: 13, weight: .medium))
                        .frame(width: 28, height: 28)
                        .background(Circle().strokeBorder(Color.primary.opacity(0.18)))
                }
                .buttonStyle(.plain)
                .help("New chat in this window (⌘N for a new window)")
            } else {
                Text("Select")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            Spacer()
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
            if !editing {
                Menu {
                    Button("Select…") { editing = true }
                    Button("Delete Empty Sessions") { list.deleteEmptySessions() }
                } label: {
                    Image(systemName: "ellipsis")
                        .font(.system(size: 12, weight: .medium))
                        .frame(width: 28, height: 28)
                        .background(Circle().strokeBorder(Color.primary.opacity(0.18)))
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
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
    }

    /// Bottom profile-style row: which model this window is on, plus settings.
    private var footer: some View {
        VStack(spacing: 0) {
            Divider()
            HStack(spacing: 8) {
                Image(systemName: "circle.hexagongrid.fill")
                    .font(.system(size: 18))
                    .foregroundStyle(.primary.opacity(0.7))
                Text(manager.active.model.isEmpty ? "o" : shortModel(manager.active.model))
                    .font(.callout)
                    .lineLimit(1)
                Spacer()
                Button { openSettings() } label: {
                    Image(systemName: "gearshape")
                        .foregroundStyle(.primary.opacity(0.6))
                }
                .buttonStyle(.plain)
                .help("Settings")
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 10)
        }
    }

    private func shortModel(_ m: String) -> String {
        m.hasSuffix(":latest") ? String(m.dropLast(7)) : m
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
        .tint(.primary.opacity(0.12))
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
        .tint(.primary.opacity(0.12))
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
        HStack(spacing: 7) {
            // indicator column (constant width): spinner = running in bg,
            // ring = current in this window, filled = unread
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

            Text(session.displayTitle)
                .fontWeight(isUnread ? .semibold : .regular)
                .lineLimit(1)
                .truncationMode(.tail)
            Spacer(minLength: 6)
            Text(compactRelativeAge(session.updatedAt))
                .font(.caption2)
                .foregroundStyle(.primary.opacity(0.45))
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
