import AppKit
import Combine
import SwiftUI
import UniformTypeIdentifiers

@main
struct OpenMessageApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) var appDelegate
    @StateObject private var backend: BackendManager
    @StateObject private var notifications: NotificationManager
    @StateObject private var contacts: ContactsManager

    init() {
        let backend = BackendManager()
        self._backend = StateObject(wrappedValue: backend)
        self._notifications = StateObject(wrappedValue: NotificationManager(baseURL: backend.baseURL))
        self._contacts = StateObject(wrappedValue: ContactsManager())
    }

    var body: some Scene {
        Window("OpenMessage", id: "main") {
            ContentView(backend: backend, notifications: notifications, contacts: contacts)
                .frame(minWidth: 800, minHeight: 500)
        }
        .defaultSize(width: 1100, height: 700)

        Settings {
            AppSettingsView(backend: backend, notifications: notifications)
        }

        MenuBarExtra("OpenMessage", systemImage: "message.fill") {
            MenuBarView(backend: backend)
        }
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate, @unchecked Sendable {
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false // Keep running in menu bar when window closed
    }
}

extension Notification.Name {
    static let openPlatformsRequested = Notification.Name("OpenMessageOpenPlatformsRequested")
}

@MainActor
private final class SettingsViewModel: ObservableObject {
    struct AppStatus: Decodable {
        struct GoogleStatus: Decodable {
            let connected: Bool
            let paired: Bool
            let needs_pairing: Bool
        }

        struct BackfillStatus: Decodable {
            let running: Bool
            let phase: String?
            let conversations_found: Int?
            let messages_found: Int?
            let folders_scanned: Int?
            let errors: Int?
        }

        struct CompanionStatus: Decodable {
            struct HistorySyncStatus: Decodable {
                let running: Bool
                let started_at: Int?
                let completed_at: Int?
                let imported_conversations: Int?
                let imported_messages: Int?
            }

            let connected: Bool
            let connecting: Bool?
            let paired: Bool
            let pairing: Bool?
            let history_sync: HistorySyncStatus?
        }

        let google: GoogleStatus?
        let backfill: BackfillStatus?
        let whatsapp: CompanionStatus?
        let signal: CompanionStatus?

        static let empty = AppStatus(google: nil, backfill: nil, whatsapp: nil, signal: nil)
    }

    @Published var appStatus: AppStatus = .empty
    @Published var notificationState = NotificationManager.BridgeState(supported: true, enabled: true, permission: "default")
    @Published var feedback = ""
    @Published var isRefreshing = false

    private let baseURL: URL

    init(baseURL: URL) {
        self.baseURL = baseURL
    }

    func refresh(notifications: NotificationManager) async {
        isRefreshing = true
        defer { isRefreshing = false }
        notificationState = await notifications.bridgeState()

        do {
            let (data, response) = try await URLSession.shared.data(from: endpoint("api/status"))
            guard let http = response as? HTTPURLResponse, http.statusCode == 200 else {
                throw URLError(.badServerResponse)
            }
            appStatus = try JSONDecoder().decode(AppStatus.self, from: data)
        } catch {
            feedback = "Could not refresh settings."
        }
    }

    func reconnectGoogleMessages() async {
        do {
            var request = URLRequest(url: endpoint("api/google/reconnect"))
            request.httpMethod = "POST"
            let (_, response) = try await URLSession.shared.data(for: request)
            guard let http = response as? HTTPURLResponse, http.statusCode == 200 else {
                throw URLError(.badServerResponse)
            }
            feedback = "Google Messages reconnecting."
        } catch {
            feedback = "Failed to reconnect Google Messages."
        }
    }

    func startGoogleHistorySync() async {
        do {
            var request = URLRequest(url: endpoint("api/backfill"))
            request.httpMethod = "POST"
            let (_, response) = try await URLSession.shared.data(for: request)
            guard let http = response as? HTTPURLResponse, http.statusCode == 200 else {
                throw URLError(.badServerResponse)
            }
            feedback = "Google Messages history sync started."
        } catch {
            feedback = "Failed to start Google Messages history sync."
        }
    }

