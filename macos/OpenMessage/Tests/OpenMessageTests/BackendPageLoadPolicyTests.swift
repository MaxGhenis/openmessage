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

    // 2026-11-07: the web view's URL never equalled the backend address
    // (WebKit adds a trailing slash, and the page adds `?conversation=`), so
    // every SwiftUI update of the window (Signal reconnecting changed the menu
    // bar alert) reloaded the page and discarded what the user was typing.
    // Updates for the backend the page came from load nothing, however many
    // arrive.
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

    // A page that never arrived must load again: window updates alone would
    // otherwise leave the window blank.
    func testAFailedLoadLoadsAgainOnce() {
        var policy = BackendPageLoadPolicy()
        XCTAssertNotNil(policy.request(for: backend))
        XCTAssertEqual(policy.loadFailed(), backend)
        XCTAssertNil(policy.loadedURL)
        XCTAssertEqual(policy.request(for: backend)?.url, backend)
        XCTAssertNil(policy.request(for: backend))
    }

    func testRetriesOnlyWhenThePageNeverArrived() {
        let refused = NSError(domain: NSURLErrorDomain, code: NSURLErrorCannotConnectToHost)
        let replaced = NSError(domain: NSURLErrorDomain, code: NSURLErrorCancelled)
        let policyStopped = NSError(
            domain: "WebKitErrorDomain",
            code: BackendPageLoadPolicy.frameLoadInterruptedByPolicyChange
        )
        XCTAssertTrue(BackendPageLoadPolicy.shouldRetry(after: refused))
        XCTAssertFalse(BackendPageLoadPolicy.shouldRetry(after: replaced))
        XCTAssertFalse(BackendPageLoadPolicy.shouldRetry(after: policyStopped))
    }
}
