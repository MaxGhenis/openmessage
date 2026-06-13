import AppKit
import Foundation
import Combine
import Darwin
import os

/// Manages the Go backend binary as a subprocess.
/// The binary serves the web UI on localhost and handles Google Messages protocol.
@MainActor
final class BackendManager: ObservableObject {
    enum State: Equatable {
        case stopped
        case starting
        case running
        case needsPairing
        case error(String)
    }

    @Published var state: State = .stopped
    @Published var port: Int = 7007

    /// Non-nil when a paired platform has silently stopped syncing and needs
    /// the user's attention (e.g. Google Messages session invalidated). The
    /// backend process itself stays healthy when this happens — WhatsApp and
    /// Signal keep it alive — so the overall `state` stays `.running` and the
    /// menu bar would otherwise show a misleading green "Connected". This
    /// surfaces the per-platform problem so a dead bridge can't silently rot.
    @Published var platformAlert: String?

    private var process: Process?
    /// PID of a backend we adopted instead of spawning (process == nil). Tracked
    /// so stop()/quit can still terminate it — otherwise an orphan adopted on a
    /// later launch would be unkillable by the app.
    private var reusedBackendPID: pid_t?
    private let logger = Logger(subsystem: "com.openmessage.app", category: "Backend")
    private var healthCheckTask: Task<Void, Never>?
    private var connectionMonitorTask: Task<Void, Never>?

    // ── Bounded auto-restart ──
    // When the backend exits unexpectedly we retry a few times with exponential
    // backoff instead of immediately dumping the user into a manual "Try again"
    // screen. The counter resets once a restarted backend has stayed healthy for
    // `healthyResetInterval`, so a backend that crashes once an hour doesn't burn
    // through its budget permanently. Only after the budget is exhausted do we
    // surface `.error(...)`.
    private static let maxRestartAttempts = 3
    private static let restartBackoff: [Duration] = [.seconds(1), .seconds(3), .seconds(9)]
    private static let healthyResetInterval: Duration = .seconds(60)
    private var restartAttempts = 0
    private var pendingRestartTask: Task<Void, Never>?
    private var healthyResetTask: Task<Void, Never>?

    init() {
        // Standard Cmd-Q / app-menu "Quit" terminates via NSApplication without
        // going through the menu-bar button's stop(), which left the spawned
        // `serve` process reparented to launchd, holding the port and DB. Hook
        // app termination to shut the backend down cleanly.
        NotificationCenter.default.addObserver(
            forName: NSApplication.willTerminateNotification,
            object: nil,
            queue: .main
        ) { [weak self] _ in
            MainActor.assumeIsolated {
                self?.terminateForQuit()
            }
        }
    }

    /// Path to the embedded Go binary inside the app bundle.
    var binaryPath: String {
        if let resourcePath = Bundle.main.resourceURL?.appendingPathComponent("openmessage").path,
           FileManager.default.fileExists(atPath: resourcePath) {
            return resourcePath
        }
        if let executablePath = Bundle.main.executableURL?.deletingLastPathComponent().path {
            let embedded = (executablePath as NSString).appendingPathComponent("openmessage-helper")
            if FileManager.default.fileExists(atPath: embedded) {
                return embedded
            }
        }
        let systemPath = "/usr/local/bin/openmessage"
        if FileManager.default.fileExists(atPath: systemPath) {
            return systemPath
        }
        // Fallback: look next to the app or in a known dev location
        let devPath = FileManager.default.currentDirectoryPath + "/openmessage"
        if FileManager.default.fileExists(atPath: devPath) {
            return devPath
        }
        // Last resort: return the expected system install path
        return systemPath
    }

    /// Data directory for session, DB, etc.
    /// Prefer the stable user Application Support path used by the local app install.
    /// Fall back to the containerized path if the direct location cannot be created.
    var dataDir: String {
        let direct = (NSHomeDirectory() as NSString).appendingPathComponent("Library/Application Support/OpenMessage")
        if ensureDirectoryExists(at: direct) {
            return direct
        }
        let appSupport = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first!
        let container = appSupport.appendingPathComponent("OpenMessage").path
        _ = ensureDirectoryExists(at: container)
        return container
    }

