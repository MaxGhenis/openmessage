import Darwin
import Foundation
import XCTest
@testable import OpenMessage

@MainActor
final class BackendLauncherSmokeTests: XCTestCase {
    func testRealBackendLaunchOneRestartAndCleanQuit() async throws {
        let fileManager = FileManager.default
        guard let configuredPath = ProcessInfo.processInfo.environment["OPENMESSAGE_BINARY"],
              !configuredPath.isEmpty else {
            throw XCTSkip("OPENMESSAGE_BINARY is not set")
        }

        let binaryPath = (configuredPath as NSString).standardizingPath
        var isDirectory: ObjCBool = false
        guard fileManager.fileExists(atPath: binaryPath, isDirectory: &isDirectory),
              !isDirectory.boolValue,
              fileManager.isExecutableFile(atPath: binaryPath) else {
            throw XCTSkip("OPENMESSAGE_BINARY is missing or not executable: \(binaryPath)")
        }

        let temporaryRoot = fileManager.temporaryDirectory
            .appendingPathComponent("openmessage-f2-smoke-\(UUID().uuidString)", isDirectory: true)
        let dataDirectory = temporaryRoot.appendingPathComponent("data", isDirectory: true)
        try fileManager.createDirectory(at: dataDirectory, withIntermediateDirectories: true)
        defer { try? fileManager.removeItem(at: temporaryRoot) }

        let logURL = temporaryRoot.appendingPathComponent("backend.log")
        guard fileManager.createFile(atPath: logURL.path, contents: nil) else {
            throw SmokeFailure("Could not create backend log at \(logURL.path)")
        }
        let logHandle = try FileHandle(forWritingTo: logURL)
        defer { try? logHandle.close() }

        let port = try reserveLoopbackPort()
        let configuration = BackendLaunchConfiguration.application(
            executablePath: binaryPath,
            dataDirectory: dataDirectory.path,
            port: port,
            homeDirectory: temporaryRoot.path,
            additionalEnvironment: [
                "OPENMESSAGES_HOST": "127.0.0.1",
                "OPENMESSAGES_SIGNAL_TMP_SWEEP": "0",
                "OPENMESSAGES_STARTUP_BACKFILL": "off",
                "OPENMESSAGE_TELEMETRY": "0",
                "OPENMESSAGES_DEMO": "0",
            ]
        )
        let launcher = BackendLauncherCore(configuration: configuration)
        defer {
            if launcher.process != nil {
                _ = launcher.forceTerminateAndWait(timeout: 2)
            }
        }

        let configureProcess: (Process) -> Void = { process in
            process.standardOutput = logHandle
            process.standardError = logHandle
        }

        let firstProcess = try launcher.launch(configure: configureProcess)
        try await waitForTwoHTTP200Responses(
            at: configuration.baseURL,
            process: firstProcess,
            timeout: 12,
            logHandle: logHandle,
            logURL: logURL
        )
        let firstPID = firstProcess.processIdentifier
        try requireCleanExit(
            launcher.terminateAndWait(timeout: 5),
            phase: "first launch",
            logHandle: logHandle,
            logURL: logURL
        )
        XCTAssertFalse(firstProcess.isRunning)

        let restartedProcess = try launcher.launch(configure: configureProcess)
        XCTAssertNotEqual(restartedProcess.processIdentifier, firstPID)
        try await waitForTwoHTTP200Responses(
            at: configuration.baseURL,
            process: restartedProcess,
            timeout: 12,
            logHandle: logHandle,
            logURL: logURL
        )
        try requireCleanExit(
            launcher.terminateAndWait(timeout: 5),
            phase: "restarted launch",
            logHandle: logHandle,
            logURL: logURL
        )
        XCTAssertFalse(restartedProcess.isRunning)
        XCTAssertNil(launcher.process)
    }

