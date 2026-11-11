import Foundation
import XCTest
@testable import OpenMessage

/// Drives `BackendPageLoader` the way `WebViewContainer` and its coordinator
/// do: `show` on every window update, `navigationFailed` from the navigation
/// delegate.
@MainActor
final class BackendPageLoaderTests: XCTestCase {
    private let backend = URL(string: "http://127.0.0.1:7007")!
    private let movedBackend = URL(string: "http://127.0.0.1:7008")!
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

    /// Waits up to 5 s for the recorder to see `count` loads.
    private func waitForLoads(_ recorder: Recorder, count: Int) async {
        for _ in 0..<500 where recorder.loads.count < count {
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

    // A backend that refuses connections gets one load per pause, not one
    // per main-actor turn.
    func testWaitsThePauseBeforeRetrying() async {
        let recorder = Recorder()
        let loader = makeLoader(recorder, retryDelay: .seconds(1))
        loader.show(backend)
        loader.navigationFailed(refused)
        try? await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(recorder.loads, [backend])
        await waitForLoads(recorder, count: 2)
        XCTAssertEqual(recorder.loads, [backend, backend])
    }

    // Two failure reports for one navigation load the page once more.
    func testRepeatedFailuresLoadThePageOnceMore() async {
        let recorder = Recorder()
        let loader = makeLoader(recorder)
        loader.show(backend)
        loader.navigationFailed(refused)
        loader.navigationFailed(refused)
        await waitForLoads(recorder, count: 2)
        try? await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(recorder.loads, [backend, backend])
    }

    // A window update during the pause loads the page; the retry then finds
    // a page loading and does nothing.
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

    // The retry never loads the old address over a newer one.
    func testANewBackendAddressDuringThePauseWins() async {
        let recorder = Recorder()
        let loader = makeLoader(recorder, retryDelay: .milliseconds(50))
        loader.show(backend)
        loader.navigationFailed(refused)
        loader.show(movedBackend)
        try? await Task.sleep(for: .milliseconds(200))
        XCTAssertEqual(recorder.loads, [backend, movedBackend])
    }

    // Two pending retries: the newer failure's retry replaces the older
    // one, so the old address never loads over the new.
    func testOnlyTheNewestFailureRetries() async {
        let recorder = Recorder()
        let loader = makeLoader(recorder, retryDelay: .milliseconds(50))
        loader.show(backend)
        loader.navigationFailed(refused)
        loader.show(movedBackend)
        loader.navigationFailed(refused)
        await waitForLoads(recorder, count: 3)
        try? await Task.sleep(for: .milliseconds(150))
        XCTAssertEqual(recorder.loads, [backend, movedBackend, movedBackend])
    }

    // The pending retry doesn't keep a loader alive after its web view has
    // gone, and loads nothing once it has.
    func testALoaderGoneDuringThePauseLoadsNothing() async {
        let recorder = Recorder()
        var loader: BackendPageLoader? = makeLoader(recorder, retryDelay: .milliseconds(50))
        loader?.show(backend)
        loader?.navigationFailed(refused)
        weak var released: BackendPageLoader?
        released = loader
        loader = nil
        XCTAssertNil(released)
        try? await Task.sleep(for: .milliseconds(200))
        XCTAssertEqual(recorder.loads, [backend])
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
