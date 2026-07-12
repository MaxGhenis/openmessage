import Darwin
import Foundation

/// Pure command-line classification used when deciding whether a process
/// listening on the configured port belongs to OpenMessage.
struct BackendCommandMatcher: Sendable {
    let binaryPath: String

    /// A backend is reusable only when it was launched from the exact helper
    /// path this app resolved. Keep the substring behavior in sync with the
    /// pre-seam adoption logic; stronger identity checks belong to a later
    /// protocol-handshake change.
    func isReusableBackendCommand(_ command: String) -> Bool {
        command.contains("\(binaryPath) serve")
    }

    /// Broader recognition used only to clean up a conflicting OpenMessage
    /// helper before launching the app's exact helper.
    func isOpenMessageBackendCommand(_ command: String) -> Bool {
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
}

/// Complete, inspectable description of one backend launch.
struct BackendLaunchConfiguration: Equatable, Sendable {
    let executableURL: URL
    let arguments: [String]
    let environment: [String: String]
    let currentDirectoryURL: URL
    let dataDirectoryURL: URL
    let port: Int

    var baseURL: URL {
        URL(string: "http://127.0.0.1:\(port)")!
    }

    /// Configuration used by the native app. `additionalEnvironment` exists so
    /// isolated package tests can add stricter test-only controls without
    /// changing or contradicting the canonical environment shipped by
    /// BackendManager.
    static func application(
        executablePath: String,
        dataDirectory: String,
        port: Int,
        homeDirectory: String = NSHomeDirectory(),
        additionalEnvironment: [String: String] = [:]
    ) -> BackendLaunchConfiguration {
        var environment = additionalEnvironment
        environment.merge([
            "OPENMESSAGES_PORT": String(port),
            "OPENMESSAGES_DATA_DIR": dataDirectory,
            "OPENMESSAGES_LOG_LEVEL": "info",
            "OPENMESSAGES_APP_SANDBOX": "1",
            "OPENMESSAGES_MACOS_NOTIFICATIONS": "0",
            "HOME": homeDirectory,
            "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
        ]) { _, canonical in canonical }

        let dataDirectoryURL = URL(fileURLWithPath: dataDirectory, isDirectory: true)
        return BackendLaunchConfiguration(
            executableURL: URL(fileURLWithPath: executablePath),
            arguments: ["serve", "--web"],
            environment: environment,
            currentDirectoryURL: dataDirectoryURL,
            dataDirectoryURL: dataDirectoryURL,
            port: port
        )
    }
}

/// Injectable process construction boundary. Tests can inspect the immutable
/// launch configuration or substitute process creation without involving
/// BackendManager, AppKit, or ObservableObject.
protocol BackendProcessSpawning {
    func makeProcess(for configuration: BackendLaunchConfiguration) -> Process
}

struct FoundationBackendProcessSpawner: BackendProcessSpawning {
    func makeProcess(for configuration: BackendLaunchConfiguration) -> Process {
        let process = Process()
        process.executableURL = configuration.executableURL
        process.arguments = configuration.arguments
        process.environment = configuration.environment
        process.currentDirectoryURL = configuration.currentDirectoryURL
        return process
    }
}

enum BackendTerminationResult {
    case noProcess
    case exited(status: Int32, reason: Process.TerminationReason)
    case timedOut
}

/// Foundation-only process lifecycle core shared by BackendManager and the
/// non-interactive package smoke test.
/// BackendManager and the smoke test serialize process ownership on the main
/// actor, preventing launch/terminate transitions from racing one another.
@MainActor
final class BackendLauncherCore {
    let configuration: BackendLaunchConfiguration
    private let processSpawner: any BackendProcessSpawning
    private(set) var process: Process?

    init(
        configuration: BackendLaunchConfiguration,
        processSpawner: any BackendProcessSpawning = FoundationBackendProcessSpawner()
    ) {
        self.configuration = configuration
        self.processSpawner = processSpawner
    }

    /// Creates a fresh Process for every launch. Callers may attach output and
    /// termination handlers in `configure` before the process starts.
    @discardableResult
    func launch(configure: (Process) -> Void = { _ in }) throws -> Process {
        if let process, process.isRunning {
            throw BackendLauncherError.alreadyRunning
        }

        let process = processSpawner.makeProcess(for: configuration)
        configure(process)
        self.process = process
        do {
            try process.run()
            return process
        } catch {
            self.process = nil
            throw error
        }
    }

    /// Non-blocking SIGTERM used by BackendManager.stop(), preserving its
    /// existing UI behavior.
    func terminate() {
        guard let process else { return }
        if process.isRunning {
            process.terminate()
        }
        self.process = nil
    }

    /// Sends SIGTERM and waits only until the supplied deadline. A timeout does
    /// not silently force success; callers decide whether a SIGKILL is suitable.
    func terminateAndWait(timeout: TimeInterval) -> BackendTerminationResult {
        guard let process else { return .noProcess }
        if process.isRunning {
            process.terminate()
        }
        return waitForExit(of: process, timeout: timeout)
    }

    /// Leak protection for a timed-out test or app quit. The returned outcome
    /// remains explicit so a forced exit can never count as a clean smoke pass.
    func forceTerminateAndWait(timeout: TimeInterval) -> BackendTerminationResult {
        guard let process else { return .noProcess }
        if process.isRunning {
            _ = Darwin.kill(process.processIdentifier, SIGKILL)
        }
        return waitForExit(of: process, timeout: timeout)
    }

    private func waitForExit(of process: Process, timeout: TimeInterval) -> BackendTerminationResult {
        let deadline = Date().addingTimeInterval(max(0, timeout))
        while process.isRunning && Date() < deadline {
            Thread.sleep(forTimeInterval: 0.02)
        }
        guard !process.isRunning else { return .timedOut }

        if self.process === process {
            self.process = nil
        }
        return .exited(status: process.terminationStatus, reason: process.terminationReason)
    }

    deinit {
        if let process, process.isRunning {
            process.terminate()
        }
    }
}

enum BackendLauncherError: Error {
    case alreadyRunning
}
