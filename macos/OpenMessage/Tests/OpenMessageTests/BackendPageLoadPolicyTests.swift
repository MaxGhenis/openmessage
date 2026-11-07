import Foundation
import XCTest
@testable import OpenMessage

final class BackendPageLoadPolicyTests: XCTestCase {
    private let backend = URL(string: "http://127.0.0.1:7007/")!

    func testLoadsTheBackendPageOnce() {
        var policy = BackendPageLoadPolicy()
        XCTAssertEqual(policy.request(for: backend)?.url, backend)
        XCTAssertEqual(policy.loadedURL, backend)
        XCTAssertNil(policy.request(for: backend))
    }

    // 2026-11-07: while a conversation was open the page's URL carried
    // `?conversation=`, and every SwiftUI update of the window (Signal
    // reconnecting changed the menu bar alert) reloaded the page and
    // discarded what the user was typing. Updates for the backend the page came
    // from load nothing, however many arrive.
    func testUpdatesForTheSameBackendNeverReload() {
        var policy = BackendPageLoadPolicy()
        XCTAssertNotNil(policy.request(for: backend))
        for _ in 0..<100 {
            XCTAssertNil(policy.request(for: backend))
        }
    }

    // A backend on another port serves the page from another origin, so the
    // page there is a new load, once; returning to the first port loads again.
    func testANewBackendAddressLoadsOnce() {
        var policy = BackendPageLoadPolicy()
        XCTAssertNotNil(policy.request(for: backend))
        let moved = URL(string: "http://127.0.0.1:7008/")!
        XCTAssertEqual(policy.request(for: moved)?.url, moved)
        XCTAssertNil(policy.request(for: moved))
        XCTAssertEqual(policy.request(for: backend)?.url, backend)
    }
}
