import Foundation
import XCTest
@testable import OpenMessage

final class BackendLaunchConfigurationTests: XCTestCase {
    func testApplicationConfigurationPreservesBackendLaunchContract() {
        let configuration = BackendLaunchConfiguration.application(
            executablePath: "/Applications/OpenMessage.app/Contents/Resources/openmessage",
            dataDirectory: "/Users/example/Library/Application Support/OpenMessage",
            port: 8123,
            homeDirectory: "/Users/example",
            appSandboxed: false
        )

        XCTAssertEqual(
            configuration.executableURL.path,
            "/Applications/OpenMessage.app/Contents/Resources/openmessage"
        )
        XCTAssertEqual(configuration.arguments, ["serve", "--web"])
        XCTAssertEqual(configuration.currentDirectoryURL, configuration.dataDirectoryURL)
        XCTAssertEqual(
            configuration.dataDirectoryURL.path,
            "/Users/example/Library/Application Support/OpenMessage"
        )
        XCTAssertEqual(configuration.port, 8123)
        XCTAssertEqual(configuration.baseURL.absoluteString, "http://127.0.0.1:8123")
        XCTAssertEqual(
            configuration.environment,
            [
                "OPENMESSAGES_PORT": "8123",
                "OPENMESSAGES_DATA_DIR": "/Users/example/Library/Application Support/OpenMessage",
                "OPENMESSAGES_LOG_LEVEL": "info",
                "OPENMESSAGES_APP_SANDBOX": "0",
                "OPENMESSAGES_MACOS_NOTIFICATIONS": "0",
                "HOME": "/Users/example",
                "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
            ]
        )
    }

    func testSandboxedAppTellsTheBackendAndChangesNothingElse() {
        let unsandboxed = BackendLaunchConfiguration.application(
            executablePath: "/Applications/OpenMessage.app/Contents/Resources/openmessage",
            dataDirectory: "/Users/example/Library/Application Support/OpenMessage",
            port: 8123,
            homeDirectory: "/Users/example",
            appSandboxed: false
        )
        let sandboxed = BackendLaunchConfiguration.application(
            executablePath: "/Applications/OpenMessage.app/Contents/Resources/openmessage",
            dataDirectory: "/Users/example/Library/Application Support/OpenMessage",
            port: 8123,
            homeDirectory: "/Users/example",
            appSandboxed: true
        )

        XCTAssertEqual(sandboxed.environment["OPENMESSAGES_APP_SANDBOX"], "1")
        var expected = unsandboxed.environment
        expected["OPENMESSAGES_APP_SANDBOX"] = "1"
        XCTAssertEqual(sandboxed.environment, expected)
        XCTAssertEqual(sandboxed.arguments, unsandboxed.arguments)
        XCTAssertEqual(sandboxed.executableURL, unsandboxed.executableURL)
    }

    func testAppSandboxIsReadFromTheContainerIDTheSandboxSets() {
        XCTAssertFalse(AppSandbox.isActive(environment: [:]))
        XCTAssertFalse(AppSandbox.isActive(environment: ["HOME": "/Users/example"]))
        XCTAssertFalse(AppSandbox.isActive(environment: ["APP_SANDBOX_CONTAINER_ID": ""]))
        XCTAssertFalse(AppSandbox.isActive(environment: ["APP_SANDBOX_CONTAINER_ID": " \n"]))
        XCTAssertTrue(AppSandbox.isActive(environment: ["APP_SANDBOX_CONTAINER_ID": "com.openmessage.app"]))
        // The backend's own flag is an output of this check, never an input:
        // an inherited OPENMESSAGES_APP_SANDBOX=1 does not make the app sandboxed.
        XCTAssertFalse(AppSandbox.isActive(environment: ["OPENMESSAGES_APP_SANDBOX": "1"]))
    }