    /// Migrate session and DB from old data dir (~/.local/share/openmessage) if present.
    private func migrateOldDataIfNeeded() {
        let oldDir = NSHomeDirectory() + "/.local/share/openmessage"
        let newDir = dataDir
        let fm = FileManager.default
        guard fm.fileExists(atPath: oldDir + "/session.json"),
              !fm.fileExists(atPath: newDir + "/session.json") else { return }
        for file in ["session.json", "messages.db", "messages.db-shm", "messages.db-wal"] {
            let src = oldDir + "/" + file
            let dst = newDir + "/" + file
            if fm.fileExists(atPath: src) {
                try? fm.copyItem(atPath: src, toPath: dst)
            }
        }
        logger.info("Migrated data from \(oldDir) to \(newDir)")
    }

    /// Whether a session file exists (i.e. phone is already paired).
    var hasSession: Bool {
        migrateOldDataIfNeeded()
        return FileManager.default.fileExists(atPath: dataDir + "/session.json")
    }

    var baseURL: URL {
        URL(string: "http://127.0.0.1:\(port)")!
    }

    /// User- or lifecycle-initiated start. Resets the auto-restart budget so a
    /// deliberate (re)start always gets the full retry allowance, then launches.
    func start() {
        guard state != .starting, state != .running else { return }
        cancelPendingRestart()
        restartAttempts = 0
        launchBackend()
    }

    /// Spawns (or adopts) the backend process. Shared by the public `start()` and
    /// the auto-restart path, so a restart doesn't trip `start()`'s running/
    /// starting guard.
    private func launchBackend() {
        migrateOldDataIfNeeded()

        state = .starting
        healthCheckTask?.cancel()
        healthCheckTask = nil
        connectionMonitorTask?.cancel()
        connectionMonitorTask = nil
        cancelHealthyResetTimer()
        if reuseExistingBackendIfNeeded() {
            return
        }
        cleanupConflictingBackendIfNeeded()
        if reuseExistingBackendIfNeeded() {
            return
        }
        let proc = Process()
        let path = binaryPath
        let dir = dataDir
        proc.executableURL = URL(fileURLWithPath: path)
        proc.arguments = ["serve"]
        proc.currentDirectoryURL = URL(fileURLWithPath: dir, isDirectory: true)
        proc.environment = [
            "OPENMESSAGES_PORT": String(port),
            "OPENMESSAGES_DATA_DIR": dir,
            "OPENMESSAGES_LOG_LEVEL": "info",
            "OPENMESSAGES_APP_SANDBOX": "1",
            // The native app owns notifications when it wraps the backend:
            // NotificationManager posts UNUserNotificationCenter banners off the
            // SSE event stream, with tap-to-open and foreground suppression that
            // the Go side can't do. Disable the backend's own osascript/
            // terminal-notifier banners so a single inbound message doesn't fire
            // two notifications. Bare `openmessage serve` in a terminal (no app,
            // env var unset) still gets Go-side banners via its default logic.
            "OPENMESSAGES_MACOS_NOTIFICATIONS": "0",
            "HOME": NSHomeDirectory(),
            "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
        ]
        logger.info("Launching backend at \(path, privacy: .public) with data dir \(dir, privacy: .public)")

        let pipe = Pipe()
        proc.standardOutput = pipe
        proc.standardError = pipe

        // Read output for logging. Mark the interpolation as .public so
        // that diagnostic lines like "WhatsApp message fell through to
        // [Unsupported message]" (with content_types field) can be read
        // via `log show --predicate 'subsystem == "com.openmessage.app"'`
        // without flipping the system-wide private_data flag.
        pipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            guard !data.isEmpty, let line = String(data: data, encoding: .utf8) else { return }
            let trimmed = line.trimmingCharacters(in: .whitespacesAndNewlines)
            self?.logger.info("\(trimmed, privacy: .public)")
        }

        proc.terminationHandler = { [weak self] proc in
            let manager = self
            let reason = proc.terminationReason == .uncaughtSignal ? "signal" : "exit"
            let code = proc.terminationStatus
            Task { @MainActor in
                guard let manager else { return }
                manager.logger.warning("Backend terminated via \(reason, privacy: .public) with code \(code)")
                guard manager.process === proc else { return }
                manager.process = nil
                // Only react to crashes while we believed the backend was up. A
                // deliberate stop()/quit clears `state` first, so this won't
                // fight an intentional shutdown.
                if manager.state == .running || manager.state == .starting {
                    manager.handleUnexpectedTermination(code: code)
                }
            }
        }

