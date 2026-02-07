import SwiftUI

struct MenuBarView: View {
    @ObservedObject var backend: BackendManager
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            // Status
            HStack(spacing: 8) {
                Circle()
                    .fill(statusColor)
                    .frame(width: 8, height: 8)
                Text(statusText)
                    .font(.callout)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)

            Divider()

            Button("Open Messages") {
                openWindow(id: "main")
                NSApp.activate(ignoringOtherApps: true)
            }
            .keyboardShortcut("o")

            Divider()

            Button("Quit OpenMessages") {
                backend.stop()
                NSApp.terminate(nil)
            }
            .keyboardShortcut("q")
        }
    }

    private var statusColor: Color {
        switch backend.state {
        case .running: .green
        case .starting: .yellow
        case .error: .red
        default: .gray
        }
    }

    private var statusText: String {
        switch backend.state {
        case .running: "Connected"
        case .starting: "Starting..."
        case .needsPairing: "Needs pairing"
        case .error: "Error"
        case .stopped: "Stopped"
        }
    }
}
