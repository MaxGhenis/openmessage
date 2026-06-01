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

            // Per-platform alert: a paired bridge silently stopped syncing.
            // Surfaced here because the backend process stays "running" when
            // one platform dies, so the green dot alone would hide the problem
            // (this is exactly how Google Messages rotted for months unnoticed).
            if let alert = backend.platformAlert {
                Divider()
                Button {
                    openWindow(id: "main")
                    NSApp.activate(ignoringOtherApps: true)
                    NotificationCenter.default.post(name: .openPlatformsRequested, object: nil)
                } label: {
                    HStack(alignment: .top, spacing: 8) {
                        Image(systemName: "exclamationmark.triangle.fill")
                            .foregroundStyle(.orange)
                        Text(alert)
                            .font(.callout)
                            .multilineTextAlignment(.leading)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .help("Open Platforms to re-pair")
            }

            Divider()

            Button("Open Messages") {
                openWindow(id: "main")
                NSApp.activate(ignoringOtherApps: true)
            }
            .keyboardShortcut("o")

            SettingsLink {
                Text("Settings…")
            }

            Divider()

            Button("Quit OpenMessage") {
                backend.stop()
                NSApp.terminate(nil)
            }
            .keyboardShortcut("q")
        }
    }

    private var statusColor: Color {
        // A platform alert outranks the green "running" dot — a paired bridge
        // that stopped syncing is a yellow-warning condition even though the
        // backend process is healthy.
        if backend.platformAlert != nil && backend.state == .running {
            return .yellow
        }
        switch backend.state {
        case .running: return .green
        case .starting: return .yellow
        case .error: return .red
        default: return .gray
        }
    }

    private var statusText: String {
        if backend.platformAlert != nil && backend.state == .running {
            return "Attention needed"
        }
        switch backend.state {
        case .running: return "Connected"
        case .starting: return "Starting..."
        case .needsPairing: return "Needs pairing"
        case .error: return "Error"
        case .stopped: return "Stopped"
        }
    }
}