    private func waitForTwoHTTP200Responses(
        at baseURL: URL,
        process: Process,
        timeout: TimeInterval,
        logHandle: FileHandle,
        logURL: URL
    ) async throws {
        let sessionConfiguration = URLSessionConfiguration.ephemeral
        sessionConfiguration.requestCachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        sessionConfiguration.urlCache = nil
        sessionConfiguration.connectionProxyDictionary = [:]
        sessionConfiguration.timeoutIntervalForRequest = 0.75
        sessionConfiguration.timeoutIntervalForResource = 1
        let session = URLSession(configuration: sessionConfiguration)
        defer { session.invalidateAndCancel() }

        let deadline = Date().addingTimeInterval(timeout)
        var consecutiveSuccesses = 0
        var lastFailure = "no response"

        while Date() < deadline {
            guard process.isRunning else {
                throw SmokeFailure(
                    "Backend exited before readiness (status \(process.terminationStatus)).\n"
                        + backendLog(logHandle: logHandle, logURL: logURL)
                )
            }

            var request = URLRequest(url: baseURL)
            request.httpMethod = "GET"
            request.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
            request.setValue("no-cache", forHTTPHeaderField: "Cache-Control")

            do {
                let (_, response) = try await session.data(for: request)
                if let http = response as? HTTPURLResponse, http.statusCode == 200 {
                    consecutiveSuccesses += 1
                    if consecutiveSuccesses == 2 {
                        return
                    }
                } else {
                    consecutiveSuccesses = 0
                    lastFailure = "HTTP \((response as? HTTPURLResponse)?.statusCode ?? -1)"
                }
            } catch {
                consecutiveSuccesses = 0
                lastFailure = error.localizedDescription
            }

            try await Task.sleep(for: .milliseconds(100))
        }

        throw SmokeFailure(
            "Backend did not answer GET / with 200 within \(timeout)s (last failure: \(lastFailure)).\n"
                + backendLog(logHandle: logHandle, logURL: logURL)
        )
    }

    private func requireCleanExit(
        _ result: BackendTerminationResult,
        phase: String,
        logHandle: FileHandle,
        logURL: URL
    ) throws {
        switch result {
        case let .exited(status, reason):
            guard reason == .exit, status == 0 else {
                throw SmokeFailure(
                    "Backend \(phase) ended via \(reason) with status \(status), expected clean exit 0.\n"
                        + backendLog(logHandle: logHandle, logURL: logURL)
                )
            }
        case .timedOut:
            throw SmokeFailure(
                "Backend \(phase) did not quit within 5s.\n"
                    + backendLog(logHandle: logHandle, logURL: logURL)
            )
        case .noProcess:
            throw SmokeFailure("Backend \(phase) lost its Process handle before quit")
        }
    }

    private func backendLog(logHandle: FileHandle, logURL: URL) -> String {
        try? logHandle.synchronize()
        guard let data = try? Data(contentsOf: logURL), !data.isEmpty else {
            return "Backend log was empty."
        }
        return "Backend log:\n\(String(decoding: data, as: UTF8.self))"
    }

    private func reserveLoopbackPort() throws -> Int {
        let descriptor = Darwin.socket(AF_INET, SOCK_STREAM, 0)
        guard descriptor >= 0 else {
            throw SmokeFailure("socket() failed with errno \(errno)")
        }
        defer { Darwin.close(descriptor) }

        var address = sockaddr_in()
        address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        address.sin_family = sa_family_t(AF_INET)
        address.sin_port = 0
        address.sin_addr = in_addr(s_addr: inet_addr("127.0.0.1"))

        let bindResult = withUnsafePointer(to: &address) { addressPointer in
            addressPointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { socketAddress in
                Darwin.bind(
                    descriptor,
                    socketAddress,
                    socklen_t(MemoryLayout<sockaddr_in>.size)
                )
            }
        }
        guard bindResult == 0 else {
            throw SmokeFailure("bind() failed with errno \(errno)")
        }

        var addressLength = socklen_t(MemoryLayout<sockaddr_in>.size)
        let nameResult = withUnsafeMutablePointer(to: &address) { addressPointer in
            addressPointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { socketAddress in
                Darwin.getsockname(descriptor, socketAddress, &addressLength)
            }
        }
        guard nameResult == 0 else {
            throw SmokeFailure("getsockname() failed with errno \(errno)")
        }

        return Int(UInt16(bigEndian: address.sin_port))
    }
}

private struct SmokeFailure: Error, CustomStringConvertible {
    let description: String

    init(_ description: String) {
        self.description = description
    }
}
