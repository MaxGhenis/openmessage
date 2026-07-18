import Foundation
import XCTest
@testable import OpenMessage

final class BackendLaunchConfigurationTests: XCTestCase {
    func testApplicationConfigurationPreservesBackendLaunchContract() {
        let configuration = BackendLaunchConfiguration.application(
            executablePath: "/Applications/OpenMessage.app/Contents/Resources/openmessage",
            dataDirectory: "/Users/example/Library/Application Support/OpenMessage",
            port: 8123,
            homeDirectory: "/Users/example"
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
                "OPENMESSAGES_APP_SANDBOX": "1",
                "OPENMESSAGES_MACOS_NOTIFICATIONS": "0",
                "HOME": "/Users/example",
                "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
            ]
        )
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
