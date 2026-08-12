import SwiftUI

@main
struct OApp: App {
    var body: some Scene {
        WindowGroup("o", for: SessionSpec.self) { $spec in
            RootView(spec: spec ?? .new)
        }
        .defaultSize(width: 980, height: 720)

        Settings {
            SettingsView()
        }
    }
}
