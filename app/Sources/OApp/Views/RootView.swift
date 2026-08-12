import AppKit
import SwiftUI

struct RootView: View {
    let spec: SessionSpec
    @State private var controller = SessionController()
    @State private var diffStore = DiffStore()
    @State private var showDiff = false
    @State private var showPrompt = false
    @State private var reviewFullScreen = false
    @State private var booted = false

    var body: some View {
        NavigationSplitView {
            SidebarView(current: controller)
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
            controller.start(spec)
            await SettingsStore.shared.reloadModels()
        }
        .onChange(of: controller.workingDir) { _, dir in
            diffStore.setDirectory(dir)
        }
        .onDisappear {
            controller.stop()
        }
    }

    @ViewBuilder
    private var detail: some View {
        if reviewFullScreen {
            ReviewSurface(store: diffStore, compact: false, onClose: { reviewFullScreen = false })
                .onReceive(NotificationCenter.default.publisher(for: .oSessionsChanged)) { _ in
                    Task { await diffStore.refresh() }
                }
        } else {
            ChatView(controller: controller, diffStore: diffStore)
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
            Button {
                showPrompt = true
            } label: {
                Image(systemName: "doc.text.magnifyingglass")
            }
            .help("Prompt inspector (system prompt, tools, messages)")

            // side panel
            Button {
                if !reviewFullScreen { showDiff.toggle() }
            } label: {
                Label("Changes", systemImage: "checklist")
            }
            .help("Toggle changes panel")
            .disabled(reviewFullScreen)

            // full-screen review
            Button {
                reviewFullScreen = true
                showDiff = false
            } label: {
                Image(systemName: "arrow.up.left.and.arrow.down.forward")
            }
            .help("Review changes full screen")
            .disabled(reviewFullScreen)
        }
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
        }
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
