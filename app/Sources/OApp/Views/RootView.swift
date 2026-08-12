import AppKit
import SwiftUI

struct RootView: View {
    let spec: SessionSpec
    @State private var controller = SessionController()
    @State private var diffStore = DiffStore()
    @State private var showDiff = false
    @State private var showPrompt = false
    @State private var booted = false

    var body: some View {
        NavigationSplitView {
            SidebarView(current: controller)
                .navigationSplitViewColumnWidth(min: 200, ideal: 230, max: 300)
        } detail: {
            ChatView(controller: controller)
        }
        .navigationTitle(title)
        .toolbar { toolbar }
        .inspector(isPresented: $showDiff) {
            DiffPanelView(store: diffStore, workingDir: controller.workingDir)
                .inspectorColumnWidth(min: 320, ideal: 420, max: 640)
        }
        .sheet(isPresented: $showPrompt) {
            PromptInspectorView(controller: controller)
        }
        .task {
            guard !booted else { return }
            booted = true
            controller.start(spec)
            await SettingsStore.shared.reloadModels()
        }
        .onDisappear {
            controller.stop()
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
            Toggle(isOn: $showDiff) {
                Image(systemName: "doc.badge.plus")
            }
            .toggleStyle(.button)
            .help("Working copy changes")
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
        if !controller.sessionName.isEmpty { return controller.sessionName }
        if !controller.model.isEmpty { return controller.model }
        return "o"
    }
}
