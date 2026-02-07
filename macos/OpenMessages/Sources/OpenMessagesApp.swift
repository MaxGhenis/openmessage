import SwiftUI

@main
struct OpenMessagesApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) var appDelegate
    @StateObject private var backend = BackendManager()

    var body: some Scene {
        Window("OpenMessages", id: "main") {
            ContentView(backend: backend)
                .frame(minWidth: 800, minHeight: 500)
        }
        .defaultSize(width: 1100, height: 700)

        MenuBarExtra("OpenMessages", systemImage: "message.fill") {
            MenuBarView(backend: backend)
        }
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate, @unchecked Sendable {
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false // Keep running in menu bar when window closed
    }
}
