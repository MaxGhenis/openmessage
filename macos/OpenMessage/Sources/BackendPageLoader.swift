import Foundation

/// Loads the backend's page into the app's web view: once per backend
/// address, however often the window updates (see `BackendPageLoadPolicy`),
/// and again after a load that never arrived.
@MainActor
final class BackendPageLoader {
    private var policy = BackendPageLoadPolicy()
    private let retryDelay: Duration
    private let load: (URLRequest) -> Void
    private var retry: Task<Void, Never>?

    /// `load` hands a request to the web view.
    init(retryDelay: Duration = .seconds(2), load: @escaping (URLRequest) -> Void) {
        self.retryDelay = retryDelay
        self.load = load
    }

    /// Shows the page from `backendURL`. Loads it only when the page didn't
    /// already come from that address, so window updates never reload it.
    func show(_ backendURL: URL) {
        if let request = policy.request(for: backendURL) {
            load(request)
        }
    }

    /// Handles a navigation that failed before any page arrived. Without a
    /// retry, window updates (which load only a new backend address) would
    /// leave the window blank. If the backend itself has gone, BackendManager
    /// leaves `.running` and removes the web view, and with it this loader.
    /// The retry does nothing if a page started loading during the pause,
    /// and a later failure replaces a pending retry, so a retry never
    /// replaces a window update's load or loads an older backend address.
    func navigationFailed(_ error: Error) {
        guard BackendPageLoadPolicy.shouldRetry(after: error),
              let failedURL = policy.loadFailed() else { return }
        retry?.cancel()
        let delay = retryDelay
        retry = Task { [weak self] in
            try? await Task.sleep(for: delay)
            guard !Task.isCancelled, let self, self.policy.loadedURL == nil else { return }
            self.show(failedURL)
        }
    }
}
