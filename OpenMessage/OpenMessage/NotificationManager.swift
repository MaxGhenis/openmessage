import Foundation
import Combine
import AppKit
import UserNotifications
import os

/// Subscribes to backend invalidations and sends native macOS notifications.
@MainActor
final class NotificationManager: NSObject, ObservableObject, UNUserNotificationCenterDelegate {
    struct BridgeState: Encodable {
        let supported: Bool
        let enabled: Bool
        let permission: String
    }

    private struct ConversationSummary: Decodable {
        let conversationID: String
        let lastMessageTS: Int64
        let name: String?

        enum CodingKeys: String, CodingKey {
            case conversationID = "ConversationID"
            case lastMessageTS = "LastMessageTS"
            case name = "Name"
        }
    }

    private struct MessageSummary: Decodable {
        let messageID: String
        let timestampMS: Int64
        let isFromMe: Bool
        let body: String?
        let senderName: String?

        enum CodingKeys: String, CodingKey {
            case messageID = "MessageID"
            case timestampMS = "TimestampMS"
            case isFromMe = "IsFromMe"
            case body = "Body"
            case senderName = "SenderName"
        }
    }

    private let logger = Logger(subsystem: "com.openmessage.app", category: "Notifications")
    private let defaults = UserDefaults.standard
    private let baseURL: URL
    private var streamTask: Task<Void, Never>?
    private var lastSeenTimestamps: [String: Int64] = [:]
    private var seenMessageIDs = Set<String>()
    private var seenMessageOrder: [String] = []
    private var preferenceEnabled: Bool

    private let recentConversationLimit = 50
    private let recentMessageLimit = 10
    private let reconnectDelay: Duration = .seconds(2)
    private let seenMessageCap = 500
    private let preferenceKey = "desktopNotificationsEnabled"

    init(baseURL: URL) {
        self.baseURL = baseURL
        if defaults.object(forKey: preferenceKey) == nil {
            defaults.set(true, forKey: preferenceKey)
        }
        self.preferenceEnabled = defaults.bool(forKey: preferenceKey)
        super.init()
        UNUserNotificationCenter.current().delegate = self
    }

    func start() {
        stop()
        Task {
            await startIfAllowed()
        }
    }

    func stop() {
        streamTask?.cancel()
        streamTask = nil
    }

    /// Helper that crosses the actor boundary returning only the Sendable
    /// auth status, not the full UNNotificationSettings (which is not Sendable).
    private nonisolated func currentAuthStatus() async -> UNAuthorizationStatus {
        let settings = await UNUserNotificationCenter.current().notificationSettings()
        return settings.authorizationStatus
    }

    func bridgeState() async -> BridgeState {
        let status = await currentAuthStatus()
        return BridgeState(
            supported: true,
            enabled: preferenceEnabled && isGranted(status),
            permission: permissionString(status)
        )
    }

    func setEnabled(_ enabled: Bool) async -> BridgeState {
        if !enabled {
            disablePreferenceAndStop()
            return await bridgeState()
        }

        preferenceEnabled = true
        defaults.set(true, forKey: preferenceKey)

        let status = await currentAuthStatus()
        if status == .notDetermined {
            let granted = await requestPermission()
            if !granted {
                disablePreferenceAndStop()
                return await bridgeState()
            }
        } else if !isGranted(status) {
            disablePreferenceAndStop()
            return await bridgeState()
        }

        await startIfAllowed()
        return await bridgeState()
    }

