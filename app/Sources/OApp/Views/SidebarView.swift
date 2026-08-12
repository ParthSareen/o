import SwiftUI

struct SidebarView: View {
    @Environment(\.openWindow) private var openWindow
    let current: SessionController
    @State private var list = SessionListStore.shared
    @State private var selection: String? = nil

    var body: some View {
        VStack(spacing: 0) {
            header
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
                openWindow(value: SessionSpec.new)
            } label: {
                Image(systemName: "square.and.pencil")
            }
            .buttonStyle(.plain)
            .help("New session in new window")
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
    }

    private var sessionList: some View {
        List(selection: $selection) {
            ForEach(list.sessions) { session in
                SessionRow(session: session, isCurrent: session.id == current.sessionID)
                    .tag(session.id)
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
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 6) {
                if isCurrent {
                    Circle().fill(Color.accentColor).frame(width: 6, height: 6)
                }
                Text(session.displayTitle)
                    .lineLimit(1)
                    .truncationMode(.tail)
            }
            HStack(spacing: 6) {
                Text(session.model).lineLimit(1)
                Spacer()
                Text(session.updatedAt, style: .relative)
            }
            .font(.caption2)
            .foregroundStyle(.secondary)
        }
        .padding(.vertical, 2)
    }
}
