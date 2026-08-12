import SwiftUI

struct SidebarView: View {
    @Environment(\.openWindow) private var openWindow
    @Bindable var manager: SessionManager
    @State private var list = SessionListStore.shared
    @State private var selection: String? = nil

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            if let error = list.loadError {
                Text(error).font(.caption).foregroundStyle(.red).padding(8)
            }
            sessionList
        }
        .onAppear { list.start() }
        .onChange(of: selection) { _, newValue in
            guard let id = newValue, id != manager.active.sessionID else { return }
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
                    .foregroundStyle(.white)
                    .padding(.horizontal, 6)
                    .padding(.vertical, 1)
                    .background(Color.accentColor)
                    .clipShape(Capsule())
                    .help("\(list.unreadIDs.count) finished while away")
            }
            Spacer()
            Button {
                selection = nil
                manager.startNewChat()
            } label: {
                Image(systemName: "square.and.pencil")
            }
            .buttonStyle(.plain)
            .help("New chat in this window (⌘N for a new window)")
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
    }

    @ViewBuilder
    private func contextMenu(for session: SessionSummary) -> some View {
        Button("Open in New Window") {
            openWindow(value: SessionSpec(sessionID: session.id,
                                          workingDir: session.workingDir.nilIfEmpty))
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
            // green = current in this window, blue = unread
            Group {
                if isRunning {
                    ProgressView()
                        .controlSize(.mini)
                        .frame(width: 10)
                } else {
                    Circle().fill(indicatorColor).frame(width: 6, height: 6)
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

    private var indicatorColor: Color {
        if isCurrent { return .green }
        if isUnread { return .accentColor }
        return .clear
    }
}
