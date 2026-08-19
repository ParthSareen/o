import AppKit
import SwiftUI

struct RootView: View {
    let spec: SessionSpec
    @State private var manager = SessionManager()
    @State private var diffStore = DiffStore()
    @State private var showDiff = false
    @State private var showPrompt = false
    @State private var reviewFullScreen = false
    @State private var booted = false
    @State private var settings = SettingsStore.shared

    private var controller: SessionController { manager.active }

    var body: some View {
        NavigationSplitView {
            SidebarView(manager: manager)
                .navigationSplitViewColumnWidth(min: 200, ideal: 230, max: 300)
        } detail: {
            detail
        }
        .navigationTitle(title)
        .toolbar { toolbar }
        .sheet(isPresented: $showPrompt) {
            PromptInspectorView(controller: controller)
        }
        .task {
            guard !booted else { return }
            booted = true
            manager.boot(spec)
            await SettingsStore.shared.reloadModels()
        }
        .onChange(of: activeWorkingDir) { _, dir in
            diffStore.setDirectory(dir)
        }
        .onReceive(NotificationCenter.default.publisher(for: .oSessionsChanged)) { _ in
            Task { await diffStore.refresh() }
        }
        .onDisappear {
            manager.stopAll()
        }
    }

    private var activeWorkingDir: String { controller.workingDir }

    @ViewBuilder
    private var detail: some View {
        detailContent
            // explicit point-size scaling; dynamicTypeSize proved unreliable
            // for the AttributedString-backed transcript
            .environment(\.chatTextScale, settings.prefs.textScale)
            // ⌘. cancels the in-flight run (window-scoped, per-window controller)
            .background(
                Button("") { controller.cancelRun() }
                    .keyboardShortcut(".", modifiers: .command)
                    .hidden()
                    .accessibilityHidden(true)
            )
    }

    @ViewBuilder
    private var detailContent: some View {
        if reviewFullScreen {
            ReviewSurface(store: diffStore, compact: false, onClose: { reviewFullScreen = false })
        } else {
            ChatView(controller: controller, diffStore: diffStore)
                .toolbarBackgroundVisibility(.hidden, for: .windowToolbar)
                .inspector(isPresented: $showDiff) {
                    DiffPanelView(store: diffStore, workingDir: controller.workingDir)
                        .inspectorColumnWidth(min: 340, ideal: 460, max: 720)
                }
        }
    }

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        ToolbarItemGroup(placement: .primaryAction) {
            workingDirButton
            circleButton("doc.text.magnifyingglass",
                         help: "Prompt inspector (system prompt, tools, messages)") {
                showPrompt = true
            }

            circleButton("checklist", help: "Toggle changes panel") {
                if !reviewFullScreen { showDiff.toggle() }
            }
            .disabled(reviewFullScreen)

            circleButton("arrow.up.left.and.arrow.down.right",
                         help: "Review changes full screen") {
                reviewFullScreen = true
                showDiff = false
            }
            .disabled(reviewFullScreen)
        }
    }

    /// Toolbar icon in a filled circle — the top-right pills in the reference UI.
    private func circleButton(_ systemName: String, help: String,
                              action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: systemName)
                .font(.system(size: 12, weight: .medium))
                .frame(width: 26, height: 26)
                .background(Circle().fill(Color.primary.opacity(0.06)))
        }
        .buttonStyle(.plain)
        .help(help)
    }

    private var workingDirButton: some View {
        Button {
            let panel = NSOpenPanel()
            panel.canChooseFiles = false
            panel.canChooseDirectories = true
            panel.allowsMultipleSelection = false
            panel.prompt = "Use as Working Directory"
            if !controller.workingDir.isEmpty {
                panel.directoryURL = URL(fileURLWithPath: controller.workingDir)
            }
            if panel.runModal() == .OK, let url = panel.url {
                controller.changeWorkingDir(url.path)
            }
        } label: {
            Image(systemName: "folder")
                .font(.system(size: 12, weight: .medium))
                .frame(width: 26, height: 26)
                .background(Circle().fill(Color.primary.opacity(0.06)))
        }
        .buttonStyle(.plain)
        .help(workingDirHelp)
    }

    private var workingDirHelp: String {
        controller.workingDir.isEmpty
            ? "Choose working directory"
            : "Change working directory (currently \(controller.workingDir))"
    }

    private var title: String {
        if reviewFullScreen { return "Review" }
        if !controller.sessionName.isEmpty { return controller.sessionName }
        if !controller.model.isEmpty { return controller.model }
        return "o"
    }
}
