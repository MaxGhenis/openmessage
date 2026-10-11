import XCTest
@testable import OpenMessage

final class BackendStartupWaitTests: XCTestCase {
    private let wait = BackendStartupWait()

    func testWaitsThroughTheGracePeriodWhateverTheProcessState() {
        XCTAssertEqual(wait.decide(elapsed: .seconds(1), processAlive: true), .keepWaiting)
        XCTAssertEqual(wait.decide(elapsed: .seconds(29), processAlive: false), .keepWaiting)
    }

    func testKeepsWaitingForALiveBackendThatIsStillMigrating() {
        // A 2.2M-message store took 47 s to build its search indexes.
        XCTAssertEqual(wait.decide(elapsed: .seconds(47), processAlive: true), .keepWaiting)
        XCTAssertEqual(wait.decide(elapsed: .seconds(599), processAlive: true), .keepWaiting)
    }

    func testGivesUpOnADeadBackendOnceTheGracePeriodEnds() {
        XCTAssertEqual(
            wait.decide(elapsed: .seconds(30), processAlive: false),
            .giveUp("Backend failed to start within 30 seconds")
        )
    }

    func testGivesUpOnALiveBackendAtTheCeiling() {
        XCTAssertEqual(
            wait.decide(elapsed: .seconds(600), processAlive: true),
            .giveUp("Backend did not become ready within 10 minutes")
        )
    }

    func testCustomBoundsAreReportedInTheMessage() {
        let short = BackendStartupWait(grace: .seconds(5), ceiling: .seconds(60))
        XCTAssertEqual(short.decide(elapsed: .seconds(5), processAlive: false), .giveUp("Backend failed to start within 5 seconds"))
        XCTAssertEqual(short.decide(elapsed: .seconds(60), processAlive: true), .giveUp("Backend did not become ready within 1 minute"))
    }
}

@MainActor
final class BackendStartupPollTests: XCTestCase {
    // The production bounds scaled down: a 50 ms grace, a 2 s ceiling.
    private let wait = BackendStartupWait(grace: .milliseconds(50), ceiling: .seconds(2))

    func testABackendThatAnswersAfterTheGracePeriodIsReady() async {
        // Like a backend still building its search indexes at the grace
        // period's end: alive, silent, then ready.
        let readyAfter = ContinuousClock.now + .milliseconds(300)
        var slowStartReported = 0
        let outcome = await wait.poll(
            interval: .milliseconds(10),
            probe: { ContinuousClock.now >= readyAfter },
            processAlive: { true },
            onSlowStart: { _ in slowStartReported += 1 }
        )
        guard case .ready = outcome else {
            return XCTFail("outcome = \(outcome), want ready")
        }
        XCTAssertEqual(slowStartReported, 1, "the slow start is reported once")
    }

    func testADeadBackendIsReportedOnceTheGracePeriodEnds() async {
        let started = ContinuousClock.now
        let outcome = await wait.poll(interval: .milliseconds(10), probe: { false }, processAlive: { false })
        XCTAssertEqual(outcome, .gaveUp("Backend failed to start within 0 seconds"))
        XCTAssertLessThan(ContinuousClock.now - started, .seconds(1))
    }

    func testALiveBackendThatNeverAnswersHitsTheCeiling() async {
        let outcome = await wait.poll(interval: .milliseconds(20), probe: { false }, processAlive: { true })
        XCTAssertEqual(outcome, .gaveUp("Backend did not become ready within 2 seconds"))
    }

    func testCancellationEndsTheWait() async {
        let task = Task { await wait.poll(interval: .milliseconds(10), probe: { false }, processAlive: { true }) }
        try? await Task.sleep(for: .milliseconds(50))
        task.cancel()
        let outcome = await task.value
        XCTAssertEqual(outcome, .cancelled)
    }
}
