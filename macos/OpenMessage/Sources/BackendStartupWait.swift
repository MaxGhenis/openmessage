import Foundation

/// Decides how long the app keeps waiting for a starting backend to answer
/// `/api/status`.
///
/// The backend applies pending store migrations before it opens its HTTP
/// listener. Most take well under a second, but building the v2 store's search
/// indexes (migration 0012) took 1.7–4.7 s on a copy of a 72k-message store,
/// 13–34 s at 722k messages and 47 s at 2.2M. A flat 30-second deadline would
/// leave the app on its error screen while a healthy backend finished
/// migrating. So past the grace period the app keeps waiting as long as the
/// backend process is alive, up to `ceiling`; a backend that dies is the
/// termination handler's to restart or report.
struct BackendStartupWait: Equatable {
    enum Decision: Equatable {
        case keepWaiting
        case giveUp(String)
    }

    /// How long a backend may take to answer before its liveness matters.
    var grace: Duration = .seconds(30)
    /// How long the app keeps polling a live backend that has not answered.
    /// It is a polling threshold: a probe already in flight when it passes
    /// still counts if the backend answers.
    var ceiling: Duration = .seconds(600)

    func decide(elapsed: Duration, processAlive: Bool) -> Decision {
        if elapsed < grace {
            return .keepWaiting
        }
        if !processAlive {
            return .giveUp("Backend failed to start within \(Self.describe(grace))")
        }
        if elapsed < ceiling {
            return .keepWaiting
        }
        return .giveUp("Backend did not become ready within \(Self.describe(ceiling))")
    }

    private static func describe(_ duration: Duration) -> String {
        let seconds = duration.components.seconds
        if seconds >= 60, seconds % 60 == 0 {
            let minutes = seconds / 60
            return minutes == 1 ? "1 minute" : "\(minutes) minutes"
        }
        return "\(seconds) seconds"
    }
}

extension BackendStartupWait {
    enum Outcome: Equatable {
        /// The backend answered on the given check.
        case ready(attempt: Int)
        case gaveUp(String)
        case cancelled
    }

    /// Polls `probe` every `interval` until it reports the backend ready, the
    /// wait gives up (`decide`), or the task is cancelled. Cancellation wins
    /// over a probe's result, so a stop that cancels an in-flight probe never
    /// sees that probe's outcome applied. `processAlive` is consulted only
    /// after the grace period. `onSlowStart` runs once, the first time the wait
    /// continues past the grace period. It runs on the main actor with its
    /// caller, `BackendManager`.
    @MainActor
    func poll(
        interval: Duration = .milliseconds(500),
        probe: () async -> Bool,
        processAlive: () -> Bool,
        onSlowStart: (Int) -> Void = { _ in }
    ) async -> Outcome {
        let clock = ContinuousClock()
        let started = clock.now
        var attempt = 0
        var reportedSlowStart = false
        while !Task.isCancelled {
            attempt += 1
            try? await Task.sleep(for: interval)
            if Task.isCancelled { return .cancelled }
            let answered = await probe()
            if Task.isCancelled { return .cancelled }
            if answered {
                return .ready(attempt: attempt)
            }
            let elapsed = clock.now - started
            let pastGrace = elapsed >= grace
            switch decide(elapsed: elapsed, processAlive: !pastGrace || processAlive()) {
            case .keepWaiting:
                if pastGrace && !reportedSlowStart {
                    reportedSlowStart = true
                    onSlowStart(attempt)
                }
            case .giveUp(let message):
                return .gaveUp(message)
            }
        }
        return .cancelled
    }
}