    func setNotificationsEnabled(_ enabled: Bool, notifications: NotificationManager) async {
        notificationState = await notifications.setEnabled(enabled)
        feedback = notificationState.enabled ? "Desktop notifications enabled." : "Desktop notifications disabled."
    }

    func copyDiagnostics() async {
        do {
            let text = try await diagnosticsText()
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(text, forType: .string)
            feedback = "Diagnostics copied."
        } catch {
            feedback = "Failed to copy diagnostics."
        }
    }

    func exportDiagnostics() async {
        do {
            let text = try await diagnosticsText()
            let panel = NSSavePanel()
            panel.canCreateDirectories = true
            panel.nameFieldStringValue = "openmessage-diagnostics.json"
            panel.allowedContentTypes = [.json]
            guard panel.runModal() == .OK, let url = panel.url else { return }
            try text.write(to: url, atomically: true, encoding: .utf8)
            feedback = "Diagnostics exported."
        } catch {
            feedback = "Failed to export diagnostics."
        }
    }

    func reportIssue() async {
        let body = """
        ## What happened

        <describe the problem>

        ## Diagnostics

        Paste the copied diagnostics snapshot here.
        """
        let url = URL(string: "https://github.com/MaxGhenis/openmessage/issues/new?title=\(urlEscape("OpenMessage issue report"))&body=\(urlEscape(body))")!
        NSWorkspace.shared.open(url)
        feedback = "Opened GitHub issue form."
    }

    private func diagnosticsText() async throws -> String {
        let (data, response) = try await URLSession.shared.data(from: endpoint("api/diagnostics"))
        guard let http = response as? HTTPURLResponse, http.statusCode == 200 else {
            throw URLError(.badServerResponse)
        }
        let object = try JSONSerialization.jsonObject(with: data)
        let pretty = try JSONSerialization.data(withJSONObject: object, options: [.prettyPrinted, .sortedKeys])
        return String(decoding: pretty, as: UTF8.self)
    }

    private func endpoint(_ path: String) -> URL {
        baseURL.appendingPathComponent(path)
    }

    private func urlEscape(_ value: String) -> String {
        value.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? value
    }
}

private struct AppSettingsView: View {
    @ObservedObject var backend: BackendManager
    @ObservedObject var notifications: NotificationManager
    @StateObject private var model: SettingsViewModel
    @Environment(\.openWindow) private var openWindow

    init(backend: BackendManager, notifications: NotificationManager) {
        self.backend = backend
        self.notifications = notifications
        self._model = StateObject(wrappedValue: SettingsViewModel(baseURL: backend.baseURL))
    }

