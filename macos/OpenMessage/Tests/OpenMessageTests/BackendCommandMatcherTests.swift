import Foundation
import XCTest
@testable import OpenMessage

final class BackendCommandMatcherTests: XCTestCase {
    private let rootBinary = "/Users/example/src/openmessage/openmessage"

    func testExactRootTreeHelperIsReusableForServeWeb() {
        let matcher = BackendCommandMatcher(binaryPath: rootBinary)

        XCTAssertTrue(matcher.isReusableBackendCommand("\(rootBinary) serve --web"))
        XCTAssertTrue(matcher.isOpenMessageBackendCommand("\(rootBinary) serve --web"))
        XCTAssertFalse(matcher.isReusableBackendCommand("\(rootBinary) pair"))
        XCTAssertFalse(matcher.isOpenMessageBackendCommand("\(rootBinary) pair"))
    }

    func testOnlyTheResolvedHelperPathIsReusable() {
        let matcher = BackendCommandMatcher(binaryPath: rootBinary)

        XCTAssertFalse(matcher.isReusableBackendCommand("/tmp/openmessage serve --web"))
        XCTAssertFalse(matcher.isReusableBackendCommand("/usr/local/bin/openmessage serve --web"))
        XCTAssertTrue(matcher.isOpenMessageBackendCommand("/usr/local/bin/openmessage serve --web"))
        XCTAssertFalse(matcher.isOpenMessageBackendCommand("/opt/homebrew/bin/openmessage serve --web"))
    }

    func testResolvedSystemHelperCanBeReused() {
        let matcher = BackendCommandMatcher(binaryPath: "/usr/local/bin/openmessage")

        XCTAssertTrue(matcher.isReusableBackendCommand("/usr/local/bin/openmessage serve --web"))
    }

    func testRecognizesPackagedResourceAndMacOSHelpers() {
        let matcher = BackendCommandMatcher(binaryPath: rootBinary)

        XCTAssertTrue(
            matcher.isOpenMessageBackendCommand(
                "/Applications/OpenMessage.app/Contents/Resources/openmessage serve --web"
            )
        )
        XCTAssertTrue(
            matcher.isOpenMessageBackendCommand(
                "/Applications/OpenMessage.app/Contents/MacOS/openmessage-helper serve --web"
            )
        )
        XCTAssertTrue(
            matcher.isOpenMessageBackendCommand(
                "\\Applications\\OpenMessage.app\\Contents\\Resources\\openmessage serve --web"
            )
        )
    }

    func testRejectsForeignAndNonServeCommands() {
        let matcher = BackendCommandMatcher(binaryPath: rootBinary)

        let commands = [
            "/Applications/Other.app/Contents/Resources/openmessage serve --web",
            "/Applications/Other.app/Contents/MacOS/openmessage-helper serve --web",
            "/Applications/OpenMessage.app/Contents/Resources/openmessage pair",
            "/usr/local/bin/openmessage pair",
            "/bin/sh -c serve --web",
            "/tmp/not-openmessage serve --web",
        ]

        for command in commands {
            XCTAssertFalse(
                matcher.isOpenMessageBackendCommand(command),
                "Unexpectedly recognized foreign command: \(command)"
            )
        }
    }
}
