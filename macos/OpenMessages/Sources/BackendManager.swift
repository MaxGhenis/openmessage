import Foundation
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

    private var process: Process?
    private let logger = Logger(subsystem: "com.openmessages.app", category: "Backend")
    private var healthCheckTask: Task<Void, Never>?

    /// Path to the embedded Go binary inside the app bundle.
    var binaryPath: String {
        if let bundlePath = Bundle.main.resourcePath {
            let embedded = (bundlePath as NSString).appendingPathComponent("openmessages")
            if FileManager.default.fileExists(atPath: embedded) {
                return embedded
            }
        }
        // Fallback: look next to the app or in a known dev location
        let devPath = FileManager.default.currentDirectoryPath + "/openmessages"
        if FileManager.default.fileExists(atPath: devPath) {
            return devPath
        }
        // Last resort: search PATH
        return "/usr/local/bin/openmessages"
    }

    /// Data directory for session, DB, etc.
    var dataDir: String {
        let dir = NSHomeDirectory() + "/.local/share/openmessages"
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        return dir
    }

    /// Whether a session file exists (i.e. phone is already paired).
    var hasSession: Bool {
        FileManager.default.fileExists(atPath: dataDir + "/session.json")
    }

    var baseURL: URL {
        URL(string: "http://localhost:\(port)")!
    }

    func start() {
        guard state == .stopped || state == .needsPairing || state != .running else { return }

        if !hasSession {
            state = .needsPairing
            return
        }

        state = .starting
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: binaryPath)
        proc.arguments = ["serve"]
        proc.environment = [
            "OPENMESSAGES_PORT": String(port),
            "OPENMESSAGES_DATA_DIR": dataDir,
            "OPENMESSAGES_LOG_LEVEL": "info",
            "HOME": NSHomeDirectory(),
            "PATH": "/usr/local/bin:/usr/bin:/bin",
        ]

        let pipe = Pipe()
        proc.standardOutput = pipe
        proc.standardError = pipe

        // Read output for logging
        pipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            guard !data.isEmpty, let line = String(data: data, encoding: .utf8) else { return }
            self?.logger.info("\(line.trimmingCharacters(in: .whitespacesAndNewlines))")
        }

        proc.terminationHandler = { [weak self] proc in
            Task { @MainActor in
                guard let self else { return }
                self.logger.warning("Backend exited with code \(proc.terminationStatus)")
                if self.state == .running {
                    self.state = .error("Backend exited unexpectedly (code \(proc.terminationStatus))")
                }
            }
        }

        do {
            try proc.run()
            process = proc
            startHealthCheck()
        } catch {
            state = .error("Failed to launch backend: \(error.localizedDescription)")
            logger.error("Launch failed: \(error)")
        }
    }

    func stop() {
        healthCheckTask?.cancel()
        healthCheckTask = nil
        process?.terminate()
        process = nil
        state = .stopped
    }

    /// Poll /api/status until the backend is ready.
    private func startHealthCheck() {
        healthCheckTask = Task {
            for attempt in 1...30 {
                if Task.isCancelled { return }
                try? await Task.sleep(for: .milliseconds(500))
                do {
                    let url = baseURL.appendingPathComponent("api/status")
                    let (data, response) = try await URLSession.shared.data(from: url)
                    if let http = response as? HTTPURLResponse, http.statusCode == 200,
                       let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                       json["connected"] as? Bool == true {
                        self.state = .running
                        self.logger.info("Backend ready after \(attempt) checks")
                        return
                    }
                } catch {
                    self.logger.debug("Health check \(attempt): \(error)")
                }
            }
            if !Task.isCancelled {
                self.state = .error("Backend failed to start within 15 seconds")
            }
        }
    }

    /// Run the pairing flow. Returns the QR code URL for display.
    func startPairing() async -> AsyncStream<PairingEvent> {
        let binPath = self.binaryPath
        let dataDirPath = self.dataDir
        return AsyncStream { continuation in
            Task.detached {
                let proc = Process()
                proc.executableURL = URL(fileURLWithPath: binPath)
                proc.arguments = ["pair"]
                proc.environment = [
                    "OPENMESSAGES_DATA_DIR": dataDirPath,
                    "HOME": NSHomeDirectory(),
                    "PATH": "/usr/local/bin:/usr/bin:/bin",
                ]

                let pipe = Pipe()
                proc.standardOutput = pipe
                proc.standardError = pipe

                pipe.fileHandleForReading.readabilityHandler = { handle in
                    let data = handle.availableData
                    guard !data.isEmpty, let line = String(data: data, encoding: .utf8) else { return }
                    let trimmed = line.trimmingCharacters(in: .whitespacesAndNewlines)

                    // The pair command outputs the QR URL
                    if trimmed.hasPrefix("https://") || trimmed.hasPrefix("http://") {
                        continuation.yield(.qrURL(trimmed))
                    } else if trimmed.lowercased().contains("success") || trimmed.lowercased().contains("paired") {
                        continuation.yield(.success)
                    } else if !trimmed.isEmpty {
                        continuation.yield(.log(trimmed))
                    }
                }

                proc.terminationHandler = { proc in
                    if proc.terminationStatus == 0 {
                        continuation.yield(.success)
                    } else {
                        continuation.yield(.failed("Pairing exited with code \(proc.terminationStatus)"))
                    }
                    continuation.finish()
                }

                do {
                    try proc.run()
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

enum PairingEvent {
    case qrURL(String)
    case log(String)
    case success
    case failed(String)
}
