import SwiftUI

struct SidebarView: View {
    @Environment(\.openWindow) private var openWindow
    let current: SessionController
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
            guard let id = newValue, id != current.sessionID else { return }
            let dir = list.sessions.first(where: { $0.id == id })?.workingDir
            current.restart(with: SessionSpec(sessionID: id, workingDir: dir.nilIfEmpty))
        }
        .onChange(of: current.sessionID) { _, newValue in
            selection = newValue
        }
    }

    private var header: some View {
        HStack {
            Text("Sessions").font(.headline)
            Spacer()
            Button {
                startNewChat()
            } label: {
                Image(systemName: "square.and.pencil")
            }
            .buttonStyle(.plain)
            .help("New chat in this window (⌘N for a new window)")
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
    }

    /// "New session" starts a fresh conversation in this window. An in-flight
    /// run isn't killed: its process detaches, finishes, persists, and exits.
    /// New *windows* stay under ⌘N.
    private func startNewChat() {
        selection = nil
        current.startNewChat()
    }

    private var sessionList: some View {
        List(selection: $selection) {
            ForEach(list.sessions) { session in
                SessionRow(session: session, isCurrent: session.id == current.sessionID)
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
            list.delete(session.id)
        }
    }
}

private struct SessionRow: View {
    let session: SessionSummary
    let isCurrent: Bool

    var body: some View {
        HStack(alignment: .top, spacing: 7) {
            // constant-width live indicator: titles never shift when the
            // current session changes
            Circle()
                .fill(isCurrent ? Color.green : Color.clear)
                .frame(width: 6, height: 6)
                .padding(.top, 5)

            VStack(alignment: .leading, spacing: 2) {
                Text(session.displayTitle)
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
}