    func openSystemSettings() {
        let candidates = [
            "x-apple.systempreferences:com.apple.Notifications-Settings.extension",
            "x-apple.systempreferences:com.apple.preference.notifications",
        ]
        for raw in candidates {
            guard let url = URL(string: raw) else { continue }
            if NSWorkspace.shared.open(url) {
                return
            }
        }
    }

    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        if #available(macOS 11.0, *) {
            completionHandler([.banner, .list, .sound, .badge])
        } else {
            completionHandler([.alert, .sound, .badge])
        }
    }

    private func startIfAllowed() async {
        guard preferenceEnabled else { return }
        let status = await currentAuthStatus()
        if status == .notDetermined {
            let granted = await requestPermission()
            guard granted else {
                disablePreferenceAndStop()
                return
            }
        } else if !isGranted(status) {
            return
        }

        streamTask = Task {
            await primeBaseline()
            await runEventLoop()
        }
    }

    private func requestPermission() async -> Bool {
        await withCheckedContinuation { continuation in
            UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge]) { granted, error in
                if let error {
                    self.logger.error("Notification permission error: \(error)")
                }
                self.logger.info("Notification permission granted: \(granted)")
                continuation.resume(returning: granted)
            }
        }
    }

    private func isGranted(_ status: UNAuthorizationStatus) -> Bool {
        switch status {
        case .authorized, .ephemeral, .provisional:
            return true
        default:
            return false
        }
    }

    private func permissionString(_ status: UNAuthorizationStatus) -> String {
        switch status {
        case .authorized, .ephemeral, .provisional:
            return "granted"
        case .denied:
            return "denied"
        case .notDetermined:
            return "default"
        @unknown default:
            return "default"
        }
    }

    private func runEventLoop() async {
        while !Task.isCancelled {
            do {
                try await streamEvents()
            } catch {
                logger.debug("Notification stream error: \(error)")
            }

            if Task.isCancelled {
                return
            }

            // Catch up after any reconnect or transport failure.
            await scanRecentConversations(notify: true)
            try? await Task.sleep(for: reconnectDelay)
        }
    }

    private func primeBaseline() async {
        await scanRecentConversations(notify: false)
    }

    private func streamEvents() async throws {
        let request = URLRequest(url: apiURL(pathComponents: ["api", "events"]))
        let (bytes, response) = try await URLSession.shared.bytes(for: request)
        guard let http = response as? HTTPURLResponse, http.statusCode == 200 else {
            throw URLError(.badServerResponse)
        }

        logger.info("Notification event stream connected")

        var eventName = ""
        var dataLines: [String] = []

        for try await rawLine in bytes.lines {
            if Task.isCancelled {
                return
            }

            let line = String(rawLine)
            if line.isEmpty {
                await handleEvent(named: eventName, data: dataLines.joined(separator: "\n"))
                eventName = ""
                dataLines.removeAll(keepingCapacity: true)
                continue
            }
            if line.hasPrefix(":") {
                continue
            }
            if line.hasPrefix("event: ") {
                eventName = String(line.dropFirst("event: ".count))
                continue
            }
            if line.hasPrefix("data: ") {
                dataLines.append(String(line.dropFirst("data: ".count)))
            }
        }
    }

    private func handleEvent(named eventName: String, data: String) async {
        switch eventName {
        case "messages", "conversations":
            await scanRecentConversations(notify: true)
        default:
            break
        }
    }

    private func scanRecentConversations(notify: Bool) async {
        do {
            let conversations = try await fetchRecentConversations()
            for conversation in conversations {
                let previousTS = lastSeenTimestamps[conversation.conversationID] ?? 0
                if !notify || previousTS == 0 {
                    lastSeenTimestamps[conversation.conversationID] = max(previousTS, conversation.lastMessageTS)
                    continue
                }
                guard conversation.lastMessageTS > previousTS else {
                    lastSeenTimestamps[conversation.conversationID] = max(previousTS, conversation.lastMessageTS)
                    continue
                }

                await fetchAndNotify(conversationID: conversation.conversationID, name: conversation.name ?? "OpenMessage", since: previousTS)
                lastSeenTimestamps[conversation.conversationID] = max(previousTS, conversation.lastMessageTS)
            }
        } catch {
            logger.debug("Notification scan error: \(error)")
        }
    }

    private func fetchRecentConversations() async throws -> [ConversationSummary] {
        let url = apiURL(
            pathComponents: ["api", "conversations"],
            queryItems: [URLQueryItem(name: "limit", value: String(recentConversationLimit))]
        )
        let (data, _) = try await URLSession.shared.data(from: url)
        return try JSONDecoder().decode([ConversationSummary].self, from: data)
    }

    private func fetchAndNotify(conversationID: String, name: String, since: Int64) async {
        do {
            let url = apiURL(
                pathComponents: ["api", "conversations", conversationID, "messages"],
                queryItems: [
                    URLQueryItem(name: "after", value: String(since)),
                    URLQueryItem(name: "limit", value: String(recentMessageLimit)),
                ]
            )
            let (data, _) = try await URLSession.shared.data(from: url)
            let messages = try JSONDecoder().decode([MessageSummary].self, from: data)

            let incoming = messages
                .filter { !$0.isFromMe && $0.timestampMS > since }
                .filter { rememberMessage(id: $0.messageID) }
                .sorted { lhs, rhs in
                    if lhs.timestampMS != rhs.timestampMS {
                        return lhs.timestampMS < rhs.timestampMS
                    }
                    return lhs.messageID < rhs.messageID
                }

            guard !incoming.isEmpty else {
                return
            }

            if incoming.count == 1, let latest = incoming.last {
                sendNotification(
                    title: latest.senderName ?? name,
                    body: latest.body ?? "New message",
                    conversationID: conversationID
                )
                return
            }

            sendNotification(
                title: name,
                body: "\(incoming.count) new messages",
                conversationID: conversationID
            )
        } catch {
            logger.debug("Notification fetch error: \(error)")
        }
    }

    private func rememberMessage(id: String) -> Bool {
        guard !id.isEmpty else { return false }
        if seenMessageIDs.contains(id) {
            return false
        }
        seenMessageIDs.insert(id)
        seenMessageOrder.append(id)
        if seenMessageOrder.count > seenMessageCap {
            let evicted = seenMessageOrder.removeFirst()
            seenMessageIDs.remove(evicted)
        }
        return true
    }

    private func sendNotification(title: String, body: String, conversationID: String) {
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.sound = .default
        content.userInfo = ["conversationID": conversationID]

        let request = UNNotificationRequest(
            identifier: "msg-\(conversationID)-\(Date().timeIntervalSince1970)",
            content: content,
            trigger: nil
        )

        UNUserNotificationCenter.current().add(request) { error in
            if let error {
                self.logger.error("Failed to send notification: \(error)")
            }
        }
    }

    private func apiURL(pathComponents: [String], queryItems: [URLQueryItem] = []) -> URL {
        let url = pathComponents.reduce(baseURL) { partial, component in
            partial.appendingPathComponent(component)
        }
        guard !queryItems.isEmpty else {
            return url
        }
        return url.appending(queryItems: queryItems)
    }

    private func disablePreference() {
        preferenceEnabled = false
        defaults.set(false, forKey: preferenceKey)
    }

    private func disablePreferenceAndStop() {
        disablePreference()
        stop()
    }
}