    var body: some View {
        TabView {
            platformsTab
                .tabItem { Label("Platforms", systemImage: "square.stack.3d.up.fill") }
            notificationsTab
                .tabItem { Label("Notifications", systemImage: "bell.badge.fill") }
            supportTab
                .tabItem { Label("Support", systemImage: "lifepreserver.fill") }
        }
        .frame(width: 560, height: 420)
        .task {
            await model.refresh(notifications: notifications)
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(5))
                await model.refresh(notifications: notifications)
            }
        }
    }

    private var platformsTab: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Pair and manage the services connected to your local inbox.")
                .font(.subheadline)
                .foregroundStyle(.secondary)

            platformRow(
                title: "Google Messages",
                subtitle: "SMS and RCS through your Android phone",
                status: googleStatusText
            ) {
                if googleNeedsPairing {
                    Button("Start pairing") {
                        openMainWindow()
                        backend.beginGooglePairing()
                    }
                    .buttonStyle(.borderedProminent)
                } else if googleNeedsReconnect {
                    Button("Reconnect") {
                        Task {
                            await model.reconnectGoogleMessages()
                            await model.refresh(notifications: notifications)
                        }
                    }
                    .buttonStyle(.borderedProminent)
                } else {
                    VStack(alignment: .leading, spacing: 10) {
                        HStack(spacing: 10) {
                            Button("Open inbox") {
                                openMainWindow()
                            }
                            .buttonStyle(.bordered)

                            Button(model.appStatus.backfill?.running == true ? "Syncing history…" : "Sync history") {
                                Task {
                                    await model.startGoogleHistorySync()
                                    await model.refresh(notifications: notifications)
                                }
                            }
                            .buttonStyle(.borderedProminent)
                            .disabled(model.appStatus.backfill?.running == true)
                        }

                        Text(googleBackfillText)
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                    }
                }
            }

            platformRow(
                title: "WhatsApp",
                subtitle: "Live linked-device sync, media, typing, and replies",
                status: companionStatusText(model.appStatus.whatsapp)
            ) {
                Button(companionActionLabel(model.appStatus.whatsapp)) {
                    openPlatformsInMainWindow()
                }
                .buttonStyle(.borderedProminent)
            }

            platformRow(
                title: "Signal",
                subtitle: "Private text sync from a linked Signal device",
                status: companionStatusText(model.appStatus.signal)
            ) {
                VStack(alignment: .leading, spacing: 10) {
                    Button(companionActionLabel(model.appStatus.signal)) {
                        openPlatformsInMainWindow()
                    }
                    .buttonStyle(.borderedProminent)

                    Text(signalHistoryText)
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                }
            }

            Spacer()

            settingsFooter
        }
        .padding(22)
    }

    private var notificationsTab: some View {
        VStack(alignment: .leading, spacing: 18) {
            Toggle("Enable desktop notifications", isOn: Binding(
                get: { model.notificationState.enabled },
                set: { enabled in
                    Task { await model.setNotificationsEnabled(enabled, notifications: notifications) }
                }
            ))
            .toggleStyle(.switch)

            LabeledContent("Permission") {
                Text(notificationPermissionLabel)
                    .foregroundStyle(.secondary)
            }

            Text("Conversation-level mute and mentions-only controls still live inside each thread.")
                .font(.subheadline)
                .foregroundStyle(.secondary)

            HStack(spacing: 10) {
                Button("Open System Settings") {
                    notifications.openSystemSettings()
                }
                .buttonStyle(.bordered)

                Button("Refresh state") {
                    Task { await model.refresh(notifications: notifications) }
                }
                .buttonStyle(.bordered)
            }

            Spacer()

            settingsFooter
        }
        .padding(22)
    }

    private var supportTab: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Export diagnostics and open the issue form if something breaks.")
                .font(.subheadline)
                .foregroundStyle(.secondary)

            HStack(spacing: 10) {
                Button("Copy diagnostics") {
                    Task { await model.copyDiagnostics() }
                }
                .buttonStyle(.borderedProminent)

                Button("Export JSON") {
                    Task { await model.exportDiagnostics() }
                }
                .buttonStyle(.bordered)

                Button("Report issue") {
                    Task { await model.reportIssue() }
                }
                .buttonStyle(.bordered)
            }

            Spacer()

            settingsFooter
        }
        .padding(22)
    }

    private func platformRow<Actions: View>(title: String, subtitle: String, status: String, @ViewBuilder actions: () -> Actions) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .firstTextBaseline) {
                VStack(alignment: .leading, spacing: 3) {
                    Text(title)
                        .font(.headline)
                    Text(subtitle)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
                Spacer()
                Text(status)
                    .font(.caption.weight(.semibold))
                    .padding(.horizontal, 10)
                    .padding(.vertical, 6)
                    .background(Capsule().fill(Color.secondary.opacity(0.12)))
            }
            actions()
        }
        .padding(16)
        .background(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .fill(Color(nsColor: .controlBackgroundColor))
        )
    }

    private var googleNeedsPairing: Bool {
        guard let google = model.appStatus.google else { return true }
        return !google.paired || google.needs_pairing
    }

    private var googleNeedsReconnect: Bool {
        guard let google = model.appStatus.google else { return false }
        return google.paired && !google.connected && !google.needs_pairing
    }

    private var googleStatusText: String {
        guard let google = model.appStatus.google else { return "Unavailable" }
        if google.connected { return "Connected" }
        if google.paired && !google.needs_pairing { return "Needs reconnect" }
        return "Needs pairing"
    }

    private var googleBackfillText: String {
        guard let backfill = model.appStatus.backfill else {
            return "Fetch older SMS and RCS threads from your Android phone when you need them."
        }
        guard backfill.running else {
            return "Fetch older SMS and RCS threads from your Android phone when you need them."
        }
        var parts: [String] = []
        if let phase = backfill.phase, !phase.isEmpty {
            parts.append("phase: \(phase)")
        }
        if let folders = backfill.folders_scanned, folders > 0 {
            parts.append("\(folders) folders")
        }
        if let conversations = backfill.conversations_found, conversations > 0 {
            parts.append("\(conversations) conversations")
        }
        if let messages = backfill.messages_found, messages > 0 {
            parts.append("\(messages) messages")
        }
        if let errors = backfill.errors, errors > 0 {
            parts.append("\(errors) errors")
        }
        if parts.isEmpty {
            return "Google Messages history sync is running."
        }
        return "Google Messages history sync is running • " + parts.joined(separator: " • ")
    }

    private func companionStatusText(_ status: SettingsViewModel.AppStatus.CompanionStatus?) -> String {
        guard let status else { return "Unavailable" }
        if status.history_sync?.running == true { return "Importing history" }
        if status.connected { return "Connected" }
        if status.connecting == true { return "Connecting" }
        if status.pairing == true { return "Awaiting scan" }
        if status.paired { return "Paired" }
        return "Not paired"
    }

    private func companionActionLabel(_ status: SettingsViewModel.AppStatus.CompanionStatus?) -> String {
        guard let status else { return "Open inbox" }
        if status.connected { return "Manage in inbox" }
        if status.connecting == true || status.pairing == true { return "Open inbox" }
        if status.paired { return "Reconnect in inbox" }
        return "Pair in inbox"
    }

    private var signalHistoryText: String {
        if let sync = model.appStatus.signal?.history_sync {
            let chats = sync.imported_conversations ?? 0
            let messages = sync.imported_messages ?? 0
            if sync.running {
                if chats > 0 || messages > 0 {
                    return "Signal history import is running • \(chats) chats • \(messages) messages imported so far."
                }
                return "Signal history import is running. Keep your phone nearby while the linked-device transfer completes."
            }
            if sync.completed_at != nil {
                if chats > 0 || messages > 0 {
                    return "Signal history import finished • \(chats) chats • \(messages) messages imported during pairing."
                }
                return "Signal pairing finished, but no older history arrived. If you expected existing chats, use Reset Signal in the inbox and pair again, then choose Transfer Message History on your phone."
            }
        }
        if model.appStatus.signal?.pairing == true {
            return "Signal history transfer happens during pairing. After you scan, choose Transfer Message History on your phone. Initial import can take a while."
        }
        if let signal = model.appStatus.signal, signal.connected || signal.connecting == true || signal.paired {
            return "Signal history transfer only happens during pairing. If you skipped it, use Reset Signal in the inbox and pair again, then choose Transfer Message History on your phone."
        }
        return "To import existing Signal chats, choose Transfer Message History on your phone while pairing. If you skip it, you can Reset Signal and pair again later."
    }

    private var notificationPermissionLabel: String {
        switch model.notificationState.permission {
        case "granted":
            return "Allowed"
        case "denied":
            return "Blocked in macOS"
        default:
            return "Not decided"
        }
    }

    private var settingsFooter: some View {
        HStack {
            if model.isRefreshing {
                ProgressView()
                    .controlSize(.small)
            }
            Text(model.feedback)
                .font(.footnote)
                .foregroundStyle(.secondary)
            Spacer()
        }
    }

    private func openMainWindow() {
        openWindow(id: "main")
        NSApp.activate(ignoringOtherApps: true)
    }

    private func openPlatformsInMainWindow() {
        openMainWindow()
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) {
            NotificationCenter.default.post(name: .openPlatformsRequested, object: nil)
        }
    }
}
