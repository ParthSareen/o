import SwiftUI

@main
struct OApp: App {
    @State private var settings = SettingsStore.shared

    var body: some Scene {
        WindowGroup("o", for: SessionSpec.self) { $spec in
            RootView(spec: spec ?? .new)
        }
        .defaultSize(width: 980, height: 720)
        .commands {
            CommandGroup(after: .toolbar) {
                Button("Make Text Bigger") {
                    settings.increaseTextScale()
                }
                .keyboardShortcut("=", modifiers: .command)
                Button("Make Text Smaller") {
                    settings.decreaseTextScale()
                }
                .keyboardShortcut("-", modifiers: .command)
                Button("Reset Text Size") {
                    settings.resetTextScale()
                }
                .keyboardShortcut("0", modifiers: .command)
            }
        }
        Settings {
            SettingsView()
        }
    }
}
