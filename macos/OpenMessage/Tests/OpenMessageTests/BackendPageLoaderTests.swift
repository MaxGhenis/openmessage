import Foundation
import XCTest
@testable import OpenMessage

/// Drives `BackendPageLoader` the way `WebViewContainer` and its coordinator
/// do: `show` on every window update, `navigationFailed` from the navigation
/// delegate.
@MainActor
final class BackendPageLoaderTests: XCTestCase {
    private let backend = URL(string: "http://127.0.0.1:7007")!
    private let refused = NSError(domain: NSURLErrorDomain, code: NSURLErrorCannotConnectToHost)
    private let replaced = NSError(domain: NSURLErrorDomain, code: NSURLErrorCancelled)

    private final class Recorder {
        var loads: [URL?] = []
    }

    private func makeLoader(_ recorder: Recorder, retryDelay: Duration = .milliseconds(20)) -> BackendPageLoader {
        BackendPageLoader(retryDelay: retryDelay) { request in
            recorder.loads.append(request.url)
        }
    }

    private func waitForLoads(_ recorder: Recorder, count: Int) async {
        for _ in 0..<100 where recorder.loads.count < count {
            try? await Task.sleep(for: .milliseconds(10))
        }
    }

    // 2026-11-07: every window update reloaded the page and discarded
    // unsent text. However many updates arrive, the page loads once.
    func testWindowUpdatesLoadThePageOnce() {
        let recorder = Recorder()
        let loader = makeLoader(recorder)
        for _ in 0..<100 {
            loader.show(backend)
        }
        XCTAssertEqual(recorder.loads, [backend])
    }

    func testRetriesAPageThatNeverArrived() async {
        let recorder = Recorder()
        let loader = makeLoader(recorder)
        loader.show(backend)
        loader.navigationFailed(refused)
        await waitForLoads(recorder, count: 2)
        XCTAssertEqual(recorder.loads, [backend, backend])
        loader.show(backend)
        XCTAssertEqual(recorder.loads, [backend, backend])
    }

    func testRepeatedFailuresScheduleOneRetry() async {
        let recorder = Recorder()
        let loader = makeLoader(recorder)
        loader.show(backend)
        loader.navigationFailed(refused)
        loader.navigationFailed(refused)
        await waitForLoads(recorder, count: 3)
        XCTAssertEqual(recorder.loads, [backend, backend])
    }

    // A window update during the pause loads the page; the retry then finds
    // it loaded and does nothing.
    func testAWindowUpdateDuringTheRetryPauseLoadsOnce() async {
        let recorder = Recorder()
        let loader = makeLoader(recorder, retryDelay: .milliseconds(50))
        loader.show(backend)
        loader.navigationFailed(refused)
        loader.show(backend)
        XCTAssertEqual(recorder.loads, [backend, backend])
        try? await Task.sleep(for: .milliseconds(200))
        XCTAssertEqual(recorder.loads, [backend, backend])
    }

    // A navigation that another one replaced left a page in place.
    func testIgnoresAReplacedNavigation() async {
        let recorder = Recorder()
        let loader = makeLoader(recorder)
        loader.show(backend)
        loader.navigationFailed(replaced)
        try? await Task.sleep(for: .milliseconds(100))
        loader.show(backend)
        XCTAssertEqual(recorder.loads, [backend])
    }
}