    func testDefaultSandboxStateIsTheRunningProcess() {
        let configuration = BackendLaunchConfiguration.application(
            executablePath: "/tmp/openmessage",
            dataDirectory: "/tmp/openmessage-data",
            port: 9002,
            homeDirectory: "/tmp/home"
        )

        let expected = AppSandbox.isActive(environment: ProcessInfo.processInfo.environment) ? "1" : "0"
        XCTAssertEqual(configuration.environment["OPENMESSAGES_APP_SANDBOX"], expected)
        // `swift test` and the shipped Developer ID app both run unsandboxed.
        XCTAssertNil(ProcessInfo.processInfo.environment["APP_SANDBOX_CONTAINER_ID"])
        XCTAssertEqual(configuration.environment["OPENMESSAGES_APP_SANDBOX"], "0")
    }

    func testAdditionalEnvironmentCannotSupplyTheSandboxFlag() {
        for (appSandboxed, wanted) in [(false, "0"), (true, "1")] {
            for injected in ["0", "1", "true", ""] {
                let configuration = BackendLaunchConfiguration.application(
                    executablePath: "/tmp/openmessage",
                    dataDirectory: "/tmp/openmessage-data",
                    port: 9003,
                    homeDirectory: "/tmp/home",
                    appSandboxed: appSandboxed,
                    additionalEnvironment: ["OPENMESSAGES_APP_SANDBOX": injected]
                )
                XCTAssertEqual(
                    configuration.environment["OPENMESSAGES_APP_SANDBOX"],
                    wanted,
                    "appSandboxed=\(appSandboxed) injected=\(injected)"
                )
            }
        }
    }

    func testV2PrimaryLeverAddsExactlyOneEnvironmentVariable() {
        let base = BackendLaunchConfiguration.application(
            executablePath: "/Applications/OpenMessage.app/Contents/Resources/openmessage",
            dataDirectory: "/Users/example/Library/Application Support/OpenMessage",
            port: 8123,
            homeDirectory: "/Users/example"
        )
        let primary = BackendLaunchConfiguration.application(
            executablePath: "/Applications/OpenMessage.app/Contents/Resources/openmessage",
            dataDirectory: "/Users/example/Library/Application Support/OpenMessage",
            port: 8123,
            homeDirectory: "/Users/example",
            v2Primary: true
        )

        // Default stays byte-identical to the shipped contract: the lever off
        // must not perturb the environment in any way.
        XCTAssertNil(base.environment["OPENMESSAGES_V2_PRIMARY"])
        XCTAssertEqual(primary.environment["OPENMESSAGES_V2_PRIMARY"], "1")
        var expected = base.environment
        expected["OPENMESSAGES_V2_PRIMARY"] = "1"
        XCTAssertEqual(primary.environment, expected)
    }

    func testAdditionalEnvironmentIsExplicitWithoutOverridingCanonicalFields() {
        let configuration = BackendLaunchConfiguration.application(
            executablePath: "/tmp/openmessage",
            dataDirectory: "/tmp/openmessage-data",
            port: 9000,
            homeDirectory: "/tmp/home",
            additionalEnvironment: [
                "OPENMESSAGES_HOST": "127.0.0.1",
                "OPENMESSAGES_PORT": "9999",
            ]
        )

        XCTAssertEqual(configuration.environment["OPENMESSAGES_HOST"], "127.0.0.1")
        XCTAssertEqual(configuration.environment["OPENMESSAGES_PORT"], "9000")
        XCTAssertEqual(configuration.environment["OPENMESSAGES_LOG_LEVEL"], "info")
    }

    func testFoundationSpawnerAppliesTheCompleteConfiguration() {
        let configuration = BackendLaunchConfiguration.application(
            executablePath: "/tmp/openmessage",
            dataDirectory: "/tmp/openmessage-data",
            port: 9001,
            homeDirectory: "/tmp/home"
        )

        let process = FoundationBackendProcessSpawner().makeProcess(for: configuration)

        XCTAssertEqual(process.executableURL, configuration.executableURL)
        XCTAssertEqual(process.arguments, configuration.arguments)
        XCTAssertEqual(process.environment, configuration.environment)
        XCTAssertEqual(process.currentDirectoryURL, configuration.currentDirectoryURL)
    }
}