        do {
            try proc.run()
            process = proc
            reusedBackendPID = nil
            startHealthCheck()
        } catch {
            state = .error("Failed to launch backend: \(error.localizedDescription)")
            logger.error("Launch failed: \(error)")
        }
    }

    private func reuseExistingBackendIfNeeded() -> Bool {
        let pids = listeningPIDs(on: port)
        guard !pids.isEmpty else { return false }
        for pid in pids {
            guard pid > 0 else { continue }
            guard let command = commandLine(for: pid), isReusableBackendCommand(command) else { continue }
            logger.info("Reusing existing backend pid \(pid): \(command, privacy: .public)")
            process = nil
            reusedBackendPID = pid
            startHealthCheck()
            return true
        }
        return false
    }

    private func cleanupConflictingBackendIfNeeded() {
        let pids = listeningPIDs(on: port)
        guard !pids.isEmpty else { return }

        var terminatedAny = false
        for pid in pids {
            guard pid > 0 else { continue }
            guard let command = commandLine(for: pid) else { continue }
            guard isOpenMessageBackendCommand(command) else { continue }

            logger.warning("Stopping conflicting backend pid \(pid): \(command, privacy: .public)")
            _ = Darwin.kill(pid_t(pid), SIGTERM)
            terminatedAny = true
        }

        if terminatedAny {
            waitForPortRelease()
            for pid in listeningPIDs(on: port) {
                guard pid > 0 else { continue }
                guard let command = commandLine(for: pid), isOpenMessageBackendCommand(command) else { continue }
                logger.warning("Force stopping lingering backend pid \(pid): \(command, privacy: .public)")
                _ = Darwin.kill(pid_t(pid), SIGKILL)
            }
            waitForPortRelease()
        }
    }

    private func isReusableBackendCommand(_ command: String) -> Bool {
        command.contains("\(binaryPath) serve")
    }

    private func isOpenMessageBackendCommand(_ command: String) -> Bool {
        let normalized = command.replacingOccurrences(of: "\\", with: "/")
        if isReusableBackendCommand(normalized) {
            return true
        }
        if normalized.contains("/usr/local/bin/openmessage serve") {
            return true
        }
        return normalized.contains("/OpenMessage")
            && (
                normalized.contains(".app/Contents/Resources/openmessage serve")
                || normalized.contains(".app/Contents/MacOS/openmessage-helper serve")
            )
    }

    private func waitForPortRelease() {
        for _ in 0..<20 {
            if listeningPIDs(on: port).isEmpty {
                return
            }
            Thread.sleep(forTimeInterval: 0.1)
        }
    }

    private func ensureDirectoryExists(at path: String) -> Bool {
        do {
            try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true, attributes: nil)
            return true
        } catch {
            logger.error("Failed to create data dir \(path, privacy: .public): \(error.localizedDescription, privacy: .public)")
            return false
        }
    }

    private func listeningPIDs(on port: Int) -> [Int32] {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/usr/sbin/lsof")
        proc.arguments = ["-ti", "tcp:\(port)", "-sTCP:LISTEN"]
        let pipe = Pipe()
        proc.standardOutput = pipe
        proc.standardError = Pipe()

        do {
            try proc.run()
            proc.waitUntilExit()
            guard proc.terminationStatus == 0 else { return [] }
            let data = pipe.fileHandleForReading.readDataToEndOfFile()
            let text = String(data: data, encoding: .utf8) ?? ""
            return text
                .split(whereSeparator: \.isNewline)
                .compactMap { Int32($0) }
        } catch {
            logger.error("Failed to inspect port \(port): \(error.localizedDescription, privacy: .public)")
            return []
        }
    }

    private func commandLine(for pid: Int32) -> String? {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/bin/ps")
        proc.arguments = ["-o", "command=", "-p", String(pid)]
        let pipe = Pipe()
        proc.standardOutput = pipe
        proc.standardError = Pipe()

        do {
            try proc.run()
            proc.waitUntilExit()
            guard proc.terminationStatus == 0 else { return nil }
            let data = pipe.fileHandleForReading.readDataToEndOfFile()
            return String(data: data, encoding: .utf8)?
                .trimmingCharacters(in: .whitespacesAndNewlines)
        } catch {
            logger.error("Failed to inspect pid \(pid): \(error.localizedDescription, privacy: .public)")
            return nil
        }
    }

    func stop() {
        cancelPendingRestart()
        restartAttempts = 0
        healthCheckTask?.cancel()
        healthCheckTask = nil
        connectionMonitorTask?.cancel()
        connectionMonitorTask = nil
        if let process {
            process.terminate()
        } else if let pid = reusedBackendPID {
            // We adopted this backend rather than spawning it, so there's no
            // Process handle — signal it by PID so it can still be stopped.
            _ = Darwin.kill(pid, SIGTERM)
        }
        process = nil
        reusedBackendPID = nil
        state = .stopped
    }

    /// Synchronously terminate the backend on app quit, briefly waiting for the
    /// spawned process to exit so it isn't reparented to launchd (which would
    /// leave it holding the port and the SQLite/session files).
    private func terminateForQuit() {
        cancelPendingRestart()
        healthCheckTask?.cancel()
        connectionMonitorTask?.cancel()
        if let process, process.isRunning {
            process.terminate()
            let deadline = Date().addingTimeInterval(2)
            while process.isRunning && Date() < deadline {
                Thread.sleep(forTimeInterval: 0.05)
            }
            if process.isRunning {
                _ = Darwin.kill(process.processIdentifier, SIGKILL)
            }
        } else if let pid = reusedBackendPID {
            _ = Darwin.kill(pid, SIGTERM)
        }
    }

    // ── Auto-restart plumbing ──

    /// Reacts to the backend dying while we believed it was up: retry with
    /// backoff until the budget is exhausted, then surface the error UI.
    private func handleUnexpectedTermination(code: Int32) {
        guard restartAttempts < Self.maxRestartAttempts else {
            logger.error("Backend exited (code \(code)) and the restart budget is exhausted")
            state = .error("Backend exited unexpectedly (code \(code))")
            return
        }
        let delay = Self.restartBackoff[min(restartAttempts, Self.restartBackoff.count - 1)]
        restartAttempts += 1
        let attempt = restartAttempts
        logger.warning("Backend exited (code \(code)); auto-restart \(attempt)/\(Self.maxRestartAttempts) in \(String(describing: delay), privacy: .public)")
        state = .starting
        pendingRestartTask?.cancel()
        pendingRestartTask = Task { @MainActor [weak self] in
            try? await Task.sleep(for: delay)
            guard let self, !Task.isCancelled else { return }
            self.pendingRestartTask = nil
            self.launchBackend()
        }
    }

    /// Once a (re)started backend has stayed healthy for a minute, forget
    /// past crashes so an occasional failure never permanently drains the
    /// retry budget.
    private func scheduleRestartBudgetReset() {
        healthyResetTask?.cancel()
        healthyResetTask = Task { @MainActor [weak self] in
            try? await Task.sleep(for: Self.healthyResetInterval)
            guard let self, !Task.isCancelled else { return }
            if self.state == .running {
                self.restartAttempts = 0
            }
            self.healthyResetTask = nil
        }
    }

    private func cancelPendingRestart() {
        pendingRestartTask?.cancel()
        pendingRestartTask = nil
    }

    private func cancelHealthyResetTimer() {
        healthyResetTask?.cancel()
        healthyResetTask = nil
    }

    /// Poll /api/status until the backend is ready.
    private func startHealthCheck() {
        healthCheckTask = Task {
            for attempt in 1...60 {
                if Task.isCancelled { return }
                try? await Task.sleep(for: .milliseconds(500))
                do {
                    let url = baseURL.appendingPathComponent("api/status")
                    let (data, response) = try await URLSession.shared.data(from: url)
                    if let http = response as? HTTPURLResponse, http.statusCode == 200,
                       let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
                        _ = json
                        self.state = .running
                        self.logger.info("Backend ready after \(attempt) checks")
                        self.scheduleRestartBudgetReset()
                        self.startConnectionMonitor()
                        return
                    }
                } catch {
                    self.logger.debug("Health check \(attempt): \(error)")
                }
            }
            if !Task.isCancelled {
                self.state = .error("Backend failed to start within 30 seconds")
            }
        }
    }

    /// Periodically polls /api/status while running.
    /// If the backend becomes unreachable, surface an error state.
    private func startConnectionMonitor() {
        connectionMonitorTask = Task {
            var consecutiveFailures = 0
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(3))
                if Task.isCancelled { return }
                do {
                    let url = baseURL.appendingPathComponent("api/status")
                    let (data, response) = try await URLSession.shared.data(from: url)
                    if let http = response as? HTTPURLResponse, http.statusCode == 200 {
                        consecutiveFailures = 0
                        self.updatePlatformAlert(from: data)
                    } else {
                        consecutiveFailures += 1
                        self.logger.warning("Backend monitor HTTP failure (\(consecutiveFailures)/3)")
                    }
                } catch {
                    self.logger.debug("Connection monitor error: \(error)")
                    consecutiveFailures += 1
                }
                if consecutiveFailures >= 5 {
                    // Only treat this as a real outage if the backend process
                    // actually died. A transient unreachable window (sleep/wake,
                    // a GC pause, a slow localhost round-trip) must not tear
                    // down a healthy backend and drop the user's session — the
                    // previous behavior killed the process on ~9s of trouble.
                    if self.backendProcessIsDead() {
                        self.logger.error("Backend process is gone — showing error state")
                        self.state = .error("Lost connection to backend")
                        return
                    }
                    self.logger.warning("Backend unreachable but process is alive; continuing to monitor")
                    consecutiveFailures = 0
                }
            }
        }
    }

    /// Whether the backend is genuinely down (vs. briefly unreachable).
    private func backendProcessIsDead() -> Bool {
        if let process {
            return !process.isRunning
        }
        // Adopted backend (no Process handle): dead if nothing is listening.
        return listeningPIDs(on: port).isEmpty
    }

    /// Inspect the /api/status body for a paired platform that has silently
    /// stopped syncing, and surface it via `platformAlert`. Only flags a
    /// platform that *was* paired (so we don't nag about platforms the user
    /// never set up).
    private func updatePlatformAlert(from data: Data) {
        guard let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return }

        let freshness = json["freshness"] as? [String: Any]

        func needsAttention(_ key: String) -> Bool {
            guard let p = json[key] as? [String: Any] else { return false }
            let paired = (p["paired"] as? Bool) ?? false
            let connected = (p["connected"] as? Bool) ?? false
            let needsPairing = (p["needs_pairing"] as? Bool) ?? false
            let needsReauth = (p["needs_reauth"] as? Bool) ?? false
            // A paired platform that is not connected (or explicitly flags a
            // re-pair/reauth need) has silently stopped syncing.
            if needsPairing || needsReauth || (paired && !connected) {
                return true
            }
            // Zombie guard: `connected` can stay true while a bridge has
            // silently stopped delivering. The connection flag lies; the data
            // doesn't. Trust freshness — a paired platform whose latest message
            // trails the newest overall by the stale threshold needs attention
            // even while it reports connected.
            if paired,
               let fresh = freshness?[key] as? [String: Any],
               (fresh["stale"] as? Bool) ?? false {
                return true
            }
            return false
        }

        var stale: [String] = []
        if needsAttention("google") { stale.append("Google Messages") }
        if needsAttention("whatsapp") { stale.append("WhatsApp") }
        if needsAttention("signal") { stale.append("Signal") }

        let alert: String?
        switch stale.count {
        case 0: alert = nil
        case 1: alert = "\(stale[0]) needs re-pairing — it has stopped syncing."
        default: alert = "\(stale.joined(separator: ", ")) need re-pairing — they have stopped syncing."
        }
        if alert != platformAlert {
            platformAlert = alert
        }
    }

    /// Stop the backend, clean up session, and go back to pairing.
    private func handleDisconnect() {
        // Tell the backend to clean up (capture URL before stopping)
        let unpairURL = baseURL.appendingPathComponent("api/unpair")
        Task.detached {
            var request = URLRequest(url: unpairURL)
            request.httpMethod = "POST"
            _ = try? await URLSession.shared.data(for: request)
        }

        // Delete session file locally so pairing starts fresh
        let sessionPath = dataDir + "/session.json"
        try? FileManager.default.removeItem(atPath: sessionPath)

        // Stop the backend process and go to pairing
        stop()
        state = .needsPairing
    }

    func beginGooglePairing() {
        handleDisconnect()
    }

    /// Run the pairing flow. Returns the QR code URL for display.
    func startPairing() async -> AsyncStream<PairingEvent> {
        pairingEvents(arguments: ["pair"], stdinText: nil)
    }

    func startGooglePairing(cookieInput: String) async -> AsyncStream<PairingEvent> {
        pairingEvents(arguments: ["pair", "--google-stdin"], stdinText: cookieInput)
    }

    private func pairingEvents(arguments: [String], stdinText: String?) -> AsyncStream<PairingEvent> {
        let binPath = self.binaryPath
        let dataDirPath = self.dataDir
        return AsyncStream { continuation in
            Task.detached {
                let proc = Process()
                proc.executableURL = URL(fileURLWithPath: binPath)
                proc.arguments = arguments
                proc.environment = [
                    "OPENMESSAGES_DATA_DIR": dataDirPath,
                    "HOME": NSHomeDirectory(),
                    "PATH": "/usr/local/bin:/usr/bin:/bin",
                ]

                let pipe = Pipe()
                proc.standardOutput = pipe
                proc.standardError = pipe
                var stdinPipe: Pipe?
                if stdinText != nil {
                    let p = Pipe()
                    proc.standardInput = p
                    stdinPipe = p
                }

                // When the consumer cancels (user navigates away / restarts the
                // pairing flow), terminate the subprocess so abandoned `pair`
                // attempts don't linger holding a Google connection.
                let procBox = ProcBox()
                continuation.onTermination = { _ in
                    if let p = procBox.proc, p.isRunning {
                        p.terminate()
                    }
                }

                pipe.fileHandleForReading.readabilityHandler = { handle in
                    let data = handle.availableData
                    guard !data.isEmpty, let text = String(data: data, encoding: .utf8) else { return }

                    // Output may contain multiple lines (QR art + URL)
                    for line in text.components(separatedBy: .newlines) {
                        let trimmed = line.trimmingCharacters(in: .whitespacesAndNewlines)
                        guard !trimmed.isEmpty else { continue }

                        // Extract URL from lines like "URL: https://..." or bare URLs
                        if let range = trimmed.range(of: "https://", options: .caseInsensitive) {
                            let url = String(trimmed[range.lowerBound...])
                            continuation.yield(.qrURL(url))
                        } else if trimmed.hasPrefix("EMOJI:") {
                            let emoji = trimmed.replacingOccurrences(of: "EMOJI:", with: "").trimmingCharacters(in: .whitespacesAndNewlines)
                            continuation.yield(.emoji(emoji))
                        } else if trimmed.hasPrefix("http://") {
                            continuation.yield(.qrURL(trimmed))
                        } else if trimmed.lowercased().contains("success") || trimmed.lowercased().contains("paired") {
                            continuation.yield(.success)
                        } else if !trimmed.contains("█") && !trimmed.contains("▀") && !trimmed.contains("▄") {
                            continuation.yield(.log(trimmed))
                        }
                        // Skip QR art and other log lines to avoid noisy status updates
                    }
                }

                proc.terminationHandler = { proc in
                    // Release the pipe reader so it isn't retained after exit.
                    pipe.fileHandleForReading.readabilityHandler = nil
                    if proc.terminationStatus == 0 {
                        continuation.yield(.success)
                    } else {
                        continuation.yield(.failed("Pairing exited with code \(proc.terminationStatus)"))
                    }
                    continuation.finish()
                }

                do {
                    try proc.run()
                    procBox.proc = proc
                    // Write stdin AFTER the process is running: writing a large
                    // cookie/cURL paste before there's a reader can block on the
                    // pipe buffer, and the throwing API avoids the uncatchable
                    // Objective-C exception FileHandle.write raises on a broken
                    // pipe.
                    if let stdinText, let stdinPipe {
                        let handle = stdinPipe.fileHandleForWriting
                        DispatchQueue.global(qos: .userInitiated).async {
                            do {
                                try handle.write(contentsOf: Data(stdinText.utf8))
                                try handle.close()
                            } catch {
                                // Best-effort: the process may have already exited.
                            }
                        }
                    }
                } catch {
                    continuation.yield(.failed("Could not start pairing: \(error.localizedDescription)"))
                    continuation.finish()
                }
            }
        }
    }

    deinit {
        process?.terminate()
    }
}

/// Sendable holder so the AsyncStream's onTermination closure can reach the
/// pairing Process without capturing a non-Sendable value across the boundary.
private final class ProcBox: @unchecked Sendable {
    var proc: Process?
}

enum PairingEvent {
    case qrURL(String)
    case emoji(String)
    case log(String)
    case success
    case failed(String)
}
